package handler

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"aredn-map/internal/model"
	"aredn-map/internal/repository/ping"
	"aredn-map/internal/repository/store"
	"aredn-map/internal/service/scanner"
)

// These tests drive a real *scanner.Engine with in-memory Fetcher and Pinger tables.

const (
	localMeshIP  = "10.255.0.1"
	zeroMeshIP   = "10.255.0.2"
	superMeshIP  = "10.255.0.5"
	dtdMeshIP    = "10.255.0.6"
	dtdDialogIP  = "10.255.99.9"
	lanServiceIP = "10.255.0.42"
)

// scriptMesh is the fake mesh every engine test sweeps: RF, WireGuard, DtD and supernode hosts.
func scriptMesh() map[string]scriptHost {
	return map[string]scriptHost{
		"localnode": {
			self: &model.Node{
				Name:          "Local Node",
				Hostname:      "localnode",
				URL:           "http://localnode.local.mesh/",
				MeshIP:        localMeshIP,
				Hardware:      "XR5",
				Firmware:      "4.1.0",
				Band:          model.Band5GHz,
				FreqLow:       5745,
				ChanBW:        10,
				Channel:       149,
				Lat:           shanghaiLat,
				Lon:           shanghaiLon,
				HasLocation:   true,
				Services:      []string{"ssh|" + lanServiceIP},
				DetailFetched: true,
			},
			links: []model.Link{
				rfLink("localnode", "zero-snr-rf", new(0), new(0), fl(1), zeroMeshIP, "02:00:00:00:00:01"),
				{
					From: "localnode", To: "wg-remote", Container: model.ContainerLocal,
					Type: model.LinkTypeWireguard, Status: "up",
					// The dialog hands back the tunnel endpoint and nothing else.
					IP: wgTunnelIP, PeerIP: wgTunnelIP, PeerName: "wg-remote",
					PeerHardware: "x86_64", PeerFirmware: "4.1.0",
					RxSuccessPct: 100, TxSuccessPct: 100,
				},
				{
					From: "localnode", To: "dtd-hub", Container: model.ContainerNeighborhood,
					Type: model.LinkTypeDtD, Status: "active",
					IP: dtdDialogIP, PeerIP: dtdDialogIP, PeerName: "DtD Hub",
					Metric: 6,
				},
				rfLink("localnode", "supernode-hq", new(15), new(11), new(1.5), superMeshIP, "02:00:00:00:00:02"),
			},
		},
		"zero-snr-rf": {
			self: &model.Node{
				Name:          "Zero SNR RF",
				Hostname:      "zero-snr-rf",
				MeshIP:        zeroMeshIP,
				Hardware:      "Rocket M",
				Band:          model.Band2_4GHz,
				FreqLow:       2412,
				Lat:           phoenixLat,
				Lon:           phoenixLon,
				HasLocation:   true,
				DetailFetched: true,
			},
			links: []model.Link{rfLink("zero-snr-rf", "localnode", new(3), new(4), fl(2), localMeshIP, "02:00:00:00:00:01")},
		},
		"supernode-hq": {
			self: &model.Node{
				Name:          "Supernode HQ",
				Hostname:      "supernode-hq",
				MeshIP:        superMeshIP,
				Band:          model.BandSupernode,
				IsSupernode:   true,
				DetailFetched: true,
			},
			links: []model.Link{rfLink("supernode-hq", "localnode", new(7), new(9), new(1.2), localMeshIP, "02:00:00:00:00:02")},
		},
		"dtd-hub": {
			self: &model.Node{
				Name:          "DtD Hub",
				Hostname:      "dtd-hub",
				MeshIP:        dtdMeshIP,
				Band:          model.BandNoRF,
				DetailFetched: true,
			},
		},
		// "wg-remote" is absent: its status page never answers, so only the link dialog knows it.
	}
}

