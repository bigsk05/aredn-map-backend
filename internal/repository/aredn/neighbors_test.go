package aredn

import (
	"sync/atomic"
	"testing"
	"time"

	"aredn-map/internal/model"
)

// wantInt / wantFloat / wantNil* assert on the nullable radio metrics.
// Absent and zero are different facts: a WireGuard link has no SNR, an RF link can read 0 dB.
func wantInt(t *testing.T, p *int, what string, exp int) {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is nil, want %d", what, exp)
	}
	if *p != exp {
		t.Fatalf("%s = %d, want %d", what, *p, exp)
	}
}

func wantNilInt(t *testing.T, p *int, what string) {
	t.Helper()
	if p != nil {
		t.Fatalf("%s = %d, want nil (metric does not exist for this link kind)", what, *p)
	}
}

func wantFloat(t *testing.T, p *float64, what string, exp float64) {
	t.Helper()
	if p == nil {
		t.Fatalf("%s is nil, want %v", what, exp)
	}
	if !almostEqual(*p, exp) {
		t.Fatalf("%s = %v, want %v", what, *p, exp)
	}
}

func wantNilFloat(t *testing.T, p *float64, what string) {
	t.Helper()
	if p != nil {
		t.Fatalf("%s = %v, want nil", what, *p)
	}
}

// show* render a nullable metric for logging; %v on a pointer would print an address.
func showInt(p *int) any {
	if p == nil {
		return "nil"
	}
	return *p
}

func showFloat(p *float64) any {
	if p == nil {
		return "nil"
	}
	return *p
}

// Walks every recorded adjacency fragment and asserts rows, sections and cells.
func TestParseNeighborFragment(t *testing.T) {
	cases := []struct {
		file      string
		rows      int
		container string
		to        string
		mac       string
		status    string
		stats     []string
	}{
		{
			file:      "neighbors_tst16.html",
			rows:      1,
			container: model.ContainerNeighborhood,
			to:        "tst09-ptp",
			mac:       "02:00:00:00:00:03",
			status:    "okay",
			stats:     []string{"100%", "100%", "23", "23", "40%", "34.0", "3.4"},
		},
		{
			file:      "neighbors_localnode.html",
			rows:      1,
			container: model.ContainerNeighborhood,
			to:        "tst05-tunnelserver",
			mac:       "02:00:00:00:00:04",
			status:    "excellent",
			stats:     []string{"100%", "12", "-", "-", "0%", "-", "58.9"},
		},
		{
			file:      "neighbors_tst12-hap.html",
			rows:      2,
			container: model.ContainerLocal,
			to:        "tst10",
			mac:       "02:00:00:00:00:05",
			status:    "excellent",
			stats:     []string{"100%", "", "", "", "0%", "", ""},
		},
		{
			file:      "neighbors_tst04-supernode.html",
			rows:      4,
			container: model.ContainerNeighborhood,
			to:        "tst03-supernode",
			mac:       "02:00:00:00:00:06",
			status:    "excellent",
			stats:     []string{"100%", "143", "-", "-", "0%", "-", "1160.4"},
		},
	}

	for _, tc := range cases {
		rows := parseNeighborFragment(fixtureString(t, tc.file))
		if len(rows) != tc.rows {
			t.Errorf("%s: %d rows, want %d (%+v)", tc.file, len(rows), tc.rows, rows)
			continue
		}
		first := rows[0]
		if first.container != tc.container {
			t.Errorf("%s: container = %q, want %q", tc.file, first.container, tc.container)
		}
		if first.to != tc.to {
			t.Errorf("%s: to = %q, want %q", tc.file, first.to, tc.to)
		}
		if first.mac != tc.mac {
			t.Errorf("%s: mac = %q, want %q", tc.file, first.mac, tc.mac)
		}
		if first.status != tc.status {
			t.Errorf("%s: status = %q, want %q", tc.file, first.status, tc.status)
		}
		if len(first.stats) != len(tc.stats) {
			t.Errorf("%s: %d stat cells %v, want %d %v", tc.file, len(first.stats), first.stats, len(tc.stats), tc.stats)
		} else {
			for i, v := range first.stats {
				if v != tc.stats[i] {
					t.Errorf("%s: cell %d = %q, want %q", tc.file, i, v, tc.stats[i])
				}
			}
		}
	}
}

