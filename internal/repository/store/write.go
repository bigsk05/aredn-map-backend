package store

import (
	"reflect"
	"strings"
	"time"

	"aredn-map/internal/coord"
	"aredn-map/internal/model"
)

type ProbeOutcome struct {
	Online     bool
	LatencyMs  float64
	PacketLoss float64
	Sent       int
	Recv       int
	TTL        int
	JitterMs   float64
	IP         string
	ProbedAt   time.Time
	Err        string
}

// stampsOnly reports whether a write moved only UpdatedAt and DiscoveredAt, which no reader shows.
// before must be a deep copy taken before the write, since slices and maps share backing arrays.
func stampsOnly(before, now *model.Node) bool {
	a, b := *before, *now
	a.UpdatedAt, a.DiscoveredAt = b.UpdatedAt, b.DiscoveredAt
	return reflect.DeepEqual(a, b)
}

// publishNode stamps a row when it is new or its visible content moved; the store lock is held.
func (s *Store) publishNode(id string, before, now *model.Node, isNew bool) {
	if isNew || !stampsOnly(before, now) {
		s.stampNode(id)
	}
}

// UpsertNode merges n into the row for its canonical ID; zero values never blank known fields.
func (s *Store) UpsertNode(n *model.Node) {
	if n == nil {
		return
	}
	id := canonicalID(n.Hostname, n.Name)
	if id == "" {
		return
	}
	in := *n
	in.Hostname = id

	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev.Add(1)

	now := time.Now()
	cur, ok := s.nodes[id]
	if !ok {
		cur = &model.Node{Hostname: id, DiscoveredAt: orTime(in.DiscoveredAt, now), UpdatedAt: orTime(in.UpdatedAt, now)}
		s.nodes[id] = cur
	}
	before := cloneNode(cur)
	mergeInto(cur, &in)
	if cur.Name == "" {
		cur.Name = id
	}
	derive(cur)
	s.publishNode(id, before, cur, !ok)
}

// derive fills computed fields so every write path agrees: GCJ-02 for Tencent maps, plus band.
func derive(n *model.Node) {
	if n.HasLocation && n.GCJLat == 0 && n.GCJLon == 0 {
		n.GCJLat, n.GCJLon = coord.WGS84ToGCJ02(n.Lat, n.Lon)
	}
	n.Band = model.BandOf(n)
	// Supernode outranks a radio band, the same precedence model.DeriveBand uses.
	if n.IsSupernode {
		n.Band = model.BandSupernode
	}
	n.BandLabel = model.BandLabels[n.Band]
}

// MergeNode runs fn on the live row under the write lock; fn must not re-enter the store.
// Hostname is restored to the canonical key, sets re-canonicalised, stamps moved forward.
func (s *Store) MergeNode(id string, fn func(*model.Node)) {
	key := canonicalID(id, "")
	if key == "" || fn == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev.Add(1)

	n, ok := s.nodes[key]
	if !ok {
		n = &model.Node{Hostname: key, Name: key}
		s.nodes[key] = n
	}
	before := cloneNode(n)
	fn(n)
	n.Hostname = key
	normaliseSets(n)
	derive(n)

	now := time.Now()
	if n.DiscoveredAt.IsZero() {
		n.DiscoveredAt = now
	}
	if n.UpdatedAt.Before(now) {
		n.UpdatedAt = now
	}
	s.publishNode(key, before, n, !ok)
}

// DeleteNode drops a node; Topology prunes the edges that lost an endpoint.
func (s *Store) DeleteNode(id string) {
	key := canonicalID(id, "")
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev.Add(1)
	delete(s.nodes, key)
	s.markGone(key)
}

// UpsertLink keys on l.Key(); the incoming measurement wins, zero timestamps keep the stored ones.
func (s *Store) UpsertLink(l model.Link) {
	l.From = canonicalID(l.From, "")
	l.To = canonicalID(l.To, "")
	if l.From == "" || l.To == "" {
		return
	}
	l.MAC = lowerASCII(strings.TrimSpace(l.MAC))

	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev.Add(1)

	key := l.Key()
	if old, ok := s.links[key]; ok {
		if l.LastSeen.IsZero() {
			l.LastSeen = old.LastSeen
		}
		if l.ScannedAt.IsZero() {
			l.ScannedAt = old.ScannedAt
		}
	}
	s.links[key] = l
	s.stampLink(key)
}

