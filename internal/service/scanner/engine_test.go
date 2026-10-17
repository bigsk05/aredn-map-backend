package scanner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"aredn-map/internal/infra/config"
	"aredn-map/internal/model"
	"aredn-map/internal/repository/aredn"
	"aredn-map/internal/repository/ping"
	"aredn-map/internal/repository/store"
)

// testNode builds a fixture node; "localnode" is the seed alias that re-keys onto the access point.
func testNode(host, name string) *model.Node {
	return &model.Node{
		Hostname: host,
		Name:     name,
		URL:      aredn.NodeURL(host),
		Band:     model.Band2_4GHz,
		Source:   "status-page",
		// A node's own page never states distance; zero would read as "the root".
		Hops: -1,
	}
}

func testLink(from, to, kind, container string) model.Link {
	return model.Link{
		From:      from,
		To:        to,
		Type:      kind,
		Container: container,
		MAC:       "02:00:00:00:00:0c",
		IP:        "172.31.9.9",
		PeerName:  to,
	}
}

type fakeNet struct {
	mu sync.Mutex

	// self and neighbours keyed by normalized host; a missing key answers error.
	self      map[string]*model.Node
	canonical map[string]string // crawled label -> node record hostname
	neigh     map[string][]model.Link
	mesh      map[string]*aredn.MeshData

	selfCalls     map[string]int
	selfSeq       []string
	neighCalls    map[string]int
	neighDetail   map[string]bool
	absorbHosts   map[string][]string
	detailBeyondH map[string]bool

	// hold, when set, stalls every FetchSelf so the overlapping-sweep test cannot race.
	hold chan struct{}
}

func newFakeNet() *fakeNet {
	return &fakeNet{
		self: map[string]*model.Node{}, canonical: map[string]string{},
		neigh: map[string][]model.Link{}, mesh: map[string]*aredn.MeshData{},
		selfCalls: map[string]int{}, neighCalls: map[string]int{},
		neighDetail: map[string]bool{}, detailBeyondH: map[string]bool{},
		absorbHosts: map[string][]string{},
	}
}

func (f *fakeNet) add(host string, n *model.Node, links []model.Link) {
	f.self[NormalizeHost(host)] = n
	f.neigh[NormalizeHost(host)] = links
}

// resolve maps a crawled label onto the node it really is, the way a seed alias does.
func (f *fakeNet) resolve(h string) string {
	if canon, ok := f.canonical[h]; ok && f.hasSelf(canon) {
		return canon
	}
	return h
}

func (f *fakeNet) hasSelf(h string) bool {
	_, ok := f.self[h]
	return ok
}

func (f *fakeNet) FetchSelf(ctx context.Context, hostname string) (*model.Node, error) {
	f.mu.Lock()
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.resolve(NormalizeHost(hostname))
	f.selfCalls[h]++
	f.selfSeq = append(f.selfSeq, h)
	n, ok := f.self[h]
	if !ok {
		return nil, fmt.Errorf("aredn: no route to %s", h)
	}
	out := *n
	out.Hostname = h
	if canon, ok := f.canonical[NormalizeHost(hostname)]; ok {
		out.CanonicalHost = canon
	}
	return &out, nil
}

func (f *fakeNet) FetchNeighbors(ctx context.Context, hostname string, withDetail bool, concurrency int) ([]model.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.resolve(NormalizeHost(hostname))
	f.neighCalls[h]++
	// Keep the first discovery answer; later detail re-fetches must not overwrite it.
	if _, seen := f.neighDetail[h]; !seen {
		f.neighDetail[h] = withDetail
	}
	links, ok := f.neigh[h]
	if !ok {
		return nil, fmt.Errorf("aredn: no route to %s", h)
	}
	out := make([]model.Link, len(links))
	copy(out, links)
	return out, nil
}

func (f *fakeNet) FetchMesh(ctx context.Context, hostname string) (*aredn.MeshData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.resolve(NormalizeHost(hostname))
	md, ok := f.mesh[h]
	if !ok {
		return nil, fmt.Errorf("aredn: no mesh table at %s", h)
	}
	return md, nil
}

func (f *fakeNet) count(m map[string]int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(m)
}

func (f *fakeNet) seen(m map[string]int, host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return m[NormalizeHost(host)]
}

type fakePinger struct {
	mu    sync.Mutex
	hosts []string
	live  map[string]bool
}

