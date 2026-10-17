package store

import (
	"fmt"
	"testing"
	"time"

	"aredn-map/internal/model"
)

// Design point: ~11k supernode hosts, roughly five radio edges per node.
const (
	meshNodes = 10_000
	meshLinks = 50_000
)

// buildMesh fills a store the way a sweep does: the first third gets full detail.
func buildMesh(nodes, links int) *Store {
	s := New()
	bands := []string{model.Band5GHz, model.Band2_4GHz, model.Band900MHz, model.BandHaLow, model.BandSupernode}
	stamp := time.Now()

	for i := range nodes {
		host := fmt.Sprintf("n%05d.region.arenmesh.org", i)
		n := &model.Node{
			Hostname:     host,
			Name:         fmt.Sprintf("N%05d", i),
			URL:          "http://" + host + "/",
			Hops:         i % 6,
			Band:         bands[i%len(bands)],
			Channel:      i % 8,
			Source:       "status-page",
			Neighbors:    []string{fmt.Sprintf("n%05d", (i+1)%nodes), fmt.Sprintf("n%05d", (i+7)%nodes)},
			LinkTypes:    map[string]int{model.LinkTypeRF: 2, model.LinkTypeDtD: 1},
			DiscoveredAt: stamp,
			UpdatedAt:    stamp,
		}
		if i%3 == 0 {
			n.Firmware = "4.1.1"
			n.Hardware = "wa500xi"
			n.Lat = 30 + float64(i%1000)/1000
			n.Lon = 120 + float64(i%900)/1000
			n.HasLocation = true
			n.DetailFetched = true
			n.Services = []string{"Node BBS", "HTTP Port Forwarding"}
		}
		s.UpsertNode(n)
		if i%2 == 0 {
			s.MarkProbe(host, ProbeOutcome{Online: i%7 != 0, LatencyMs: float64(i%200) / 10,
				Sent: 3, Recv: 2, TTL: 63, ProbedAt: stamp})
		}
	}

	types := []string{model.LinkTypeRF, model.LinkTypeDtD, model.LinkTypeLocal, model.LinkTypeWireguard}
	for i := range links {
		s.UpsertLink(model.Link{
			From:         fmt.Sprintf("n%05d.region.arenmesh.org", i%nodes),
			To:           fmt.Sprintf("n%05d.region.arenmesh.org", (i*7+1)%nodes),
			MAC:          fmt.Sprintf("aa:bb:cc:%02x:%02x:%02x", i%256, (i/256)%256, (i/65536)%256),
			Type:         types[i%len(types)],
			Band:         bands[i%len(bands)],
			RxSuccessPct: float64(i % 101),
			TxSuccessPct: float64((i + 13) % 101),
			LastSeen:     stamp,
			ScannedAt:    stamp,
		})
	}
	s.SetSeeds([]string{"localnode.local.mesh"})
	return s
}

// TestTenThousandNodeMesh is the scale guard: every reader must still produce a correct picture.
func TestTenThousandNodeMesh(t *testing.T) {
	s := buildMesh(meshNodes, meshLinks)
	if got := s.NodeCount(); got != meshNodes {
		t.Fatalf("NodeCount = %d, want %d", got, meshNodes)
	}
	c := s.Counts()
	tp := s.Topology()
	if c.Total != meshNodes {
		t.Errorf("Counts = %+v", c)
	}
	// Counts.Links is physical edges, not the meshLinks observation rows this fixture writes.
	if c.Links <= 0 || c.Links > meshLinks {
		t.Errorf("Counts.Links = %d, want between 1 and %d rows", c.Links, meshLinks)
	}
	if c.Links != len(tp.Links) {
		t.Errorf("status claims %d links but the graph emits %d", c.Links, len(tp.Links))
	}
	if c.Online+c.Offline+c.Unprobed != c.Total {
		t.Errorf("Online+Offline+Unprobed = %d, total %d", c.Online+c.Offline+c.Unprobed, c.Total)
	}
	if c.Located != (meshNodes+2)/3 {
		t.Errorf("Located = %d, want a third of the mesh", c.Located)
	}
	if len(tp.Nodes) != meshNodes || tp.UnconnectedCount != 0 {
		t.Errorf("topology = %d nodes / %d unconnected, want every node on an edge", len(tp.Nodes), tp.UnconnectedCount)
	}
	ids := s.IDsNeedingDetail(-1, 10*time.Minute)
	if len(ids) != meshNodes-(meshNodes+2)/3 {
		t.Errorf("IDsNeedingDetail = %d, want every stub and stale detail", len(ids))
	}
	var bandTotal int
	for _, v := range s.BandCounts() {
		bandTotal += v
	}
	if bandTotal != meshNodes {
		t.Errorf("BandCounts sum = %d, want %d", bandTotal, meshNodes)
	}
	var ltTotal int
	for _, v := range s.LinkTypeCounts() {
		ltTotal += v
	}
	if ltTotal != meshLinks {
		t.Errorf("LinkTypeCounts sum = %d, want %d", ltTotal, meshLinks)
	}
	if got := len(s.LiveNodeNames()); got != c.Online {
		t.Errorf("LiveNodeNames = %d, want %d", got, c.Online)
	}
}

