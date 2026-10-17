package handler

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	gojson "go.gh.ink/json"

	"aredn-map/internal/model"
	"aredn-map/internal/repository/store"
	"aredn-map/internal/service/scanner"
)

// Endpoint-by-endpoint coverage of the API the dashboard polls, plus its transport promises.

func contractHarness(t testing.TB) *harness {
	t.Helper()
	st := store.New()
	seedContractStore(st)
	return newHarness(t, nil, st, nil, nil)
}

func TestHealthEndpoint(t *testing.T) {
	h := contractHarness(t)
	code, header, body := h.get("/api/health")
	if code != http.StatusOK {
		t.Fatalf("health -> %d", code)
	}
	if got := string(body); got != `{"ok":true}` {
		t.Errorf("health body = %s, want {\"ok\":true}", got)
	}
	if ct := header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("health content type = %q", ct)
	}
}

// List endpoints answer with an array, never null; the dashboard filters them directly.
func TestEmptyStoreStillServesArrays(t *testing.T) {
	h := newHarness(t, nil, nil, nil, nil)

	nodes := h.getObj("/api/nodes")
	requireLiteral(t, nodes, "nodes", "empty /api/nodes", "[]")
	requireLiteral(t, nodes, "total", "empty /api/nodes", "0")

	links := h.getObj("/api/links")
	requireLiteral(t, links, "links", "empty /api/links", "[]")
	requireLiteral(t, links, "total", "empty /api/links", "0")

	topo := h.getObj("/api/topology")
	requireLiteral(t, topo, "nodes", "empty /api/topology", "[]")
	requireLiteral(t, topo, "links", "empty /api/topology", "[]")

	lookup := rawArray(t, requireKey(t, h.getObj("/api/lookup?ip=10.0.0.1"), "results", "empty lookup"))
	if len(lookup) != 0 {
		t.Errorf("an empty store answered a lookup with %v", lookup)
	}
	status := h.getObj("/api/status")
	requireLiteral(t, status, "band_counts", "empty status", "{}")
	requireLiteral(t, status, "link_type_counts", "empty status", "{}")
	requireLiteral(t, status, "seeds", "empty status", `["localnode.local.mesh"]`)
	counts := rawObject(t, requireKey(t, status, "counts", "empty status"))
	for _, key := range []string{"total", "online", "offline", "unprobed", "links"} {
		requireLiteral(t, counts, key, "empty counts", "0")
	}
}

func TestNodesEndpointShape(t *testing.T) {
	h := contractHarness(t)
	nodes := h.getObj("/api/nodes")
	objs := nodeArray(t, nodes, "nodes body")
	if len(objs) != 9 {
		t.Fatalf("got %d nodes, want the nine fixture nodes", len(objs))
	}
	if got := decodeInt(t, requireKey(t, nodes, "total", "nodes body"), "total"); got != len(objs) {
		t.Errorf("total = %d but the array holds %d nodes", got, len(objs))
	}

	// Ordered by display name, so the table and the map agree without sorting.
	names := make([]string, 0, len(objs))
	for _, n := range objs {
		names = append(names, decodeString(t, requireKey(t, n, "name", "node"), "name"))
	}
	if !slices.IsSortedFunc(names, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) }) {
		t.Errorf("nodes are not ordered by name: %v", names)
	}

	// Every contract member is on the wire, even ones omitempty would swallow for zero values.
	for _, n := range objs {
		host := decodeString(t, requireKey(t, n, "hostname", "node"), "hostname")
		requireKeys(t, n, "node "+host,
			"name", "hostname", "url", "mesh_ip", "band", "band_label", "channel",
			"lat", "lon", "gcj_lat", "gcj_lon", "has_location", "is_supernode",
			"hops", "etx", "online", "latency_ms", "packet_loss_pct",
			"packets_sent", "packets_recv", "discovered_at", "updated_at",
			"detail_fetched", "crawled")
	}

	// The shape the handlers are typed against.
	var typed struct {
		Nodes []*model.Node `json:"nodes"`
		Total int           `json:"total"`
	}
	_, _, body := h.get("/api/nodes")
	decodeInto(t, body, &typed)
	if typed.Total != len(typed.Nodes) || len(typed.Nodes) != 9 {
		t.Fatalf("typed decode mismatch: total=%d nodes=%d", typed.Total, len(typed.Nodes))
	}
	for _, n := range typed.Nodes {
		if n.BandLabel != model.BandLabels[n.Band] {
			t.Errorf("typed decode: node %s band_label=%q band=%q", n.Hostname, n.BandLabel, n.Band)
		}
	}
}

