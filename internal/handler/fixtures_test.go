package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	gojson "go.gh.ink/json"
	"go.uber.org/zap"

	"aredn-map/internal/controller"
	"aredn-map/internal/infra/config"
	"aredn-map/internal/model"
	"aredn-map/internal/repository/aredn"
	"aredn-map/internal/repository/ping"
	"aredn-map/internal/repository/store"
	"aredn-map/internal/service/scanner"
)

// Offline harness: a real app, store and engine over scripted fakes; no mesh, socket or clock.

// Pointer helpers for the nullable radio metrics.

//go:fix inline
func snr(v int) *int { return new(v) }

//go:fix inline
func fl(v float64) *float64 { return new(v) }

// Scripted mesh: a fake scanner.Fetcher + scanner.Pinger.

// scriptHost is one node as the scripted mesh answers it.
type scriptHost struct {
	self    *model.Node
	links   []model.Link
	selfErr error
	// mesh is what /a/mesh answers for this host, when the sweep asks.
	mesh *aredn.MeshData
}

// fakeFetcher answers scanner.Fetcher from a static script, recorded under a mutex.
type fakeFetcher struct {
	mu      sync.Mutex
	hosts   map[string]scriptHost
	calls   map[string]int
	gate    chan struct{} // when set, every FetchSelf blocks until it is closed
	gateOne sync.Once
}

func newFakeFetcher(hosts map[string]scriptHost) *fakeFetcher {
	return &fakeFetcher{hosts: hosts, calls: map[string]int{}}
}

func (f *fakeFetcher) note(kind, host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[kind+"\x00"+host]++
}

// count reports how often the engine asked for kind/host, so tests wait instead of sleeping.
func (f *fakeFetcher) count(kind, host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[kind+"\x00"+host]
}

func (f *fakeFetcher) blocked() {
	f.mu.Lock()
	g := f.gate
	f.mu.Unlock()
	if g == nil {
		return
	}
	<-g
}

func (f *fakeFetcher) release() {
	f.mu.Lock()
	g := f.gate
	f.mu.Unlock()
	if g != nil {
		f.gateOne.Do(func() { close(g) })
	}
}

func (f *fakeFetcher) FetchSelf(_ context.Context, hostname string) (*model.Node, error) {
	f.note("self", hostname)
	f.blocked()
	h, ok := f.hosts[scanner.NormalizeHost(hostname)]
	if !ok {
		return nil, fmt.Errorf("aredn: no route to %s", hostname)
	}
	if h.selfErr != nil {
		return nil, h.selfErr
	}
	if h.self == nil {
		return nil, nil
	}
	return cloneScriptNode(h.self), nil
}

// cloneScriptNode hands the engine its own copy, since it appends aliases and flips flags.
func cloneScriptNode(n *model.Node) *model.Node {
	cp := *n
	cp.Aliases = slices.Clone(n.Aliases)
	cp.Services = slices.Clone(n.Services)
	cp.LocalServices = slices.Clone(n.LocalServices)
	cp.LocalDevices = slices.Clone(n.LocalDevices)
	cp.Neighbors = slices.Clone(n.Neighbors)
	cp.LinkTypes = maps.Clone(n.LinkTypes)
	cp.RFTypeCounts = maps.Clone(n.RFTypeCounts)
	return &cp
}

func (f *fakeFetcher) FetchNeighbors(_ context.Context, hostname string, _ bool, _ int) ([]model.Link, error) {
	f.note("neighbors", hostname)
	h, ok := f.hosts[scanner.NormalizeHost(hostname)]
	if !ok {
		return nil, fmt.Errorf("aredn: no route to %s", hostname)
	}
	out := make([]model.Link, len(h.links))
	copy(out, h.links)
	return out, nil
}

func (f *fakeFetcher) FetchMesh(_ context.Context, hostname string) (*aredn.MeshData, error) {
	f.note("mesh", hostname)
	h, ok := f.hosts[scanner.NormalizeHost(hostname)]
	if !ok || h.mesh == nil {
		return nil, errors.New("aredn: /a/mesh unavailable")
	}
	return h.mesh, nil
}