func (p *fakePinger) Mode() string { return "unprivileged" }

// Active/Slots back the status counters; the fake never holds more than it answers for.
func (p *fakePinger) Active() int { return 0 }
func (p *fakePinger) Slots() int  { return 16 }

// Ping answers the same way as the batch path, since the live probe pool calls it.
func (p *fakePinger) Ping(ctx context.Context, host string) ping.Result {
	p.mu.Lock()
	p.hosts = append(p.hosts, NormalizeHost(host))
	p.mu.Unlock()
	r := ping.Result{Host: host, Sent: 3, Mode: "unprivileged", AvgMs: 4.2, MdevMs: 0.3, TTL: 64}
	if p.live == nil || p.live[NormalizeHost(host)] {
		r.Recv = 3
	} else {
		r.Recv = 0
		r.Err = errors.New("no reply")
	}
	return r
}

func (p *fakePinger) PingMany(ctx context.Context, hosts []string) map[string]ping.Result {
	p.mu.Lock()
	p.hosts = append(p.hosts, hosts...)
	p.mu.Unlock()
	out := make(map[string]ping.Result, len(hosts))
	for _, h := range hosts {
		r := ping.Result{Host: h, Sent: 3, Mode: "unprivileged", AvgMs: 4.2, MdevMs: 0.3, TTL: 64}
		if p.live == nil || p.live[NormalizeHost(h)] {
			r.Recv = 3
		} else {
			r.Recv, r.Sent = 0, 3
			r.Err = errors.New("no reply")
		}
		out[h] = r
	}
	return out
}

func (p *fakePinger) probed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]string(nil), p.hosts...)
	sort.Strings(out)
	return out
}

func testCfg() *config.Config {
	c := &config.Config{}
	c.Scan.Seeds = []string{"localnode.local.mesh"}
	c.Scan.MaxHops = -1
	c.Scan.MaxNodes = 0
	c.Scan.WorkerGoroutines = 4
	c.Scan.DetailConcurrency = 2
	c.Scan.SweepTimeout = 30 * time.Second
	c.Scan.NodeTimeout = 2 * time.Second
	c.Scan.RelazyAfter = time.Minute
	c.Scan.DeferDetailBeyondHops = -1
	c.Scan.AbsorbMeshHosts = true
	c.ICMP.Count = 3
	return c
}

func build(t *testing.T, cfg *config.Config, net *fakeNet, p *fakePinger) (*Engine, *store.Store) {
	t.Helper()
	st := store.New()
	st.SetSeeds(cfg.Scan.Seeds)
	eng := New(cfg, st, net, p, zap.NewNop())
	return eng, st
}

func hostsOf(nodes []*model.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Hostname)
	}
	sort.Strings(out)
	return out
}

func contains(hay []string, needle string) bool {
	return slices.Contains(hay, needle)
}

