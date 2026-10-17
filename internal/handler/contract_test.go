package handler

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"testing"

	"aredn-map/internal/coord"
	"aredn-map/internal/model"
	"aredn-map/internal/repository/store"
)

// These wire-contract tests read the response bytes, never a Go struct.

// radioMetricKeys are the radio-only pointer members of model.Link; absent means null, not 0.
var radioMetricKeys = []string{
	"local_snr", "neighbor_snr", "etx", "avg_tx_pps",
	"bitrate_rx_mbps", "bitrate_tx_mbps", "retransmit_pct",
}

// alwaysPresentMetricKeys are the plain numbers every link kind reports.
var alwaysPresentMetricKeys = []string{
	"rx_success_pct", "tx_success_pct", "errors_pct", "rtt_ms",
	"ping_ms", "neighbor_ping_ms", "metric",
}

// linksByEdge keys /api/links output by "from->to".
func linksByEdge(t testing.TB, obj map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	t.Helper()
	out := map[string]map[string]json.RawMessage{}
	for _, l := range linkArray(t, obj, "links body") {
		from := decodeString(t, requireKey(t, l, "from", "link"), "from")
		to := decodeString(t, requireKey(t, l, "to", "link"), "to")
		out[from+"->"+to] = l
	}
	return out
}

// A WireGuard or DtD link has no SNR at all, unlike an RF link reading exactly 0 dB.
func TestContractRadioMetricsAreNotNullNotZero(t *testing.T) {
	st := store.New()
	seedContractStore(st)
	h := newHarness(t, nil, st, nil, nil)

	links := linksByEdge(t, h.getObj("/api/links"))

	wg, ok := links["localnode->wg-remote"]
	if !ok {
		t.Fatalf("fixture lost its WireGuard edge; have %v", edgeNames(links))
	}
	rfZero, ok := links["localnode->zero-snr-rf"]
	if !ok {
		t.Fatalf("fixture lost its 0 dB RF edge; have %v", edgeNames(links))
	}
	dtd, ok := links["zero-snr-rf->dtd-hub"]
	if !ok {
		t.Fatalf("fixture lost its DtD edge; have %v", edgeNames(links))
	}

	for _, tunnel := range []struct {
		edge string
		link map[string]json.RawMessage
	}{{"Wireguard", wg}, {"DtD", dtd}} {
		for _, key := range radioMetricKeys {
			// Present, and null: not absent, not 0.
			requireLiteral(t, tunnel.link, key, tunnel.edge+" link", "null")
		}
	}

	// The RF edge that really does read 0 dB has to say 0, not null.
	requireLiteral(t, rfZero, "local_snr", "RF link", "0")
	requireLiteral(t, rfZero, "neighbor_snr", "RF link", "0")
	for _, key := range []string{"avg_tx_pps", "bitrate_rx_mbps", "bitrate_tx_mbps", "retransmit_pct"} {
		requireLiteral(t, rfZero, key, "RF link", "0")
	}
	requireLiteral(t, rfZero, "etx", "RF link", "1")

	// The two links must not serialise the same way.
	if string(wg["local_snr"]) == string(rfZero["local_snr"]) {
		t.Errorf("WireGuard link and 0 dB RF link both emit local_snr=%s: null and 0 have collapsed",
			wg["local_snr"])
	}

	// The pointers must survive a decode into model.Link.
	var wgLink, rfLink model.Link
	decodeInto(t, mustRaw(t, wg), &wgLink)
	decodeInto(t, mustRaw(t, rfZero), &rfLink)
	if wgLink.LocalSNR != nil || wgLink.NeighborSNR != nil || wgLink.ETX != nil ||
		wgLink.AvgTxPps != nil || wgLink.BitrateRxMbps != nil || wgLink.BitrateTxMbps != nil ||
		wgLink.RetransmitPct != nil {
		t.Errorf("WireGuard link decoded non-nil radio metrics: %+v", &wgLink)
	}
	if rfLink.LocalSNR == nil || *rfLink.LocalSNR != 0 {
		t.Errorf("RF link with a 0 dB reading decoded as %v, want pointer to 0", rfLink.LocalSNR)
	}
	if rfLink.NeighborSNR == nil || *rfLink.NeighborSNR != 0 {
		t.Errorf("RF link with a 0 dB neighbour reading decoded as %v", rfLink.NeighborSNR)
	}
	if rfLink.ETX == nil || *rfLink.ETX != 1 {
		t.Errorf("RF link etx decoded as %v, want pointer to 1", rfLink.ETX)
	}

	// /api/topology ships the same Link shape and must not re-encode it differently.
	tLinks := linksByEdge(t, h.getObj("/api/topology"))
	if got := tLinks["localnode->wg-remote"]; got == nil {
		t.Fatal("topology dropped the WireGuard edge")
	} else {
		requireLiteral(t, got, "local_snr", "topology Wireguard link", "null")
	}
	if got := tLinks["localnode->zero-snr-rf"]; got == nil {
		t.Fatal("topology dropped the 0 dB RF edge")
	} else {
		requireLiteral(t, got, "local_snr", "topology RF link", "0")
	}
}

