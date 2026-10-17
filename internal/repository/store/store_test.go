package store

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aredn-map/internal/model"
)

func discoveryStub(host string, hops int) *model.Node {
	return &model.Node{
		Hostname: host,
		Name:     strings.ToUpper(host),
		URL:      "http://" + host + "/",
		Hops:     hops,
		Source:   "neighbor-list",
	}
}

func mustNode(t *testing.T, s *Store, id string) *model.Node {
	t.Helper()

	n, ok := s.NodeByID(id)
	if !ok {
		t.Fatalf("node %q missing", id)
	}
	return n
}

func TestUpsertNodeCanonicalisesIdentity(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{Hostname: "N5BAD-1.SHA3.ARENMESH.NET", Name: "N5BAD-1", URL: "http://x/"})

	if s.NodeCount() != 1 {
		t.Fatalf("NodeCount = %d, want 1", s.NodeCount())
	}
	n := mustNode(t, s, "n5bad-1.sha3.arenmesh.net")
	if n.Hostname != "n5bad-1.sha3.arenmesh.net" {
		t.Errorf("Hostname = %q, want lowercase", n.Hostname)
	}
	if n.Name != "N5BAD-1" {
		t.Errorf("Name = %q, want original casing kept", n.Name)
	}
	if n.DiscoveredAt.IsZero() || n.UpdatedAt.IsZero() {
		t.Error("DiscoveredAt and UpdatedAt must be stamped on the first write")
	}
	if got := mustNode(t, s, "N5BAD-1.SHA3.ARENMESH.NET"); got.Hostname != n.Hostname {
		t.Error("lookup must be case-insensitive")
	}

	// The display name falls back to the canonical ID so ordering stays sane.
	s.UpsertNode(&model.Node{Hostname: "anon-2"})
	if got := mustNode(t, s, "anon-2").Name; got != "anon-2" {
		t.Errorf("Name = %q, want the hostname fallback", got)
	}

	s.UpsertNode(nil)
	s.UpsertNode(&model.Node{})
	if s.NodeCount() != 2 {
		t.Fatalf("NodeCount = %d after the empty writes, want 2", s.NodeCount())
	}
}

func TestMergeKeepsKnownFieldsAgainstZeroValues(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{
		Hostname:    "n5bad-1",
		Name:        "N5BAD-1",
		Firmware:    "4.1.1",
		Hardware:    "wa500xi",
		Band:        model.Band5GHz,
		FreqLow:     5195000,
		ChanBW:      10,
		Lat:         31.02,
		Lon:         121.44,
		GridLocator: "OM11jt",
		HasLocation: true,
		Hops:        2,
		ETX:         12,
		IsSupernode: true,
		Services:    []string{"Node BBS"},
		Neighbors:   []string{"W6Cool-7"},
		LinkTypes:   map[string]int{model.LinkTypeRF: 3},
	})
	before := mustNode(t, s, "n5bad-1")

	// A cheap discovery pass knows almost nothing: nothing may be blanked.
	s.UpsertNode(discoveryStub("n5bad-1", 0))
	after := mustNode(t, s, "n5bad-1")

	fields := []struct {
		name string
		got  any
		want any
	}{
		{"Firmware", after.Firmware, before.Firmware},
		{"Hardware", after.Hardware, before.Hardware},
		{"Band", after.Band, before.Band},
		{"FreqLow", after.FreqLow, before.FreqLow},
		{"ChanBW", after.ChanBW, before.ChanBW},
		{"Lat", after.Lat, before.Lat},
		{"Lon", after.Lon, before.Lon},
		{"GridLocator", after.GridLocator, before.GridLocator},
		{"HasLocation", after.HasLocation, before.HasLocation},
		{"Hops", after.Hops, before.Hops},
		{"ETX", after.ETX, before.ETX},
		{"IsSupernode", after.IsSupernode, before.IsSupernode},
		{"Services", after.Services, before.Services},
		{"Neighbors", after.Neighbors, before.Neighbors},
		{"LinkTypes", after.LinkTypes, before.LinkTypes},
		{"DiscoveredAt", after.DiscoveredAt, before.DiscoveredAt},
	}
	for _, f := range fields {
		if !reflect.DeepEqual(f.got, f.want) {
			t.Errorf("%s = %v after a stub upsert, want %v kept", f.name, f.got, f.want)
		}
	}

	// The opposite order works too: a pass only overwrites what it actually reports.
	s2 := New()
	s2.UpsertNode(&model.Node{Hostname: "w6cool-7", Firmware: "4.1.1"})
	s2.UpsertNode(&model.Node{Hostname: "w6cool-7", Band: model.Band2_4GHz, Channel: 1})
	got := mustNode(t, s2, "w6cool-7")
	if got.Firmware != "4.1.1" || got.Band != model.Band2_4GHz || got.Channel != 1 {
		t.Errorf("merge lost fields: %+v", got)
	}
}

func TestUpdatedAtAndDiscoveredAt(t *testing.T) {
	s := New()
	old := time.Now().Add(-2 * time.Hour)
	s.UpsertNode(&model.Node{Hostname: "a", DiscoveredAt: old, UpdatedAt: old})

	newer := time.Now().Add(-time.Minute)
	s.UpsertNode(&model.Node{Hostname: "a", DiscoveredAt: newer, UpdatedAt: newer})
	n := mustNode(t, s, "a")
	if !n.DiscoveredAt.Equal(old) {
		t.Errorf("DiscoveredAt = %v, want the first value %v frozen", n.DiscoveredAt, old)
	}
	if !n.UpdatedAt.Equal(newer) {
		t.Errorf("UpdatedAt = %v, want %v", n.UpdatedAt, newer)
	}

	// An older timestamp must not move the clock backwards.
	s.UpsertNode(&model.Node{Hostname: "a", UpdatedAt: old})
	if back := mustNode(t, s, "a").UpdatedAt; !back.Equal(newer) {
		t.Errorf("UpdatedAt regressed to %v, want %v", back, newer)
	}
}

