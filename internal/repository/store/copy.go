package store

import (
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"aredn-map/internal/model"
)

// cloneNode deep-copies a node so callers never share its slices and maps; nil in, nil out.
func cloneNode(n *model.Node) *model.Node {
	if n == nil {
		return nil
	}
	c := *n
	cloneRefs(&c, n)
	return &c
}

// cloneRefs copies the reference-typed fields of src into dst.
func cloneRefs(dst, src *model.Node) {
	dst.Aliases = slices.Clone(src.Aliases)
	dst.Services = slices.Clone(src.Services)
	dst.LocalServices = slices.Clone(src.LocalServices)
	dst.LocalDevices = slices.Clone(src.LocalDevices)
	dst.Neighbors = slices.Clone(src.Neighbors)
	dst.LinkTypes = maps.Clone(src.LinkTypes)
	dst.RFTypeCounts = maps.Clone(src.RFTypeCounts)
}

// cloneNodes snapshots the node map ordered by display name, canonical ID breaking ties.
func cloneNodes(nodes map[string]*model.Node) []*model.Node {
	out := make([]*model.Node, 0, len(nodes))
	if len(nodes) == 0 {
		return out
	}
	backing := make([]model.Node, len(nodes))
	i := 0
	for _, n := range nodes {
		c := *n
		cloneRefs(&c, n)
		backing[i] = c
		out = append(out, &backing[i])
		i++
	}
	slices.SortFunc(out, func(a, b *model.Node) int {
		if c := compareFold(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Hostname, b.Hostname)
	})
	return out
}

// copyLinks snapshots the edge map into a slice ordered by from, to, mac.
func copyLinks(links map[string]model.Link) []model.Link {
	in := make([]model.Link, 0, len(links))
	for _, l := range links {
		in = append(in, l)
	}
	return orderLinks(in)
}

// orderLinks arranges a caller-owned slice of edges by from, to, mac.
func orderLinks(in []model.Link) []model.Link {
	type key struct {
		from, to, mac string
		idx           int
	}
	keys := make([]key, len(in))
	for i, l := range in {
		keys[i] = key{from: l.From, to: l.To, mac: l.MAC, idx: i}
	}
	slices.SortFunc(keys, func(a, b key) int {
		if c := strings.Compare(a.from, b.from); c != 0 {
			return c
		}
		if c := strings.Compare(a.to, b.to); c != 0 {
			return c
		}
		return strings.Compare(a.mac, b.mac)
	})

	ordered := make([]model.Link, len(in))
	for pos, k := range keys {
		ordered[pos] = in[k.idx]
	}
	return ordered
}

// compareLinks is the store's canonical edge order: from, then to, then mac.
func compareLinks(a, b model.Link) int {
	if c := strings.Compare(a.From, b.From); c != 0 {
		return c
	}
	if c := strings.Compare(a.To, b.To); c != 0 {
		return c
	}
	return strings.Compare(a.MAC, b.MAC)
}

// orderTopologyNodes arranges the graph vertices by display name, the ID breaking ties.
func orderTopologyNodes(in []model.TopologyNode) []model.TopologyNode {
	type key struct {
		name, disp, id string
		idx            int
	}
	keys := make([]key, len(in))
	for i, n := range in {
		keys[i] = key{name: n.Name, disp: n.Name, id: n.ID, idx: i}
	}
	slices.SortFunc(keys, func(a, b key) int {
		if c := compareFold(a.name, b.name); c != 0 {
			return c
		}
		if c := strings.Compare(a.disp, b.disp); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})

	ordered := make([]model.TopologyNode, len(in))
	for pos, k := range keys {
		ordered[pos] = in[k.idx]
	}
	return ordered
}

// compareFold orders two strings case-insensitively, then by raw bytes so the ordering stays total.
func compareFold(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		ca, cb := foldByte(a[i]), foldByte(b[i])
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func foldByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// lowerASCII is strings.ToLower without the allocation for an already lowercase string.
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c >= 'A' && c <= 'Z') || c >= utf8.RuneSelf {
			return strings.ToLower(s)
		}
	}
	return s
}