func TestNodesQueryFilter(t *testing.T) {
	h := contractHarness(t)

	cases := []struct {
		query string
		want  []string
		why   string
	}{
		{"q=homeserver", []string{"localnode"}, "display name, case-insensitively"},
		{"q=HOMESERVER", []string{"localnode"}, "the handler lowercases the needle"},
		{"q=zero-snr", []string{"zero-snr-rf"}, "hostname"},
		{"q=10.255.0.6", []string{"dtd-hub"}, "mesh address"},
		{"q=rooftop", []string{"localnode"}, "description"},
		{"q=xr5", []string{"localnode"}, "hardware"},
		{"q=ssh", []string{"localnode"}, "a published service string"},
		{"q=10.255.0.42", []string{"localnode"}, "the LAN address inside a service string"},
		{"q=grafana", nil, "an alias is NOT searched, only services"},
		{"q=nobody-here", nil, "no match"},
		{"q=", []string{""}, "empty needle means no filtering"},
	}
	for _, tc := range cases {
		obj := h.getObj("/api/nodes?" + tc.query)
		got := hostsOf(t, nodeArray(t, obj, "nodes body"))
		if tc.want == nil {
			if len(got) != 0 {
				t.Errorf("%s (%s): got %v, want nothing", tc.query, tc.why, got)
			}
			requireLiteral(t, obj, "nodes", tc.query, "[]")
			continue
		}
		if len(tc.want) == 1 && tc.want[0] == "" {
			if len(got) != 9 {
				t.Errorf("%s (%s): got %d nodes, want all 9", tc.query, tc.why, len(got))
			}
			continue
		}
		if !equalStrings(got, tc.want) {
			t.Errorf("%s (%s): got %v, want %v", tc.query, tc.why, got, tc.want)
		}
		if n := decodeInt(t, requireKey(t, obj, "total", tc.query), "total"); n != len(got) {
			t.Errorf("%s: total=%d but array holds %d", tc.query, n, len(got))
		}
	}
}

func TestNodesBandFilter(t *testing.T) {
	h := contractHarness(t)

	cases := []struct {
		query string
		want  []string
	}{
		{"band=5", []string{"localnode"}},
		{"band=2", []string{"zero-snr-rf"}},
		{"band=n", []string{"dtd-hub", "far-cn-peer"}},
		{"band=s", []string{"supernode-hq"}},
		{"band=w", []string{"wg-remote"}},
		{"band=u", []string{"odd-radio"}},
		{"band=h", []string{"never-probed-b"}},
		{"band=9", []string{"never-probed-a"}},
		{"band=all", nil},
		{"band=", nil},
		{"band=nope", []string{}},
		{"band=W", []string{}},
	}
	for _, tc := range cases {
		obj := h.getObj("/api/nodes?" + tc.query)
		got := hostsOf(t, nodeArray(t, obj, "nodes body"))
		switch {
		case tc.want == nil:
			if len(got) != 9 {
				t.Errorf("%s: got %d nodes, want the unfiltered 9: %v", tc.query, len(got), got)
			}
		case len(tc.want) == 0:
			if len(got) != 0 {
				t.Errorf("%s: got %v, want none", tc.query, got)
			}
		default:
			if !equalStrings(got, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.query, got, tc.want)
			}
		}
		// The band key a node is filtered by must be the key it reports.
		for _, n := range nodeArray(t, obj, "nodes body") {
			if b := decodeString(t, requireKey(t, n, "band", "node"), "band"); tc.query != "band=all" && tc.query != "band=" &&
				strings.HasPrefix(tc.query, "band=") && !strings.HasSuffix(tc.query, "nope") && !strings.HasSuffix(tc.query, "W") {
				if decodeString(t, requireKey(t, n, "band", "node"), "band") != strings.TrimPrefix(tc.query, "band=") {
					t.Errorf("%s: node reports band %q", tc.query, b)
				}
			}
		}
	}
}