func TestLivenessIsAuthoritativeOnlyForARealProbeAnswer(t *testing.T) {
	s := New()
	probedAt := time.Now().Add(-5 * time.Minute)
	s.MarkProbe("n5bad-1", ProbeOutcome{
		Online: true, LatencyMs: 12.5, PacketLoss: 0, Sent: 3, Recv: 3,
		TTL: 64, JitterMs: 1.2, IP: "10.1.1.1", ProbedAt: probedAt,
	})

	// A status-page record with no verdict must not read as offline; the engine probes first.
	s.UpsertNode(&model.Node{Hostname: "n5bad-1", Name: "N5BAD-1", Firmware: "4.1.1"})
	n := mustNode(t, s, "n5bad-1")
	if !n.Online || n.LatencyMs != 12.5 || n.PacketsSent != 3 || n.PacketsRecv != 3 ||
		n.JitterMs != 1.2 || !n.ProbedAt.Equal(probedAt) {
		t.Errorf("a verdict-free upsert wiped the liveness block: %+v", n)
	}

	// A fresh probe answer that says offline wins even though metrics are zero.
	offlineAt := time.Now()
	s.UpsertNode(&model.Node{
		Hostname: "n5bad-1", URL: "http://n5bad-1/", Online: false,
		PacketLossPct: 100, PacketsSent: 3, PacketsRecv: 0,
		ProbeError: "no reply", ProbedAt: offlineAt,
	})
	n = mustNode(t, s, "n5bad-1")
	if n.Online {
		t.Fatal("node still online after an offline probe answer")
	}
	if n.LatencyMs != 0 || n.JitterMs != 0 || n.PacketsRecv != 0 || n.PacketsSent != 3 {
		t.Errorf("offline answer left stale metrics: %+v", n)
	}
	if n.PacketLossPct != 100 || n.ProbeError != "no reply" || !n.ProbedAt.Equal(offlineAt) {
		t.Errorf("offline answer not recorded: %+v", n)
	}
	if n.URL == "" || n.Firmware != "4.1.1" {
		t.Errorf("non-liveness fields lost: %+v", n)
	}

	// MarkProbe itself always replaces the block.
	s.MarkProbe("n5bad-1", ProbeOutcome{Online: true, LatencyMs: 3, ProbedAt: time.Now()})
	n = mustNode(t, s, "n5bad-1")
	if !n.Online || n.LatencyMs != 3 || n.PacketLossPct != 0 || n.ProbeError != "" || n.PacketsSent != 0 {
		t.Errorf("MarkProbe did not replace the block: %+v", n)
	}
}

func TestGeoBlockClearedByDetailAnswer(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{Hostname: "a", Lat: 1, Lon: 2, GCJLat: 3, GCJLon: 4, GridLocator: "OM", HasLocation: true})

	// The detail pass reports the node and it has no position this time.
	s.UpsertNode(&model.Node{Hostname: "a", DetailFetched: true, Firmware: "4.1.1"})
	n := mustNode(t, s, "a")
	if n.HasLocation || n.Lat != 0 || n.Lon != 0 || n.GCJLat != 0 || n.GridLocator != "" {
		t.Errorf("a detail answer must clear the geography block: %+v", n)
	}
	if !n.DetailFetched || n.Firmware != "4.1.1" {
		t.Errorf("detail answer not merged: %+v", n)
	}

	// A link-dialog stub asserting a position replaces the block as a unit.
	s.UpsertNode(&model.Node{Hostname: "a", Lat: 9, Lon: 8, HasLocation: true})
	n = mustNode(t, s, "a")
	if n.Lat != 9 || n.Lon != 8 || n.GridLocator != "" || !n.HasLocation {
		t.Errorf("geo block not taken as a unit: %+v", n)
	}
}

func TestSetFieldsUnionDedupAndSort(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{
		Hostname:  "n5bad-1",
		Neighbors: []string{"W6Cool-7", "K6AA-2", "w6cool-7"},
		Aliases:   []string{"N5BAD-1.arenmesh.net", "n5bad-1.local.mesh"},
		Services:  []string{"http Port Forwarding", "Node BBS"},
	})
	// MergeNode appends raw names the way the scan engine does.
	s.MergeNode("n5bad-1", func(n *model.Node) {
		n.Neighbors = append(n.Neighbors, "K6BB-1", "w6cool-7")
		n.Aliases = append(n.Aliases, "N5BAD-1.LOCAL.MESH")
		n.Services = append(n.Services, "HTTP port forwarding")
	})

	n := mustNode(t, s, "n5bad-1")
	if want := []string{"k6aa-2", "k6bb-1", "w6cool-7"}; !slices.Equal(n.Neighbors, want) {
		t.Errorf("Neighbors = %q, want lowercased, deduped, sorted %q", n.Neighbors, want)
	}
	if want := []string{"n5bad-1.arenmesh.net", "n5bad-1.local.mesh"}; !slices.Equal(n.Aliases, want) {
		t.Errorf("Aliases = %q, want %q", n.Aliases, want)
	}
	if want := []string{"http Port Forwarding", "Node BBS"}; !slices.Equal(n.Services, want) {
		t.Errorf("Services = %q, want the first casing kept and deduped %q", n.Services, want)
	}

	// UpsertNode unions too; link-type counts fold per key.
	s.UpsertNode(&model.Node{
		Hostname:  "n5bad-1",
		Neighbors: []string{"zz-1"},
		LinkTypes: map[string]int{model.LinkTypeRF: 4, model.LinkTypeDtD: 1},
	})
	s.UpsertNode(&model.Node{Hostname: "n5bad-1", LinkTypes: map[string]int{model.LinkTypeDtD: 2}})
	n = mustNode(t, s, "n5bad-1")
	if want := []string{"k6aa-2", "k6bb-1", "w6cool-7", "zz-1"}; !slices.Equal(n.Neighbors, want) {
		t.Errorf("Neighbors after union = %q, want %q", n.Neighbors, want)
	}
	if got := n.LinkTypes[model.LinkTypeRF]; got != 4 {
		t.Errorf("LinkTypes[RF] = %d, want the incoming value", got)
	}
	if got := n.LinkTypes[model.LinkTypeDtD]; got != 2 {
		t.Errorf("LinkTypes[DtD] = %d, want 2", got)
	}
}