func BenchmarkSnapshot(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		snap := s.Snapshot()
		if len(snap.Nodes) != meshNodes {
			b.Fatalf("snapshot nodes = %d", len(snap.Nodes))
		}
	}
}

func BenchmarkNodes(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if len(s.Nodes()) != meshNodes {
			b.Fatal("nodes missing")
		}
	}
}

func BenchmarkTopology(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if len(s.Topology().Links) != meshLinks {
			b.Fatal("topology links missing")
		}
	}
}

func BenchmarkCounts(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = s.Counts()
	}
}

func BenchmarkBandAndLinkTypeCounts(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = s.BandCounts()
		_ = s.LinkTypeCounts()
	}
}

func BenchmarkLinks(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if len(s.Links()) != meshLinks {
			b.Fatal("links missing")
		}
	}
}

func BenchmarkIDsNeedingDetail(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = s.IDsNeedingDetail(-1, 10*time.Minute)
	}
}

func BenchmarkNodeByID(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, ok := s.NodeByID(fmt.Sprintf("n%05d.region.arenmesh.org", i%meshNodes)); !ok {
			b.Fatal("node missing")
		}
	}
}

// BenchmarkUpsertNode measures the merge path against an already known node.
func BenchmarkUpsertNode(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	stamp := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		s.UpsertNode(&model.Node{
			Hostname:    fmt.Sprintf("n%05d.region.arenmesh.org", i%meshNodes),
			Name:        fmt.Sprintf("N%05d", i%meshNodes),
			Firmware:    "4.1.1",
			Hardware:    "wa500xi",
			Lat:         31.5,
			Lon:         121.5,
			HasLocation: true,
			Neighbors:   []string{"n00001.region.arenmesh.org"},
			Services:    []string{"Node BBS"},
			LinkTypes:   map[string]int{model.LinkTypeRF: 2},
			Online:      true,
			LatencyMs:   4.2,
			ProbedAt:    stamp,
			UpdatedAt:   time.Now(),
		})
	}
}

func BenchmarkUpsertLink(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		s.UpsertLink(model.Link{
			From:     fmt.Sprintf("n%05d.region.arenmesh.org", i%meshNodes),
			To:       fmt.Sprintf("n%05d.region.arenmesh.org", (i*3+1)%meshNodes),
			MAC:      fmt.Sprintf("aa:bb:cc:%02x:%02x:%02x", i%256, (i/256)%256, (i/65536)%256),
			Type:     model.LinkTypeRF,
			LastSeen: time.Now(),
		})
	}
}

// BenchmarkSweepReaders runs the reader mix the API performs on a 10s poll.
func BenchmarkSweepReaders(b *testing.B) {
	s := buildMesh(meshNodes, meshLinks)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		snap := s.Snapshot()
		_ = s.Counts()
		_ = s.Topology()
		_ = s.LiveNodeNames()
		if len(snap.Nodes) != meshNodes {
			b.Fatal("snapshot wrong")
		}
	}
}

// BenchmarkScaling is the linearity guard: doubling the mesh must roughly double a poll.
func BenchmarkScaling(b *testing.B) {
	for _, nodes := range []int{1250, 2500, 5000, 10000, 20000} {
		s := buildMesh(nodes, nodes*5)
		b.Run(fmt.Sprintf("nodes=%d", nodes), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				snap := s.Snapshot()
				if len(snap.Nodes) != nodes || len(snap.Links) != nodes*5 {
					b.Fatalf("snapshot = %d/%d", len(snap.Nodes), len(snap.Links))
				}
			}
		})
	}
}