func rfLink(from, to string, localSNR, neighborSNR *int, etx *float64, peerIP, mac string) model.Link {
	return model.Link{
		From: from, To: to, Container: model.ContainerNeighborhood,
		Type: model.LinkTypeRF, Status: "active", MAC: mac, IP: peerIP, PeerIP: peerIP,
		RxSuccessPct: 100, TxSuccessPct: 100, Metric: 2,
		RTTMs: 5, PingMs: 5, NeighborPingMs: 5,
		LocalSNR: localSNR, NeighborSNR: neighborSNR, ETX: etx,
		AvgTxPps: fl(10), BitrateRxMbps: fl(100), BitrateTxMbps: fl(100), RetransmitPct: fl(1),
	}
}

// engineHarness returns a harness whose engine sweeps scriptMesh.
func engineHarness(t testing.TB) (*harness, *store.Store) {
	t.Helper()
	st := store.New()
	cfg := testConfig()
	fetch := newFakeFetcher(scriptMesh())
	pg := &fakePinger{mode: cfg.ICMP.Mode, results: map[string]ping.Result{
		"localnode":    {Host: "localnode", Sent: 3, Recv: 3, AvgMs: 4.5, TTL: 63, IP: localMeshIP},
		"zero-snr-rf":  {Host: "zero-snr-rf", Sent: 3, Recv: 3, AvgMs: 9.5, IP: zeroMeshIP},
		"supernode-hq": {Host: "supernode-hq", Sent: 3, Recv: 2, AvgMs: 2, IP: superMeshIP, Err: ping.ErrNoReply},
		"dtd-hub":      {Host: "dtd-hub", Sent: 3, Recv: 0, Err: ping.ErrNoReply},
	}}
	h := newHarness(t, cfg, st, fetch, pg)
	st.SetSeeds(cfg.Scan.Seeds)
	if err := h.eng.Sweep(context.Background(), scanner.KindFull); err != nil {
		t.Fatalf("sweep of the scripted mesh failed: %v", err)
	}
	return h, st
}