func TestReadsHandOutDeepCopies(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{
		Hostname: "n5bad-1", Name: "N5BAD-1", Firmware: "4.1.1",
		Neighbors: []string{"w6cool-7"}, Services: []string{"bbs"},
		Aliases: []string{"a1"}, LinkTypes: map[string]int{model.LinkTypeRF: 2},
		RFTypeCounts: map[string]int{"wa500xi": 1},
	})
	s.UpsertNode(&model.Node{Hostname: "w6cool-7", Name: "W6COOL-7"})
	s.UpsertLink(model.Link{From: "n5bad-1", To: "w6cool-7", Type: model.LinkTypeRF, RxSuccessPct: 99})
	s.SetSeeds([]string{"localnode.local.mesh"})

	// NodeByID.
	n := mustNode(t, s, "n5bad-1")
	n.Firmware = "hacked"
	n.Name = "hacked"
	n.Neighbors[0] = "hacked"
	n.Services[0] = "hacked"
	n.Aliases[0] = "hacked"
	n.LinkTypes[model.LinkTypeRF] = 99
	n.RFTypeCounts["wa500xi"] = 99
	n.Neighbors = append(n.Neighbors, "invented")

	fresh := mustNode(t, s, "n5bad-1")
	if fresh.Firmware != "4.1.1" || fresh.Name != "N5BAD-1" {
		t.Errorf("scalar fields aliased: %+v", fresh)
	}
	if want := []string{"w6cool-7"}; !slices.Equal(fresh.Neighbors, want) {
		t.Errorf("Neighbors aliased: %q", fresh.Neighbors)
	}
	if want := []string{"bbs"}; !slices.Equal(fresh.Services, want) {
		t.Errorf("Services aliased: %q", fresh.Services)
	}
	if want := []string{"a1"}; !slices.Equal(fresh.Aliases, want) {
		t.Errorf("Aliases aliased: %q", fresh.Aliases)
	}
	if fresh.LinkTypes[model.LinkTypeRF] != 2 || fresh.RFTypeCounts["wa500xi"] != 1 {
		t.Errorf("maps aliased: %v %v", fresh.LinkTypes, fresh.RFTypeCounts)
	}

	// Nodes().
	nodes := s.Nodes()
	nodes[0].Name = "hacked"
	nodes[0].Neighbors = append(nodes[0].Neighbors, "invented")
	if again := s.Nodes(); again[0].Name == "hacked" || len(again[0].Neighbors) != 1 {
		t.Errorf("Nodes() leaked live slices: %+v", again[0])
	}

	// Links().
	links := s.Links()
	links[0].RxSuccessPct = 0
	links[0].To = "hacked"
	if again := s.Links(); again[0].RxSuccessPct != 99 || again[0].To == "hacked" {
		t.Errorf("Links() leaked live values: %+v", again[0])
	}

	// Snapshot().
	snap := s.Snapshot()
	snap.Nodes[0].Firmware = "hacked"
	snap.Nodes[0].Services = append(snap.Nodes[0].Services, "invented")
	snap.Links[0].Type = "hacked"
	snap.BandCounts[model.Band5GHz] = 42
	snap.LinkTypeCounts[model.LinkTypeRF] = 42
	again := s.Snapshot()
	if again.Nodes[0].Firmware == "hacked" || len(again.Nodes[0].Services) != 1 ||
		again.Links[0].Type == "hacked" || again.LinkTypeCounts[model.LinkTypeRF] == 42 {
		t.Errorf("Snapshot() not isolated: %+v", again)
	}

	// Topology() vertices and edges are freshly built.
	tp := s.Topology()
	tp.Nodes[0].Name = "hacked"
	tp.Links[0].Type = "hacked"
	if redo := s.Topology(); redo.Nodes[0].Name == "hacked" || redo.Links[0].Type == "hacked" {
		t.Error("Topology() leaked vertices or edges")
	}

	// Seeds().
	seeds := s.Seeds()
	seeds[0] = "hacked"
	if redo := s.Seeds(); redo[0] == "hacked" {
		t.Error("Seeds() returned the live slice")
	}

	// The copies were inert, so the store still answers normally.
	s.MarkProbe("n5bad-1", ProbeOutcome{Online: true, ProbedAt: time.Now()})
	if got := s.LiveNodeNames(); len(got) != 1 || got[0] != "n5bad-1" {
		t.Errorf("LiveNodeNames = %q", got)
	}
	if got := s.IDsNeedingDetail(-1, 0); len(got) == 0 {
		t.Error("IDsNeedingDetail must still list the never-fetched node")
	}
}

func TestNodesSortedAndNodeByIDMisses(t *testing.T) {
	s := New()
	for _, host := range []string{"zulu", "Alpha", "mike", "bravo"} {
		s.UpsertNode(&model.Node{Hostname: host, Name: strings.ToUpper(host)})
	}
	var names []string
	for _, n := range s.Nodes() {
		names = append(names, n.Name)
	}
	if want := []string{"ALPHA", "BRAVO", "MIKE", "ZULU"}; !slices.Equal(names, want) {
		t.Errorf("Nodes order = %q, want %q", names, want)
	}
	if _, ok := s.NodeByID("nope"); ok {
		t.Error("unknown id reported present")
	}
	if _, ok := s.NodeByID(""); ok {
		t.Error("empty id reported present")
	}
}

func TestDeleteNode(t *testing.T) {
	s := New()
	s.UpsertNode(discoveryStub("a", 0))
	s.UpsertNode(discoveryStub("b", 1))
	s.UpsertLink(model.Link{From: "a", To: "b", Type: model.LinkTypeRF})
	s.DeleteNode("A")

	if s.NodeCount() != 1 {
		t.Fatalf("node survived delete: %d", s.NodeCount())
	}
	if _, ok := s.NodeByID("a"); ok {
		t.Fatal("node still readable")
	}
	if len(s.Links()) != 1 {
		t.Fatal("links must survive a node delete; Topology is what prunes them")
	}
	if got := len(s.Topology().Links); got != 0 {
		t.Fatalf("Topology kept a dangling edge: %d", got)
	}
	s.DeleteNode("missing")
	s.DeleteNode("")
	if s.NodeCount() != 1 {
		t.Fatalf("NodeCount = %d after the no-op deletes", s.NodeCount())
	}
}

func TestUpsertLinkKeyingAndOrder(t *testing.T) {
	s := New()
	first := time.Now().Add(-time.Hour)
	s.UpsertLink(model.Link{From: "B", To: "A", MAC: "02:00:00:00:00:0b", Type: model.LinkTypeDtD,
		LastSeen: first, ScannedAt: first, RxSuccessPct: 50})

	// Same key, no timestamps: the measurement is replaced, the times survive.
	s.UpsertLink(model.Link{From: "b", To: "a", MAC: "02:00:00:00:00:0b", Type: model.LinkTypeDtD, RxSuccessPct: 90})
	links := s.Links()
	if len(links) != 1 {
		t.Fatalf("links = %d, want the key to dedup", len(links))
	}
	if links[0].RxSuccessPct != 90 || !links[0].LastSeen.Equal(first) || !links[0].ScannedAt.Equal(first) {
		t.Errorf("link merge: %+v", links[0])
	}
	if links[0].From != "b" || links[0].To != "a" || links[0].MAC != "02:00:00:00:00:0b" {
		t.Errorf("link identity not canonicalised: %+v", links[0])
	}

	s.UpsertLink(model.Link{From: "a", To: "c", Type: model.LinkTypeRF})
	s.UpsertLink(model.Link{From: "a", To: "b", MAC: "00:11", Type: model.LinkTypeRF})
	s.UpsertLink(model.Link{From: "a", To: "b", Type: model.LinkTypeRF})
	got := s.Links()
	if len(got) != 4 {
		t.Fatalf("links = %d, want 4", len(got))
	}
	for i := 1; i < len(got); i++ {
		if compareLinks(got[i-1], got[i]) >= 0 {
			t.Fatalf("links not ordered by from,to,mac: %+v", got)
		}
	}

	// Edges without both endpoints cannot exist in the graph.
	s.UpsertLink(model.Link{From: "", To: "c"})
	s.UpsertLink(model.Link{From: "c", To: ""})
	s.UpsertLink(model.Link{From: "  ", To: "c"})
	if len(s.Links()) != 4 {
		t.Errorf("links = %d, want empty-endpoint edges ignored", len(s.Links()))
	}
	if s.NodeCount() != 0 {
		t.Errorf("NodeCount = %d, links must not invent nodes", s.NodeCount())
	}
}