func TestNodesStateFilters(t *testing.T) {
	h := contractHarness(t)

	wantOnline := []string{"localnode", "supernode-hq", "zero-snr-rf"}
	wantLocated := []string{"far-cn-peer", "localnode", "never-probed-b", "odd-radio", "zero-snr-rf"}

	online := hostsOf(t, nodeArray(t, h.getObj("/api/nodes?state=online"), "state=online"))
	if !equalStrings(online, wantOnline) {
		t.Errorf("state=online -> %v, want %v", online, wantOnline)
	}
	located := hostsOf(t, nodeArray(t, h.getObj("/api/nodes?state=located"), "state=located"))
	if !equalStrings(located, wantLocated) {
		t.Errorf("state=located -> %v, want %v", located, wantLocated)
	}
	supernode := hostsOf(t, nodeArray(t, h.getObj("/api/nodes?state=supernode"), "state=supernode"))
	if !equalStrings(supernode, []string{"supernode-hq"}) {
		t.Errorf("state=supernode -> %v", supernode)
	}

	// state=offline is NOT counts.offline: it is every node that is not online.
	all := hostsOf(t, nodeArray(t, h.getObj("/api/nodes"), "all nodes"))
	onlineSet := map[string]bool{}
	for _, id := range online {
		onlineSet[id] = true
	}
	var wantOffline []string
	for _, id := range all {
		if !onlineSet[id] {
			wantOffline = append(wantOffline, id)
		}
	}
	slices.Sort(wantOffline)
	gotOffline := hostsOf(t, nodeArray(t, h.getObj("/api/nodes?state=offline"), "state=offline"))
	if !equalStrings(gotOffline, wantOffline) {
		t.Errorf("state=offline -> %v, want every not-online node %v", gotOffline, wantOffline)
	}
	counts := rawObject(t, requireKey(t, h.getObj("/api/status"), "counts", "status body"))
	if n := decodeInt(t, requireKey(t, counts, "offline", "counts"), "counts.offline"); n != len(wantOffline) {
		t.Logf("documented split: state=offline returns %d nodes while counts.offline is %d; the extra %d are counts.unprobed=%d",
			len(wantOffline), n, len(wantOffline)-n, decodeInt(t, requireKey(t, counts, "unprobed", "counts"), "counts.unprobed"))
	}

	// An unrecognised state is ignored rather than rejected.
	if got := hostsOf(t, nodeArray(t, h.getObj("/api/nodes?state=unprobed"), "state=unprobed")); len(got) != 9 {
		t.Errorf("state=unprobed returned %d nodes; the switch has no such case, so it filters nothing", len(got))
	}
}

