//go:build live

package aredn

import (
	"encoding/json"
	"go.uber.org/zap"
	"sort"
	"strings"
	"testing"
	"time"

	"aredn-map/internal/model"
)

// The acceptance run against the real mesh, excluded by the "live" build tag:
// go test -tags live -run Live -v ./internal/aredn/ from inside an AREDN network.

func liveClient(t *testing.T) *Client {
	t.Helper()
	// Generous budget: an RF hop is slow and a supernode page is about 500 KB.
	c := New(Config{
		Timeout:      60 * time.Second,
		ConnectTO:    8 * time.Second,
		MaxRetries:   2,
		RetryBackoff: 2 * time.Second,
		UserAgent:    "aredn-map-live-test",
	}, zap.NewNop())
	return c
}

func dump(t *testing.T, label string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Logf("%s: %v", label, err)
		return
	}
	t.Logf("%s:\n%s", label, b)
}

// TestLiveFetchSelf crawls the Rocket M5 node's status page.
func TestLiveFetchSelf(t *testing.T) {
	c := liveClient(t)

	n, err := c.FetchSelf(t.Context(), "tst16")
	if err != nil {
		t.Fatalf("FetchSelf(tst16): %v", err)
	}
	dump(t, "FetchSelf tst16", n)

	if want := "TST16"; n.Name != want {
		t.Errorf("Name = %q, want %q", n.Name, want)
	}
	if want := model.Band5GHz; n.Band != want {
		t.Errorf("Band = %q, want %q (%s)", n.Band, want, n.BandLabel)
	}
	if n.FreqLow != 5670 || n.FreqHigh != 5680 {
		t.Errorf("frequency = %d-%d, want 5670-5680", n.FreqLow, n.FreqHigh)
	}
	if n.ChanBW != 10 {
		t.Errorf("ChanBW = %d, want 10", n.ChanBW)
	}
	if n.Channel != 135 {
		t.Errorf("Channel = %d, want 135", n.Channel)
	}
	if n.TxPowerDBM != 27 {
		t.Errorf("TxPowerDBM = %d, want 27", n.TxPowerDBM)
	}
	if !containsFold(n.Hardware, "Rocket M5") {
		t.Errorf("Hardware = %q, want it to contain Rocket M5", n.Hardware)
	}
	if n.MeshIP == "" {
		t.Error("MeshIP empty: the status page does state the mesh address")
	}
	if !n.HasLocation || !almostEqual(n.Lat, 59.934773) || !almostEqual(n.Lon, -2.425156) {
		t.Errorf("location = (%v,%v) has=%v", n.Lat, n.Lon, n.HasLocation)
	}
	// Outside mainland China coord passes the position through unchanged.
	if !almostEqual(n.GCJLat, n.Lat) || !almostEqual(n.GCJLon, n.Lon) {
		t.Errorf("a non-CN position must pass through: (%v,%v) vs (%v,%v)", n.GCJLat, n.GCJLon, n.Lat, n.Lon)
	}
	t.Logf("Non-CN node datum check: WGS-84 (%.6f,%.6f) == GCJ-02 (%.6f,%.6f) grid %s",
		n.Lat, n.Lon, n.GCJLat, n.GCJLon, n.GridLocator)

	// A node addressed as localnode 307s to its canonical hostname; the crawler must follow it.
	self, err := c.FetchSelf(t.Context(), "localnode")
	if err != nil {
		t.Fatalf("FetchSelf(localnode): %v", err)
	}
	dump(t, "FetchSelf localnode (via 307)", self)
	if self.Name != "TST06" {
		t.Errorf("redirected Name = %q, want TST06", self.Name)
	}
	if self.Band != model.BandNoRF {
		t.Errorf("Band = %q, want %q for an access point with no mesh radio", self.Band, model.BandNoRF)
	}
	// Inside mainland China the datum shifts.
	if almostEqual(self.Lat, self.GCJLat) || almostEqual(self.Lon, self.GCJLon) {
		t.Errorf("a Shanghai position must be offset, got (%v,%v) -> (%v,%v)",
			self.Lat, self.Lon, self.GCJLat, self.GCJLon)
	}
	t.Logf("CN node datum check: WGS-84 (%.6f,%.6f) -> GCJ-02 (%.6f,%.6f) grid %s",
		self.Lat, self.Lon, self.GCJLat, self.GCJLon, self.GridLocator)
}

// TestLiveFetchNeighborsRF crawls tst16 with detail expansion.
func TestLiveFetchNeighborsRF(t *testing.T) {
	c := liveClient(t)

	links, err := c.FetchNeighbors(t.Context(), "tst16", true, 4)
	if err != nil {
		t.Fatalf("FetchNeighbors(tst16): %v", err)
	}
	dump(t, "FetchNeighbors tst16 (with detail)", links)

	var found *model.Link
	for i := range links {
		if links[i].To == "tst09-ptp" {
			found = &links[i]
		}
	}
	if found == nil {
		t.Fatalf("no link to tst09-ptp among %d links", len(links))
	}
	if found.Type != model.LinkTypeRF {
		t.Errorf("Type = %q, want %q", found.Type, model.LinkTypeRF)
	}
	if found.From != "tst16" {
		t.Errorf("From = %q", found.From)
	}
	if found.Container != model.ContainerNeighborhood {
		t.Errorf("Container = %q, want %q", found.Container, model.ContainerNeighborhood)
	}
	if found.Status != "okay" {
		t.Errorf("Status = %q, want okay", found.Status)
	}
	wantInt(t, found.LocalSNR, "RF local_snr", 23)
	if !almostEqual(found.DistanceMI, 3.4) {
		t.Errorf("DistanceMI = %v, want 3.4", found.DistanceMI)
	}
	if found.IP == "" || found.MAC == "" {
		t.Errorf("identity cells empty: %+v", found)
	}
	t.Logf("RF link: mac=%s ip=%s lq/nlq=%v%%/%v%% etx=%v snr=%v/%v ping=%vms bitrate=%v/%vMbps retrans=%v%% metric=%d peer=%s/%s",
		found.MAC, found.IP, found.RxSuccessPct, found.TxSuccessPct, showFloat(found.ETX),
		showInt(found.LocalSNR), showInt(found.NeighborSNR), found.PingMs, showFloat(found.BitrateRxMbps),
		showFloat(found.BitrateTxMbps), showFloat(found.RetransmitPct), found.Metric,
		found.PeerHardware, found.PeerFirmware)
}