// The leading heading divs and the header row are never turned into data rows.
func TestParseNeighborFragmentIgnoresHeaders(t *testing.T) {
	rows := parseNeighborFragment(fixtureString(t, "neighbors_tst16.html"))
	for _, r := range rows {
		if r.mac == "" || r.to == "" {
			t.Fatalf("header row leaked into the data: %+v", r)
		}
	}
	// The "None" Local Nodes section contributes nothing.
	for _, r := range rows {
		if r.container == model.ContainerLocal {
			t.Errorf("empty Local Nodes section produced a row: %+v", r)
		}
	}
}

// Cell 1 has two meanings; "-" and empty cells must not become zeroes.
func TestApplyFragmentStats(t *testing.T) {
	var rf model.Link
	applyFragmentStats(&rf, []string{"100%", "100%", "23", "23", "40%", "34.0", "3.4"})
	if rf.RxSuccessPct != 100 || rf.TxSuccessPct != 100 {
		t.Errorf("RF lq/nlq = %v/%v, want 100/100", rf.RxSuccessPct, rf.TxSuccessPct)
	}
	if rf.RTTMs != 0 {
		t.Errorf("RF nlq cell became a round trip time: %v", rf.RTTMs)
	}
	wantInt(t, rf.LocalSNR, "RF local_snr", 23)
	wantInt(t, rf.NeighborSNR, "RF neighbor_snr", 23)
	if rf.ErrorsPct != 40 || rf.DistanceMI != 3.4 {
		t.Errorf("RF cells = %+v", rf)
	}

	var wg model.Link
	applyFragmentStats(&wg, []string{"100%", "12", "-", "-", "0%", "-", "58.9"})
	if wg.RTTMs != 12 {
		t.Errorf("WireGuard rtt = %v, want 12", wg.RTTMs)
	}
	if wg.TxSuccessPct != 0 {
		t.Errorf("placeholder dashes became a tx value: %+v", wg)
	}
	wantNilInt(t, wg.LocalSNR, "WireGuard local_snr")
	wantNilInt(t, wg.NeighborSNR, "WireGuard neighbor_snr")
	if !almostEqual(wg.DistanceMI, 58.9) || wg.ErrorsPct != 0 {
		t.Errorf("WireGuard cells = %+v", wg)
	}

	// A short row must not panic and must leave the tail absent.
	var short model.Link
	applyFragmentStats(&short, []string{"100%"})
	if short.RxSuccessPct != 100 || short.DistanceMI != 0 {
		t.Errorf("short row = %+v", short)
	}
}

// The three link kinds the dialog reports.
func TestParseDeviceDialog(t *testing.T) {
	rf := parseDeviceDialog(fixtureString(t, "device_rf_tst09-ptp.html"))
	for k, want := range map[string]string{
		"type":                "RF",
		"mac address":         "02:00:00:00:00:03",
		"ip address":          "10.100.6.67",
		"model":               "Ubiquiti PowerBeam M5 300",
		"firmware":            "3.25.8.0",
		"latitude":            "59.888074",
		"longitude":           "-2.455294",
		"distance":            "3.4 miles",
		"lq | rx success":     "100%",
		"nlq | tx success":    "100%",
		"etx":                 "1.0",
		"local snr":           "23",
		"neighbor snr":        "23",
		"physical rx bitrate": "27.7 Mbps",
		"physical tx bitrate": "37.6 Mbps",
		"tx retransmissions":  "79.1%",
		"babel routes":        "166",
		"babel metric":        "257",
		"olsr routes":         "13",
		"last seen":           "now",
		"state":               "routing",
		kindKey:               "neighborhood node",
		peerHrefKey:           "http://tst09-ptp.local.mesh",
	} {
		if got := rf[k]; got != want {
			t.Errorf("RF dialog %q = %q, want %q", k, got, want)
		}
	}

	wg := parseDeviceDialog(fixtureString(t, "device_wireguard_tst05-tunnelserver.html"))
	for k, want := range map[string]string{
		"type":            "Wireguard",
		"ip address":      "172.31.159.46",
		"link address":    "fe80::200:acff:fe1f:9f2e",
		"rx success":      "100%",
		"tx success":      "100%",
		"rx cost":         "206",
		"tx cost":         "206",
		"metric":          "209",
		"routes":          "9",
		"avg tx":          "11.5 pkt/sec",
		"round trip time": "12.0 ms",
		"distance":        "58.9 miles",
		kindKey:           "neighborhood node",
	} {
		if got := wg[k]; got != want {
			t.Errorf("WireGuard dialog %q = %q, want %q", k, got, want)
		}
	}
	// The ip cell is an anchor, the link-address cell a styled div; both flatten to text.
	if wg["model"] != "QEMU Standard PC (i440FX + PIIX, 1996)" {
		t.Errorf("model = %q", wg["model"])
	}

	dtd := parseDeviceDialog(fixtureString(t, "device_dtd_tst10.html"))
	if dtd["type"] != "DtD" || dtd[kindKey] != "local node" {
		t.Errorf("DtD dialog = %q/%q", dtd["type"], dtd[kindKey])
	}
	if dtd["metric"] != "96" || dtd["routes"] != "5" {
		t.Errorf("DtD routing cells = %q/%q", dtd["metric"], dtd["routes"])
	}
	if dtd["avg tx"] != "-" {
		t.Errorf("DtD avg tx = %q, want the placeholder", dtd["avg tx"])
	}
	if dtd["link uptime"] != "47 hours" {
		t.Errorf("link uptime = %q", dtd["link uptime"])
	}
}