func TestLinksEndpointAndFilters(t *testing.T) {
	h := contractHarness(t)

	all := h.getObj("/api/links")
	links := linkArray(t, all, "links body")
	if len(links) != 6 {
		t.Fatalf("got %d links, want the six fixture edges", len(links))
	}
	if got := decodeInt(t, requireKey(t, all, "total", "links body"), "total"); got != len(links) {
		t.Errorf("total = %d, array holds %d", got, len(links))
	}
	// Ordered by from, to, mac, so a poll never reshuffles the graph.
	edges := make([]string, 0, len(links))
	for _, l := range links {
		edges = append(edges, decodeString(t, requireKey(t, l, "from", "link"), "from")+"->"+
			decodeString(t, requireKey(t, l, "to", "link"), "to"))
	}
	if !slices.IsSorted(edges) {
		t.Errorf("links are not in canonical order: %v", edges)
	}

	cases := []struct {
		query string
		want  []string
	}{
		{"type=RF", []string{"gone-node->localnode", "localnode->supernode-hq", "localnode->zero-snr-rf"}},
		{"type=DtD", []string{"zero-snr-rf->dtd-hub", "zero-snr-rf->far-cn-peer"}},
		{"type=Wireguard", []string{"localnode->wg-remote"}},
		{"type=rf", nil},
		{"type=Local", nil},
		{"type=all", []string{
			"gone-node->localnode", "localnode->supernode-hq", "localnode->wg-remote",
			"localnode->zero-snr-rf", "zero-snr-rf->dtd-hub", "zero-snr-rf->far-cn-peer"}},
		{"type=", []string{
			"gone-node->localnode", "localnode->supernode-hq", "localnode->wg-remote",
			"localnode->zero-snr-rf", "zero-snr-rf->dtd-hub", "zero-snr-rf->far-cn-peer"}},
		{"node=localnode", []string{
			"gone-node->localnode", "localnode->supernode-hq", "localnode->wg-remote", "localnode->zero-snr-rf"}},
		{"node=LocalNode.local.mesh", []string{
			"gone-node->localnode", "localnode->supernode-hq", "localnode->wg-remote", "localnode->zero-snr-rf"}},
		{"node=http://LOCALNODE.local.mesh/a/status", []string{
			"gone-node->localnode", "localnode->supernode-hq", "localnode->wg-remote", "localnode->zero-snr-rf"}},
		{"node=zero-snr-rf", []string{
			"localnode->zero-snr-rf", "zero-snr-rf->dtd-hub", "zero-snr-rf->far-cn-peer"}},
		{"node=gone-node", []string{"gone-node->localnode"}},
		{"node=zzz", nil},
		{"node=", []string{
			"gone-node->localnode", "localnode->supernode-hq", "localnode->wg-remote",
			"localnode->zero-snr-rf", "zero-snr-rf->dtd-hub", "zero-snr-rf->far-cn-peer"}},
		{"node=%20%20", []string{
			"gone-node->localnode", "localnode->supernode-hq", "localnode->wg-remote",
			"localnode->zero-snr-rf", "zero-snr-rf->dtd-hub", "zero-snr-rf->far-cn-peer"}},
		{"node=localnode&type=Wireguard", []string{"localnode->wg-remote"}},
	}
	for _, c := range cases {
		obj := h.getObj("/api/links?" + c.query)
		got := edgeList(t, obj)
		switch {
		case c.want == nil:
			if len(got) != 0 {
				t.Errorf("%s: got %v, want no edge", c.query, got)
			}
			requireLiteral(t, obj, "links", c.query, "[]")
		default:
			if !equalStrings(got, c.want) {
				t.Errorf("%s: got %v, want %v", c.query, got, c.want)
			}
		}
		if n := decodeInt(t, requireKey(t, obj, "total", c.query), "total"); n != len(got) {
			t.Errorf("%s: total=%d but the array holds %d", c.query, n, len(got))
		}
		// Every returned edge really satisfies the filters that were asked for.
		if raw, ok := queryValue(c.query, "type"); ok && raw != "all" && raw != "" {
			for _, l := range linkArray(t, obj, c.query) {
				if got := decodeString(t, requireKey(t, l, "type", "link"), "type"); got != raw {
					t.Errorf("%s: edge %s carries type %q", c.query, edgeOf(t, l), got)
				}
			}
		}
		if raw, ok := queryValue(c.query, "node"); ok {
			want := scanner.NormalizeHost(raw)
			if want != "" {
				for _, l := range linkArray(t, obj, c.query) {
					from := decodeString(t, requireKey(t, l, "from", "link"), "from")
					to := decodeString(t, requireKey(t, l, "to", "link"), "to")
					if from != want && to != want {
						t.Errorf("%s: edge %s->%s does not touch %q", c.query, from, to, want)
					}
				}
			}
		}
	}
}

// queryValue pulls one parameter out of a raw query string.
func queryValue(rawQuery, key string) (string, bool) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", false
	}
	v, ok := values[key]
	if !ok || len(v) == 0 {
		return "", false
	}
	return v[0], true
}

// edgeOf renders one link member pair for a failure message.
func edgeOf(t testing.TB, l map[string]json.RawMessage) string {
	t.Helper()
	return decodeString(t, requireKey(t, l, "from", "link"), "from") + "->" +
		decodeString(t, requireKey(t, l, "to", "link"), "to")
}

// edgeList reads "from->to" out of a links payload.
func edgeList(t testing.TB, obj map[string]json.RawMessage) []string {
	t.Helper()
	out := make([]string, 0)
	for _, l := range linkArray(t, obj, "links body") {
		out = append(out, decodeString(t, requireKey(t, l, "from", "link"), "from")+"->"+
			decodeString(t, requireKey(t, l, "to", "link"), "to"))
	}
	return out
}

func hostsOf(t testing.TB, objs []map[string]json.RawMessage) []string {
	t.Helper()
	out := make([]string, 0, len(objs))
	for _, n := range objs {
		out = append(out, decodeString(t, requireKey(t, n, "hostname", "node"), "hostname"))
	}
	slices.Sort(out)
	return out
}