func TestMarkProbeShape(t *testing.T) {
	s := New()
	s.MarkProbe("brand-new", ProbeOutcome{Online: true, LatencyMs: 4.5, Sent: 4, Recv: 4, TTL: 63, IP: "10.2.2.2"})

	n := mustNode(t, s, "brand-new")
	if !n.Online || n.LatencyMs != 4.5 || n.PacketsSent != 4 || n.PacketsRecv != 4 || n.TTL != 63 {
		t.Errorf("probe not recorded: %+v", n)
	}
	if n.MeshIP != "10.2.2.2" {
		t.Errorf("MeshIP = %q", n.MeshIP)
	}
	if n.ProbedAt.IsZero() || n.UpdatedAt.IsZero() || n.DiscoveredAt.IsZero() {
		t.Error("an unknown id must get a shell node so no verdict is dropped")
	}

	// A later failure reuses the same shell.
	s.MarkProbe("brand-new", ProbeOutcome{PacketLoss: 100, Sent: 3, Recv: 0, Err: "host unreachable"})
	n = mustNode(t, s, "brand-new")
	if n.Online || n.LatencyMs != 0 || n.PacketsRecv != 0 || n.ProbeError != "host unreachable" {
		t.Errorf("offline verdict: %+v", n)
	}
	if n.PacketLossPct != 100 || n.TTL != 63 {
		t.Errorf("loss or TTL mishandled: %+v", n)
	}
	if s.NodeCount() != 1 {
		t.Fatalf("NodeCount = %d, want the shell reused", s.NodeCount())
	}

	// A successful probe clears a previous fetch error and the park that came with it.
	s.MarkFailure("brand-new", "404 not found", time.Now().Add(time.Minute))
	if mustNode(t, s, "brand-new").LastError == "" {
		t.Fatal("MarkFailure did not record")
	}
	s.MarkProbe("brand-new", ProbeOutcome{Online: true, ProbedAt: time.Now()})
	if got := mustNode(t, s, "brand-new").LastError; got != "" {
		t.Errorf("LastError = %q, a live probe clears it", got)
	}

	s.MarkProbe("", ProbeOutcome{Online: true})
	if s.NodeCount() != 1 {
		t.Fatalf("empty id created a node: %d", s.NodeCount())
	}
}

// A peer's "my link to that node is down" describes the peer's link, not the node's liveness.
func TestMeshStateYieldsToLiveEvidence(t *testing.T) {
	s := New()
	s.UpsertNode(discoveryStub("leaf-1", 2))

	down := time.Date(2026, 10, 17, 2, 10, 0, 0, time.UTC)
	s.MarkMeshState("leaf-1", model.MeshStateDisconnected, down)
	n := mustNode(t, s, "leaf-1")
	if !n.Disconnected || n.MeshState != model.MeshStateDisconnected || !n.LinkDownSince.Equal(down) {
		t.Fatalf("stamp not recorded: %+v", n)
	}

	// The node was stamped while unprobed, so the MarkMeshState guard had nothing to object to.
	s.MarkProbe("leaf-1", ProbeOutcome{Online: true, LatencyMs: 33.8, Sent: 4, Recv: 4})
	n = mustNode(t, s, "leaf-1")
	if n.Disconnected || n.MeshState != "" || !n.LinkDownSince.IsZero() {
		t.Errorf("a live probe left the down label in place: %+v", n)
	}

	// A node we are already talking to is never stamped at all.
	s.MarkMeshState("leaf-1", model.MeshStateDisconnected, down)
	if n = mustNode(t, s, "leaf-1"); n.Disconnected {
		t.Errorf("online node got marked disconnected by a peer's link")
	}

	// Still unreachable, so the mesh's word stands.
	s.MarkProbe("leaf-1", ProbeOutcome{PacketLoss: 100, Sent: 3, Recv: 0})
	s.MarkMeshState("leaf-1", model.MeshStateDisconnected, down)
	if n = mustNode(t, s, "leaf-1"); !n.Disconnected {
		t.Errorf("unreachable node lost the mesh report: %+v", n)
	}
}

func TestMarkDetailFetchedAndFailure(t *testing.T) {
	s := New()
	s.UpsertNode(discoveryStub("a", 1))
	if mustNode(t, s, "a").DetailFetched {
		t.Fatal("a fresh stub must be pending")
	}
	s.MarkDetailFetched("A")
	s.MarkFailure("a", "read tcp: timeout", time.Time{})

	n := mustNode(t, s, "a")
	if !n.DetailFetched || n.LastError != "read tcp: timeout" {
		t.Errorf("marks not applied: %+v", n)
	}
	// A bookkeeping mark must not invent observed fields; Band is only the unknown marker here.
	if n.Band != model.BandUnresolved || n.BandLabel != model.BandLabels[model.BandUnresolved] ||
		n.Source != "neighbor-list" || n.URL == "" {
		t.Errorf("marks clobbered other fields: %+v", n)
	}
	s.MarkFailure("a", "   ", time.Time{})
	if mustNode(t, s, "a").LastError != "read tcp: timeout" {
		t.Error("a blank reason must not overwrite a real one")
	}
	s.MarkDetailFetched("ghost")
	if s.NodeCount() != 2 {
		t.Fatalf("MarkDetailFetched should create a shell: %d", s.NodeCount())
	}
}

func TestMergeNodeCreatesAndStamps(t *testing.T) {
	s := New()
	s.MergeNode("  Node-9 ", func(n *model.Node) {
		n.Name = "Node-9"
		n.Neighbors = []string{"W6Cool-7", "w6cool-7"}
		n.Hostname = "clobbered"
	})
	n := mustNode(t, s, "node-9")
	if n.Hostname != "node-9" {
		t.Errorf("Hostname = %q, want the canonical key restored", n.Hostname)
	}
	if n.DiscoveredAt.IsZero() || n.UpdatedAt.IsZero() {
		t.Error("a created shell must be stamped")
	}
	if want := []string{"w6cool-7"}; !slices.Equal(n.Neighbors, want) {
		t.Errorf("MergeNode must canonicalise sets: %q", n.Neighbors)
	}

	first := n.UpdatedAt
	time.Sleep(2 * time.Millisecond)
	s.MergeNode("node-9", func(n *model.Node) { n.Description = "x" })
	if after := mustNode(t, s, "node-9").UpdatedAt; !after.After(first) {
		t.Errorf("UpdatedAt = %v, must advance past %v", after, first)
	}
	s.MergeNode("node-9", nil)
	s.MergeNode("", func(n *model.Node) { n.Name = "boom" })
	if s.NodeCount() != 1 {
		t.Fatalf("NodeCount = %d", s.NodeCount())
	}
}

