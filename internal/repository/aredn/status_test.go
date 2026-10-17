package aredn

import (
	"math"
	"strings"
	"testing"

	"aredn-map/internal/model"
)

// Every field below was read from a live status page recorded in testdata/.
func TestFetchSelfRF(t *testing.T) {
	c, rec := newFixtureServer(t, downFixture())

	n, err := c.FetchSelf(t.Context(), "TST16")
	if err != nil {
		t.Fatalf("FetchSelf: %v", err)
	}
	if want := "tst16"; n.Hostname != want {
		t.Errorf("Hostname = %q, want %q", n.Hostname, want)
	}
	if want := "TST16"; n.Name != want {
		t.Errorf("Name = %q, want %q", n.Name, want)
	}
	if want := "http://tst16.local.mesh"; n.URL != want {
		t.Errorf("URL = %q, want %q", n.URL, want)
	}
	// Hops is the crawl's business, not the page's; 0 is reserved for the crawl root.
	if n.Hops != -1 {
		t.Errorf("Hops = %d, want -1 (unknown), because a status page never states it", n.Hops)
	}
	if want := "10.100.3.254"; n.MeshIP != want {
		t.Errorf("MeshIP = %q, want %q", n.MeshIP, want)
	}
	if want := "Ubiquiti Rocket M5"; n.Hardware != want {
		t.Errorf("Hardware = %q, want %q", n.Hardware, want)
	}
	if want := "3.25.8.0"; n.Firmware != want {
		t.Errorf("Firmware = %q, want %q", n.Firmware, want)
	}

	// Band follows the worldmap rule: 5670 keys on 5.
	if n.Band != model.Band5GHz || n.BandLabel != "5 GHz" {
		t.Errorf("Band = %q/%q, want 5/5 GHz", n.Band, n.BandLabel)
	}
	for _, tc := range []struct {
		field string
		got   int
		want  int
	}{
		{"Channel", n.Channel, 135},
		{"FreqLow", n.FreqLow, 5670},
		{"FreqHigh", n.FreqHigh, 5680},
		{"ChanBW", n.ChanBW, 10},
		{"TxPowerDBM", n.TxPowerDBM, 27},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.field, tc.got, tc.want)
		}
	}
	if !almostEqual(n.MaxDistMiles, 4) {
		t.Errorf("MaxDistMiles = %v, want 4", n.MaxDistMiles)
	}
	// "-" placeholders must stay zero, and never leak into the strings.
	if n.Antenna != "" || n.Azimuth != 0 || n.HeightM != 0 {
		t.Errorf("absent antenna fields leaked: %+v", []any{n.Antenna, n.Azimuth, n.HeightM})
	}
	if want := "TSTNET relay node. See www.example.invalid"; n.Description != want {
		t.Errorf("Description = %q, want %q", n.Description, want)
	}

	// 54.93N 1.42W is outside coord's mainland-China box, so the GCJ-02 pair equals WGS-84.
	if !n.HasLocation || !almostEqual(n.Lat, 59.934773) || !almostEqual(n.Lon, -2.425156) {
		t.Errorf("location = (%v,%v) has=%v", n.Lat, n.Lon, n.HasLocation)
	}
	if !almostEqual(n.GCJLat, n.Lat) || !almostEqual(n.GCJLon, n.Lon) {
		t.Errorf("GCJ-02 should pass a non-CN position through, got (%v,%v)", n.GCJLat, n.GCJLon)
	}
	if want := "IO94gw"; n.GridLocator != want {
		t.Errorf("GridLocator = %q, want %q", n.GridLocator, want)
	}
	if n.IsSupernode {
		t.Error("a plain RF node must not be flagged as a supernode")
	}
	if rec.count("tst16.local.mesh"+statusPath) != 1 {
		t.Errorf("status fetched %d times, want 1", rec.count("tst16.local.mesh"+statusPath))
	}
}

