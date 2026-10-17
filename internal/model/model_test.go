package model

import "testing"

// Precedence: supernode, then no-RF, then band; a supernode keeps its marker colour either way.
func TestDeriveBand(t *testing.T) {
	cases := []struct {
		name             string
		freq, bw, chanNo int
		supernode        bool
		hasRF            bool
		want             string
	}{
		{"5 GHz", 5670, 10, 0, false, true, Band5GHz},
		{"2.4 GHz", 2457, 10, 0, false, true, Band2_4GHz},
		{"3.4 GHz", 3450, 10, 0, false, true, Band3_4GHz},
		{"900 MHz wide", 915, 10, 0, false, true, Band900MHz},
		{"900 MHz 5k keeps 900", 915, 5, 0, false, true, Band900MHz},
		{"HaLow is narrow 9xx", 917, 1, 0, false, true, BandHaLow},
		{"HaLow 2 MHz", 917, 2, 0, false, true, BandHaLow},
		{"HaLow 8 MHz", 917, 8, 0, false, true, BandHaLow},
		// HaLow channel 5 sits at 904.5 MHz, classic 900 channel 5 at 912 MHz.
		{"HaLow ch5 as reported", 904, 1, 5, false, true, BandHaLow},
		{"classic 900 ch5 as reported", 909, 5, 5, false, true, Band900MHz},
		{"HaLow ch49", 926, 1, 49, false, true, BandHaLow},
		// With no width in uci the page assumes 20 MHz, so the low edge lands 10 MHz under the centre.
		{"HaLow ch5 with bogus 20 MHz width", 894, 20, 5, false, true, BandHaLow},
		{"supernode outranks radio", 5670, 10, 0, true, true, BandSupernode},
		{"supernode without radio", 0, 0, 0, true, false, BandSupernode},
		{"no rf", 0, 0, 0, false, false, BandNoRF},
		// A channel with no frequency is an unnamed radio, not an absent one.
		{"channel without frequency", 0, 0, -2, false, true, BandUnresolved},
		{"rf off reports no frequency", 0, 10, 0, false, false, BandNoRF},
		{"unrecognised decade", 700, 10, 0, false, true, BandUnresolved},
		{"bandwidth zero still bands", 2412, 0, 0, false, true, Band2_4GHz},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveBand(c.freq, c.bw, c.chanNo, c.supernode, c.hasRF); got != c.want {
				t.Errorf("DeriveBand(%d,%d,%d,%v,%v) = %q, want %q", c.freq, c.bw, c.chanNo, c.supernode, c.hasRF, got, c.want)
			}
		})
	}
}

// A derivable band with no label or colour renders a blank swatch on the map or graph.
func TestEveryDerivableBandHasLabelAndColor(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range []struct{ freq, bw, chanNo int }{{915, 10, 0}, {917, 1, 0}, {2457, 10, 0}, {3450, 10, 0}, {5670, 10, 0}, {0, 0, 0}, {700, 10, 0}, {894, 20, 5}} {
		seen[DeriveBand(c.freq, c.bw, c.chanNo, false, c.freq > 0)] = true
		seen[DeriveBand(c.freq, c.bw, c.chanNo, true, true)] = true
	}
	for band := range seen {
		if BandLabels[band] == "" {
			t.Errorf("band %q has no label", band)
		}
		if BandColors[band] == "" {
			t.Errorf("band %q has no colour", band)
		}
	}
}

func TestLooksLikeSupernode(t *testing.T) {
	yes := [][]string{
		{"tst04-supernode"},
		{"TST04-SUPERNODE"},
		{"supernode.tst04-supernode.local.mesh"},
		{"anything", "Supernode.GW.local.mesh"},
	}
	for _, parts := range yes {
		if !LooksLikeSupernode(parts...) {
			t.Errorf("LooksLikeSupernode(%q) = false, want true", parts)
		}
	}
	no := [][]string{
		{"tst16"},
		{"", ""},
		{"node-a", "10.66.66.66"},
		{"supernode-free-standing-is-not-a-rule"}, // matches the substring on purpose? see below
	}
	for _, parts := range no[:3] {
		if LooksLikeSupernode(parts...) {
			t.Errorf("LooksLikeSupernode(%q) = true, want false", parts)
		}
	}
	// Documented looseness: the rule is a substring test, so an operator who
	// names a plain node with "supernode" in it is classified as one.
	if !LooksLikeSupernode(no[3][0]) {
		t.Error("substring rule changed; naming convention is the only signal AREDN exposes")
	}
}

func TestNodeIDAndLinkKey(t *testing.T) {
	n := &Node{Hostname: "TST16.local.mesh"}
	if n.ID() != "TST16.local.mesh" {
		t.Errorf("ID() = %q, want the hostname verbatim; normalisation is the store's job", n.ID())
	}
	a := Link{From: "a", To: "b", MAC: "02:00"}
	b := Link{From: "a", To: "b", MAC: "02:01"}
	if a.Key() == b.Key() {
		t.Error("links differing only by MAC collide")
	}
	// The separator must not be formable inside a hostname, or two distinct edges
	// could alias.
	c := Link{From: "a", To: "b\x00c", MAC: "02:00"}
	if c.Key() == a.Key() {
		t.Error("a NUL inside an endpoint aliases two edges")
	}
}

// Unscanned and Unknown are different claims and must not share a bucket: the
// first is about us, the second about the radio.
func TestBandOfSeparatesIgnoranceFromAmbiguity(t *testing.T) {
	cases := []struct {
		name string
		n    Node
		want string
	}{
		{"directory name, never read", Node{Hostname: "cloud0"}, BandUnscanned},
		{"read page, radio unidentifiable", Node{Hostname: "a", Band: BandUnresolved, Crawled: true}, BandUnresolved},
		{"detail fetched but never expanded", Node{Hostname: "b", DetailFetched: true}, BandUnresolved},
		{"a real band always wins", Node{Hostname: "c", Band: BandHaLow}, BandHaLow},
		{"a band the table does not know is not kept", Node{Hostname: "d", Band: "x"}, BandUnscanned},
	}
	for _, c := range cases {
		if got := BandOf(&c.n); got != c.want {
			t.Errorf("%s: BandOf = %q, want %q", c.name, got, c.want)
		}
	}
}

// Every band key must have both a label and a colour, or one of the two renderers
// shows a blank swatch. The tables are maintained apart, so this checks the pair.
func TestBandLabelAndColorTablesAgree(t *testing.T) {
	for band := range BandLabels {
		if BandColors[band] == "" {
			t.Errorf("band %q has a label but no colour", band)
		}
	}
	for band := range BandColors {
		if BandLabels[band] == "" {
			t.Errorf("band %q has a colour but no label", band)
		}
	}
}