func TestLookupEndpoint(t *testing.T) {
	h := contractHarness(t)

	// A missing or blank address is a client error, not an empty answer.
	for _, path := range []string{"/api/lookup", "/api/lookup?ip=", "/api/lookup?ip=%20%20", "/api/lookup?other=10.255.0.1"} {
		code, header, raw := h.getWithOrigin(path, "http://localhost:3000")
		if code != http.StatusBadRequest {
			t.Errorf("%s -> %d, want 400: %s", path, code, raw)
			continue
		}
		obj := h.jsonBody(code, header, raw)
		requireKeys(t, obj, path+" 400 body", "error")
		if got := header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s: a 400 needs a CORS header too, got %q", path, got)
		}
	}

	// The mesh address of a node.
	mesh := h.getObj("/api/lookup?ip=10.255.0.1")
	requireLiteral(t, mesh, "query", "lookup body", `"10.255.0.1"`)
	hits := rawArray(t, requireKey(t, mesh, "results", "lookup body"))
	if len(hits) != 1 {
		t.Fatalf("lookup of a node's own mesh address returned %d hits, want 1: %s", len(hits), mustRaw(t, mesh))
	}
	hit := rawObject(t, hits[0])
	requireLiteral(t, hit, "match", "mesh hit", `"mesh_ip"`)
	got := indexByID(t, []map[string]json.RawMessage{rawObject(t, requireKey(t, hit, "node", "mesh hit"))}, "hostname", "mesh hit node")
	if _, ok := got["localnode"]; !ok {
		t.Errorf("mesh address 10.255.0.1 resolved to %v", slices.Sorted(maps.Keys(got)))
	}
	finding(t, "GET /api/lookup (api.go:178-181) stops at the mesh_ip hit for a node: localnode also "+
		"publishes dns|10.255.0.1 on the same address, and the `continue` means the answer reports one way "+
		"to reach it instead of both")

	// Aliases and services are stored as "name|lanIP", so a LAN address matches by suffix.
	lan := h.getObj("/api/lookup?ip=" + lanServiceIP)
	lanHits := rawArray(t, requireKey(t, lan, "results", "lookup body"))
	if len(lanHits) != 3 {
		t.Fatalf("lookup of LAN address %s returned %d hits, want 3: %s", lanServiceIP, len(lanHits), mustRaw(t, lan))
	}
	var triples []string
	names := make([]string, 0, len(lanHits))
	for _, raw := range lanHits {
		o := rawObject(t, raw)
		kind := decodeString(t, requireKey(t, o, "match", "lan hit"), "match")
		node := rawObject(t, requireKey(t, o, "node", "lan hit"))
		name := decodeString(t, requireKey(t, node, "name", "lan hit node"), "name")
		names = append(names, name)
		label := ""
		switch kind {
		case "alias":
			label = decodeString(t, requireKey(t, o, "alias", "lan hit"), "alias")
		case "service":
			label = decodeString(t, requireKey(t, o, "service", "lan hit"), "service")
		case "mesh_ip":
			t.Errorf("lookup of a LAN address reported a mesh_ip hit: %s", mustRaw(t, o))
		}
		triples = append(triples, name+"|"+kind+"|"+label)
	}
	want := []string{
		"TST01 HomeServer|alias|grafana",
		"TST01 HomeServer|service|ssh",
		"Zero SNR RF|alias|zerorfrouter",
	}
	slices.Sort(triples)
	if !equalStrings(triples, want) {
		t.Errorf("LAN lookup hits = %v, want %v", triples, want)
	}
	// Ordered by node name, so the drawer groups the answers predictably.
	if !slices.IsSorted(names) {
		t.Errorf("lookup results are not ordered by node name: %v", names)
	}
	// The "|lanIP" suffix must not match a longer address that ends the same way.
	if partial := rawArray(t, requireKey(t, h.getObj("/api/lookup?ip=9.42"), "results", "partial lookup")); len(partial) != 0 {
		t.Errorf("a partial address matched %s", partial)
	}

	// Surrounding space is trimmed before the comparison.
	trimmed := rawArray(t, requireKey(t, h.getObj("/api/lookup?ip=%20"+localMeshIP+"%20"), "results", "trimmed lookup"))
	if len(trimmed) != 1 {
		t.Errorf("a padded address returned %d hits, want the one mesh_ip hit", len(trimmed))
	}

	// Nothing known: an empty array, not null.
	none := h.getObj("/api/lookup?ip=10.255.255.255")
	requireLiteral(t, none, "results", "unknown address", "[]")

	// Hostnames and bare aliases are out of scope for this endpoint.
	for _, q := range []string{"localnode", "TST01 HomeServer", "rf-hub"} {
		res := rawArray(t, requireKey(t, h.getObj("/api/lookup?ip="+url.QueryEscape(q)), "results", "name lookup"))
		if len(res) != 0 {
			t.Errorf("lookup of %q matched %s; the endpoint only answers addresses", q, res)
		}
	}
	finding(t, "GET /api/lookup cannot resolve a name at all, and a sweep's canonical re-key stores the seed "+
		"as a bare alias (scan.go:270), so an alias without the \"name|lanIP\" form - e.g. the localname "+
		"\"rf-hub\" the fixture carries - is unreachable through the API")
}

