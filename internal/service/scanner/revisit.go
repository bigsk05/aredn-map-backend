package scanner

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"aredn-map/internal/model"
	"aredn-map/internal/repository/aredn"
)

func (e *Engine) parkAfterFailure(host, why string) {
	e.st.MarkFailure(host, why, e.revisitDue())
}

// parkUnanswered parks a silent node with no reason; MarkProbe already stored the ICMP one.
func (e *Engine) parkUnanswered(host string) {
	e.st.MarkFailure(host, "", e.revisitDue())
}

// heldBack reports whether a node is parked and not yet due for another look.
func (e *Engine) heldBack(host string) bool {
	n, ok := e.st.NodeByID(host)
	if !ok {
		return false
	}
	return !n.RevisitAt.IsZero() && n.RevisitAt.After(time.Now())
}

// revisitDue is the next look-at time, or zero when scan.revisit_after is off.
func (e *Engine) revisitDue() time.Time {
	d := e.cfg.Scan.RevisitAfter
	if d <= 0 {
		return time.Time{}
	}
	return time.Now().Add(d)
}

func (e *Engine) revisit(ctx context.Context) {
	e.mu.Lock()
	start := e.sweepStart
	e.mu.Unlock()
	ids := e.st.IDsDueForRevisit(revisitBatch, start)
	if len(ids) == 0 {
		return
	}
	e.patch(func(s *model.ScanStatus) {
		s.Phase = model.PhaseRevisit
		s.NodeTotal, s.NodeProgress = len(ids), 0
	})
	e.log.Info("revisiting parked nodes", zap.Int("hosts", len(ids)))

	var wg sync.WaitGroup
	sem := make(chan struct{}, max1(e.cfg.Scan.WorkerGoroutines))
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		// Unpark before looking: the attempt re-parks it if it is still dead.
		e.st.MarkReached(id)
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// A detailed node needs only a liveness check; others need the full read.
			if n, ok := e.st.NodeByID(host); ok && n.DetailFetched {
				e.probeOne(ctx, host)
				return
			}
			e.refreshOne(ctx, host)
		}(id)
	}
	wg.Wait()
	e.patch(func(s *model.ScanStatus) { s.NodeProgress = len(ids) })
}

// revisitBatch caps how many parked nodes one sweep looks at. It must stay above the number a
// mesh of a few thousand parks in one run, or revisit_after is a promise the queue cannot keep.
const revisitBatch = 500

func (e *Engine) refreshOne(ctx context.Context, host string) {
	if ctx.Err() != nil {
		return
	}
	e.busy.Add(1)
	defer e.busy.Add(-1)
	e.patch(func(s *model.ScanStatus) { s.CurrentTarget = host })

	// The node's known band labels links whose dialog gave no frequency of their own.
	band := ""
	if self, err := e.cl.FetchSelf(ctx, host); err == nil && self != nil {
		// Answering its own page sets Crawled, so the probe set includes it.
		self.Crawled = true
		e.st.UpsertNode(self)
		// A page answer proves it is back: leave the lane, get a verdict this sweep.
		e.st.MarkReached(host)
		e.probeOne(ctx, host)
		band = self.Band
	} else if err != nil {
		e.markFailed(host, err)
		return
	}

	links, err := e.cl.FetchNeighbors(ctx, host, true, e.cfg.Scan.DetailConcurrency)
	if err != nil {
		e.log.Debug("detail neighborhood failed", zap.String("host", host), zap.Error(err))
		return
	}
	now := time.Now()
	names := make([]string, 0, len(links))
	byType := make(map[string]int, 4)
	for _, l := range links {
		l.ScannedAt = now
		if l.LastSeen.IsZero() {
			l.LastSeen = now
		}
		if l.Band == "" {
			l.Band = band
		}
		byType[l.Type]++
		e.st.UpsertLink(l)
		if l.To != "" {
			e.st.UpsertNode(peerStub(&l, -1))
			e.st.MarkMeshState(l.To, l.State, l.LastSeen)
			names = append(names, l.To)
		}
	}
	e.st.MergeNode(host, func(n *model.Node) {
		n.Neighbors = append(n.Neighbors, names...)
		n.LinkTypes = byType
	})
	e.st.MarkDetailFetched(host)
	e.patch(func(s *model.ScanStatus) { s.NodeProgress++; s.Completed++ })
}

func (e *Engine) markFailed(host string, err error) {
	e.parkAfterFailure(NormalizeHost(host), err.Error())
	e.patch(func(s *model.ScanStatus) { s.Failed++ })
	e.log.Debug("node unreachable", zap.String("host", host), zap.Error(err))
}

// peerStub builds a plottable record for a peer seen only in a link dialog; hops < 0 is unknown.
// The dialog's "ip address" is a tunnel or DtD endpoint, so it is never stored as MeshIP.
func peerStub(l *model.Link, hops int) *model.Node {
	name := l.PeerName
	if name == "" {
		name = l.To
	}
	n := &model.Node{
		Hostname:    l.To,
		Name:        name,
		URL:         aredn.NodeURL(l.To),
		Hardware:    l.PeerHardware,
		Firmware:    l.PeerFirmware,
		Lat:         l.PeerLat,
		Lon:         l.PeerLon,
		HasLocation: l.PeerLat != 0 || l.PeerLon != 0,
		Source:      "link-dialog",
		Hops:        hops,
	}
	stampSupernode(n, n.Name, l.To)
	return n
}

// NormalizeHost gives the lowercase store key shared with Link.To; IP forms are kept as they are.