// After a real sweep, no node may carry a link dialog address as its mesh identity.
func TestEngineSweepNeverAdoptsALinkDialogAddressAsMeshIP(t *testing.T) {
	h, st := engineHarness(t)

	nodes := nodeArray(t, h.getObj("/api/nodes"), "nodes body")
	byHost := indexByID(t, nodes, "hostname", "nodes body")
	if len(byHost) != st.NodeCount() {
		t.Fatalf("/api/nodes shows %d nodes, the store holds %d", len(byHost), st.NodeCount())
	}

	banned := []string{wgTunnelIP, dtdDialogIP}
	for host, n := range byHost {
		ip := decodeString(t, requireKey(t, n, "mesh_ip", "node "+host), "mesh_ip ("+host+")")
		if slices.Contains(banned, ip) {
			t.Errorf("node %q got mesh_ip %q, which is a link dialog address (WireGuard tunnel / DtD side), not a mesh identity", host, ip)
		}
	}

	// The WireGuard peer exists, is plotted, and has no mesh identity at all.
	wg, ok := byHost["wg-remote"]
	if !ok {
		t.Fatalf("the scripted WireGuard peer never reached the node list: %v", slices.Sorted(maps.Keys(byHost)))
	}
	requireLiteral(t, wg, "mesh_ip", "wg-remote", `""`)
	requireLiteral(t, wg, "source", "wg-remote", `"neighbor-list"`)
	requireLiteral(t, wg, "online", "wg-remote", "false")
	// It was never probed, so it is unprobed rather than offline.
	requireKey(t, wg, "probed_at", "wg-remote")
	if hasVerdict(wg) {
		t.Errorf("wg-remote looks probed on the wire: %s", mustRaw(t, wg))
	}

	// The DtD hub keeps the mesh address its own page labelled, not its neighbour's dialog address.
	dtd := requireKey(t, byHost["dtd-hub"], "mesh_ip", "dtd-hub")
	requireLiteral(t, byHost["dtd-hub"], "mesh_ip", "dtd-hub", `"`+dtdMeshIP+`"`)
	if string(dtd) == `"`+dtdDialogIP+`"` {
		t.Error("DtD-side address leaked into mesh_ip")
	}

	// The tunnel address is still on the edge, where it belongs.
	links := linksByEdge(t, h.getObj("/api/links"))
	wgEdge := links["localnode->wg-remote"]
	if wgEdge == nil {
		t.Fatal("the WireGuard edge disappeared from /api/links")
	}
	requireLiteral(t, wgEdge, "ip", "wg edge", `"`+wgTunnelIP+`"`)
	for _, key := range radioMetricKeys {
		requireLiteral(t, wgEdge, key, "wg edge after a real sweep", "null")
	}
	// The 0 dB RF edge from the same sweep keeps its 0.
	requireLiteral(t, links["localnode->zero-snr-rf"], "local_snr", "rf edge after a real sweep", "0")

	// And the lookup endpoint agrees: the tunnel address owns nothing.
	code, _, body := h.get("/api/lookup?ip=" + wgTunnelIP)
	if code != http.StatusOK {
		t.Fatalf("lookup -> %d", code)
	}
	if got := rawArray(t, requireKey(t, rawObject(t, body), "results", "lookup body")); len(got) != 0 {
		t.Errorf("lookup resolved the tunnel address to %s", got)
	}

	// Band label derivation and the supernode stamp hold for everything a sweep produced.
	for _, n := range nodes {
		host := decodeString(t, requireKey(t, n, "hostname", "node"), "hostname")
		band := decodeString(t, requireKey(t, n, "band", host), "band")
		label := decodeString(t, requireKey(t, n, "band_label", host), "band_label")
		if model.BandLabels[band] != label {
			t.Errorf("swept node %q: band=%q label=%q want %q", host, band, label, model.BandLabels[band])
		}
		if decodeBool(t, requireKey(t, n, "has_location", host), "has_location") {
			if string(n["gcj_lat"]) == "0" && string(n["lat"]) != "0" {
				t.Errorf("swept node %q is located but has no gcj_lat", host)
			}
		}
	}
	if byHost["supernode-hq"] == nil {
		t.Fatal("supernode-hq missing")
	}
	requireLiteral(t, byHost["supernode-hq"], "band", "supernode", `"s"`)
	requireLiteral(t, byHost["supernode-hq"], "band_label", "supernode", `"Supernode"`)
	requireLiteral(t, byHost["localnode"], "lat", "localnode wgs lat", "31.2304")
	if string(byHost["localnode"]["gcj_lat"]) == "31.2304" {
		t.Error("the swept Chinese seed kept its WGS-84 pair as gcj")
	}

	// Over a real sweep: 3 online, 1 probed-dead, 1 never probed.
	counts := rawObject(t, requireKey(t, h.getObj("/api/status"), "counts", "status body"))
	online := decodeInt(t, requireKey(t, counts, "online", "counts"), "online")
	offline := decodeInt(t, requireKey(t, counts, "offline", "counts"), "offline")
	unprobed := decodeInt(t, requireKey(t, counts, "unprobed", "counts"), "unprobed")
	total := decodeInt(t, requireKey(t, counts, "total", "counts"), "total")
	if online+offline+unprobed != total {
		t.Errorf("swept counts break the invariant: %d+%d+%d != %d", online, offline, unprobed, total)
	}
	if online != 3 || offline != 1 || unprobed != 1 || total != 5 {
		t.Errorf("swept counts = online %d offline %d unprobed %d total %d, want 3/1/1/5", online, offline, unprobed, total)
	}
}