func TestCountsDefinitions(t *testing.T) {
	s := New()
	// Online, located, detailed, supernode.
	s.UpsertNode(&model.Node{
		Hostname: "sup-1", Name: "SUP", HasLocation: true, Lat: 1, IsSupernode: true,
		DetailFetched: true, Band: model.BandSupernode, Services: []string{"BBS"},
	})
	s.MarkProbe("sup-1", ProbeOutcome{Online: true, LatencyMs: 2, ProbedAt: time.Now()})
	// Located but unreachable: the probe failed.
	s.UpsertNode(&model.Node{Hostname: "n5bad-1", Name: "N5BAD", HasLocation: true, Lat: 2, Services: []string{"bbs"}})
	s.MarkProbe("n5bad-1", ProbeOutcome{Online: false, Err: "no reply", ProbedAt: time.Now()})
	// Discovered only: no location, no detail, never probed -> pending + unreachable.
	s.UpsertNode(discoveryStub("w6cool-7", 1))
	// Detailed but still unlocated -> not pending, unreachable.
	s.UpsertNode(&model.Node{Hostname: "k6aa-2", Name: "K6AA", DetailFetched: true})
	// Online, located and detailed.
	s.UpsertNode(&model.Node{Hostname: "online-loc", Name: "OL", HasLocation: true, Lat: 3, DetailFetched: true})
	s.MarkProbe("online-loc", ProbeOutcome{Online: true, ProbedAt: time.Now()})

	s.UpsertLink(model.Link{From: "sup-1", To: "n5bad-1", Type: model.LinkTypeRF})
	s.UpsertLink(model.Link{From: "sup-1", To: "w6cool-7", Type: model.LinkTypeDtD})

	want := model.Counts{Total: 5, Online: 2, Offline: 1, Unprobed: 2, Unreachable: 3, Located: 3,
		Supernodes: 1, Pending: 1, Links: 2, Services: 1}
	if got := s.Counts(); got != want {
		t.Errorf("Counts = %+v, want %+v", got, want)
	}

	// Services dedup case-insensitively; local services are not published.
	s.UpsertNode(&model.Node{Hostname: "k6aa-2", Name: "K6AA", LocalServices: []string{"printer"}, LocalDevices: []string{"cam"}})
	if got := s.Counts(); got.Services != 1 {
		t.Errorf("Services = %d, LocalServices must not count", got.Services)
	}
	if empty := New().Counts(); empty != (model.Counts{}) {
		t.Errorf("empty store counts = %+v", empty)
	}
}

func TestBandAndLinkTypeCounts(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{Hostname: "a", Band: model.Band5GHz})
	s.UpsertNode(&model.Node{Hostname: "b", Band: model.Band5GHz})
	s.UpsertNode(discoveryStub("c", 1)) // never read: Unscanned, not Unknown
	s.UpsertLink(model.Link{From: "a", To: "b", Type: model.LinkTypeRF})
	s.UpsertLink(model.Link{From: "b", To: "a", Type: model.LinkTypeRF})
	s.UpsertLink(model.Link{From: "a", To: "c", Type: model.LinkTypeDtD})
	s.UpsertLink(model.Link{From: "c", To: "a"}) // no type -> counted as RF

	if got := s.BandCounts(); !reflect.DeepEqual([]int{got[model.Band5GHz], got[model.BandUnscanned], len(got)}, []int{2, 1, 2}) {
		t.Errorf("BandCounts = %v", got)
	}
	if got := s.LinkTypeCounts(); !reflect.DeepEqual([]int{got[model.LinkTypeRF], got[model.LinkTypeDtD], len(got)}, []int{3, 1, 2}) {
		t.Errorf("LinkTypeCounts = %v", got)
	}
	if got := New().BandCounts(); len(got) != 0 {
		t.Errorf("empty BandCounts = %v", got)
	}
}

func TestSeedsAndReset(t *testing.T) {
	s := New()
	s.SetSeeds([]string{"Localnode.Local.Mesh", " localnode.local.mesh ", "", "other.mesh"})
	if want := []string{"Localnode.Local.Mesh", "other.mesh"}; !slices.Equal(s.Seeds(), want) {
		t.Errorf("Seeds = %q, want %q", s.Seeds(), want)
	}
	s.UpsertNode(discoveryStub("a", 0))
	s.MarkProbe("a", ProbeOutcome{Online: true, ProbedAt: time.Now()})
	s.UpsertLink(model.Link{From: "a", To: "b"})

	s.Reset()
	if s.NodeCount() != 0 || len(s.Links()) != 0 {
		t.Fatalf("Reset left data: %d nodes, %d links", s.NodeCount(), len(s.Links()))
	}
	if c := s.Counts(); c != (model.Counts{}) {
		t.Errorf("Counts after Reset = %+v", c)
	}
	if len(s.Topology().Nodes) != 0 || len(s.Topology().Links) != 0 {
		t.Error("Topology after Reset not empty")
	}
	if len(s.IDsNeedingDetail(-1, time.Minute)) != 0 || len(s.LiveNodeNames()) != 0 {
		t.Error("reads after Reset not empty")
	}
	if want := []string{"Localnode.Local.Mesh", "other.mesh"}; !slices.Equal(s.Seeds(), want) {
		t.Errorf("Reset must keep the operator seeds, got %q", s.Seeds())
	}
	s.SetSeeds(nil)
	if len(s.Seeds()) != 0 {
		t.Errorf("Seeds = %q after nil", s.Seeds())
	}

	// The store stays usable for the next sweep.
	s.UpsertNode(discoveryStub("a", 0))
	if s.NodeCount() != 1 || s.Counts().Total != 1 {
		t.Fatal("store unusable after Reset")
	}
}

