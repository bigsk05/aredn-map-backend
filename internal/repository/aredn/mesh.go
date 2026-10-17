package aredn

import (
	"context"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// MeshData is the network-wide host table a supernode publishes.
type MeshData struct {
	// Hosts maps a mesh IP to names, each "name" or "name|lanIP" when an alias carries one.
	Hosts map[string][]string

	// Services maps a mesh IP to the services published there, each rendered as "name|target".
	Services map[string][]string

	// ETX is always empty: /a/sysinfo carries no routing costs; ETX comes from each node's links.
	ETX map[string]int

	// NodeNames lists the distinct lowercased hostnames in Hosts, sorted, ready for NodeURL.
	NodeNames []string
}

// FetchMesh reads the supernode's mesh table from /a/sysinfo.
// "nodes" is the mesh-node list; "hosts" is the wider LAN set the crawler does not absorb.
func (c *Client) FetchMesh(ctx context.Context, hostname string) (*MeshData, error) {
	si, err := c.FetchSysinfo(ctx, hostname, "nodes", "services")
	if err != nil {
		return nil, err
	}
	md := newMeshData()
	seen := map[string]bool{}
	for _, h := range si.Nodes {
		name := strings.TrimSpace(h.Name)
		ip := strings.TrimSpace(h.IP)
		if name == "" || ip == "" {
			continue
		}
		first := len(md.Hosts[ip]) == 0
		md.Hosts[ip] = append(md.Hosts[ip], name)
		// Only an address's first name is a crawl target; the rest are aliases of the same node.
		key := strings.ToLower(name)
		if first && !seen[key] {
			seen[key] = true
			md.NodeNames = append(md.NodeNames, key)
		}
	}
	for _, s := range si.Services {
		if s.Name == "" || s.IP == "" {
			continue
		}
		target := s.Link
		if target == "" {
			target = s.Protocol
		}
		md.Services[s.IP] = append(md.Services[s.IP], s.Name+"|"+target)
	}
	sort.Strings(md.NodeNames)
	c.log.Debug("aredn mesh payload parsed",
		zap.String("hostname", trimMeshDomain(hostname)),
		zap.Int("hosts", len(md.Hosts)),
		zap.Int("services", len(md.Services)),
		zap.Int("nodes", len(md.NodeNames)))
	return md, nil
}

func newMeshData() *MeshData {
	return &MeshData{
		Hosts:    map[string][]string{},
		Services: map[string][]string{},
		ETX:      map[string]int{},
	}
}
