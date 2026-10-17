package store

import (
	"slices"
	"strings"
	"time"

	"aredn-map/internal/model"
)

// Nodes returns a deep copy of every known node, ordered by display name.
func (s *Store) Nodes() []*model.Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneNodes(s.nodes)
}

// NodeByID returns a copy of one node; mutating it cannot affect the store.
func (s *Store) NodeByID(id string) (*model.Node, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	n, ok := s.nodes[canonicalID(id, "")]
	if !ok {
		return nil, false
	}
	return cloneNode(n), true
}

// Links returns copies of every edge, ordered by from, to, mac.
func (s *Store) Links() []model.Link {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return copyLinks(s.links)
}

// Counts is the summary block; see countsLocked for what each field means.
func (s *Store) Counts() model.Counts {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.countsLocked()
}

// BandCounts is nodes per band key; an unclassified node counts as unknown, not under an empty key.
func (s *Store) BandCounts() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return bandCountsLocked(s.nodes)
}

// LinkTypeCounts is edges per link type (RF, DtD, Wireguard, Local).
func (s *Store) LinkTypeCounts() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return linkTypeCountsLocked(s.links)
}

// Seeds returns a copy of the configured entry points.
func (s *Store) Seeds() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.seeds)
}

// NodeCount is the number of known nodes.
func (s *Store) NodeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.nodes)
}

// Snapshot is one coherent read of everything the API needs, taken under a single blocking lock.
type Snapshot struct {
	Nodes          []*model.Node
	Links          []model.Link
	Counts         model.Counts
	BandCounts     map[string]int
	LinkTypeCounts map[string]int
	TakenAt        time.Time
}

// Snapshot returns deep copies of the whole mesh picture.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodes := cloneNodes(s.nodes)
	links := copyLinks(s.links)
	return Snapshot{
		Nodes:          nodes,
		Links:          links,
		Counts:         s.countsLocked(),
		BandCounts:     bandCountsLocked(s.nodes),
		LinkTypeCounts: linkTypeCountsLocked(s.links),
		TakenAt:        time.Now(),
	}
}

// countsLocked defines every figure in the summary card; callers must hold at least the read lock.
// Offline is probed-negatively only, Unreachable is Offline+Unprobed, Disconnected is orthogonal.
// Pending is work still queued; a node parked after a failure is counted as Failed, not Pending.
func (s *Store) countsLocked() model.Counts {
	c := model.Counts{Total: len(s.nodes)}
	// Links counts physical edges; both directions of one wire are stored as separate rows.
	edges := make(map[string]bool, len(s.links)/2)
	services := make(map[string]bool)
	for _, n := range s.nodes {
		switch {
		case n.Online:
			c.Online++
		case hasProbeVerdict(n):
			c.Offline++
			c.Unreachable++
		default:
			c.Unprobed++
			c.Unreachable++
		}
		// What the mesh reports about the link, not what ICMP from here concluded.
		if n.Disconnected {
			c.Disconnected++
		}
		if n.HasLocation {
			c.Located++
		}
		if n.Crawled {
			c.Crawled++
		}
		if n.IsSupernode {
			c.Supernodes++
		}
		// A parked node was tried and did not answer: that is a failure, not a queue entry.
		if !n.HasLocation && !n.DetailFetched && n.RevisitAt.IsZero() {
			c.Pending++
		}
		if n.FailCount > 0 && !n.Crawled {
			c.Failed++
		}
		for _, svc := range n.Services {
			if k := lowerASCII(strings.TrimSpace(svc)); k != "" {
				services[k] = true
			}
		}
	}
	for _, l := range s.links {
		edges[undirectedKey(l)] = true
	}
	c.Links = len(edges)
	c.Services = len(services)
	return c
}

func bandCountsLocked(nodes map[string]*model.Node) map[string]int {
	out := make(map[string]int, len(model.BandLabels))
	for _, n := range nodes {
		out[model.BandOf(n)]++
	}
	return out
}

func linkTypeCountsLocked(links map[string]model.Link) map[string]int {
	out := make(map[string]int, 4)
	for _, l := range links {
		typ := l.Type
		if typ == "" {
			typ = model.LinkTypeRF
		}
		out[typ]++
	}
	return out
}