func TestConfigEndpoint(t *testing.T) {
	h := contractHarness(t)
	cfg := h.getObj("/api/config")

	requireKeys(t, cfg, "config body",
		"amap_key", "amap_security_code",
		"center_lat", "center_lon", "zoom",
		"cron", "seeds", "max_hops", "icmp_mode",
		"band_labels", "band_colors", "version")

	// Status arrives over /api/ws, so no polling cadence key is served.
	for _, gone := range []string{"poll_interval", "poll_seconds"} {
		if _, ok := cfg[gone]; ok {
			t.Errorf("config body still carries %q after status moved to the socket", gone)
		}
	}
	if got := decodeInt(t, cfg["max_hops"], "max_hops"); got != 1 {
		t.Errorf("max_hops = %d, want the configured hop limit", got)
	}
	if got := decodeString(t, cfg["version"], "version"); got != fixedVersion {
		t.Errorf("version = %q", got)
	}
	labels := rawObject(t, requireKey(t, cfg, "band_labels", "config body"))
	if len(labels) != len(model.BandLabels) {
		t.Errorf("band_labels has %d entries, model.BandLabels has %d", len(labels), len(model.BandLabels))
	}
	for key, want := range model.BandLabels {
		if got := decodeString(t, requireKey(t, labels, key, "band_labels"), key); got != want {
			t.Errorf("band_labels[%q] = %q, want %q", key, got, want)
		}
	}
	colors := rawObject(t, requireKey(t, cfg, "band_colors", "config body"))
	for key, want := range model.BandColors {
		if got := decodeString(t, requireKey(t, colors, key, "band_colors"), key); got != want {
			t.Errorf("band_colors[%q] = %q, want %q", key, got, want)
		}
	}
	finding(t, "GET /api/config (api.go:212-232) serialises the configured basemap credentials verbatim "+
		"on an endpoint the CORS middleware opens to every origin — including amap_security_code, which the "+
		"JS API demands in the page. Frontend code has no StatusPayload field for any of them, so they are "+
		"only ever read by useBackendConfig.ts")
}

func TestScanEnableValidatesItsBody(t *testing.T) {
	h := contractHarness(t)

	// A browser sends Origin only cross-origin, so the POST carries one to reach the CORS middleware.
	for _, body := range []string{`{"enabled":true}`, `{"enabled":false}`} {
		code, header, raw := h.do(http.MethodPost, "/api/scan/enable",
			strings.NewReader(body), map[string]string{
				"Content-Type": "application/json",
				"Origin":       "http://localhost:3210",
			})
		if code != http.StatusOK {
			t.Fatalf("enable %s -> %d: %s", body, code, raw)
		}
		obj := h.jsonBody(code, header, raw)
		want := "true"
		if strings.Contains(body, "false") {
			want = "false"
		}
		requireLiteral(t, obj, "scanning_enabled", "enable response", want)
		if got := h.eng.Enabled(); got != (want == "true") {
			t.Errorf("engine flag is %v after enabling with %s", got, body)
		}
		if got := header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3210" && got != "*" {
			t.Errorf("cross-origin POST returned %q, want the echoed origin or *", got)
		}
	}

	// Leave the flag on a known value, then check /api/status agrees with it.
	h.post("/api/scan/enable", `{"enabled":true}`)
	if !h.eng.Enabled() {
		t.Fatal("setup: engine flag should be true before the status check")
	}
	if got := decodeBool(t, requireKey(t, h.getObj("/api/status"), "scanning_enabled", "status body"), "scanning_enabled"); !got {
		t.Error("status disagrees with the enabled flag the last accepted call set")
	}

	bad := []string{`{}`, ``, `   `, `{"other":true}`, `{"enabled":null}`, `{"enabled":"yes"}`,
		`{"enabled":1}`, `not json`, `{`, `[]`, `null`, `"true"`}
	for _, body := range bad {
		code, header, raw := h.post("/api/scan/enable", body)
		if code != http.StatusBadRequest {
			t.Errorf("enable %q -> %d, want 400: %s", body, code, raw)
			continue
		}
		obj := h.jsonBody(code, header, raw)
		requireKeys(t, obj, "enable 400 body", "error")
		// The accepted call left the flag true; a rejected body must not move it.
		if h.eng.Enabled() != true {
			t.Errorf("a rejected body %q must not move the flag", body)
		}
	}

	// Wrong method: fiber's own router answers, not the handler.
	code, header, raw := h.do(http.MethodGet, "/api/scan/enable", nil, nil)
	if code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/scan/enable -> %d: %s", code, raw)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Logf("documented: the %d answer for a wrong method is %q, not JSON (%s)", code, ct, raw)
	}
}