// fakePinger answers scanner.Pinger from a static verdict table.
type fakePinger struct {
	mu      sync.Mutex
	results map[string]ping.Result
	mode    string
	probed  []string
}

// Active/Slots answer the status counters; a fake holds no sessions.
func (p *fakePinger) Active() int { return 0 }
func (p *fakePinger) Slots() int  { return 16 }

func (p *fakePinger) PingMany(_ context.Context, hosts []string) map[string]ping.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probed = append(p.probed, hosts...)
	out := make(map[string]ping.Result, len(hosts))
	for _, h := range hosts {
		if r, ok := p.results[h]; ok {
			out[h] = r
			continue
		}
		out[h] = ping.Result{Host: h, Mode: p.mode}
	}
	return out
}

// Ping answers the per-host call from the same table as the batch path.
func (p *fakePinger) Ping(_ context.Context, host string) ping.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probed = append(p.probed, host)
	if r, ok := p.results[host]; ok {
		return r
	}
	return ping.Result{Host: host, Mode: p.mode}
}

func (p *fakePinger) Mode() string { return p.mode }

func (p *fakePinger) probedHosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.probed...)
}

var (
	_ scanner.Fetcher = (*fakeFetcher)(nil)
	_ scanner.Pinger  = (*fakePinger)(nil)
)

// Config / app construction.

const (
	fixedVersion = "0.1.0"
	// fixedNextSweep is a constant slot so no test reads the wall clock.
	fixedNextSweep = "2026-01-01T08:00:00Z"
)

func fixedNextSweepTime() time.Time {
	t, err := time.Parse(time.RFC3339, fixedNextSweep)
	if err != nil {
		panic(err)
	}
	return t
}

// testConfig is a complete, validated configuration that never touches the network.
// No static dir skips compress and the SPA mount; the sweep is bounded for the fakes.
func testConfig() *config.Config {
	return &config.Config{
		Path: "",
		Server: config.Server{
			Host:         "127.0.0.1",
			Port:         8899,
			StaticDir:    "",
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
		},
		Scan: config.Scan{
			Seeds:                 []string{"localnode.local.mesh"},
			Cron:                  "0 */30 * * * *",
			CronZone:              "Asia/Shanghai",
			RunOnStartup:          false,
			MaxHops:               1,
			MaxNodes:              0,
			WorkerGoroutines:      4,
			DetailConcurrency:     2,
			NodeTimeout:           2 * time.Second,
			SweepTimeout:          20 * time.Second,
			DeferDetailBeyondHops: -1,
			AbsorbMeshHosts:       false,
			RelazyAfter:           time.Hour,
		},
		HTTP: config.HTTP{Timeout: 2 * time.Second, UserAgent: "aredn-map-test"},
		ICMP: config.ICMP{
			Mode:           "unprivileged",
			Count:          3,
			Interval:       time.Millisecond,
			Timeout:        time.Second,
			OverallTimeout: time.Second,
			Concurrency:    4,
		},
		Map: config.Map{
			AmapKey:          "TEST-AMAP-KEY",
			AmapSecurityCode: "TEST-AMAP-SCODE",
			CenterLat:        31.2304,
			CenterLon:        121.4737,
			Zoom:             8,
		},
	}
}

// harness is the app under test plus everything it reads.
type harness struct {
	t     testing.TB
	app   *fiber.App
	st    *store.Store
	cfg   *config.Config
	eng   *scanner.Engine
	ping  *fakePinger
	fetch *fakeFetcher
}

