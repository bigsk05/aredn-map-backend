package scanner

import (
	"context"
	"time"

	"go.uber.org/zap"

	"aredn-map/internal/model"
	"aredn-map/internal/repository/aredn"
)

// expand crawls one node's record and links, then queues everything it points at.
func (e *Engine) expand(ctx context.Context, t task, enqueue func(task), pool *probePool) {
	if ctx.Err() != nil {
		return
	}
	// A parked peer stays parked even when a reachable neighbour names it again.
	// Seeds are exempt: holding back the measuring point would leave nothing to report.
	if t.hop > 0 && e.heldBack(t.host) {
		return
	}
	e.busy.Add(1)
	defer e.busy.Add(-1)
	e.patch(func(s *model.ScanStatus) { s.CurrentTarget = t.host })

	self, err := e.cl.FetchSelf(ctx, t.host)
	if err != nil {
		// A node that fails /a/status fails everything, so it stays visible as a stub.
		e.st.UpsertNode(&model.Node{Hostname: t.host, URL: aredn.NodeURL(t.host), Hops: t.hop, Source: "neighbor-list"})
		e.markFailed(t.host, err)
		return
	}
	if self == nil {
		self = &model.Node{Hostname: t.host}
	}
	self.Hops = t.hop
	if self.Source == "" {
		self.Source = "status-page"
	}
	self.Crawled = true

	// A seed alias and the name neighbours report are one box, so re-key onto one record.
	key := t.host
	if canon := NormalizeHost(self.CanonicalHost); canon != "" && canon != t.host {
		self.Aliases = append(self.Aliases, t.host)
		self.Hostname = canon
		e.st.DeleteNode(t.host)
		key = canon
	}
	e.st.UpsertNode(self)
	e.recordHop(key, t.hop)
	// Answering its own page is proof it is back, whoever parked it.
	e.st.MarkReached(key)
	// Liveness is recorded on reach, so the online count climbs during discovery.
	pool.enqueue(key)

	// Beyond DeferDetailBeyondHops only the roster is read; detail is a second frontier lane.
	deferBeyond := e.cfg.Scan.DeferDetailBeyondHops
	detail := deferBeyond < 0 || t.hop <= deferBeyond || t.detail
	if !t.detail && deferBeyond >= 0 && t.hop > deferBeyond {
		enqueue(task{host: t.host, hop: t.hop, detail: true})
	}
	links, lerr := e.cl.FetchNeighbors(ctx, t.host, detail, e.cfg.Scan.DetailConcurrency)
	if lerr != nil {
		e.log.Warn("neighborhood fetch failed", zap.String("host", t.host), zap.Error(lerr))
		e.markFailed(t.host, lerr)
		return
	}

	now := time.Now()
	names := make([]string, 0, len(links))
	byType := make(map[string]int, 4)
	for _, l := range links {
		l.ScannedAt = now
		// LastSeen is the peer's reported link history, not this scan's timestamp.
		if l.LastSeen.IsZero() {
			l.LastSeen = now
		}
		// Keep the edge on the canonical record, not the alias we crawled under.
		l.From = key
		// A radio link sits on the node's own channel, so its band labels links with none.
		if l.Band == "" {
			l.Band = self.Band
		}
		byType[l.Type]++
		e.st.UpsertLink(l)
		// A row with no resolvable peer hostname is adjacency but not a node.
		if l.To != "" {
			e.st.UpsertNode(peerStub(&l, t.hop+1))
			e.st.MarkMeshState(l.To, l.State, l.LastSeen)
			e.recordHop(l.To, t.hop+1)
			names = append(names, l.To)
		}
		enqueue(task{host: l.To, hop: t.hop + 1})
	}
	e.st.MergeNode(key, func(n *model.Node) {
		n.Neighbors = append(n.Neighbors, names...)
		n.LinkTypes = byType
	})

	if detail && lerr == nil {
		e.st.MarkDetailFetched(key)
	}

	if e.cfg.Scan.AbsorbMeshHosts && detail {
		e.absorbMeshHosts(ctx, key)
	}

	e.patch(func(s *model.ScanStatus) {
		s.Completed++
		s.Succeeded++
	})
}

// absorbMeshHosts registers a node's /a/mesh host table as known names with no route proven.
func (e *Engine) absorbMeshHosts(ctx context.Context, host string) {
	md, err := e.cl.FetchMesh(ctx, host)
	if err != nil || md == nil {
		if err != nil {
			e.log.Debug("mesh host table unavailable", zap.String("host", host), zap.Error(err))
		}
		return
	}
	e.log.Info("absorbed mesh host table", zap.String("host", host),
		zap.Int("hosts", len(md.Hosts)), zap.Int("etx", len(md.ETX)))

	limit := e.cfg.Scan.MaxNodes
	for ip, names := range md.Hosts {
		if len(names) == 0 {
			continue
		}
		// max_nodes bounds this run's whole footprint, including mesh-host absorption.
		if limit > 0 && e.st.NodeCount() >= limit {
			e.log.Debug("mesh host absorption capped", zap.String("host", host), zap.Int("limit", limit))
			return
		}
		id := NormalizeHost(names[0])
		aliases := names[1:]
		etx, hasETX := md.ETX[ip]
		e.st.MergeNode(id, func(n *model.Node) {
			if n.Name == "" {
				n.Name = names[0]
			}
			// The /a/mesh table keys are mesh addresses by construction, trusted as such.
			if n.MeshIP == "" {
				n.MeshIP = ip
			}
			if n.URL == "" {
				n.URL = aredn.NodeURL(id)
			}
			if n.Source == "" {
				n.Source = "mesh-list"
			}
			// A peer named only here has unknown distance: Hops -1, not a hop count.
			if n.Hops == 0 && !n.Crawled {
				n.Hops = -1
			}
			if hasETX && n.ETX == 0 {
				n.ETX = etx
			}
			n.Aliases = append(n.Aliases, aliases...)
			stampSupernode(n, append([]string{id, names[0]}, aliases...)...)
		})
	}
}