// Topology emits one vertex per connected node and only edges whose both endpoints are known.
func (s *Store) Topology() model.TopologyResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Degree comes from the edge set, not from a node's Neighbors roster.
	// A node absent from the store is a dropped edge; one row per physical pair per type.
	seen := make(map[string]int, len(s.links)/2)
	degree := make(map[string]int, len(s.links))
	tlinks := make([]model.Link, 0, len(s.links)/2)
	for _, l := range s.links {
		if _, ok := s.nodes[l.From]; !ok {
			continue
		}
		if _, ok := s.nodes[l.To]; !ok {
			continue
		}
		key := undirectedKey(l)
		if at, dup := seen[key]; dup {
			// Map order is random, so the row that wins per wire is chosen by content.
			if fresher(l, tlinks[at]) {
				tlinks[at] = l
			}
			continue
		}
		seen[key] = len(tlinks)
		degree[l.From]++
		if l.To != l.From {
			degree[l.To]++
		}
		tlinks = append(tlinks, l)
	}

	tnodes := make([]model.TopologyNode, 0, len(s.nodes))
	unconnected := 0
	for _, n := range s.nodes {
		if degree[n.Hostname] == 0 {
			unconnected++
			continue
		}
		band := model.BandOf(n)
		label, ok := model.BandLabels[band]
		if !ok {
			label = n.BandLabel
		}
		color, ok := model.BandColors[band]
		if !ok {
			color = model.BandColors[model.BandUnresolved]
		}
		tnodes = append(tnodes, model.TopologyNode{
			ID:           n.Hostname,
			Name:         n.Name,
			Band:         band,
			BandLabel:    label,
			Color:        color,
			IsSupernode:  n.IsSupernode,
			Hops:         n.Hops,
			ETX:          n.ETX,
			Online:       n.Online,
			Probed:       hasProbeVerdict(n),
			MeshState:    n.MeshState,
			Disconnected: n.Disconnected,
			LatencyMs:    n.LatencyMs,
			Lat:          n.Lat,
			Lon:          n.Lon,
			NeighborCnt:  degree[n.Hostname],
		})
	}

	return model.TopologyResponse{
		Nodes:            orderTopologyNodes(tnodes),
		Links:            orderLinks(tlinks),
		GeneratedAt:      time.Now(),
		UnconnectedCount: unconnected,
	}
}

// undirectedKey is the endpoint pair plus link type, so an RF and a WireGuard path stay two edges.
func undirectedKey(l model.Link) string {
	a, b := l.From, l.To
	if b < a {
		a, b = b, a
	}
	return a + "\x00" + b + "\x00" + l.Type
}

// fresher reports whether a is a more recent observation of the same wire than b, ScannedAt first.
func fresher(a, b model.Link) bool {
	if !a.ScannedAt.Equal(b.ScannedAt) {
		return a.ScannedAt.After(b.ScannedAt)
	}
	return a.LastSeen.After(b.LastSeen)
}

// unknownHopOrder sorts nodes whose hop distance is unknown last, without excluding them.
const unknownHopOrder = 1 << 20

// IDsNeedingDetail lists never-fetched and relazy-stale nodes; maxHops -1 means no limit.
// Nodes parked for revisit are held back; unknown distance is queued last, never dropped.
func (s *Store) IDsNeedingDetail(maxHops int, relazy time.Duration) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	cutoff := now.Add(-relazy)
	type item struct {
		id    string
		order int
	}
	var todo []item
	for id, n := range s.nodes {
		if maxHops >= 0 && n.Hops >= 0 && n.Hops > maxHops {
			continue
		}
		if !n.RevisitAt.IsZero() && n.RevisitAt.After(now) {
			continue
		}
		if n.DetailFetched && !n.UpdatedAt.Before(cutoff) {
			continue
		}
		// Unknown distance (Hops < 0) sorts last rather than being dropped.
		order := n.Hops
		if order < 0 {
			order = unknownHopOrder
		}
		todo = append(todo, item{id: id, order: order})
	}
	slices.SortFunc(todo, func(a, b item) int {
		if a.order != b.order {
			return a.order - b.order
		}
		return strings.Compare(a.id, b.id)
	})

	out := make([]string, len(todo))
	for i, t := range todo {
		out[i] = t.id
	}
	return out
}

// IDsDueForRevisit returns up to limit nodes parked before since, the sweep's start.
func (s *Store) IDsDueForRevisit(limit int, since time.Time) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		return nil
	}
	type item struct {
		id   string
		at   time.Time
		rank int
	}
	var due []item
	for id, n := range s.nodes {
		if n.RevisitAt.IsZero() || !n.RevisitAt.Before(since) {
			continue
		}
		due = append(due, item{id: id, at: n.RevisitAt, rank: n.FailCount})
	}
	// Overdue first, then fewest failures, so a long-dead node cannot starve a fresh one.
	slices.SortFunc(due, func(a, b item) int {
		if !a.at.Equal(b.at) {
			return a.at.Compare(b.at)
		}
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return strings.Compare(a.id, b.id)
	})
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]string, len(due))
	for i, d := range due {
		out[i] = d.id
	}
	return out
}

// LiveNodeNames returns the sorted hostnames the latest probe round found reachable.
func (s *Store) LiveNodeNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]string, 0, len(s.nodes))
	for id, n := range s.nodes {
		if n.Online {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