// newHarness builds the app exactly the way main.go does, but with fakes.
func newHarness(t testing.TB, cfg *config.Config, st *store.Store, fetch *fakeFetcher, pg *fakePinger) *harness {
	t.Helper()
	if cfg == nil {
		cfg = testConfig()
	}
	if st == nil {
		st = store.New()
		st.SetSeeds(cfg.Scan.Seeds)
	}
	if fetch == nil {
		fetch = newFakeFetcher(map[string]scriptHost{})
	}
	if pg == nil {
		pg = &fakePinger{mode: cfg.ICMP.Mode, results: map[string]ping.Result{}}
	}
	eng := scanner.New(cfg, st, fetch, pg, zap.NewNop())
	pushCtx, stopPush := context.WithCancel(context.Background())
	app := New(controller.Deps{
		Ctx:     pushCtx,
		Cfg:     cfg,
		Store:   st,
		Engine:  eng,
		Log:     zap.NewNop(),
		Started: time.Now(),
		NextRun: func() time.Time { return fixedNextSweepTime() },
	})
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config is not valid: %v", err)
	}
	_ = app
	t.Cleanup(func() {
		// Stops the push loop first: it holds the context and would outlive the test.
		stopPush()
		// app.Test opens no socket, but Shutdown drains its worker pool.
		_ = app.Shutdown()
	})
	return &harness{t: t, app: app, st: st, cfg: cfg, eng: eng, ping: pg, fetch: fetch}
}

// A sweep running in the background, so a test can call the API while the fetch is blocked.

type runningSweep struct {
	err      chan error
	finished chan struct{}
}

// startSweep runs a sweep in a goroutine and guarantees the goroutine ends with the test.
func (h *harness) startSweep(kind string, fetch *fakeFetcher) *runningSweep {
	rs := &runningSweep{err: make(chan error, 1), finished: make(chan struct{})}
	go func() {
		err := h.eng.Sweep(context.Background(), kind)
		rs.err <- err
		close(rs.finished)
	}()
	h.t.Cleanup(func() {
		fetch.release()
		select {
		case <-rs.finished:
		case <-time.After(15 * time.Second):
			h.t.Errorf("the scripted %s sweep never finished", kind)
		}
	})
	return rs
}

// wait blocks until the sweep returned and hands back its error.
func (rs *runningSweep) wait(t testing.TB) error {
	t.Helper()
	select {
	case err := <-rs.err:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the scripted sweep never returned")
		return nil
	}
}

// Request helpers.

var testTimeout = fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true}