func TestTopologyShapeAndDanglingEdges(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{
		Hostname: "Sup-1", Name: "SUP-1", Band: model.BandSupernode, IsSupernode: true,
		Neighbors: []string{"n5bad-1", "ghost-9"}, Lat: 31.2, Lon: 121.4, HasLocation: true,
	})
	s.MarkProbe("sup-1", ProbeOutcome{Online: true, LatencyMs: 1.5, ProbedAt: time.Now()})
	s.UpsertNode(&model.Node{Hostname: "n5bad-1", Name: "N5BAD-1", Band: model.Band5GHz, Hops: 1, ETX: 120})
	// Crawled makes this "read and unidentifiable" (Unknown), not "never read" (Unscanned).
	s.UpsertNode(&model.Node{Hostname: "odd-1", Name: "ODD-1", Band: "zz", BandLabel: "Weird band", Crawled: true})
	// Known by name only from a directory: in the node list, but never in a graph.
	s.UpsertNode(&model.Node{Hostname: "lonely-9", Name: "LONELY-9", Hops: -1})

	s.UpsertLink(model.Link{From: "sup-1", To: "odd-1", Type: model.LinkTypeRF})
	s.UpsertLink(model.Link{From: "sup-1", To: "n5bad-1", Type: model.LinkTypeRF, Band: model.Band5GHz})
	s.UpsertLink(model.Link{From: "sup-1", To: "ghost-9", Type: model.LinkTypeRF})
	s.UpsertLink(model.Link{From: "n5bad-1", To: "deleted-1", Type: model.LinkTypeDtD})
	s.UpsertLink(model.Link{From: "n5bad-1", To: "sup-1", Type: model.LinkTypeRF, MAC: "aa:bb"})

	tp := s.Topology()
	// Three nodes sit on an edge; lonely-9 does not, so it is not a vertex.
	if len(tp.Nodes) != 3 {
		t.Fatalf("Topology nodes = %d, want sup-1/n5bad-1/odd-1 and not lonely-9", len(tp.Nodes))
	}
	for _, n := range tp.Nodes {
		if n.ID == "lonely-9" {
			t.Error("a node with no observed edge was emitted as a vertex")
		}
	}
	if tp.UnconnectedCount != 1 {
		t.Errorf("UnconnectedCount = %d, want the 1 directory-only node", tp.UnconnectedCount)
	}
	if tp.GeneratedAt.IsZero() {
		t.Error("GeneratedAt must be stamped")
	}
	byID := map[string]model.TopologyNode{}
	for _, n := range tp.Nodes {
		byID[n.ID] = n
	}
	sup := byID["sup-1"]
	if sup.ID != "sup-1" {
		t.Errorf("vertex ID = %q, want the lowercase hostname", sup.ID)
	}
	if sup.Color != model.BandColors[model.BandSupernode] || sup.BandLabel != model.BandLabels[model.BandSupernode] {
		t.Errorf("supernode vertex not coloured/labeled from the shared tables: %+v", sup)
	}
	if sup.NeighborCnt != 2 || !sup.Online || sup.LatencyMs != 1.5 {
		t.Errorf("supernode vertex: %+v", sup)
	}
	if sup.Lat != 31.2 || sup.Lon != 121.4 || !sup.IsSupernode {
		t.Errorf("supernode vertex position/flag: %+v", sup)
	}
	if byID["odd-1"].Color != model.BandColors[model.BandUnresolved] {
		t.Errorf("unknown band color = %q", byID["odd-1"].Color)
	}
	// BandLabel is derived, never carried; a hand-written label could disagree with the band key.
	if byID["odd-1"].BandLabel != model.BandLabels[model.BandUnresolved] {
		t.Errorf("unknown band label = %q, want it derived from the band key", byID["odd-1"].BandLabel)
	}
	if byID["n5bad-1"].Band != model.Band5GHz || byID["n5bad-1"].BandLabel != "5 GHz" || byID["n5bad-1"].ETX != 120 {
		t.Errorf("n5bad vertex: %+v", byID["n5bad-1"])
	}
	// sup-1/n5bad-1 is stored from both sides for its own SNR/ETX; as a drawing edge it is one wire.
	if len(tp.Links) != 2 {
		t.Fatalf("Topology links = %d, want sup-1↔odd-1 and sup-1↔n5bad-1: %+v", len(tp.Links), tp.Links)
	}
	for _, l := range tp.Links {
		if l.To == "ghost-9" || l.To == "deleted-1" || l.From == "ghost-9" {
			t.Errorf("dangling edge kept: %+v", l)
		}
	}
	for i := 1; i < len(tp.Links); i++ {
		if compareLinks(tp.Links[i-1], tp.Links[i]) > 0 {
			t.Errorf("topology edges unordered: %+v", tp.Links)
		}
	}

	// Once the neighbor is discovered itself, its edge becomes real.
	s.UpsertNode(&model.Node{Hostname: "ghost-9", Name: "GHOST-9"})
	if got := len(s.Topology().Links); got != 3 {
		t.Errorf("Topology links = %d after ghost-9 was discovered, want 3", got)
	}

	empty := New().Topology()
	if empty.Nodes == nil || empty.Links == nil {
		t.Errorf("an empty topology should marshal as arrays, got %+v", empty)
	}
}

func TestIDsNeedingDetail(t *testing.T) {
	s := New()
	now := time.Now()
	fresh := now.Add(-time.Minute)
	stale := now.Add(-30 * time.Minute)

	put := func(host string, hops int, fetched bool, at time.Time) {
		s.UpsertNode(&model.Node{Hostname: host, Name: host, Hops: hops, DetailFetched: fetched, UpdatedAt: at})
	}
	put("never-a", 0, false, stale)
	put("never-b", 1, false, stale)
	put("never-c", 5, false, stale)
	put("fresh-d", 1, true, fresh)
	put("stale-e", 2, true, stale)
	put("future-f", 2, true, now.Add(time.Hour))

	// Never fetched is always wanted, a fetch inside the relazy window is skipped, hops then ID.
	if got := s.IDsNeedingDetail(-1, 10*time.Minute); !slices.Equal(got, []string{"never-a", "never-b", "stale-e", "never-c"}) {
		t.Errorf("IDsNeedingDetail(-1, 10m) = %q", got)
	}
	// maxHops keeps nodes at or within the limit; -1 means no limit.
	if got := s.IDsNeedingDetail(1, 10*time.Minute); !slices.Equal(got, []string{"never-a", "never-b"}) {
		t.Errorf("IDsNeedingDetail(1) = %q, want the hop filter applied", got)
	}
	if got := s.IDsNeedingDetail(0, 10*time.Minute); !slices.Equal(got, []string{"never-a"}) {
		t.Errorf("IDsNeedingDetail(0) = %q, want only hop 0", got)
	}
	// With relazy = 1h even the half-hour-old detail is still fresh enough.
	if got := s.IDsNeedingDetail(-1, time.Hour); !slices.Equal(got, []string{"never-a", "never-b", "never-c"}) {
		t.Errorf("IDsNeedingDetail(-1, 1h) = %q", got)
	}
	// relazy = 0 makes every past timestamp stale, but not a future-dated one.
	if got := s.IDsNeedingDetail(-1, 0); !slices.Equal(got, []string{"never-a", "fresh-d", "never-b", "stale-e", "never-c"}) {
		t.Errorf("IDsNeedingDetail(-1, 0) = %q, want everything but future-f", got)
	}
	if got := s.IDsNeedingDetail(-1, 24*time.Hour); !slices.Equal(got, []string{"never-a", "never-b", "never-c"}) {
		t.Errorf("IDsNeedingDetail(-1, 24h) = %q, want only never-fetched", got)
	}
	if got := New().IDsNeedingDetail(-1, time.Minute); got == nil || len(got) != 0 {
		t.Errorf("empty store IDsNeedingDetail = %v, want an empty non-nil slice", got)
	}
	if !slices.Equal(s.IDsNeedingDetail(-1, time.Minute), s.IDsNeedingDetail(-1, time.Minute)) {
		t.Error("IDsNeedingDetail order is not deterministic")
	}
}

func TestLiveNodeNames(t *testing.T) {
	s := New()
	for _, host := range []string{"zz-1", "aa-1", "mm-1"} {
		s.UpsertNode(discoveryStub(host, 0))
	}
	s.MarkProbe("mm-1", ProbeOutcome{Online: true, ProbedAt: time.Now()})
	s.MarkProbe("ZZ-1", ProbeOutcome{Online: true, ProbedAt: time.Now()})
	s.MarkProbe("aa-1", ProbeOutcome{Online: false, Err: "no route", ProbedAt: time.Now()})

	if got := s.LiveNodeNames(); !slices.Equal(got, []string{"mm-1", "zz-1"}) {
		t.Errorf("LiveNodeNames = %q, want the sorted online hostnames", got)
	}
	s.MarkProbe("mm-1", ProbeOutcome{Online: false, Err: "gone", ProbedAt: time.Now()})
	if got := s.LiveNodeNames(); !slices.Equal(got, []string{"zz-1"}) {
		t.Errorf("LiveNodeNames after a failure = %q", got)
	}
}