// No mesh radio: no frequencies cell, and a Chinese position that GCJ-02 really moves.
func TestFetchSelfNoRadio(t *testing.T) {
	c, _ := newFixtureServer(t, homeNodeFixture())

	n, err := c.FetchSelf(t.Context(), "localnode")
	if err != nil {
		t.Fatalf("FetchSelf via 307 redirect: %v", err)
	}
	if n.Band != model.BandNoRF || n.BandLabel != "No RF" {
		t.Errorf("Band = %q/%q, want n/No RF", n.Band, n.BandLabel)
	}
	if n.FreqLow != 0 || n.FreqHigh != 0 || n.ChanBW != 0 || n.Channel != 0 {
		t.Errorf("radio fields leaked on a node with no mesh radio: %+v", n)
	}
	if want := "10.100.4.200"; n.MeshIP != want {
		t.Errorf("MeshIP = %q, want %q", n.MeshIP, want)
	}
	if want := "Home Server (S805)"; n.Hardware != want {
		t.Errorf("Hardware = %q, want %q", n.Hardware, want)
	}
	if !strings.Contains(n.Firmware, "4.26.7.0") {
		t.Errorf("Firmware = %q, want the 4.26.7.0 snapshot", n.Firmware)
	}
	if !almostEqual(n.Lat, 25.01531) || !almostEqual(n.Lon, 104.99390) {
		t.Errorf("location = (%v,%v)", n.Lat, n.Lon)
	}
	if want := "PM00rv"; n.GridLocator != want {
		t.Errorf("GridLocator = %q, want %q", n.GridLocator, want)
	}
	// Shanghai is inside the GCJ-02 offset box, so the datum really shifts.
	if almostEqual(n.GCJLat, n.Lat) || almostEqual(n.GCJLon, n.Lon) {
		t.Errorf("expected a GCJ-02 offset in China, got (%v,%v) vs (%v,%v)", n.GCJLat, n.GCJLon, n.Lat, n.Lon)
	}
	if d := math.Abs(n.GCJLat - n.Lat); d > 0.01 || d < 0.0001 {
		t.Errorf("GCJ-02 latitude offset = %v, want a few hundred metres", d)
	}
	// The access point inlines its own services/devices; both are "None" here.
	if len(n.LocalServices) != 0 || len(n.LocalDevices) != 0 {
		t.Errorf("None sections became items: %v / %v", n.LocalServices, n.LocalDevices)
	}
}

// A status page missing every section is not an error; only transport failures are.
func TestFetchSelfDegrades(t *testing.T) {
	c, _ := newFixtureServer(t, fixture{routes: map[string]string{
		"bare.local.mesh" + statusPath: "status_bare.html",
	}})
	n, err := c.FetchSelf(t.Context(), "bare")
	if err != nil {
		t.Fatalf("FetchSelf on a bare page: %v", err)
	}
	if n.Name != "BARE-TEST" {
		t.Errorf("Name = %q", n.Name)
	}
	if n.Band != model.BandNoRF {
		t.Errorf("Band = %q, want %q", n.Band, model.BandNoRF)
	}
	if n.MeshIP != "" || n.HasLocation || n.Firmware != "" || n.Channel != 0 {
		t.Errorf("missing sections became values: %+v", n)
	}
}

// Supernode detection plus the RF fields a non-5 GHz node reports.
func TestParseSupernodeAlias(t *testing.T) {
	c, _ := newFixtureServer(t, fixture{routes: map[string]string{
		"tst03-supernode.local.mesh" + statusPath: "status_supernode_synth.html",
	}})
	n, err := c.FetchSelf(t.Context(), "tst03-supernode")
	if err != nil {
		t.Fatalf("FetchSelf: %v", err)
	}
	if !n.IsSupernode {
		t.Fatal("a node whose title says SUPERNODE must be flagged")
	}
	if n.Band != model.BandSupernode || n.BandLabel != "Supernode" {
		t.Errorf("Band = %q/%q, want s/Supernode", n.Band, n.BandLabel)
	}
	// The radio block is parsed even though the band key is the supernode pseudo-band.
	if n.FreqLow != 903 || n.FreqHigh != 908 || n.ChanBW != 5 {
		t.Errorf("radio = %+v", []any{n.FreqLow, n.FreqHigh, n.ChanBW})
	}
	if model.DeriveBand(n.FreqLow, n.ChanBW, n.Channel, false, true) != model.Band900MHz {
		t.Errorf("without the supernode flag 903/5 must be 900 MHz, channel %d", n.Channel)
	}
	if n.Antenna != "900-1311" || n.Azimuth != 90 || n.HeightM != 12 {
		t.Errorf("antenna fields = %q/%d/%d", n.Antenna, n.Azimuth, n.HeightM)
	}
	if n.MeshIP != "10.66.66.66" {
		t.Errorf("MeshIP = %q", n.MeshIP)
	}
	if !almostEqual(n.Lat, 31.2304) || almostEqual(n.GCJLat, n.Lat) {
		t.Errorf("Shanghai position must be offset: %v %v", n.Lat, n.GCJLat)
	}
}