func (h *harness) do(method, path string, body io.Reader, headers map[string]string) (int, http.Header, []byte) {
	h.t.Helper()
	req := httptest.NewRequest(method, path, body)
	if headers != nil {
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	resp, err := h.app.Test(req, testTimeout)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header, raw
}

func (h *harness) get(path string) (int, http.Header, []byte) {
	return h.do(http.MethodGet, path, nil, nil)
}

// getWithOrigin is the request the Nuxt dev server actually makes.
func (h *harness) getWithOrigin(path, origin string) (int, http.Header, []byte) {
	return h.do(http.MethodGet, path, nil, map[string]string{"Origin": origin})
}

func (h *harness) post(path, body string) (int, http.Header, []byte) {
	return h.do(http.MethodPost, path, strings.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
}

// jsonBody asserts JSON and returns raw members, so "absent" differs from "null".
func (h *harness) jsonBody(status int, header http.Header, body []byte) map[string]json.RawMessage {
	h.t.Helper()
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		h.t.Fatalf("content type = %q, want application/json (body %s)", ct, body)
	}
	return rawObject(h.t, body)
}

// getObj performs a GET and decodes the JSON object it returns.
func (h *harness) getObj(path string) map[string]json.RawMessage {
	h.t.Helper()
	status, header, body := h.get(path)
	if status != http.StatusOK {
		h.t.Fatalf("GET %s -> %d: %s", path, status, body)
	}
	return h.jsonBody(status, header, body)
}

// getObjWithOrigin is getObj for a cross-origin caller.
func (h *harness) getObjWithOrigin(path string) map[string]json.RawMessage {
	h.t.Helper()
	status, header, body := h.getWithOrigin(path, "http://localhost:3000")
	if status != http.StatusOK {
		h.t.Fatalf("GET %s -> %d: %s", path, status, body)
	}
	return h.jsonBody(status, header, body)
}

// The mesh picture every contract test reads.

// Timestamps are fixed instants so nothing in the suite drifts with the wall clock.
var (
	fixtureDiscovered = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	fixtureProbed     = time.Date(2026, 3, 1, 10, 5, 0, 0, time.UTC)
	fixtureSeen       = time.Date(2026, 3, 1, 10, 6, 0, 0, time.UTC)
)

// SHANGHAI and PHOENIX sit one inside the GCJ-02 offset zone and one outside it.
const (
	shanghaiLat, shanghaiLon = 31.2304, 121.4737
	phoenixLat, phoenixLon   = 33.4484, -112.0740
	wgTunnelIP               = "172.31.159.46"
)

// seedContractStore fills the smallest mesh that exercises every clause of the wire contract.
func seedContractStore(st *store.Store) {
	st.SetSeeds([]string{"localnode.local.mesh"})

	upsert := func(n *model.Node) { st.UpsertNode(n) }

	// 1: crawled, online Chinese node whose band label is a deliberate lie on the way in.
	upsert(&model.Node{
		Name:          "TST01 HomeServer",
		Hostname:      "localnode",
		URL:           "http://localnode.local.mesh/",
		MeshIP:        "10.255.0.1",
		Aliases:       []string{"grafana|10.255.0.42"},
		Services:      []string{"ssh|10.255.0.42", "dns|10.255.0.1"},
		Description:   "Rooftop node inside the offset zone",
		Hardware:      "XR5",
		Firmware:      "4.1.0",
		Band:          model.Band5GHz,
		BandLabel:     "No RF",
		Channel:       149,
		FreqLow:       5745,
		ChanBW:        10,
		Lat:           shanghaiLat,
		Lon:           shanghaiLon,
		HasLocation:   true,
		Hops:          0,
		ETX:           0,
		Neighbors:     []string{"zero-snr-rf", "wg-remote", "supernode-hq"},
		LinkTypes:     map[string]int{"RF": 2, "Wireguard": 1},
		Online:        true,
		LatencyMs:     4.5,
		PacketsSent:   3,
		PacketsRecv:   3,
		TTL:           63,
		ProbedAt:      fixtureProbed,
		DiscoveredAt:  fixtureDiscovered,
		UpdatedAt:     fixtureProbed,
		DetailFetched: true,
		Crawled:       true,
		Source:        "status-page",
	})

	// 2: an RF neighbour outside China with a real 0 dB reading.
	upsert(&model.Node{
		Name:          "Zero SNR RF",
		Hostname:      "zero-snr-rf",
		URL:           "http://zero-snr-rf.local.mesh/",
		MeshIP:        "10.255.0.2",
		Hardware:      "Rocket M",
		Band:          model.Band2_4GHz,
		Channel:       1,
		FreqLow:       2412,
		Lat:           phoenixLat,
		Lon:           phoenixLon,
		HasLocation:   true,
		Hops:          1,
		Aliases:       []string{"zerorfrouter|10.255.0.42", "rf-hub"},
		Neighbors:     []string{"dtd-hub", "far-cn-peer"},
		Online:        true,
		LatencyMs:     9.25,
		PacketsSent:   3,
		PacketsRecv:   3,
		ProbedAt:      fixtureProbed,
		DiscoveredAt:  fixtureDiscovered,
		UpdatedAt:     fixtureProbed,
		DetailFetched: true,
		Crawled:       true,
		Source:        "status-page",
	})

	// 3: WireGuard peer from the link dialog only: no probe, no coordinates, no band.
	upsert(&model.Node{
		Name:         "wg-remote",
		Hostname:     "wg-remote",
		URL:          "http://wg-remote.local.mesh/",
		Band:         model.BandWireguard,
		Source:       "link-dialog",
		Hops:         1,
		DiscoveredAt: fixtureDiscovered,
		UpdatedAt:    fixtureDiscovered,
	})

	// 4: an unrecognised incoming band key must arrive as "u" / "Unknown".
	upsert(&model.Node{
		Name:        "Odd Radio",
		Hostname:    "odd-radio",
		MeshIP:      "10.255.0.4",
		Band:        "x",
		BandLabel:   "No RF",
		Lat:         shanghaiLat + 0.05,
		Lon:         shanghaiLon + 0.05,
		HasLocation: true,
		Hops:        2,
		// Read page: the unidentifiable band came from its own page, not from never being read.
		Crawled:      true,
		ProbedAt:     fixtureProbed,
		Online:       false,
		ProbeError:   "ping: no reply",
		DiscoveredAt: fixtureDiscovered,
		UpdatedAt:    fixtureProbed,
	})

	// 5: a supernode.
	upsert(&model.Node{
		Name:         "Supernode HQ",
		Hostname:     "supernode-hq",
		MeshIP:       "10.255.0.5",
		IsSupernode:  true,
		Band:         model.BandSupernode,
		ProbedAt:     fixtureProbed,
		Online:       true,
		LatencyMs:    2.0,
		DiscoveredAt: fixtureDiscovered,
		UpdatedAt:    fixtureProbed,
		Crawled:      true,
		Neighbors:    []string{"localnode"},
	})

	// 6: a DtD hub, probed offline.
	upsert(&model.Node{
		Name:         "DtD Hub",
		Hostname:     "dtd-hub",
		MeshIP:       "10.255.0.6",
		Band:         model.BandNoRF,
		ProbedAt:     fixtureProbed,
		Online:       false,
		ProbeError:   "ping: no reply: 3 sent",
		DiscoveredAt: fixtureDiscovered,
		UpdatedAt:    fixtureProbed,
		Crawled:      true,
	})

	// 7 & 8: nodes with no ICMP verdict at all.
	upsert(&model.Node{
		Name:         "Never Probed A",
		Hostname:     "never-probed-a",
		Band:         model.Band900MHz,
		Source:       "mesh-list",
		DiscoveredAt: fixtureDiscovered,
		UpdatedAt:    fixtureDiscovered,
	})
	upsert(&model.Node{
		Name:         "Never Probed B",
		Hostname:     "never-probed-b",
		MeshIP:       "10.255.0.8",
		Band:         model.BandHaLow,
		Lat:          shanghaiLat - 0.2,
		Lon:          shanghaiLon + 0.2,
		HasLocation:  true,
		Source:       "mesh-list",
		DiscoveredAt: fixtureDiscovered,
		UpdatedAt:    fixtureDiscovered,
	})

	// 9: neighbour stub with a Chinese position from its dialog; GCJ-02 must still be computed.
	upsert(&model.Node{
		Name:         "Far CN Peer",
		Hostname:     "far-cn-peer",
		URL:          "http://far-cn-peer.local.mesh/",
		Band:         model.BandNoRF,
		Lat:          24.3741,
		Lon:          103.6551,
		HasLocation:  true,
		Hops:         2,
		Source:       "link-dialog",
		DiscoveredAt: fixtureDiscovered,
		UpdatedAt:    fixtureDiscovered,
	})

	st.SetSeeds([]string{"localnode.local.mesh", "supernode-hq.local.mesh"})

	// Links: dialog-always metrics on every edge, radio-only pointers only on RF edges.
	st.UpsertLink(model.Link{
		From: "localnode", To: "zero-snr-rf", Container: model.ContainerNeighborhood,
		MAC: "02:00:00:00:00:01", IP: "10.255.0.2", Type: model.LinkTypeRF,
		Band: model.Band2_4GHz, Status: "active", DistanceMI: 4.2,
		RxSuccessPct: 100, TxSuccessPct: 100, ErrorsPct: 0,
		RTTMs: 5.5, PingMs: 5.5, NeighborPingMs: 6.1, Metric: 2,
		LocalSNR: new(0), NeighborSNR: new(0), ETX: fl(1),
		AvgTxPps: fl(0), BitrateRxMbps: fl(0), BitrateTxMbps: fl(0), RetransmitPct: fl(0),
		PeerName: "Zero SNR RF", PeerHardware: "Rocket M", PeerIP: "10.255.0.2",
		LastSeen: fixtureSeen, ScannedAt: fixtureSeen,
	})
	st.UpsertLink(model.Link{
		From: "localnode", To: "supernode-hq", Container: model.ContainerNeighborhood,
		MAC: "02:00:00:00:00:02", IP: "10.255.0.5", Type: model.LinkTypeRF,
		Band: model.Band5GHz, Status: "active",
		RxSuccessPct: 98.5, TxSuccessPct: 99.5, ErrorsPct: 1.5,
		RTTMs: 3.25, PingMs: 3.25, NeighborPingMs: 3.4, Metric: 4,
		LocalSNR: new(12), NeighborSNR: new(-8), ETX: new(2.5),
		AvgTxPps: new(120.5), BitrateRxMbps: new(116.7), BitrateTxMbps: fl(200),
		RetransmitPct: new(3.5),
		PeerName:      "Supernode HQ", PeerIP: "10.255.0.5",
		LastSeen: fixtureSeen, ScannedAt: fixtureSeen,
	})
	st.UpsertLink(model.Link{
		From: "localnode", To: "wg-remote", Container: model.ContainerLocal,
		Type: model.LinkTypeWireguard, Band: model.BandWireguard, Status: "up",
		// A tunnel link reports nothing radio-ish: every pointer stays nil.
		LocalSNR: nil, NeighborSNR: nil, ETX: nil, AvgTxPps: nil,
		BitrateRxMbps: nil, BitrateTxMbps: nil, RetransmitPct: nil,
		// The dialog's "ip address" is the tunnel endpoint, not a mesh identity.
		IP: wgTunnelIP, PeerIP: wgTunnelIP,
		PeerName: "wg-remote", PeerHardware: "x86_64",
		LastSeen: fixtureSeen, ScannedAt: fixtureSeen,
	})
	st.UpsertLink(model.Link{
		From: "zero-snr-rf", To: "dtd-hub", Container: model.ContainerNeighborhood,
		Type: model.LinkTypeDtD, Band: model.BandNoRF, Status: "active",
		// A DtD edge has no SNR and, when the hub is down, 0% success; both must reach the browser.
		RxSuccessPct: 0, TxSuccessPct: 0, ErrorsPct: 0, Metric: 0,
		LocalSNR: nil, NeighborSNR: nil, ETX: nil,
		PeerName: "DtD Hub", IP: "10.255.99.9",
		LastSeen: fixtureSeen, ScannedAt: fixtureSeen,
	})
	// The edge that introduced the stub "far-cn-peer", whose addresses are not mesh IPs.
	st.UpsertLink(model.Link{
		From: "zero-snr-rf", To: "far-cn-peer", Container: model.ContainerNeighborhood,
		Type: model.LinkTypeDtD, Band: model.BandNoRF, Status: "active",
		RxSuccessPct: 100, TxSuccessPct: 100, Metric: 3,
		PeerName: "Far CN Peer", PeerIP: "10.255.99.10",
		PeerLat: 24.3741, PeerLon: 103.6551,
		LastSeen: fixtureSeen, ScannedAt: fixtureSeen,
	})
	// A dangling edge whose far endpoint was dropped must not reach the topology payload.
	st.UpsertLink(model.Link{
		From: "gone-node", To: "localnode", Type: model.LinkTypeRF, Band: model.Band5GHz,
		LocalSNR: new(20), ETX: new(1.2),
		LastSeen: fixtureSeen, ScannedAt: fixtureSeen,
	})
}

// JSON assertions.

func rawObject(t testing.TB, body []byte) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("decode json object: %v\n%s", err, body)
	}
	return obj
}