// The sweep also has to leave the scan block describing itself.
func TestEngineSweepReportsScanProgressInStatus(t *testing.T) {
	h, _ := engineHarness(t)

	status := h.getObj("/api/status")
	scan := rawObject(t, requireKey(t, status, "scan", "status body"))
	if got := decodeString(t, requireKey(t, scan, "status", "scan"), "scan status"); got != "completed with failures" {
		t.Errorf("scan.status = %q; the scripted mesh has one node whose status page never answers, "+
			"so the engine reports %q", got, "completed with failures")
	}
	if got := decodeString(t, requireKey(t, scan, "phase", "scan"), "scan phase"); got != model.PhaseCompleted {
		t.Errorf("scan.phase = %q, want %q", got, model.PhaseCompleted)
	}
	if got := decodeInt(t, requireKey(t, scan, "failed", "scan"), "scan failed"); got == 0 {
		t.Error("scan.failed = 0 although the unreachable peer was recorded")
	}
	// node_total carries the live per-phase denominator so the bar never sits at "5/0".
	if got := decodeInt(t, requireKey(t, scan, "node_total", "scan"), "scan node_total"); got == 0 {
		t.Errorf("scan.node_total = 0 after a completed sweep, want the probed node count")
	}
	total := decodeInt(t, requireKey(t, scan, "total", "scan"), "scan total")
	done := decodeInt(t, requireKey(t, scan, "completed", "scan"), "scan completed")
	failed := decodeInt(t, requireKey(t, scan, "failed", "scan"), "scan failed")
	if total != done+failed {
		t.Errorf("scan.total = %d, want completed+failed = %d", total, done+failed)
	}

	// completed_at is an RFC3339 stamp the status handler re-parses.
	if got := decodeString(t, requireKey(t, scan, "completed_at", "scan"), "scan completed_at"); got == "" {
		t.Error("scan.completed_at empty after a completed sweep")
	} else if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("scan.completed_at %q is not RFC3339: %v", got, err)
	}

	// The unreachable peer is visible with its reason.
	nodes := indexByID(t, nodeArray(t, h.getObj("/api/nodes"), "nodes body"), "hostname", "nodes body")
	if got := decodeString(t, requireKey(t, nodes["dtd-hub"], "probe_error", "dtd-hub"), "probe_error"); got == "" {
		t.Error("dtd-hub was probed dead but serialises no probe_error")
	}
	if got := decodeString(t, requireKey(t, nodes["wg-remote"], "last_error", "wg-remote"), "last_error"); got == "" {
		t.Error("the unreachable peer serialises no last_error after a failed fetch")
	}
}