// The offline twin of the live acceptance check for tst16.
func TestFetchNeighborsWithDetail(t *testing.T) {
	c, rec := newFixtureServer(t, downFixture())

	links, err := c.FetchNeighbors(t.Context(), "tst16", true, 4)
	if err != nil {
		t.Fatalf("FetchNeighbors: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("%d links, want 1: %+v", len(links), links)
	}
	l := links[0]
	if l.From != "tst16" || l.To != "tst09-ptp" {
		t.Errorf("edge = %q -> %q", l.From, l.To)
	}
	if l.Type != model.LinkTypeRF {
		t.Errorf("Type = %q, want %q", l.Type, model.LinkTypeRF)
	}
	if l.Container != model.ContainerNeighborhood {
		t.Errorf("Container = %q", l.Container)
	}
	if l.Status != "okay" {
		t.Errorf("Status = %q, want okay", l.Status)
	}
	if l.MAC != "02:00:00:00:00:03" || l.IP != "10.100.6.67" {
		t.Errorf("MAC/IP = %q/%q", l.MAC, l.IP)
	}
	if !almostEqual(l.DistanceMI, 3.4) {
		t.Errorf("DistanceMI = %v, want 3.4", l.DistanceMI)
	}
	wantInt(t, l.LocalSNR, "dialog local_snr", 23)
	wantInt(t, l.NeighborSNR, "dialog neighbor_snr", 23)
	if l.RxSuccessPct != 100 || l.TxSuccessPct != 100 {
		t.Errorf("lq/nlq = %v/%v", l.RxSuccessPct, l.TxSuccessPct)
	}
	wantFloat(t, l.ETX, "dialog etx", 1.0)
	if !almostEqual(l.PingMs, 3.9) || !almostEqual(l.NeighborPingMs, 3.0) {
		t.Errorf("ping = %v/%v", l.PingMs, l.NeighborPingMs)
	}
	wantFloat(t, l.AvgTxPps, "dialog avg_tx_pps", 2.0)
	if l.ErrorsPct != 43 {
		t.Errorf("errors = %v, want 43", l.ErrorsPct)
	}
	wantFloat(t, l.BitrateRxMbps, "bitrate_rx_mbps", 27.7)
	wantFloat(t, l.BitrateTxMbps, "bitrate_tx_mbps", 37.6)
	wantFloat(t, l.RetransmitPct, "retransmit_pct", 79.1)
	if l.Metric != 257 {
		t.Errorf("Metric = %d, want 257 (babel metric)", l.Metric)
	}
	// Peer identity from the dialog, so an unexpanded neighbour can still be plotted.
	if l.PeerName != "tst09-ptp" {
		t.Errorf("PeerName = %q", l.PeerName)
	}
	if l.PeerHardware != "Ubiquiti PowerBeam M5 300" || l.PeerFirmware != "3.25.8.0" {
		t.Errorf("peer hardware/firmware = %q / %q", l.PeerHardware, l.PeerFirmware)
	}
	if !almostEqual(l.PeerLat, 59.888074) || !almostEqual(l.PeerLon, -2.455294) {
		t.Errorf("peer position = %v/%v", l.PeerLat, l.PeerLon)
	}
	if l.LastSeen.IsZero() {
		t.Error("last seen not parsed")
	}
	if got := rec.count("tst16.local.mesh" + devicePath + "?m=02:00:00:00:00:03"); got != 1 {
		t.Errorf("dialog fetched %d times, want 1", got)
	}
}

// The icon-derived hints still classify the obvious cases when no dialog is crawled.
func TestFetchNeighborsWithoutDetail(t *testing.T) {
	c, rec := newFixtureServer(t, downFixture())
	links, err := c.FetchNeighbors(t.Context(), "tst16", false, 4)
	if err != nil {
		t.Fatalf("FetchNeighbors: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("%d links", len(links))
	}
	if links[0].Type != model.LinkTypeRF {
		t.Errorf("Type = %q, want the wifi icon hint %q", links[0].Type, model.LinkTypeRF)
	}
	if !almostEqual(links[0].DistanceMI, 3.4) {
		t.Errorf("fragment cells not applied: %+v", links[0])
	}
	wantInt(t, links[0].LocalSNR, "fragment local_snr", 23)
	if rec.total() != 1 {
		t.Errorf("made %d requests without detail, want 1", rec.total())
	}
}

// The tunnel peer of the access point, reached through the localnode 307 redirect.
func TestFetchNeighborsWireGuard(t *testing.T) {
	c, _ := newFixtureServer(t, homeNodeFixture())
	links, err := c.FetchNeighbors(t.Context(), "localnode", true, 4)
	if err != nil {
		t.Fatalf("FetchNeighbors: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("%d links, want 1", len(links))
	}
	l := links[0]
	if l.From != "localnode" {
		t.Errorf("From = %q, want the crawled hostname", l.From)
	}
	if l.To != "tst05-tunnelserver" {
		t.Errorf("To = %q", l.To)
	}
	if l.Type != model.LinkTypeWireguard {
		t.Errorf("Type = %q, want %q", l.Type, model.LinkTypeWireguard)
	}
	if l.Container != model.ContainerNeighborhood {
		t.Errorf("Container = %q", l.Container)
	}
	if l.IP != "172.31.159.46" {
		t.Errorf("IP = %q", l.IP)
	}
	if !almostEqual(l.DistanceMI, 58.9) || l.RxSuccessPct != 100 || l.TxSuccessPct != 100 {
		t.Errorf("tunnel cells = %+v", l)
	}
	wantNilInt(t, l.LocalSNR, "WireGuard local_snr")
	wantNilInt(t, l.NeighborSNR, "WireGuard neighbor_snr")
	if l.Metric != 209 {
		t.Errorf("Metric = %d, want 209", l.Metric)
	}
	if l.PeerHardware != "QEMU Standard PC (i440FX + PIIX, 1996)" || l.PeerFirmware != "4.26.7.0" {
		t.Errorf("peer hardware/firmware = %q / %q", l.PeerHardware, l.PeerFirmware)
	}
	if !almostEqual(l.PeerLat, 24.7457) || !almostEqual(l.PeerLon, 104.0524) {
		t.Errorf("peer position = %v/%v", l.PeerLat, l.PeerLon)
	}
	if l.RTTMs != 12 {
		t.Errorf("RTTMs = %v, want the fragment/dialog round trip", l.RTTMs)
	}
}

// A populated Local Nodes section: the LAN-side peer must appear with ContainerLocal.
func TestFetchNeighborsLocalSection(t *testing.T) {
	c, _ := newFixtureServer(t, fixture{
		routes: map[string]string{
			"tst12-hap.local.mesh" + neighborsPath:                       "neighbors_tst12-hap.html",
			"tst12-hap.local.mesh" + devicePath + "?m=02:00:00:00:00:05": "device_dtd_tst10.html",
			"tst12-hap.local.mesh" + devicePath + "?m=02:00:00:00:00:07": "device_wireguard_tst05-tunnelserver.html",
		},
	})
	links, err := c.FetchNeighbors(t.Context(), "tst12-hap", true, 4)
	if err != nil {
		t.Fatalf("FetchNeighbors: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("%d links, want 2 (local + neighborhood): %+v", len(links), links)
	}
	local, neigh := links[0], links[1]
	if local.Container != model.ContainerLocal || local.To != "tst10" {
		t.Errorf("local row = %+v", local)
	}
	if local.Type != model.LinkTypeDtD {
		t.Errorf("local Type = %q, want %q", local.Type, model.LinkTypeDtD)
	}
	if local.IP != "10.100.5.115" || local.Metric != 96 {
		t.Errorf("local DtD detail = %q/%d", local.IP, local.Metric)
	}
	if local.DistanceMI != 0 {
		t.Errorf("DtD distance = %v, want 0", local.DistanceMI)
	}
	// A DtD row's empty cells must not become zeroes over the dialog's values.
	if local.RxSuccessPct != 100 {
		t.Errorf("local rx success = %v, want 100", local.RxSuccessPct)
	}
	if neigh.Container != model.ContainerNeighborhood || neigh.Type != model.LinkTypeWireguard {
		t.Errorf("neighborhood row = %+v", neigh)
	}
}

// The worker ceiling limits in-flight dialogs and every row still gets its detail.
func TestFetchNeighborsConcurrencyBound(t *testing.T) {
	c, rec := newFixtureServer(t, fixture{routes: map[string]string{
		"tst04-supernode.local.mesh" + neighborsPath:                       "neighbors_tst04-supernode.html",
		"tst04-supernode.local.mesh" + devicePath + "?m=02:00:00:00:00:06": "device_wireguard_tst05-tunnelserver.html",
		"tst04-supernode.local.mesh" + devicePath + "?m=02:00:00:00:00:08": "device_wireguard_tst05-tunnelserver.html",
		"tst04-supernode.local.mesh" + devicePath + "?m=02:00:00:00:00:09": "device_wireguard_tst05-tunnelserver.html",
		"tst04-supernode.local.mesh" + devicePath + "?m=02:00:00:00:00:0a": "device_wireguard_tst05-tunnelserver.html",
	}})

	links, err := c.FetchNeighbors(t.Context(), "tst04-supernode", true, 4)
	if err != nil {
		t.Fatalf("FetchNeighbors: %v", err)
	}
	if len(links) != 4 {
		t.Fatalf("%d links, want 4", len(links))
	}
	for _, l := range links {
		if l.To == "" || l.MAC == "" {
			t.Errorf("row lost its identity: %+v", l)
		}
		if l.Type != model.LinkTypeWireguard {
			t.Errorf("%s: Type = %q, want %q from the dialog", l.To, l.Type, model.LinkTypeWireguard)
		}
	}
	// Four rows and a ceiling of 4: the crawler may never exceed it.
	if max := atomic.LoadInt32(&rec.maxIn); max > 5 {
		t.Errorf("peak concurrency %d exceeded the ceiling (1 listing + 4 dialogs)", max)
	}
}

// A 401 dialog degrades instead of dropping the edge.
func TestFetchNeighborsDetailFailureKeepsRow(t *testing.T) {
	c, _ := newFixtureServer(t, fixture{routes: map[string]string{
		"tst16.local.mesh" + neighborsPath: "neighbors_tst16.html",
		// No device route: the server answers 401, like /a/status/e/* does.
	}})
	links, err := c.FetchNeighbors(t.Context(), "tst16", true, 2)
	if err != nil {
		t.Fatalf("FetchNeighbors: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("%d links, want 1", len(links))
	}
	l := links[0]
	if l.Type != model.LinkTypeRF || l.Status != "okay" {
		t.Errorf("fragment data lost when the dialog failed: %+v", l)
	}
	wantInt(t, l.LocalSNR, "local_snr without dialog", 23)
	if l.IP != "" {
		t.Errorf("IP = %q, want empty without a dialog", l.IP)
	}
}

// A node with both sections at None is not an error.
func TestFetchNeighborsEmptyIsNotAnError(t *testing.T) {
	c, _ := newFixtureServer(t, fixture{routes: map[string]string{
		"lonely.local.mesh" + neighborsPath: "neighbors_none.html",
	}})
	links, err := c.FetchNeighbors(t.Context(), "lonely", true, 4)
	if err != nil {
		t.Fatalf("FetchNeighbors: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("%d links from two None sections", len(links))
	}
}

// The firmware spellings of the type cell.
func TestNormalizeLinkType(t *testing.T) {
	for in, want := range map[string]string{
		"RF":        model.LinkTypeRF,
		"DtD":       model.LinkTypeDtD,
		"Wireguard": model.LinkTypeWireguard,
		"wireguard": model.LinkTypeWireguard,
		"something": "something",
	} {
		if got := normalizeLinkType(in); got != want {
			t.Errorf("normalizeLinkType(%q) = %q, want %q", in, got, want)
		}
	}
}

// The dialog's "last seen" prose becomes a timestamp.
func TestParseLastSeen(t *testing.T) {
	base := fixtureNow()
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"now", 0},
		{"47 hours", 47 * time.Hour},
		{"5 minutes", 5 * time.Minute},
		{"2 days", 48 * time.Hour},
		{"30 seconds", 30 * time.Second},
		{"1 hour", time.Hour},
		{"-", 0},
		{"", 0},
		{"routing", 0},
	}
	for _, tc := range cases {
		got := parseLastSeen(tc.in, base)
		if delta := base.Sub(got); delta != tc.want {
			t.Errorf("parseLastSeen(%q) went back %v, want %v", tc.in, delta, tc.want)
		}
	}
}