func rawArray(t testing.TB, body []byte) []json.RawMessage {
	t.Helper()
	var arr []json.RawMessage
	if err := json.Unmarshal(body, &arr); err != nil {
		t.Fatalf("decode json array: %v\n%s", err, body)
	}
	return arr
}

// requireKey fails when the member is missing entirely.
func requireKey(t testing.TB, obj map[string]json.RawMessage, key, where string) json.RawMessage {
	t.Helper()
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("%s: key %q missing; have %s", where, key, memberNames(obj))
	}
	return raw
}

// requireKeys asserts every listed member is present.
func requireKeys(t testing.TB, obj map[string]json.RawMessage, where string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		requireKey(t, obj, k, where)
	}
}

// forbidKeys asserts none of the listed members are present.
func forbidKeys(t testing.TB, obj map[string]json.RawMessage, where string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if raw, ok := obj[k]; ok {
			t.Errorf("%s: key %q must not be present, found %s", where, k, raw)
		}
	}
}

func memberNames(obj map[string]json.RawMessage) string {
	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	return "[" + strings.Join(names, " ") + "]"
}

// requireLiteral pins a member to an exact JSON token: "null", "0" and absent are three facts.
func requireLiteral(t testing.TB, obj map[string]json.RawMessage, key, where, want string) {
	t.Helper()
	raw := requireKey(t, obj, key, where)
	if got := string(raw); got != want {
		t.Errorf("%s: %s = %s, want %s", where, key, got, want)
	}
}