func TestResponsesUseTheConfiguredEncoderAndAllowOtherOrigins(t *testing.T) {
	h := contractHarness(t)

	// The app is wired to go.gh.ink/json, the encoder the running binary uses.
	encoderIdentity(t, h.app)

	// Byte for byte: the body is what that encoder writes for the same value.
	_, _, body := h.get("/api/links")
	links := h.st.Links()
	want, err := gojson.Marshal(fiber.Map{"links": links, "total": len(links)})
	if err != nil {
		t.Fatalf("encode expectation: %v", err)
	}
	if string(body) != string(want) {
		t.Errorf("/api/links is not the configured encoder's output for its payload:\ngot  %s\nwant %s", body, want)
	}

	for _, path := range []string{
		"/api/health", "/api/status", "/api/nodes", "/api/links", "/api/topology",
		"/api/config", "/api/lookup?ip=10.255.0.1",
	} {
		code, header, raw := h.getWithOrigin(path, "http://localhost:3000")
		if code != http.StatusOK {
			t.Fatalf("%s -> %d", path, code)
		}
		if ct := header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("%s content type = %q", path, ct)
		}
		if got := header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s sent no usable CORS origin to a different host: %q", path, got)
		}
		if !json.Valid(raw) {
			t.Errorf("%s body is not valid JSON: %s", path, raw)
		}
	}

	// The Nuxt page has to be able to POST, so the preflight has to answer.
	code, header, raw := h.do(http.MethodOptions, "/api/scan/enable", nil, map[string]string{
		"Origin":                        "http://localhost:3000",
		"Access-Control-Request-Method": "POST",
	})
	if code != http.StatusNoContent {
		t.Errorf("preflight -> %d: %s", code, raw)
	}
	if got := header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("preflight CORS origin = %q", got)
	}
	if got := header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, http.MethodPost) {
		t.Errorf("preflight allows %q, which omits POST", got)
	}

	// Even an error answer is readable cross-origin, or the UI just shows "failed".
	code, header, raw = h.do(http.MethodPost, "/api/scan/enable", strings.NewReader("{}"), map[string]string{
		"Origin":       "http://localhost:3000",
		"Content-Type": "application/json",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad enable body -> %d: %s", code, raw)
	}
	if got := header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("a 400 answer carries no CORS origin: %q", got)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("a 400 answer is %q, want JSON so the UI can show the reason", ct)
	}
}