// MarkMeshState records what the mesh reports about a peer link, separate from a failed local ICMP.
func (s *Store) MarkMeshState(id, state string, downSince time.Time) {
	key := canonicalID(id, "")
	if key == "" || state == "" {
		return
	}
	s.touch(key, func(n *model.Node) {
		// A node we are talking to is not down just because a peer's link to it dropped.
		if n.Online && state == model.MeshStateDisconnected {
			return
		}
		n.MeshState = state
		n.Disconnected = state == model.MeshStateDisconnected
		if n.Disconnected && !downSince.IsZero() {
			n.LinkDownSince = downSince
		}
	})
}

// MarkProbe records an ICMP verdict; the answer is authoritative, so unknown ids get a shell node.
func (s *Store) MarkProbe(id string, r ProbeOutcome) {
	key := canonicalID(id, "")
	if key == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev.Add(1)

	now := time.Now()
	n, ok := s.nodes[key]
	if !ok {
		n = &model.Node{Hostname: key, Name: key, DiscoveredAt: now}
		s.nodes[key] = n
	}
	n.Online = r.Online
	n.LatencyMs = r.LatencyMs
	n.PacketLossPct = r.PacketLoss
	n.PacketsSent = r.Sent
	n.PacketsRecv = r.Recv
	n.JitterMs = r.JitterMs
	n.ProbeError = r.Err
	if r.TTL > 0 {
		n.TTL = r.TTL
	}
	n.ProbedAt = orTime(r.ProbedAt, now)
	if ip := strings.TrimSpace(r.IP); ip != "" {
		n.MeshIP = ip
	}
	if r.Online {
		n.LastError = ""
		// An answering node leaves the revisit lane whoever parked it.
		n.FailCount = 0
		n.RevisitAt = time.Time{}
		// Answering us outranks a peer's "my link to this one is down".
		n.MeshState = ""
		n.Disconnected = false
		n.LinkDownSince = time.Time{}
	}
	if n.UpdatedAt.Before(now) {
		n.UpdatedAt = now
	}
	// derive keeps Band a key the shared tables know, even for a node first seen through a ping.
	derive(n)
	s.stampNode(key)
}

// MarkFailure parks a node that could not be reached until due; a zero due means do not park.
func (s *Store) MarkFailure(id, msg string, due time.Time) {
	key := canonicalID(id, "")
	if key == "" {
		return
	}
	s.touch(key, func(n *model.Node) {
		if strings.TrimSpace(msg) != "" {
			n.LastError = msg
		}
		n.FailCount++
		if !due.IsZero() {
			n.RevisitAt = due
		}
	})
}

// MarkReached clears the park, the counter and the failure note in one state change.
func (s *Store) MarkReached(id string) {
	key := canonicalID(id, "")
	if key == "" {
		return
	}
	s.touch(key, func(n *model.Node) {
		n.LastError = ""
		n.FailCount = 0
		n.RevisitAt = time.Time{}
	})
}

// MarkDetailFetched flags the page as parsed so the next sweep skips it until relazy expires.
func (s *Store) MarkDetailFetched(id string) {
	s.touch(id, func(n *model.Node) { n.DetailFetched = true })
}

// touch advances a node's stamps, creating a shell when the engine writes about an unknown id.
func (s *Store) touch(id string, fn func(*model.Node)) {
	key := canonicalID(id, "")
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev.Add(1)

	n, ok := s.nodes[key]
	if !ok {
		n = &model.Node{Hostname: key, Name: key}
		s.nodes[key] = n
	}
	before := cloneNode(n)
	fn(n)
	derive(n)

	now := time.Now()
	if n.DiscoveredAt.IsZero() {
		n.DiscoveredAt = now
	}
	if n.UpdatedAt.Before(now) {
		n.UpdatedAt = now
	}
	s.publishNode(key, before, n, !ok)
}
