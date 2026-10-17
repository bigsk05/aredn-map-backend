package store

import (
	"sort"
	"strings"

	"aredn-map/internal/model"
)

// deltaMaxRows paces one frame; a sweep rewrites hundreds of rows, the rest follow next tick.
const deltaMaxRows = 400

// Prefixed ledger keys let nodes and links share one map, a hostname never read as a link key.
// A link key is "from\x00to\x00mac", so it may contain a colon; the prefix is what precedes it.
const (
	nodeStamp = "n:"
	linkStamp = "l:"
)

// Delta is what changed since a reader's last frame.
type Delta struct {
	Rev uint64 `json:"rev"`
	Gen uint64 `json:"gen"`
	// Full means the cursor is no longer meaningful: reset or new connection, so re-read.
	Full bool `json:"full,omitempty"`

	Nodes   []model.Node `json:"nodes,omitempty"`
	Links   []model.Link `json:"links,omitempty"`
	Removed []string     `json:"removed,omitempty"`
}

// stampNode records that a node row moved at the current revision, under the store lock.
func (s *Store) stampNode(id string) { s.wrote[nodeStamp+id] = s.rev.Load() }

// stampLink records that an edge row moved; link keys are already canonical.
func (s *Store) stampLink(key string) { s.wrote[linkStamp+key] = s.rev.Load() }

// markGone tombstones a node; edges lost an endpoint are hidden by Topology, never deleted.
func (s *Store) markGone(id string) { s.gone[nodeStamp+id] = s.rev.Load() }

// Generation counts full resets; a reader whose generation differs must re-read rather than merge.
func (s *Store) Generation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gen
}

// pending is one ledger entry waiting to be delivered.
type pending struct {
	at   uint64
	key  string
	dead bool
}

// Delta returns records written after since, each once with its final content, oldest write first.
// Rev resumes from the last stamp sent when the backlog was cut, else the store's revision.
func (s *Store) Delta(since uint64, sinceGen uint64) Delta {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rev := s.rev.Load()
	out := Delta{Rev: rev, Gen: s.gen}
	switch {
	case sinceGen != s.gen || since > rev:
		out.Full = true
		return out
	case since == rev:
		return out
	}

	var queue []pending
	for k, at := range s.wrote {
		if at > since {
			queue = append(queue, pending{at: at, key: k})
		}
	}
	for k, at := range s.gone {
		if at <= since {
			continue
		}
		if later, ok := s.wrote[k]; ok && later > at {
			continue // deleted, then discovered again: the row itself carries it
		}
		queue = append(queue, pending{at: at, key: k, dead: true})
	}
	if len(queue) == 0 {
		return out
	}
	// Write order, not name order: a cut at deltaMaxRows cannot skip a record stamped earlier.
	sort.Slice(queue, func(i, j int) bool { return queue[i].at < queue[j].at })

	if len(queue) > deltaMaxRows {
		queue = queue[:deltaMaxRows]
		out.Rev = queue[len(queue)-1].at
	}
	for _, p := range queue {
		kind, id, _ := strings.Cut(p.key, ":")
		switch {
		case p.dead:
			out.Removed = append(out.Removed, id)
		case kind == "n":
			if n, ok := s.nodes[id]; ok {
				out.Nodes = append(out.Nodes, *n)
			}
		case kind == "l":
			if l, ok := s.links[id]; ok {
				out.Links = append(out.Links, l)
			}
		}
	}
	return out
}