func TestSnapshotCoherence(t *testing.T) {
	s := New()
	for i := range 30 {
		host := fmt.Sprintf("n%02d", i)
		s.UpsertNode(&model.Node{
			Hostname: host, Name: strings.ToUpper(host), Band: model.Band5GHz, Hops: i % 4,
			Neighbors: []string{fmt.Sprintf("n%02d", (i+1)%30)},
		})
		s.UpsertLink(model.Link{From: host, To: fmt.Sprintf("n%02d", (i+1)%30), Type: model.LinkTypeRF})
	}
	s.SetSeeds([]string{"seed.mesh"})
	s.MarkProbe("n07", ProbeOutcome{Online: true, ProbedAt: time.Now()})

	snap := s.Snapshot()
	if len(snap.Nodes) != 30 || len(snap.Links) != 30 {
		t.Fatalf("snapshot sizes = %d/%d", len(snap.Nodes), len(snap.Links))
	}
	if snap.Counts.Total != 30 || snap.Counts.Links != 30 || snap.Counts.Online != 1 {
		t.Errorf("snapshot counts = %+v", snap.Counts)
	}
	if snap.Counts != s.Counts() {
		t.Errorf("snapshot counts drifted from a separate read: %+v vs %+v", snap.Counts, s.Counts())
	}
	if snap.BandCounts[model.Band5GHz] != 30 || snap.LinkTypeCounts[model.LinkTypeRF] != 30 {
		t.Errorf("snapshot maps = %v %v", snap.BandCounts, snap.LinkTypeCounts)
	}
	if snap.TakenAt.IsZero() {
		t.Error("TakenAt not stamped")
	}
	if !slices.IsSortedFunc(snap.Nodes, func(a, b *model.Node) int { return strings.Compare(a.Name, b.Name) }) {
		t.Error("snapshot nodes unordered")
	}
	if !slices.IsSortedFunc(snap.Links, compareLinks) {
		t.Error("snapshot links unordered")
	}
}

func TestEmptyStoreReads(t *testing.T) {
	s := New()
	if n := s.Nodes(); n == nil || len(n) != 0 {
		t.Errorf("Nodes = %v", n)
	}
	if l := s.Links(); l == nil || len(l) != 0 {
		t.Errorf("Links = %v", l)
	}
	if c := s.Counts(); c != (model.Counts{}) {
		t.Errorf("Counts = %+v", c)
	}
	if s.NodeCount() != 0 || len(s.Seeds()) != 0 || len(s.LiveNodeNames()) != 0 {
		t.Error("empty store reads")
	}
	snap := s.Snapshot()
	if snap.Nodes == nil || snap.Links == nil || snap.BandCounts == nil || snap.LinkTypeCounts == nil {
		t.Errorf("a snapshot should hold empty non-nil collections: %+v", snap)
	}
	s.DeleteNode("ghost")
	s.MarkProbe("probed-before-discovery", ProbeOutcome{})
	if s.NodeCount() != 1 {
		t.Errorf("NodeCount = %d, a verdict for an unknown id must not be dropped", s.NodeCount())
	}
}

// TestConcurrentWritersAndReaders is the -race hammer: writers mutate while readers snapshot.
func TestConcurrentWritersAndReaders(t *testing.T) {
	const (
		writers = 16
		readers = 16
		rounds  = 40
	)
	s := New()
	s.SetSeeds([]string{"seed-1.local.mesh"})

	var rwg, wwg sync.WaitGroup
	stop := make(chan struct{})

	for r := range readers {
		rwg.Add(1)
		go func(r int) {
			defer rwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// A hair of pacing keeps spinning readers from starving the writers.
				time.Sleep(200 * time.Microsecond)
				snap := s.Snapshot()
				for _, n := range snap.Nodes {
					_ = len(n.Neighbors) + len(n.Services) + len(n.Aliases) + len(n.LocalDevices)
					for range n.LinkTypes {
					}
					for range n.RFTypeCounts {
					}
				}
				for _, v := range s.Topology().Nodes {
					_ = v.Color + v.BandLabel
				}
				for _, n := range s.Nodes() {
					n.Name = "discarded"
					if len(n.Neighbors) > 0 {
						n.Neighbors[0] = "discarded"
					}
					n.LinkTypes["RF"] = 1
				}
				for _, l := range s.Links() {
					_ = l.From + l.To
				}
				c := s.Counts()
				// Writers race with us, so only a snapshot's own numbers against its own slices.
				if snap.Counts.Total != len(snap.Nodes) {
					t.Errorf("snapshot self-inconsistent: %+v vs %d nodes", snap.Counts, len(snap.Nodes))
					return
				}
				// Counts.Links is physical edges while snap.Links keeps both directions.
				if snap.Counts.Links > len(snap.Links) || snap.Counts.Links == 0 && len(snap.Links) > 0 {
					t.Errorf("link count %d cannot describe %d observation rows", snap.Counts.Links, len(snap.Links))
					return
				}
				if snap.Counts.Online+snap.Counts.Offline+snap.Counts.Unprobed != snap.Counts.Total {
					t.Errorf("online+offline+unprobed != total: %+v", snap.Counts)
					return
				}
				if c.Online > c.Total || c.Offline > c.Total || c.Unreachable > c.Total {
					t.Errorf("impossible counters while sweeping: %+v", c)
					return
				}
				_ = s.BandCounts()
				_ = s.LinkTypeCounts()
				_ = s.IDsNeedingDetail(-1, 10*time.Minute)
				_ = s.IDsNeedingDetail(2, 0)
				_ = s.LiveNodeNames()
				_ = s.Seeds()
				_ = s.NodeCount()
				if n, ok := s.NodeByID(fmt.Sprintf("node-%02d-00", r%writers)); ok {
					_ = n.Firmware
				}
			}
		}(r)
	}

	for w := range writers {
		wwg.Add(1)
		go func(w int) {
			defer wwg.Done()
			for i := range rounds {
				host := fmt.Sprintf("node-%02d-%02d", w, i%7)
				s.UpsertNode(&model.Node{
					Hostname: strings.ToUpper(host), Name: host, Band: model.Band5GHz,
					Hops: i % 5, Lat: float64(i), Lon: float64(i), HasLocation: i%3 == 0,
					Neighbors: []string{fmt.Sprintf("node-%02d-%02d", (w+1)%writers, i%7)},
					Services:  []string{"BBS", "HTTP"}, LinkTypes: map[string]int{model.LinkTypeRF: i},
					Firmware: "4.1.1",
				})
				s.MarkProbe(host, ProbeOutcome{Online: i%3 != 0, LatencyMs: float64(i),
					ProbedAt: time.Now(), Sent: 3, Recv: i % 4})
				s.MergeNode(host, func(n *model.Node) { n.Neighbors = append(n.Neighbors, "w6cool-7") })
				s.UpsertLink(model.Link{From: host, To: fmt.Sprintf("node-%02d-%02d", w, i%7),
					Type: model.LinkTypeRF, MAC: fmt.Sprint(i), LastSeen: time.Now()})
				s.MarkDetailFetched(host)
				if i%10 == 0 {
					s.MarkFailure(host, "boom", time.Time{})
					s.DeleteNode(fmt.Sprintf("node-%02d-%03d", w, i))
				}
			}
		}(w)
	}

	// The seeds churn while the sweep is in flight.
	wwg.Go(func() {
		for i := range 20 {
			s.SetSeeds([]string{fmt.Sprintf("seed-%d", i)})
			time.Sleep(time.Millisecond)
		}
	})

	wwg.Wait()
	close(stop)
	rwg.Wait()

	snap := s.Snapshot()
	if snap.Counts.Total != s.NodeCount() || snap.Counts.Links > len(snap.Links) {
		t.Errorf("final snapshot inconsistent: %+v, nodes %d, link rows %d", snap.Counts, s.NodeCount(), len(snap.Links))
	}
	for _, n := range snap.Nodes {
		if !slices.IsSorted(n.Neighbors) {
			t.Fatalf("Neighbors lost their canonical order: %q", n.Neighbors)
		}
	}
	if got := len(s.LiveNodeNames()); got != snap.Counts.Online {
		t.Errorf("LiveNodeNames %d != Counts.Online %d", got, snap.Counts.Online)
	}
	if c := s.Counts(); c.Pending+c.Supernodes < 0 {
		t.Errorf("counts after the sweep: %+v", c)
	}

	s.Reset()
	if got := s.NodeCount(); got != 0 {
		t.Fatalf("NodeCount after Reset = %d", got)
	}
}