// decodeInto runs the body through the configured decoder of the app.
func decodeInto(t testing.TB, body []byte, dst any) {
	t.Helper()
	if err := gojson.Unmarshal(body, dst); err != nil {
		t.Fatalf("go.gh.ink/json decode: %v\n%s", err, body)
	}
}

// nodeArray pulls "nodes" out of a /api/nodes or /api/topology body.
func nodeArray(t testing.TB, obj map[string]json.RawMessage, where string) []map[string]json.RawMessage {
	t.Helper()
	arr := rawArray(t, requireKey(t, obj, "nodes", where))
	out := make([]map[string]json.RawMessage, len(arr))
	for i, raw := range arr {
		out[i] = rawObject(t, raw)
	}
	return out
}

// linkArray pulls "links" out of a /api/links or /api/topology body.
func linkArray(t testing.TB, obj map[string]json.RawMessage, where string) []map[string]json.RawMessage {
	t.Helper()
	arr := rawArray(t, requireKey(t, obj, "links", where))
	out := make([]map[string]json.RawMessage, len(arr))
	for i, raw := range arr {
		out[i] = rawObject(t, raw)
	}
	return out
}

// indexByID keys an array of objects by one of their string members.
func indexByID(t testing.TB, objs []map[string]json.RawMessage, key, where string) map[string]map[string]json.RawMessage {
	t.Helper()
	out := make(map[string]map[string]json.RawMessage, len(objs))
	for _, o := range objs {
		var id string
		if err := gojson.Unmarshal(requireKey(t, o, key, where), &id); err != nil {
			t.Fatalf("%s: read %s: %v", where, key, err)
		}
		if _, dup := out[id]; dup {
			t.Fatalf("%s: duplicate %s %q", where, key, id)
		}
		out[id] = o
	}
	return out
}