// The access point's only adjacency is a WireGuard tunnel reached via the localnode redirect.
func TestLiveFetchNeighborsWireGuard(t *testing.T) {
	c := liveClient(t)

	links, err := c.FetchNeighbors(t.Context(), "localnode", true, 4)
	if err != nil {
		t.Fatalf("FetchNeighbors(localnode): %v", err)
	}
	dump(t, "FetchNeighbors localnode (with detail)", links)

	var found *model.Link
	for i := range links {
		if links[i].To == "tst05-tunnelserver" {
			found = &links[i]
		}
	}
	if found == nil {
		t.Fatalf("no link to tst05-tunnelserver among %d links", len(links))
	}
	if found.Type != model.LinkTypeWireguard {
		t.Errorf("Type = %q, want %q", found.Type, model.LinkTypeWireguard)
	}
	if found.Container != model.ContainerNeighborhood {
		t.Errorf("Container = %q, want %q", found.Container, model.ContainerNeighborhood)
	}
	wantNilInt(t, found.LocalSNR, "WireGuard local_snr")
	wantNilInt(t, found.NeighborSNR, "WireGuard neighbor_snr")
	t.Logf("tunnel link: ip=%s rtt=%vms rx=%v%% dist=%v mi metric=%d lastseen=%s",
		found.IP, found.RTTMs, found.RxSuccessPct, found.DistanceMI, found.Metric,
		found.LastSeen.Format(time.RFC3339))
}

// A node whose Local Nodes section is populated, which the sections above cannot show.
func TestLiveFetchNeighborsLocalSection(t *testing.T) {
	c := liveClient(t)

	links, err := c.FetchNeighbors(t.Context(), "tst12-hap", true, 4)
	if err != nil {
		t.Fatalf("FetchNeighbors(tst12-hap): %v", err)
	}
	dump(t, "FetchNeighbors tst12-hap (with detail)", links)

	var local int
	for _, l := range links {
		if l.Container == model.ContainerLocal {
			local++
			if l.Type != model.LinkTypeDtD && l.Type != model.LinkTypeLocal {
				t.Errorf("local link type = %q", l.Type)
			}
		}
	}
	if local == 0 {
		t.Error("no Local Nodes row crawled even though the fragment has one")
	}
}

// FetchMesh runs against both the access point and a supernode.
// On a node with routes the table is the whole mesh, not the four entries window.mesh carried.
func TestLiveFetchMesh(t *testing.T) {
	c := liveClient(t)

	md, err := c.FetchMesh(t.Context(), "localnode")
	if err != nil {
		t.Fatalf("FetchMesh(localnode): %v", err)
	}
	if len(md.Hosts) < 100 {
		t.Errorf("%d hosts on the access point, want the mesh it routes to", len(md.Hosts))
	}
	if len(md.ETX) != 0 {
		t.Errorf("%d etx entries, want none: /a/sysinfo publishes no routing costs", len(md.ETX))
	}
	t.Logf("localnode mesh: %d hosts, %d node names, %d service hosts", len(md.Hosts), len(md.NodeNames), len(md.Services))

	sn, err := c.FetchMesh(t.Context(), "tst04-supernode")
	if err != nil {
		t.Fatalf("FetchMesh(tst04-supernode): %v", err)
	}
	if len(sn.Hosts) < 100 {
		t.Errorf("%d hosts from the supernode, want hundreds", len(sn.Hosts))
	}
	t.Logf("supernode mesh: %d hosts, %d service hosts, %d node names",
		len(sn.Hosts), len(sn.Services), len(sn.NodeNames))
	t.Logf("supernode sample hosts: %v", samplePairs(sn.Hosts, 5))
	t.Logf("supernode sample services: %v", samplePairs(sn.Services, 5))
	t.Logf("supernode sample names: %v", head(sn.NodeNames, 8))
	// The full struct is hundreds of KB, so the dump carries counts and one host entry.
	dump(t, "FetchMesh tst04-supernode (first host)", map[string][]string{
		firstKey(sn.Hosts): sn.Hosts[firstKey(sn.Hosts)],
	})
}

// containsFold is case-insensitive because live builds spell a model name differently.
func containsFold(hay, needle string) bool {
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

// samplePairs returns the first n entries of a map, keyed deterministically.
func samplePairs(m map[string][]string, n int) map[string][]string {
	out := map[string][]string{}
	for _, k := range sortedKeys(m) {
		if len(out) >= n {
			break
		}
		out[k] = m[k]
	}
	return out
}

// sampleInts returns the first n cost entries in key order.
func sampleInts(m map[string]int, n int) map[string]int {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]int{}
	for i, k := range keys {
		if i >= n {
			break
		}
		out[k] = m[k]
	}
	return out
}

// head trims a slice for readable log lines.
func head(s []string, n int) []string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// firstKey returns the lowest mesh address so log dumps are reproducible.
func firstKey(m map[string][]string) string {
	keys := sortedKeys(m)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
