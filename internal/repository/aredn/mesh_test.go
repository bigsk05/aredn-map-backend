package aredn

import (
	"errors"
	"strings"
	"testing"
)

// These tests assert the shape the scanner consumes out of the supernode mesh table.

func TestFetchMeshLocalnode(t *testing.T) {
	c, _ := newFixtureServer(t, meshLocalnodeFixture())

	md, err := c.FetchMesh(t.Context(), "localnode")
	if err != nil {
		t.Fatalf("FetchMesh: %v", err)
	}
	if len(md.Hosts) != 4 {
		t.Errorf("%d hosts, want 4: %v", len(md.Hosts), md.Hosts)
	}
	if got := md.Hosts["10.66.66.66"]; len(got) != 2 ||
		got[0] != "TST04-SUPERNODE" ||
		got[1] != "supernode.TST04-SUPERNODE.local.mesh" {
		t.Errorf("supernode host = %v", got)
	}
	if got := md.Hosts["10.100.8.161"]; len(got) != 1 || got[0] != "TST02" {
		t.Errorf("plain host = %v", got)
	}
	if len(md.Services) != 0 {
		t.Errorf("services = %v, want none on this node", md.Services)
	}
	// The wider `hosts` list is LAN addresses; absorbing it would make every phone a crawl target.
	for ip := range md.Hosts {
		if ip == "10.100.9.196" || ip == "10.100.7.242" {
			t.Errorf("LAN host %s leaked into the mesh table", ip)
		}
	}
	wantNames := []string{"tst02", "tst04-supernode", "tst05-tunnelserver", "tst06"}
	if strings.Join(md.NodeNames, ",") != strings.Join(wantNames, ",") {
		t.Errorf("NodeNames = %v, want %v", md.NodeNames, wantNames)
	}
}

func TestFetchMeshSupernodeShapes(t *testing.T) {
	c, _ := newFixtureServer(t, fixture{routes: map[string]string{
		"tst04-supernode.local.mesh" + sysinfoPath + "?nodes=1&services=1": "sysinfo_mesh_supernode.json",
	}})

	md, err := c.FetchMesh(t.Context(), "tst04-supernode")
	if err != nil {
		t.Fatalf("FetchMesh: %v", err)
	}
	// A name repeated on one address is one alias list; an entry missing either half is dropped.
	if got := md.Hosts["10.100.13.223"]; len(got) != 2 || got[0] != "TST12-HAP" {
		t.Errorf("duplicated node = %v, want the name twice", got)
	}
	if _, ok := md.Hosts["10.0.0.9"]; ok {
		t.Errorf("nameless entry kept: %v", md.Hosts["10.0.0.9"])
	}
	if len(md.NodeNames) != 3 {
		t.Errorf("NodeNames = %v, want 3 distinct lowercased names", md.NodeNames)
	}
	if got := md.Services["10.100.10.43"]; len(got) != 1 || got[0] != "TST17 [phone]|tcp" {
		t.Errorf("service without a link target = %v, want the protocol as the target", got)
	}
	if got := md.Services["10.100.11.83"]; len(got) != 1 ||
		!strings.HasSuffix(got[0], "|http://10.100.12.117:80/") {
		t.Errorf("service with a link target = %v", got)
	}
	if len(md.Services[""]) != 0 {
		t.Errorf("service with no address kept: %v", md.Services[""])
	}
	if len(md.ETX) != 0 {
		t.Errorf("ETX = %v, want empty: /a/sysinfo publishes no routing costs", md.ETX)
	}
}

// AREDN 3.x answers /a/sysinfo with 200 and an HTML page, not a decodable JSON body.
func TestFetchSysinfoNoJSONFace(t *testing.T) {
	c, _ := newFixtureServer(t, fixture{routes: map[string]string{
		"2e0twx-hap-1.local.mesh" + sysinfoPath + "?nodes=1&services=1": "sysinfo_error_3x.html",
	}})

	_, err := c.FetchMesh(t.Context(), "2e0twx-hap-1")
	if !errors.Is(err, ErrNoSysinfo) {
		t.Fatalf("err = %v, want ErrNoSysinfo", err)
	}
}