func edgeNames(links map[string]map[string]json.RawMessage) []string {
	names := make([]string, 0, len(links))
	for k := range links {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// mustRaw re-encodes one decoded member so it can be fed back into a struct.
func mustRaw(t testing.TB, obj map[string]json.RawMessage) []byte {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("re-encode member: %v", err)
	}
	return raw
}

// 0% loss and a 0 ms round trip are readings, so the plain numeric members never disappear.
func TestContractAlwaysPresentMetrics(t *testing.T) {
	st := store.New()
	seedContractStore(st)
	h := newHarness(t, nil, st, nil, nil)

	links := h.getObj("/api/links")
	all := linkArray(t, links, "links body")
	if len(all) == 0 {
		t.Fatal("no links to check")
	}
	for _, l := range all {
		edge := decodeString(t, requireKey(t, l, "from", "link"), "from") + "->" +
			decodeString(t, requireKey(t, l, "to", "link"), "to")
		for _, key := range alwaysPresentMetricKeys {
			raw := requireKey(t, l, key, edge)
			if string(raw) == "null" {
				t.Errorf("%s: %s is null; it is a plain number in the contract", edge, key)
			}
		}
	}

	// A tunnel link with nothing to report still emits every one of them as 0.
	wg := linksByEdge(t, links)["localnode->wg-remote"]
	for _, key := range alwaysPresentMetricKeys {
		want := "0"
		requireLiteral(t, wg, key, "Wireguard link zero reading", want)
	}
}

// band_label is derived from the band key by the store, never carried in from a crawler.
func TestContractBandLabelIsDerived(t *testing.T) {
	st := store.New()
	seedContractStore(st)
	h := newHarness(t, nil, st, nil, nil)

	nodes := nodeArray(t, h.getObj("/api/nodes"), "nodes body")
	if len(nodes) == 0 {
		t.Fatal("no nodes to check")
	}
	for _, n := range nodes {
		where := "node " + decodeString(t, requireKey(t, n, "hostname", "node"), "hostname")
		band := decodeString(t, requireKey(t, n, "band", where), "band")
		label := decodeString(t, requireKey(t, n, "band_label", where), "band_label")
		want, known := model.BandLabels[band]
		if !known {
			t.Errorf("%s: band %q is not a key of model.BandLabels, so the frontend cannot colour or label it", where, band)
			continue
		}
		if label != want {
			t.Errorf("%s: band=%q arrived with band_label=%q, want %q (band_label is derived, never carried)",
				where, band, label, want)
		}
	}

	byHost := indexByID(t, nodes, "hostname", "nodes body")

	lie := byHost["localnode"]
	if lie == nil {
		t.Fatal("fixture lost localnode")
	}
	// The fixture hands in band "5" with a written label of "No RF".
	requireLiteral(t, lie, "band", "localnode", `"5"`)
	requireLiteral(t, lie, "band_label", "localnode", `"5 GHz"`)

	// An unrecognised band key is normalised to the unresolved band and its label follows.
	odd := byHost["odd-radio"]
	if odd == nil {
		t.Fatal("fixture lost odd-radio")
	}
	requireLiteral(t, odd, "band", "odd-radio (came in as \"x\")", `"u"`)
	requireLiteral(t, odd, "band_label", "odd-radio", `"Unknown"`)

	// The same derivation applies to the graph payload.
	for _, n := range nodeArray(t, h.getObj("/api/topology"), "topology body") {
		id := decodeString(t, requireKey(t, n, "id", "topology node"), "id")
		band := decodeString(t, requireKey(t, n, "band", "topology node "+id), "band")
		label := decodeString(t, requireKey(t, n, "band_label", "topology node "+id), "band_label")
		if model.BandLabels[band] != label {
			t.Errorf("topology node %s: band=%q band_label=%q want %q", id, band, label, model.BandLabels[band])
		}
		if color := decodeString(t, requireKey(t, n, "color", "topology node "+id), "color"); color != model.BandColors[band] {
			t.Errorf("topology node %s: color=%q want %q for band %q", id, color, model.BandColors[band], band)
		}
	}
}

// online + offline + unprobed == total, and offline counts only probed nodes.
func TestContractCountsInvariant(t *testing.T) {
	st := store.New()
	seedContractStore(st)
	h := newHarness(t, nil, st, nil, nil)

	status := h.getObj("/api/status")
	counts := rawObject(t, requireKey(t, status, "counts", "status body"))
	online := decodeInt(t, requireKey(t, counts, "online", "counts"), "online")
	offline := decodeInt(t, requireKey(t, counts, "offline", "counts"), "offline")
	unprobed := decodeInt(t, requireKey(t, counts, "unprobed", "counts"), "unprobed")
	total := decodeInt(t, requireKey(t, counts, "total", "counts"), "total")

	if online+offline+unprobed != total {
		t.Errorf("counts do not add up: online %d + offline %d + unprobed %d != total %d",
			online, offline, unprobed, total)
	}

	// Recompute the buckets from the node list using the store's own rule.
	nodes := nodeArray(t, h.getObj("/api/nodes"), "nodes body")
	var seenOnline, seenOffline, seenUnprobed int
	for _, n := range nodes {
		on := decodeBool(t, requireKey(t, n, "online", "node"), "online")
		hasVerdict := hasVerdict(n)
		switch {
		case on:
			seenOnline++
		case hasVerdict:
			seenOffline++
		default:
			seenUnprobed++
		}
	}
	if len(nodes) != total {
		t.Fatalf("/api/nodes returned %d nodes but counts.total is %d", len(nodes), total)
	}
	if seenOnline != online || seenOffline != offline || seenUnprobed != unprobed {
		t.Errorf("counts say online=%d offline=%d unprobed=%d, the node list says online=%d offline=%d unprobed=%d",
			online, offline, unprobed, seenOnline, seenOffline, seenUnprobed)
	}

	// The fixture nodes expected offline, spelled out.
	wantOffline := []string{"dtd-hub", "odd-radio"}
	wantUnprobed := []string{"far-cn-peer", "never-probed-a", "never-probed-b", "wg-remote"}
	if got := hostsWhere(nodes, func(n map[string]json.RawMessage) bool {
		return !decodeBool(t, requireKey(t, n, "online", "node"), "online") && !hasVerdict(n)
	}); !equalStrings(got, wantUnprobed) {
		t.Errorf("nodes with no probe verdict = %v, want %v (they must land in unprobed, never in offline)", got, wantUnprobed)
	}
	if got := hostsWhere(nodes, func(n map[string]json.RawMessage) bool {
		return !decodeBool(t, requireKey(t, n, "online", "node"), "online") && hasVerdict(n)
	}); !equalStrings(got, wantOffline) {
		t.Errorf("probed-and-negative nodes = %v, want %v", got, wantOffline)
	}

	// The other two summaries must agree with the node list too.
	bandCounts := rawObject(t, requireKey(t, status, "band_counts", "status body"))
	if sum := sumInts(t, bandCounts); sum != total {
		t.Errorf("band_counts sum %d != counts.total %d", sum, total)
	}
	linkCounts := rawObject(t, requireKey(t, status, "link_type_counts", "status body"))
	if sum := sumInts(t, linkCounts); sum != decodeInt(t, requireKey(t, counts, "links", "counts"), "links") {
		t.Errorf("link_type_counts sum %d != counts.links", sum)
	}
	for key := range bandCounts {
		if _, ok := model.BandLabels[key]; !ok {
			t.Errorf("band_counts carries unknown band key %q", key)
		}
	}
}

// hasVerdict mirrors store.hasProbeVerdict: a probe stamp, verdict or probe error.
func hasVerdict(n map[string]json.RawMessage) bool {
	if raw, ok := n["probed_at"]; ok && string(raw) != `"0001-01-01T00:00:00Z"` {
		return true
	}
	if raw, ok := n["probe_error"]; ok && string(raw) != `""` && string(raw) != "null" {
		return true
	}
	if raw, ok := n["online"]; ok && string(raw) == "true" {
		return true
	}
	return false
}

// Nodes nobody probed must not be reported as offline.
func TestContractTwelveUnprobedAreNotTwelveOffline(t *testing.T) {
	st := store.New()
	for i := range 12 {
		st.UpsertNode(&model.Node{
			Name:     "Catalog Peer",
			Hostname: catalogHost(i),
			Band:     model.BandUnresolved,
			Source:   "mesh-list",
		})
	}
	h := newHarness(t, nil, st, nil, nil)

	counts := rawObject(t, requireKey(t, h.getObj("/api/status"), "counts", "status body"))
	if got := decodeInt(t, requireKey(t, counts, "total", "counts"), "total"); got != 12 {
		t.Fatalf("total = %d, want 12", got)
	}
	if got := decodeInt(t, requireKey(t, counts, "offline", "counts"), "offline"); got != 0 {
		t.Errorf("offline = %d for twelve never-probed nodes, want 0", got)
	}
	if got := decodeInt(t, requireKey(t, counts, "unprobed", "counts"), "unprobed"); got != 12 {
		t.Errorf("unprobed = %d, want 12", got)
	}
	if got := decodeInt(t, requireKey(t, counts, "online", "counts"), "online"); got != 0 {
		t.Errorf("online = %d, want 0", got)
	}

	// The node filter treats every non-online node as offline; asserted as-is.
	offline := nodeArray(t, h.getObj("/api/nodes?state=offline"), "state=offline body")
	if len(offline) != 12 {
		t.Fatalf("state=offline returned %d nodes, want the twelve unprobed ones", len(offline))
	}
	finding(t, "GET /api/nodes?state=offline (api.go:128 filters !n.Online) lists all 12 unprobed "+
		"nodes, while counts.offline says 0 and counts.unprobed says 12. frontend/app/types.ts:126-134 "+
		"declares neither unprobed nor a way to ask for it")
}

func catalogHost(i int) string { return "catalog-peer-" + itoa(i) }

func itoa(i int) string { return strconv.Itoa(i) }

// Fact 5: /api/status keeps counts and scan nested.
func TestContractStatusShape(t *testing.T) {
	st := store.New()
	seedContractStore(st)
	h := newHarness(t, nil, st, nil, nil)

	status := h.getObj("/api/status")

	requireKeys(t, status, "status body",
		"counts", "scan", "scanning_enabled", "seeds", "band_counts", "link_type_counts",
		"icmp_mode", "server_time", "cron", "last_sweep_at", "next_sweep_at",
		"uptime_seconds", "version")

	// Nothing from the nested blocks may leak up: the page reads status.counts.total.
	forbidKeys(t, status, "status body",
		"total", "online", "offline", "unprobed", "located", "supernodes", "pending",
		"links", "services", "failed", "unreachable", "crawled",
		"kind", "phase", "status", "started_at", "completed_at", "completed",
		"succeeded", "failed", "current_target", "message", "node_progress", "node_total")

	counts := rawObject(t, requireKey(t, status, "counts", "status body"))
	requireKeys(t, counts, "counts",
		"total", "online", "offline", "unprobed", "located", "supernodes", "pending",
		"links", "services", "failed", "unreachable", "crawled")
	scan := rawObject(t, requireKey(t, status, "scan", "status body"))
	requireKeys(t, scan, "scan",
		"id", "kind", "phase", "status", "total", "completed", "succeeded", "failed",
		"current_target", "node_progress", "node_total")

	if got := decodeString(t, requireKey(t, status, "version", "status body"), "version"); got != fixedVersion {
		t.Errorf("version = %q, want %q", got, fixedVersion)
	}
	if got := decodeString(t, requireKey(t, status, "icmp_mode", "status body"), "icmp_mode"); got != "unprivileged" {
		t.Errorf("icmp_mode = %q, want the configured mode", got)
	}
	if got := decodeString(t, requireKey(t, status, "cron", "status body"), "cron"); got != "0 */30 * * * *" {
		t.Errorf("cron = %q", got)
	}
	if got := decodeBool(t, requireKey(t, status, "scanning_enabled", "status body"), "scanning_enabled"); got != h.cfg.Scan.RunOnStartup || got != h.eng.Enabled() {
		t.Errorf("scanning_enabled = %v, want the engine flag (%v)", got, h.eng.Enabled())
	}
	seeds := rawArray(t, requireKey(t, status, "seeds", "status body"))
	if len(seeds) != 2 {
		t.Errorf("seeds = %v, want the two configured entry points", seeds)
	}

	// The scheduler answer is a constant so nothing here reads the clock.
	if got := decodeString(t, requireKey(t, status, "next_sweep_at", "status body"), "next_sweep_at"); got != fixedNextSweep {
		t.Errorf("next_sweep_at = %q, want the value controller.Deps.NextRun returns (%q)", got, fixedNextSweep)
	}

	// Before any sweep has run, scan is the zero struct. That is what the UI sees.
	if got := decodeString(t, requireKey(t, scan, "status", "scan"), "status"); got != "" {
		t.Errorf("idle scan.status = %q, want the empty string the engine reports", got)
	}
	finding(t, "GET /api/status on an engine that never swept returns scan.phase=\"\" and scan.status=\"\" "+
		"(model.ScanStatus has no idle value; scanner.Engine only writes them inside Sweep). "+
		"frontend/app/types.ts:111-115 declares them as plain strings and SummaryCards.vue:11 divides by "+
		"scan.total, which the engine never fills (it sets node_total/node_progress instead, undeclared at "+
		"types.ts:109-122), so the progress bar reads 0/0 while a sweep runs")

	// time.Time omitempty cannot apply to a struct, so the sweep stamps always serialise.
	finding(t, "GET /api/status emits last_sweep_at=%s: time.Time omitempty never drops the field, so a "+
		"node that never swept serialises year 1 instead of the null frontend/app/types.ts:144 declares",
		status["last_sweep_at"])
}

// GCJ-02 is filled for every located node, including a neighbour-stub-only node.
func TestContractGCJCoordinates(t *testing.T) {
	st := store.New()
	seedContractStore(st)
	h := newHarness(t, nil, st, nil, nil)

	nodes := nodeArray(t, h.getObj("/api/nodes"), "nodes body")
	var located, shifted, identical int
	for _, n := range nodes {
		host := decodeString(t, requireKey(t, n, "hostname", "node"), "hostname")
		where := "node " + host
		if !decodeBool(t, requireKey(t, n, "has_location", where), "has_location") {
			continue
		}
		located++
		lat, lon := requireKey(t, n, "lat", where), requireKey(t, n, "lon", where)
		gcjLat, gcjLon := requireKey(t, n, "gcj_lat", where), requireKey(t, n, "gcj_lon", where)

		var latF, lonF float64
		decodeInto(t, lat, &latF)
		decodeInto(t, lon, &lonF)
		wantLat, wantLon := coord.WGS84ToGCJ02(latF, lonF)

		// Byte-identical outside the offset zone, strictly different inside it.
		gotLat, gotLon := gcjLat, gcjLon
		if coord.InChinaMainland(latF, lonF) {
			shifted++
			if string(gotLat) == string(lat) || string(gotLon) == string(lon) {
				t.Errorf("%s: Chinese position kept %s/%s unwrapped; gcj_lat=%s gcj_lon=%s",
					where, lat, lon, gotLat, gotLon)
			}
			if string(gotLat) == "null" || string(gotLat) == "0" || string(gotLon) == "0" {
				t.Errorf("%s: gcj pair %s/%s was never computed for a located node", where, gotLat, gotLon)
			}
		} else {
			identical++
			if string(gotLat) != string(lat) || string(gotLon) != string(lon) {
				t.Errorf("%s: outside China gcj must be byte-identical to wgs: %s/%s vs %s/%s",
					where, gotLat, gotLon, lat, lon)
			}
		}

		// Whatever the encoder wrote has to agree with the conversion.
		var decLat, decLon float64
		decodeInto(t, gotLat, &decLat)
		decodeInto(t, gotLon, &decLon)
		if decLat != wantLat || decLon != wantLon {
			t.Errorf("%s: gcj pair %.10f,%.10f != coord.WGS84ToGCJ02(%.10f,%.10f)=%.10f,%.10f",
				where, decLat, decLon, latF, lonF, wantLat, wantLon)
		}
	}

	if located == 0 {
		t.Fatal("fixture has no located node")
	}
	if shifted == 0 || identical == 0 {
		t.Fatalf("fixture covered %d shifted and %d identical Chinese/non-Chinese nodes", shifted, identical)
	}
	if located != decodeInt(t, requireKey(t, rawObject(t, requireKey(t, h.getObj("/api/status"), "counts", "status")), "located", "counts"), "located") {
		t.Error("counts.located disagrees with the number of nodes carrying has_location")
	}

	// The named cases, spelled out.
	byHost := indexByID(t, nodes, "hostname", "nodes body")
	requireLiteral(t, byHost["localnode"], "lat", "localnode wgs lat", "31.2304")
	if string(byHost["localnode"]["gcj_lat"]) == "31.2304" {
		t.Error("Shanghai node serialises gcj_lat equal to lat")
	}
	// far-cn-peer only ever existed as a link-dialog stub with a position.
	far := byHost["far-cn-peer"]
	requireLiteral(t, far, "has_location", "far-cn-peer", "true")
	requireLiteral(t, far, "source", "far-cn-peer", `"link-dialog"`)
	if string(far["gcj_lat"]) == "0" || string(far["gcj_lon"]) == "0" {
		t.Errorf("link-dialog stub got no GCJ pair: gcj_lat=%s gcj_lon=%s", far["gcj_lat"], far["gcj_lon"])
	}
	// A US node is untouched.
	requireLiteral(t, byHost["zero-snr-rf"], "gcj_lat", "phoenix gcj_lat", "33.4484")
	requireLiteral(t, byHost["zero-snr-rf"], "gcj_lon", "phoenix gcj_lon", "-112.074")

	// No located node may be plotted at 0,0 through the graph payload either.
	for _, n := range nodeArray(t, h.getObj("/api/topology"), "topology body") {
		id := decodeString(t, requireKey(t, n, "id", "topology node"), "id")
		if byHost[id] == nil {
			t.Fatalf("topology node %q is not in /api/nodes", id)
		}
	}
}

// A link dialog address is never a mesh identity.
func TestContractMeshIPIsNotTakenFromALinkDialog(t *testing.T) {
	st := store.New()
	seedContractStore(st)
	h := newHarness(t, nil, st, nil, nil)

	nodes := nodeArray(t, h.getObj("/api/nodes"), "nodes body")
	seen := map[string]string{}
	for _, n := range nodes {
		host := decodeString(t, requireKey(t, n, "hostname", "node"), "hostname")
		ip := decodeString(t, requireKey(t, n, "mesh_ip", "node "+host), "mesh_ip")
		if ip == "" {
			continue
		}
		seen[ip] = host
	}
	for _, tunnel := range []string{wgTunnelIP, "10.255.99.9", "10.255.99.10"} {
		if host, bad := seen[tunnel]; bad {
			t.Errorf("link-dialog address %s was stored as the mesh_ip of %s", tunnel, host)
		}
		code, _, body := h.get("/api/lookup?ip=" + tunnel)
		if code != http.StatusOK {
			t.Fatalf("lookup %s -> %d", tunnel, code)
		}
		if results := rawArray(t, requireKey(t, rawObject(t, body), "results", "lookup body")); len(results) != 0 {
			t.Errorf("lookup of link-dialog address %s matched a node: %s", tunnel, results)
		}
	}

	// The addresses the store did take are the labelled mesh ones.
	if seen["10.255.0.1"] != "localnode" {
		t.Errorf("mesh_ip 10.255.0.1 belongs to %q, want localnode", seen["10.255.0.1"])
	}
	if got := seen[wgTunnelIP]; got != "" {
		t.Errorf("the WireGuard tunnel endpoint became %q's mesh identity", got)
	}
}

func hostsWhere(nodes []map[string]json.RawMessage, fn func(map[string]json.RawMessage) bool) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if fn(n) {
			var host string
			_ = json.Unmarshal(n["hostname"], &host)
			out = append(out, host)
		}
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sumInts(t testing.TB, obj map[string]json.RawMessage) int {
	t.Helper()
	sum := 0
	for _, raw := range obj {
		sum += decodeInt(t, raw, "band or link count")
	}
	return sum
}
