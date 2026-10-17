package scanner

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"aredn-map/internal/model"
	"aredn-map/internal/repository/store"
)

func (e *Engine) Sweep(ctx context.Context, kind string) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return ErrAlreadyRunning
	}
	e.running = true
	sweepCtx, cancel := context.WithTimeout(ctx, e.cfg.Scan.SweepTimeout)
	e.cancel = cancel
	id := int(e.seq.Add(1))
	e.status = model.ScanStatus{
		ID:        id,
		Kind:      kind,
		Phase:     model.PhaseDiscovery,
		Status:    "running",
		StartedAt: time.Now().Format(time.RFC3339),
		NodeTotal: len(e.cfg.Scan.Seeds),
	}
	// The revisit pass anchors due times on this instant, so a park made now is not due now.
	e.sweepStart = time.Now()
	e.mu.Unlock()
	e.st.BumpRev()

	defer func() {
		cancel()
		e.mu.Lock()
		e.running = false
		e.cancel = nil
		e.mu.Unlock()
	}()

	e.log.Info("sweep started", zap.Int("id", id), zap.String("kind", kind),
		zap.Int("max_hops", e.cfg.Scan.MaxHops), zap.Int("max_nodes", e.cfg.Scan.MaxNodes),
		zap.Int("workers", e.cfg.Scan.WorkerGoroutines), zap.Strings("seeds", e.cfg.Scan.Seeds))

	pool := e.newProbePool(sweepCtx)
	e.discover(sweepCtx, pool)
	pool.close()
	if sweepCtx.Err() == nil {
		e.probe(sweepCtx)
		e.refreshDetail(sweepCtx)
		e.revisit(sweepCtx)
	}

	failed := e.Status().Failed
	phase, status := model.PhaseCompleted, "completed"
	switch {
	case errors.Is(sweepCtx.Err(), context.DeadlineExceeded):
		phase, status = model.PhaseFailed, "timed out"
	case errors.Is(sweepCtx.Err(), context.Canceled):
		phase, status = model.PhaseStopped, "stopped"
	case failed > 0:
		status = "completed with failures"
	}
	e.patch(func(s *model.ScanStatus) {
		s.Phase = phase
		s.Status = status
		s.CompletedAt = time.Now().Format(time.RFC3339)
		s.CurrentTarget = ""
		// Discovery has no known denominator, so a finished sweep closes its own total.
		if s.Total < s.Completed+s.Failed {
			s.Total = s.Completed + s.Failed
		}
	})
	e.log.Info("sweep finished", zap.Int("id", id), zap.String("status", status),
		zap.Int("nodes", e.st.NodeCount()), zap.Any("counts", e.st.Counts()))
	return sweepCtx.Err()
}

// discover runs the neighbour-driven BFS, fanning each expanded node's links into the frontier.
func (e *Engine) discover(ctx context.Context, pool *probePool) {
	cfg := e.cfg.Scan
	var (
		wg      sync.WaitGroup
		visitMu sync.Mutex
		visited = map[string]bool{}
		sem     = make(chan struct{}, max1(cfg.WorkerGoroutines))
		opened  int
	)

	var enqueue func(t task)
	enqueue = func(t task) {
		// One slot per lane: the roster pass and the detail pass never suppress each other.
		lane := t.host
		if t.detail {
			lane += "\x00detail"
		}
		visitMu.Lock()
		blocked := t.host == "" || visited[lane] || (cfg.MaxHops >= 0 && t.hop > cfg.MaxHops) ||
			(cfg.MaxNodes > 0 && !t.detail && opened >= cfg.MaxNodes)
		if !blocked {
			visited[lane] = true
			// max_nodes counts distinct nodes, so a detail follow-up takes no second slot.
			if !t.detail {
				opened++
			}
		}
		visitMu.Unlock()
		if blocked {
			return
		}

		// Add while the parent is still counted, so the group never reads zero mid-expansion.
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			e.expand(ctx, t, enqueue, pool)
		})
	}

	for _, seed := range cfg.Seeds {
		enqueue(task{host: NormalizeHost(seed), hop: 0})
	}
	wg.Wait()
	e.patch(func(s *model.ScanStatus) { s.NodeTotal = e.st.NodeCount() })
}

// recordHop stores the lowest hop count seen; store upserts never clobber a measured field.
func (e *Engine) recordHop(id string, hop int) {
	if hop < 0 {
		return
	}
	e.st.MergeNode(id, func(n *model.Node) {
		// 0 is the crawl root's distance, not an absence; a longer route must not relabel it.
		store.MergeHops(n, hop)
	})
}

// stampSupernode applies the mesh naming convention to nodes built without a status page.
func stampSupernode(n *model.Node, parts ...string) {
	if model.LooksLikeSupernode(parts...) {
		n.IsSupernode = true
		n.Band = model.BandSupernode
		n.BandLabel = model.BandLabels[n.Band]
	}
}

// probe re-checks every node whose verdict predates this sweep, so a node that died between
// sweeps cannot stay online on the strength of an old ping. Parked nodes are the revisit lane's.
func (e *Engine) probe(ctx context.Context) {
	start := e.sweepStart
	nodes := e.st.Nodes()
	hosts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.ProbedAt.Before(start) && n.RevisitAt.IsZero() {
			hosts = append(hosts, n.Hostname)
		}
	}
	if len(hosts) == 0 {
		return
	}
	e.patch(func(s *model.ScanStatus) {
		s.Phase = model.PhaseProbe
		s.NodeTotal, s.NodeProgress = len(hosts), 0
	})
	e.log.Info("probing nodes", zap.Int("hosts", len(hosts)), zap.String("icmp_mode", e.pg.Mode()))

	// Chunked so verdicts land while the phase runs: one batch over a whole mesh
	// reports nothing until it is over, and loses everything if the sweep is cut short.
	done := 0
	for start := 0; start < len(hosts); start += probeChunk {
		if ctx.Err() != nil {
			break
		}
		batch := hosts[start:min(start+probeChunk, len(hosts))]
		results := e.pg.PingMany(ctx, batch)
		for host, r := range results {
			e.recordProbe(NormalizeHost(host), r)
		}
		done += len(batch)
		e.patch(func(s *model.ScanStatus) { s.NodeProgress = done })
	}
}

// probeChunk is how many hosts one probe round commits at a time.
const probeChunk = 64

// refreshDetail is the safety net for the detail lane's misses and RelazyAfter re-laxes.
func (e *Engine) refreshDetail(ctx context.Context) {
	ids := e.st.IDsNeedingDetail(e.cfg.Scan.MaxHops, e.cfg.Scan.RelazyAfter)
	if n := e.cfg.Scan.MaxNodes; n > 0 && len(ids) > n {
		ids = ids[:n]
	}
	if len(ids) == 0 {
		return
	}
	e.patch(func(s *model.ScanStatus) {
		s.Phase = model.PhaseDetail
		s.NodeTotal, s.NodeProgress = len(ids), 0
	})
	e.log.Info("refreshing detail", zap.Int("candidates", len(ids)))

	var wg sync.WaitGroup
	sem := make(chan struct{}, max1(e.cfg.Scan.WorkerGoroutines))
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e.refreshOne(ctx, host)
		}(id)
	}
	wg.Wait()
}

// revisit re-checks parked nodes whose deadline, anchored to the sweep start, has passed.