func TestTopologyEndpoint(t *testing.T) {
	h := contractHarness(t)
	topo := h.getObj("/api/topology")

	requireKeys(t, topo, "topology body", "nodes", "links", "generated_at", "unconnected_nodes")
	if got := decodeString(t, topo["generated_at"], "generated_at"); got == "" {
		t.Error("generated_at is empty")
	} else if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("generated_at %q is not RFC3339: %v", got, err)
	}

	nodes := nodeArray(t, topo, "topology body")
	links := linkArray(t, topo, "topology body")

	// The payload is a graph, not a dump: one edge per physical pair, no vertex unseen on either end.
	// Both directions of a wire are stored separately because each side reports its own SNR/ETX.
	pairOf := func(a, b string) string {
		if b < a {
			a, b = b, a
		}
		return a + "\x00" + b
	}
	present := map[string]bool{}
	for _, n := range h.st.Nodes() {
		present[n.Hostname] = true
	}
	connected := map[string]bool{}
	pairs := map[string]bool{}
	for _, l := range h.st.Links() {
		// Dangling edges are pruned before anything is counted.
		if !present[l.From] || !present[l.To] {
			continue
		}
		pairs[pairOf(l.From, l.To)+"\x00"+l.Type] = true
		connected[l.From] = true
		connected[l.To] = true
	}
	if len(links) != len(pairs) {
		t.Errorf("topology has %d edges, want the %d physical pairs the store holds", len(links), len(pairs))
	}
	if len(nodes) != len(connected) {
		t.Errorf("topology has %d vertices, want the %d nodes an edge touches", len(nodes), len(connected))
	}
	if want := h.st.NodeCount() - len(connected); decodeInt(t, requireKey(t, topo, "unconnected_nodes", "topology body"), "unconnected_nodes") != want {
		t.Errorf("unconnected_nodes = %d, want %d directory-only nodes", want, want)
	}

	// Dangling edges are pruned: the renderer must never be handed an unknown id.
	ids := map[string]bool{}
	for _, n := range nodes {
		ids[decodeString(t, requireKey(t, n, "id", "topology node"), "id")] = true
	}
	if ids["gone-node"] {
		t.Error("a node that is not in the store got a vertex")
	}
	for _, l := range links {
		for _, key := range []string{"from", "to"} {
			end := decodeString(t, requireKey(t, l, key, "topology link"), key)
			if !ids[end] {
				t.Errorf("edge %s->%s references %q, which has no vertex", l["from"], l["to"], end)
			}
		}
	}
	// /api/links is the raw adjacency table, so an edge to an unregistered peer stays visible.
	// Only the graph endpoint must be a closed structure, and Topology() prunes the dangling ends.
	dangling := 0
	for _, l := range allLinkEdges(t, h) {
		if l == "gone-node->localnode" {
			dangling++
		}
	}
	if dangling != 1 {
		t.Errorf("raw /api/links dropped the unregistered peer (saw %d), want it retained", dangling)
	}

	byID := indexByID(t, nodes, "id", "topology body")
	requireKeys(t, byID["localnode"], "topology node localnode",
		"id", "name", "band", "band_label", "color", "is_supernode", "hops", "etx",
		"online", "latency_ms", "neighbor_cnt")

	// neighbor_cnt is the incident degree of the surviving edge set, not the node's reported roster.
	// The graph sizes vertices by it, so a seen-only-as-neighbour node must not collapse to a leaf.
	degree := map[string]int{}
	for _, l := range links {
		f := decodeString(t, requireKey(t, l, "from", "link"), "from")
		to := decodeString(t, requireKey(t, l, "to", "link"), "to")
		degree[f]++
		if to != f {
			degree[to]++
		}
	}
	for id, n := range byID {
		if got := decodeInt(t, requireKey(t, n, "neighbor_cnt", "topology "+id), "neighbor_cnt"); got != degree[id] {
			t.Errorf("%s: neighbor_cnt = %d, want degree %d", id, got, degree[id])
		}
	}
	// Spot-checks so a bug that makes degree and roster agree cannot pass silently.
	for id, want := range map[string]int{"localnode": 3, "wg-remote": 1, "far-cn-peer": 1, "dtd-hub": 1} {
		if got := decodeInt(t, requireKey(t, byID[id], "neighbor_cnt", "topology "+id), "neighbor_cnt"); got != want {
			t.Errorf("%s: neighbor_cnt = %d, want %d", id, got, want)
		}
	}

	// Both coordinates always serialise; 0,0 is honest and types.ts declares them required.
	if v, ok := byID["supernode-hq"]["lat"]; !ok || string(v) != "0" {
		t.Errorf("unlocated vertex lat = %s (present=%v), want an explicit 0", v, ok)
	}
	if _, ok := byID["localnode"]["lat"]; !ok {
		t.Error("a located vertex dropped lat")
	}
}

// allLinkEdges lists every edge /api/links reports, independent of topology.
func allLinkEdges(t testing.TB, h *harness) []string {
	t.Helper()
	out := make([]string, 0)
	for _, l := range linkArray(t, h.getObj("/api/links"), "links body") {
		out = append(out, decodeString(t, requireKey(t, l, "from", "link"), "from")+"->"+
			decodeString(t, requireKey(t, l, "to", "link"), "to"))
	}
	return out
}