func decodeString(t testing.TB, raw json.RawMessage, where string) string {
	t.Helper()
	var s string
	if err := gojson.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s: want string, got %s: %v", where, raw, err)
	}
	return s
}

func decodeInt(t testing.TB, raw json.RawMessage, where string) int {
	t.Helper()
	var i int
	if err := gojson.Unmarshal(raw, &i); err != nil {
		t.Fatalf("%s: want number, got %s: %v", where, raw, err)
	}
	return i
}

func decodeBool(t testing.TB, raw json.RawMessage, where string) bool {
	t.Helper()
	var b bool
	if err := gojson.Unmarshal(raw, &b); err != nil {
		t.Fatalf("%s: want bool, got %s: %v", where, raw, err)
	}
	return b
}

// finding logs a line prefixed FINDING.
func finding(t testing.TB, format string, args ...any) {
	t.Helper()
	t.Logf("FINDING: "+format, args...)
}

// encoderIdentity proves the app uses go.gh.ink/json for encoding and decoding.
func encoderIdentity(t testing.TB, app *fiber.App) {
	t.Helper()
	enc := app.Config().JSONEncoder
	if enc == nil {
		t.Fatal("app has no JSONEncoder configured")
	}
	want := reflect.ValueOf(gojson.Marshal).Pointer()
	if got := reflect.ValueOf(enc).Pointer(); got != want {
		t.Fatalf("JSONEncoder is not go.gh.ink/json.Marshal: ptr %d vs %d", got, want)
	}
	dec := app.Config().JSONDecoder
	if dec == nil || reflect.ValueOf(dec).Pointer() != reflect.ValueOf(gojson.Unmarshal).Pointer() {
		t.Fatal("JSONDecoder is not go.gh.ink/json.Unmarshal")
	}
}