// Uses the populated u-services fragment recorded from tst12-hap.
func TestParseLocalBlocks(t *testing.T) {
	body := fixtureString(t, "fragment_services_tst12-hap.html")
	services, devices := parseLocalBlocks(body)
	wantServices := []string{"BirdNet-Go", "Radiosonde Auto-RX", "ADS-B Plane Map", "ADS-B Graphs", "555-0142"}
	if len(services) != len(wantServices) {
		t.Fatalf("services = %v, want %v", services, wantServices)
	}
	for i, s := range services {
		if s != wantServices[i] {
			t.Errorf("service %d = %q, want %q", i, s, wantServices[i])
		}
	}
	if len(devices) != 2 || devices[0] != "tst08" {
		t.Errorf("devices = %v", devices)
	}
}

// The three narrow patterns the status parser relies on, one per recorded cell shape.
func TestStatusRegexAnchors(t *testing.T) {
	page := fixtureString(t, "status_tst16.html")

	if m := channelRe.FindStringSubmatch(page); m == nil || m[1] != "135" {
		t.Errorf("channelRe did not anchor the channel cell, got %v", m)
	}
	if m := titleRe.FindStringSubmatch(page); m == nil || text(m[1]) != "TST16 status" {
		t.Errorf("titleRe = %v", m)
	}
	if m := firmwareRe.FindStringSubmatch(page); m == nil || clean(m[1]) != "3.25.8.0" {
		t.Errorf("firmwareRe = %v", m)
	}
	if m := locationRe.FindStringSubmatch(page); m == nil ||
		m[1] != "59.934773" || m[2] != "-2.425156" || m[3] != "IO94gw" {
		t.Errorf("locationRe = %v", m)
	}
	if m := worldmapRe.FindStringSubmatch(page); m == nil ||
		m[1] != "59.934773" || m[2] != "-2.425156" {
		t.Errorf("worldmapRe = %v", m)
	}
}

// A page whose position survives only inside the worldmap iframe URL, as older builds emit.
func TestParseLocationWorldmapFallback(t *testing.T) {
	page := `<title>FALLBACK-TEST status</title><div id="location">` +
		`<div class="location-image"><iframe src="https://worldmap.arednmesh.org/#10/51.50000/-0.12345"></iframe></div>` +
		`</div><div id="c2c3"></div>`
	var n model.Node
	parseLocation(page, &n)
	if !n.HasLocation || !almostEqual(n.Lat, 51.5) || !almostEqual(n.Lon, -0.12345) {
		t.Fatalf("fallback location = %+v", n)
	}
	if n.GridLocator != "" {
		t.Errorf("GridLocator = %q, want empty when the cell is absent", n.GridLocator)
	}
}

// TestParseLocationAbsent covers a node with no position at all.
func TestParseLocationAbsent(t *testing.T) {
	var n model.Node
	parseLocation(`<title>NO-GPS status</title><div id="c2c3"></div>`, &n)
	if n.HasLocation || n.Lat != 0 || n.Lon != 0 || n.GCJLat != 0 {
		t.Errorf("a positionless node got coordinates: %+v", n)
	}
}

// The frequency cell variants the firmware emits.
func TestParseFreqs(t *testing.T) {
	cases := []struct {
		in        string
		low, high int
		ok        bool
	}{
		{"5670 - 5680 MHz", 5670, 5680, true},
		{"2412 - 2462 MHz", 2412, 2462, true},
		{"903.5 - 908.5 MHz", 903, 908, true},
		{"5680 MHz", 5680, 5680, true},
		{"-", 0, 0, false},
		{"", 0, 0, false},
		{"disabled", 0, 0, false},
	}
	for _, tc := range cases {
		low, high, ok := parseFreqs(tc.in)
		if ok != tc.ok || low != tc.low || high != tc.high {
			t.Errorf("parseFreqs(%q) = %d,%d,%v want %d,%d,%v", tc.in, low, high, ok, tc.low, tc.high, tc.ok)
		}
	}
}

// In 9xx MHz air a narrow channel is HaLow, a 5 MHz one is not; the channel number outranks both.
func TestDeriveBandHaLow(t *testing.T) {
	if got := model.DeriveBand(903, 2, 0, false, true); got != model.BandHaLow {
		t.Errorf("DeriveBand(903,2) = %q, want %q", got, model.BandHaLow)
	}
	if got := model.DeriveBand(903, 5, 0, false, true); got != model.Band900MHz {
		t.Errorf("DeriveBand(903,5) = %q, want %q", got, model.Band900MHz)
	}
	if got := model.DeriveBand(0, 0, 0, false, false); got != model.BandNoRF {
		t.Errorf("DeriveBand(no rf) = %q, want %q", got, model.BandNoRF)
	}
}