// /api/scan/enable is a scheduler flag: switching it off does not stop a running sweep.
func TestScanEnableFalseDoesNotInterruptARunningSweep(t *testing.T) {
	st := store.New()
	cfg := testConfig()
	cfg.Scan.RunOnStartup = true
	fetch := newFakeFetcher(scriptMesh())
	fetch.gate = make(chan struct{})
	pg := &fakePinger{mode: cfg.ICMP.Mode, results: map[string]ping.Result{}}
	h := newHarness(t, cfg, st, fetch, pg)
	st.SetSeeds(cfg.Scan.Seeds)

	if got := decodeBool(t, requireKey(t, h.getObj("/api/status"), "scanning_enabled", "status body"), "scanning_enabled"); !got {
		t.Fatal("the harness should start with scanning enabled")
	}

	rs := h.startSweep(scanner.KindFull, fetch)

	waitFor(t, func() bool { return fetch.count("self", "localnode") > 0 }, "the sweep to reach the seed")

	// The sweep is visibly running and still blocked on the scripted fetch.
	scan := rawObject(t, requireKey(t, h.getObj("/api/status"), "scan", "status body"))
	if got := decodeString(t, requireKey(t, scan, "status", "scan"), "scan status"); got != "running" {
		t.Fatalf("scan.status = %q while the scripted sweep is blocked, want running", got)
	}

	code, _, body := h.post("/api/scan/enable", `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("enable false -> %d: %s", code, body)
	}
	requireLiteral(t, rawObject(t, body), "scanning_enabled", "enable response", "false")
	if h.eng.Enabled() {
		t.Error("the engine still reports Enabled() after the endpoint turned scanning off")
	}

	// The handler only flips the flag, so the sweep still reports running.
	scan = rawObject(t, requireKey(t, h.getObj("/api/status"), "scan", "status body"))
	if got := decodeString(t, requireKey(t, scan, "status", "scan"), "scan status"); got != "running" {
		t.Errorf("after enable=false the sweep reports %q, want running: the endpoint does not cancel", got)
	}
	finding(t, "POST /api/scan/enable {\"enabled\":false} (api.go:222-233) only calls Engine.SetEnabled; it "+
		"leaves a running sweep going and does not Cancel it, so the UI's off switch has no immediate effect. "+
		"frontend/app/types.ts:137 declares scanning_enabled as a boolean and treats it as a stop control")

	fetch.release()
	if err := rs.wait(t); err != nil {
		t.Fatalf("blocked sweep returned %v", err)
	}
	if h.eng.Enabled() {
		t.Error("the flag was reset by the sweep; it must stay off until the endpoint is called again")
	}
}

// /api/scan/stop cancels a running sweep and is a no-op when nothing runs.
func TestScanStopCancelsARunningSweep(t *testing.T) {
	st := store.New()
	cfg := testConfig()
	cfg.Scan.RunOnStartup = true
	fetch := newFakeFetcher(scriptMesh())
	fetch.gate = make(chan struct{})
	pg := &fakePinger{mode: cfg.ICMP.Mode, results: map[string]ping.Result{}}
	h := newHarness(t, cfg, st, fetch, pg)
	st.SetSeeds(cfg.Scan.Seeds)

	rs := h.startSweep(scanner.KindFull, fetch)

	waitFor(t, func() bool { return fetch.count("self", "localnode") > 0 }, "the sweep to reach the seed")

	code, _, body := h.post("/api/scan/stop", "")
	if code != http.StatusOK {
		t.Fatalf("stop -> %d: %s", code, body)
	}
	obj := rawObject(t, body)
	requireKeys(t, obj, "stop body", "status")
	if _, ok := rawObject(t, obj["status"])["phase"]; !ok {
		t.Error("stop response carries no scan phase")
	}

	fetch.release()
	err := rs.wait(t)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled sweep returned %v, want context.Canceled", err)
	}
	got := h.eng.Status()
	if got.Phase != model.PhaseStopped || got.Status != "stopped" {
		t.Errorf("after stop the engine reports phase=%q status=%q, want %q/%q",
			got.Phase, got.Status, model.PhaseStopped, "stopped")
	}
	// Stopping leaves scanning_enabled set, so the next cron tick sweeps again.
	if got := decodeBool(t, requireKey(t, h.getObj("/api/status"), "scanning_enabled", "status body"), "scanning_enabled"); !got {
		t.Error("POST /api/scan/stop cleared scanning_enabled; it only cancels the running sweep")
	}
	finding(t, "POST /api/scan/stop (api.go:252) cancels the sweep but leaves Engine.Enabled() as it was, "+
		"so a scheduled sweep starts again on the next tick; its response echoes the status of the sweep "+
		"that is still winding down rather than the stopped state the UI expects")
}

func TestScanStopIsANoOpWhenNothingIsRunning(t *testing.T) {
	h, _ := engineHarness(t)

	before := h.getObj("/api/status")
	code, _, body := h.post("/api/scan/stop", "")
	if code != http.StatusOK {
		t.Fatalf("stop -> %d: %s", code, body)
	}
	after := rawObject(t, body)
	status := rawObject(t, requireKey(t, after, "status", "stop body"))
	want := rawObject(t, requireKey(t, before, "scan", "status body"))
	if string(mustRaw(t, status)) != string(mustRaw(t, want)) {
		t.Errorf("stop with no sweep running changed the reported scan state:\nbefore %s\nafter  %s",
			mustRaw(t, want), mustRaw(t, status))
	}
	// And a second sweep is still possible.
	if err := h.eng.Sweep(context.Background(), scanner.KindIncremental); err != nil {
		t.Fatalf("the engine is unusable after an idle stop: %v", err)
	}
}

// waitFor polls cond until it holds, so no test sleeps blindly.
func waitFor(t testing.TB, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// compile-time proof that the fakes satisfy the interfaces the engine needs.
var (
	_ scanner.Fetcher = (*fakeFetcher)(nil)
	_ scanner.Pinger  = (*fakePinger)(nil)
)