func TestConcurrentWritesToOneNode(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			host := fmt.Sprintf("shared-%d", i%2)
			s.UpsertNode(&model.Node{
				Hostname:  host,
				Neighbors: []string{fmt.Sprintf("nb-%02d", i)},
				Services:  []string{fmt.Sprintf("svc-%02d", i)},
			})
			s.MarkProbe(host, ProbeOutcome{Online: i%2 == 0, ProbedAt: time.Now()})
			s.UpsertLink(model.Link{From: host, To: fmt.Sprintf("nb-%02d", i), Type: model.LinkTypeRF})
		}(i)
	}
	wg.Wait()

	n := mustNode(t, s, "shared-0")
	if len(n.Neighbors) != 16 {
		t.Errorf("Neighbors = %d, want all 16 unioned", len(n.Neighbors))
	}
	if len(n.Services) != 16 {
		t.Errorf("Services = %d, want all 16 unioned", len(n.Services))
	}
	if got := len(s.Links()); got != 32 {
		t.Errorf("links = %d, want 32", got)
	}
}

// Hop 0 is a measurement, not an absence: it is the node the crawl started at.
func TestHopsMergeKeepsTheRootAtZero(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{Hostname: "home-ap", Name: "HOME-AP", Crawled: true, Hops: 0})

	// A peer two hops out lists the same node; that is a longer path, not new information.
	s.UpsertNode(&model.Node{Hostname: "home-ap", Hops: 2})
	if got := mustNode(t, s, "home-ap").Hops; got != 0 {
		t.Errorf("hops = %d after a hop-2 stub, want the root to stay at 0", got)
	}

	// Neither does the mesh host table, which knows no distance at all.
	s.UpsertNode(&model.Node{Hostname: "home-ap", Hops: -1})
	if got := mustNode(t, s, "home-ap").Hops; got != 0 {
		t.Errorf("hops = %d after an unknown stub, want 0", got)
	}

	// A node first heard of through a peer keeps that peer's count; a shorter path still wins.
	s.UpsertNode(&model.Node{Hostname: "far-1", Hops: -1})
	if got := mustNode(t, s, "far-1").Hops; got != -1 {
		t.Fatalf("hops = %d, want the unknown marker preserved", got)
	}
	s.UpsertNode(&model.Node{Hostname: "far-1", Hops: 5})
	s.UpsertNode(&model.Node{Hostname: "far-1", Hops: 3})
	if got := mustNode(t, s, "far-1").Hops; got != 3 {
		t.Errorf("hops = %d, want the shorter path", got)
	}
	s.UpsertNode(&model.Node{Hostname: "far-1", Hops: 4})
	if got := mustNode(t, s, "far-1").Hops; got != 3 {
		t.Errorf("hops = %d after a longer path, want 3 kept", got)
	}

	// The engine records a hop count through MergeNode, so the same rule has to hold there too.
	s.MergeNode("home-ap", func(n *model.Node) { MergeHops(n, 2) })
	if got := mustNode(t, s, "home-ap").Hops; got != 0 {
		t.Errorf("MergeHops let a peer two hops out relabel the root: %d", got)
	}
	s.MergeNode("far-1", func(n *model.Node) { MergeHops(n, 1) })
	if got := mustNode(t, s, "far-1").Hops; got != 1 {
		t.Errorf("MergeHops = %d, want the shorter path kept", got)
	}
}

// The band says what we know about a node, not only what its page said.
func TestBandDistinguishesUnscannedFromUnknown(t *testing.T) {
	s := New()
	s.UpsertNode(&model.Node{Hostname: "cloud-name", Name: "CLOUD-NAME"})
	s.MarkProbe("pinged-only", ProbeOutcome{Online: true, ProbedAt: time.Now()})
	s.UpsertNode(&model.Node{Hostname: "read-node", Name: "READ-NODE", Crawled: true, Band: model.BandUnresolved})

	for id, want := range map[string]string{
		"cloud-name":  model.BandUnscanned,
		"pinged-only": model.BandUnscanned,
		"read-node":   model.BandUnresolved,
	} {
		n, ok := s.NodeByID(id)
		if !ok {
			t.Fatalf("%s missing", id)
		}
		if n.Band != want || n.BandLabel != model.BandLabels[want] {
			t.Errorf("%s: band = %q/%q, want %q", id, n.Band, n.BandLabel, want)
		}
	}

	// Reading the page is what moves it out of Unscanned.
	s.UpsertNode(&model.Node{Hostname: "cloud-name", Name: "CLOUD-NAME", Crawled: true, Band: model.Band2_4GHz})
	if n, _ := s.NodeByID("cloud-name"); n.Band != model.Band2_4GHz {
		t.Errorf("band = %q after the page was read, want the reported 2.4 GHz", n.Band)
	}
	if c := s.BandCounts(); c[model.BandUnscanned] == 0 || c[model.BandUnresolved] == 0 {
		t.Errorf("band counts collapsed the two buckets: %+v", c)
	}
}
