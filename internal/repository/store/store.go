// Package store keeps the in-memory mesh: node and link tables, merge rules and the change ledger.
package store

import (
	"strings"
	"sync"
	"sync/atomic"

	"aredn-map/internal/model"
)

type Store struct {
	mu    sync.RWMutex
	nodes map[string]*model.Node
	links map[string]model.Link
	seeds []string
	// rev counts writes; each write gets its own revision.
	rev atomic.Uint64
	// gen counts full resets; a reader whose gen differs must re-read everything.
	gen uint64
	// wrote/gone map a record key to the revision that last touched it.
	wrote map[string]uint64
	gone  map[string]uint64
}

// Rev is the number of writes; it moves only when the visible picture moves.
func (s *Store) Rev() uint64 { return s.rev.Load() }

// BumpRev announces a change the store did not make, such as an engine-side scan status.
func (s *Store) BumpRev() { s.rev.Add(1) }

// New returns an empty store.
func New() *Store {
	return &Store{
		nodes: make(map[string]*model.Node),
		links: make(map[string]model.Link),
		wrote: make(map[string]uint64),
		gone:  make(map[string]uint64),
	}
}

// SetSeeds replaces the configured seeds, dropping blanks and case-insensitive duplicates.
func (s *Store) SetSeeds(hosts []string) {
	out := make([]string, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		k := strings.ToLower(h)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, h)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev.Add(1)
	s.seeds = out
}

// Reset drops nodes, links and the ledger; seeds survive and gen bumps so readers re-read.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev.Add(1)
	s.gen++

	s.nodes = make(map[string]*model.Node)
	s.links = make(map[string]model.Link)
	s.wrote = make(map[string]uint64)
	s.gone = make(map[string]uint64)
}

// canonicalID is the store key: lowercase trimmed hostname, falling back to the display name.
func canonicalID(host, name string) string {
	if id := lowerASCII(strings.TrimSpace(host)); id != "" {
		return id
	}
	return lowerASCII(strings.TrimSpace(name))
}