func sweep(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Sweep(context.Background(), KindFull); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

// ---------------------------------------------------------------- tests

func TestSweepExpandsBothNodeSections(t *testing.T) {
	net := newFakeNet()
	net.add("localnode", testNode("localnode", "TST06"), []model.Link{
		testLink("localnode", "tunnelserver", model.LinkTypeWireguard, model.ContainerNeighborhood),
		testLink("localnode", "lanpeer", model.LinkTypeDtD, model.ContainerLocal),
	})
	net.add("tunnelserver", testNode("tunnelserver", "TUNNELSERVER"), nil)
	net.add("lanpeer", testNode("lanpeer", "LANPEER"), nil)

	eng, st := build(t, testCfg(), net, &fakePinger{})
	sweep(t, eng)

	if net.seen(net.selfCalls, "lanpeer") == 0 {
		t.Errorf("Local Nodes peer never crawled; crawled %v", keysOf(net.selfCalls))
	}
	if got := hostsOf(st.Nodes()); !contains(got, "lanpeer") || !contains(got, "tunnelserver") {
		t.Errorf("store nodes = %v, want both peers", got)
	}
	if len(st.Links()) != 2 {
		t.Errorf("links = %d, want 2", len(st.Links()))
	}
}

func TestMaxHopsBoundsExpansion(t *testing.T) {
	net := chain(3)
	cfg := testCfg()
	cfg.Scan.MaxHops = 1
	eng, st := build(t, cfg, net, &fakePinger{})
	sweep(t, eng)

	if net.seen(net.selfCalls, "hop2") != 0 {
		t.Errorf("hop-2 node crawled under max_hops=1; calls=%v", keysOf(net.selfCalls))
	}
	if net.seen(net.selfCalls, "hop1") == 0 {
		t.Errorf("hop-1 node never crawled")
	}
	if n, ok := st.NodeByID("hop2"); !ok || n.Crawled {
		t.Errorf("hop2 = %+v ok=%v, want registered but not crawled", n, ok)
	}
}

func TestUnlimitedHopsReachesWholeChain(t *testing.T) {
	net := chain(4)
	cfg := testCfg()
	cfg.Scan.MaxHops = -1
	eng, st := build(t, cfg, net, &fakePinger{})
	sweep(t, eng)

	for _, h := range []string{"hop1", "hop2", "hop3"} {
		if net.seen(net.selfCalls, h) == 0 {
			t.Errorf("%s never crawled with max_hops=-1", h)
		}
	}
	if got := len(hostsOf(st.Nodes())); got < 4 {
		t.Errorf("nodes = %d, want the whole chain", got)
	}
}

func TestMeshHostsGetProbedAndDetailed(t *testing.T) {
	net := newFakeNet()
	net.add("localnode", testNode("localnode", "SEED"), []model.Link{
		testLink("localnode", "supernode", model.LinkTypeDtD, model.ContainerNeighborhood),
	})
	net.add("supernode", testNode("supernode", "SUPERNODE"), nil)

	// The /a/mesh table lists peers this node routes to but never named as a neighbour.
	md := &aredn.MeshData{Hosts: map[string][]string{}, NodeNames: []string{}}
	for i := range 400 {
		h := fmt.Sprintf("cloud%03d", i)
		md.Hosts[fmt.Sprintf("10.%d.%d.1", i%250, (i/250)%250)] = []string{h}
		md.NodeNames = append(md.NodeNames, h)
	}
	net.mesh["supernode"] = md
	// Two of them answer: the rest stand down as unreachable, which is still an answer.
	net.add("cloud007", testNode("cloud007", "CLOUD007"), nil)

	pg := &fakePinger{live: map[string]bool{"cloud007": true, "localnode": true, "supernode": true}}
	cfg := testCfg()
	cfg.Scan.MaxHops = -1
	cfg.Scan.RelazyAfter = time.Minute
	eng, st := build(t, cfg, net, pg)
	sweep(t, eng)

	probed := map[string]bool{}
	for _, h := range pg.probed() {
		probed[h] = true
	}
	fetched := map[string]bool{}
	net.mu.Lock()
	seq := append([]string(nil), net.selfSeq...)
	net.mu.Unlock()
	for _, h := range seq {
		fetched[h] = true
	}
	for i := range 400 {
		h := fmt.Sprintf("cloud%03d", i)
		if !probed[h] {
			t.Fatalf("%s never pinged: a host-table name stayed unknown forever", h)
		}
		if !fetched[h] {
			t.Fatalf("%s never fetched: a host-table name stayed unscanned forever", h)
		}
	}

	c := st.Counts()
	if c.Unprobed != 0 {
		t.Errorf("Unprobed = %d after a full sweep, want every known node to hold a verdict", c.Unprobed)
	}
	if c.Total <= c.Crawled {
		t.Errorf("Total=%d Crawled=%d, want the unreachable peers registered but not crawled", c.Total, c.Crawled)
	}
	if c.Online == 0 {
		t.Error("Online = 0, want the answering cloud node counted")
	}
	if c.Unprobed != c.Total-c.Online-c.Offline {
		t.Errorf("counts not a partition: %+v", c)
	}
}

func TestMaxNodesCapsMeshAbsorption(t *testing.T) {
	net := newFakeNet()
	net.add("localnode", testNode("localnode", "SEED"), []model.Link{
		testLink("localnode", "supernode", model.LinkTypeDtD, model.ContainerNeighborhood),
	})
	net.add("supernode", testNode("supernode", "SUPERNODE"), nil)
	md := &aredn.MeshData{Hosts: map[string][]string{}, NodeNames: []string{}}
	for i := range 500 {
		h := fmt.Sprintf("cloud%03d", i)
		md.Hosts[fmt.Sprintf("10.9.%d.%d", i/250, i%250)] = []string{h}
		md.NodeNames = append(md.NodeNames, h)
	}
	net.mesh["supernode"] = md

	cfg := testCfg()
	cfg.Scan.MaxNodes = 12
	cfg.Scan.MaxHops = 1
	eng, st := build(t, cfg, net, &fakePinger{})
	sweep(t, eng)

	if got := len(st.Nodes()); got > 30 {
		t.Errorf("store holds %d nodes with max_nodes=12; the /a/mesh absorption ignored the cap", got)
	}
}

func TestSeedAliasCollapsesOntoCanonicalNode(t *testing.T) {
	net := newFakeNet()
	seed := testNode("localnode", "TST06")
	net.canonical["localnode"] = "homeserver"
	net.add("localnode", seed, []model.Link{
		testLink("localnode", "tunnelserver", model.LinkTypeWireguard, model.ContainerNeighborhood),
	})
	// The tunnel server reports the same box by its real name.
	net.add("tunnelserver", testNode("tunnelserver", "TUNNELSERVER"), []model.Link{
		testLink("tunnelserver", "homeserver", model.LinkTypeWireguard, model.ContainerNeighborhood),
	})
	net.canonical["homeserver"] = "homeserver"

	eng, st := build(t, testCfg(), net, &fakePinger{})
	sweep(t, eng)

	if _, ok := st.NodeByID("localnode"); ok {
		t.Errorf("seed alias kept as its own node; the summary double-counts the box")
	}
	n, ok := st.NodeByID("homeserver")
	if !ok {
		t.Fatalf("canonical node missing: %v", hostsOf(st.Nodes()))
	}
	if !contains(n.Aliases, "localnode") {
		t.Errorf("aliases = %v, want the crawled label preserved", n.Aliases)
	}
	// Every edge must hang off the canonical key or the topology loses it.
	for _, l := range st.Links() {
		if l.From == "localnode" || l.To == "localnode" {
			t.Errorf("link still references the alias: %s -> %s", l.From, l.To)
		}
	}
	tp := st.Topology()
	if len(tp.Links) == 0 {
		t.Errorf("topology lost every edge after re-keying")
	}
}

func TestWireguardTunnelAddressNeverBecomesMeshIP(t *testing.T) {
	net := newFakeNet()
	l := testLink("localnode", "tunnelserver", model.LinkTypeWireguard, model.ContainerNeighborhood)
	l.IP = "172.31.159.46"
	l.PeerIP = "172.31.159.46"
	net.add("localnode", testNode("localnode", "SEED"), []model.Link{l})
	net.add("tunnelserver", testNode("tunnelserver", "TUNNELSERVER"), nil)

	eng, st := build(t, testCfg(), net, &fakePinger{})
	sweep(t, eng)

	n, ok := st.NodeByID("tunnelserver")
	if !ok {
		t.Fatalf("peer missing")
	}
	if n.MeshIP == "172.31.159.46" {
		t.Errorf("tunnel endpoint stored as mesh identity")
	}
}

func TestMeshTableKeyBecomesMeshIP(t *testing.T) {
	net := newFakeNet()
	net.add("localnode", testNode("localnode", "SEED"), []model.Link{
		testLink("localnode", "supernode", model.LinkTypeDtD, model.ContainerNeighborhood),
	})
	net.add("supernode", testNode("supernode", "SUPERNODE"), nil)
	net.mesh["localnode"] = &aredn.MeshData{
		Hosts:     map[string][]string{"10.66.66.66": {"supernode", "supernode.supernode.local.mesh"}},
		NodeNames: []string{"supernode"},
		ETX:       map[string]int{"10.66.66.66": 96},
	}

	eng, st := build(t, testCfg(), net, &fakePinger{})
	sweep(t, eng)

	n, ok := st.NodeByID("supernode")
	if !ok {
		t.Fatalf("supernode missing")
	}
	if n.MeshIP != "10.66.66.66" {
		t.Errorf("mesh_ip = %q, want the /a/mesh table key", n.MeshIP)
	}
	if !n.IsSupernode || n.Band != model.BandSupernode {
		t.Errorf("supernode not recognised from the alias form: %+v", n)
	}
}

func TestShortestHopWins(t *testing.T) {
	net := newFakeNet()
	// far is named only by mid, at hop 2.
	net.add("localnode", testNode("localnode", "SEED"), []model.Link{
		testLink("localnode", "mid", model.LinkTypeRF, model.ContainerNeighborhood),
	})
	net.add("mid", testNode("mid", "MID"), []model.Link{
		testLink("mid", "far", model.LinkTypeRF, model.ContainerNeighborhood),
	})
	net.canonical["far"] = "far"
	net.add("far", testNode("far", "FAR"), nil)

	cfg := testCfg()
	cfg.Scan.DeferDetailBeyondHops = -1
	eng, st := build(t, cfg, net, &fakePinger{})
	sweep(t, eng)

	n, ok := st.NodeByID("far")
	if !ok {
		t.Fatalf("far missing")
	}
	if n.Hops != 2 {
		t.Errorf("hops = %d, want 2", n.Hops)
	}
}

func TestLinkBandInheritedFromOwningNode(t *testing.T) {
	net := newFakeNet()
	own := testNode("localnode", "SEED")
	own.Band = model.Band5GHz
	own.BandLabel = model.BandLabels[own.Band]
	net.add("localnode", own, []model.Link{
		testLink("localnode", "rfpeer", model.LinkTypeRF, model.ContainerNeighborhood),
	})
	net.add("rfpeer", testNode("rfpeer", "RFPEER"), nil)

	eng, st := build(t, testCfg(), net, &fakePinger{})
	sweep(t, eng)

	for _, l := range st.Links() {
		if l.From != "localnode" {
			continue
		}
		if l.Band != model.Band5GHz {
			t.Errorf("link band = %q, want the owning node's %q", l.Band, model.Band5GHz)
		}
	}
}

func TestDeferDetailThresholdReachesFetcher(t *testing.T) {
	net := chain(3)
	cfg := testCfg()
	cfg.Scan.DeferDetailBeyondHops = 1
	cfg.Scan.MaxHops = 2
	cfg.Scan.AbsorbMeshHosts = false
	eng, _ := build(t, cfg, net, &fakePinger{})
	sweep(t, eng)

	if v, ok := net.neighDetail["hop0"]; !ok || v != true {
		t.Errorf("hop0 detail=%v ok=%v, want true", v, ok)
	}
	if v, ok := net.neighDetail["hop1"]; !ok || v != true {
		t.Errorf("hop1 detail=%v, want true (threshold is inclusive)", v)
	}
	if v, ok := net.neighDetail["hop2"]; !ok || v != false {
		t.Errorf("hop2 detail=%v, want false (beyond defer threshold)", v)
	}
}

func TestConcurrentSweepRejected(t *testing.T) {
	net := chain(3)
	hold := make(chan struct{})
	net.hold = hold
	eng, _ := build(t, testCfg(), net, &fakePinger{})

	first := make(chan error, 1)
	go func() { first <- eng.Sweep(context.Background(), KindFull) }()

	// Sweep publishes running=true and the status id under one lock before the first fetch.
	deadline := time.After(2 * time.Second)
	for eng.Status().ID == 0 {
		select {
		case <-deadline:
			t.Fatal("first sweep never started")
		case <-time.After(time.Millisecond):
		}
	}

	if err := eng.Sweep(context.Background(), KindFull); !errors.Is(err, ErrAlreadyRunning) {
		close(hold)
		t.Fatalf("second concurrent sweep = %v, want ErrAlreadyRunning", err)
	}
	close(hold)
	if err := <-first; err != nil {
		t.Errorf("first sweep: %v", err)
	}
	if err := eng.Sweep(context.Background(), KindIncremental); err != nil {
		t.Errorf("sweep after the first ended: %v", err)
	}
}

func TestCancelStopsExpansion(t *testing.T) {
	net := chain(6)
	hold := make(chan struct{})
	net.hold = hold
	cfg := testCfg()
	eng, st := build(t, cfg, net, &fakePinger{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Sweep(ctx, KindFull) }()

	deadline := time.After(2 * time.Second)
	for eng.Status().ID == 0 {
		select {
		case <-deadline:
			t.Fatal("sweep never started")
		case <-time.After(time.Millisecond):
		}
	}
	before := net.count(net.selfCalls)
	cancel()
	close(hold)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("sweep returned %v, want a cancellation error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not end after cancellation")
	}
	if got := net.count(net.selfCalls); got > before+1 {
		t.Errorf("expansion continued after cancel: %d hosts fetched, was %d", got, before)
	}
	if st.Counts().Total == 0 {
		t.Errorf("nothing recorded before cancel")
	}
	if s := eng.Status(); s.Phase != model.PhaseStopped {
		t.Errorf("phase = %q, want %q", s.Phase, model.PhaseStopped)
	}
}

func TestUnreachableNodeStaysVisible(t *testing.T) {
	net := newFakeNet()
	net.add("localnode", testNode("localnode", "SEED"), []model.Link{
		testLink("localnode", "ghost", model.LinkTypeRF, model.ContainerNeighborhood),
	})
	// "ghost" answers nothing: not in self, not in neigh.

	pg := &fakePinger{live: map[string]bool{"localnode": true}}
	eng, st := build(t, testCfg(), net, pg)
	sweep(t, eng)

	if net.seen(net.neighCalls, "ghost") != 0 {
		t.Errorf("a node that cannot answer /a/status must not also be retried for neighbours")
	}
	if _, ok := st.NodeByID("ghost"); !ok {
		t.Errorf("ghost dropped from the store; the map should still show it went missing")
	}
	c := st.Counts()
	if c.Total != 2 {
		t.Errorf("counts = %+v, want seed + ghost", c)
	}
}

func TestProbeFailureRecordsOfflineNotUnprobed(t *testing.T) {
	net := newFakeNet()
	net.add("localnode", testNode("localnode", "SEED"), nil)
	pg := &fakePinger{live: map[string]bool{"localnode": false}}
	eng, st := build(t, testCfg(), net, pg)
	sweep(t, eng)

	c := st.Counts()
	if c.Offline != 1 || c.Unprobed != 0 {
		t.Errorf("counts = %+v, want a probed-negative node counted offline, not unprobed", c)
	}
}

// ---------------------------------------------------------------- helpers

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v > 0 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// chain builds hop0 -> hop1 -> ... -> hop(n-1), seed addressed as localnode.
func chain(n int) *fakeNet {
	net := newFakeNet()
	net.canonical["localnode"] = "hop0"
	for i := range n {
		host := fmt.Sprintf("hop%d", i)
		var links []model.Link
		if i+1 < n {
			links = append(links, testLink(host, fmt.Sprintf("hop%d", i+1), model.LinkTypeRF, model.ContainerNeighborhood))
		}
		net.add(host, testNode(host, "HOP"+fmt.Sprint(i)), links)
	}
	return net
}

// A status-only change still has to bump the store revision the WebSocket push loop watches.
func TestStatusPatchAdvancesStoreRevision(t *testing.T) {
	eng, st := build(t, testCfg(), newFakeNet(), &fakePinger{})
	before := st.Rev()
	eng.patch(func(s *model.ScanStatus) { s.Message = "phase moved" })
	if st.Rev() == before {
		t.Errorf("store revision stayed at %d after a status-only change", before)
	}
}

// Hop 0 is the crawl root alone; a later two-hop stub must not create a second root.
func TestOnlyTheSeedKeepsZeroHops(t *testing.T) {
	net := newFakeNet()
	net.add("localnode", testNode("localnode", "TST06"), []model.Link{
		testLink("localnode", "midhop", model.LinkTypeRF, model.ContainerNeighborhood),
	})
	net.add("midhop", testNode("midhop", "MIDHOP"), []model.Link{
		testLink("midhop", "far", model.LinkTypeRF, model.ContainerNeighborhood),
	})
	net.add("far", testNode("far", "FAR"), nil)

	eng, st := build(t, testCfg(), net, &fakePinger{})
	sweep(t, eng)

	got := map[string]int{}
	var roots []string
	for _, n := range st.Nodes() {
		got[n.Hostname] = n.Hops
		if n.Hops == 0 {
			roots = append(roots, n.Hostname)
		}
	}
	if len(roots) != 1 || roots[0] != "localnode" {
		t.Errorf("hop-0 nodes = %v (hops %v), want only the seed localnode", roots, got)
	}
	if got["midhop"] != 1 || got["far"] != 2 {
		t.Errorf("hops = %v, want midhop 1 and far 2", got)
	}
}

// The lane end to end: park a silent peer, hold it back, look at it again once it is due.
func TestFailedPeerIsParkedThenRevisited(t *testing.T) {
	net := newFakeNet()
	net.add("localnode", testNode("localnode", "SEED"), []model.Link{
		testLink("localnode", "ghost", model.LinkTypeRF, model.ContainerNeighborhood),
	})
	cfg := testCfg()
	cfg.Scan.RevisitAfter = time.Hour
	eng, st := build(t, cfg, net, &fakePinger{live: map[string]bool{"localnode": true}})
	sweep(t, eng)

	ghost, ok := st.NodeByID("ghost")
	if !ok {
		t.Fatal("the unreachable peer vanished instead of staying visible")
	}
	if ghost.RevisitAt.IsZero() || ghost.FailCount == 0 || ghost.LastError == "" {
		t.Fatalf("ghost not parked after a failed fetch: %+v", ghost)
	}
	first := net.seen(net.selfCalls, "ghost")
	if first == 0 {
		t.Fatal("ghost was never attempted, so the park proves nothing")
	}

	sweep(t, eng)
	if got := net.seen(net.selfCalls, "ghost"); got != first {
		t.Errorf("a parked peer was fetched %d more times in the next sweep, want 0", got-first)
	}
	if n, _ := st.NodeByID("ghost"); n.FailCount != ghost.FailCount {
		t.Errorf("FailCount moved %d -> %d while it was being held back", ghost.FailCount, n.FailCount)
	}

	// Overdue: the frontier is allowed to reach it again.
	st.MarkFailure("ghost", "", time.Now().Add(-time.Second))
	sweep(t, eng)
	if got := net.seen(net.selfCalls, "ghost"); got <= first {
		t.Errorf("an overdue parked peer was never looked at again (%d calls)", got)
	}
}

// With the lane off the reason is still recorded and nothing is held back.
func TestRevisitLaneOffStillRecordsAndRetries(t *testing.T) {
	net := newFakeNet()
	net.add("localnode", testNode("localnode", "SEED"), []model.Link{
		testLink("localnode", "ghost", model.LinkTypeRF, model.ContainerNeighborhood),
	})
	cfg := testCfg()
	cfg.Scan.RevisitAfter = 0
	eng, st := build(t, cfg, net, &fakePinger{live: map[string]bool{"localnode": true}})
	sweep(t, eng)
	n, _ := st.NodeByID("ghost")
	if !n.RevisitAt.IsZero() {
		t.Errorf("RevisitAt = %v with the lane switched off", n.RevisitAt)
	}
	if n.LastError == "" {
		t.Error("switching the lane off lost the failure reason")
	}
	before := net.seen(net.selfCalls, "ghost")
	sweep(t, eng)
	if got := net.seen(net.selfCalls, "ghost"); got <= before {
		t.Errorf("with the lane off the peer should still be retried every sweep (%d calls)", got)
	}
}

// The status payload must report work in flight against the configured ceilings, not just totals.
func TestStatusReportsWorkInFlight(t *testing.T) {
	net := newFakeNet()
	net.add("localnode", testNode("localnode", "SEED"), []model.Link{
		testLink("localnode", "a", model.LinkTypeRF, model.ContainerNeighborhood),
		testLink("localnode", "b", model.LinkTypeRF, model.ContainerNeighborhood),
		testLink("localnode", "c", model.LinkTypeRF, model.ContainerNeighborhood),
	})
	net.add("a", testNode("a", "A"), nil)
	net.add("b", testNode("b", "B"), nil)
	net.add("c", testNode("c", "C"), nil)

	cfg := testCfg()
	cfg.Scan.WorkerGoroutines = 3
	cfg.Scan.RunOnStartup = false
	eng, _ := build(t, cfg, net, &fakePinger{})

	// Hold every status page: workers stay inside FetchSelf, where the count must be non-zero.
	hold := make(chan struct{})
	net.mu.Lock()
	net.hold = hold
	net.mu.Unlock()

	done := make(chan struct{})
	go func() {
		_ = eng.Sweep(context.Background(), KindFull)
		close(done)
	}()

	busy, slots := 0, 0
	for range 200 {
		s := eng.Status()
		busy, slots = s.WorkersBusy, s.WorkerSlots
		if busy > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if busy == 0 {
		t.Error("WorkersBusy stayed 0 while every fetch was blocked; the page could never show load")
	}
	if slots != 3 {
		t.Errorf("WorkerSlots = %d, want the configured 3", slots)
	}
	if s := eng.Status(); s.ProbeSlots != 16 {
		t.Errorf("ProbeSlots = %d, want the pinger's own ceiling", s.ProbeSlots)
	}

	close(hold)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the sweep never finished once the hold was released")
	}
	if after := eng.Status().WorkersBusy; after != 0 {
		t.Errorf("WorkersBusy = %d after the sweep, want nothing in flight", after)
	}
}
