package model

import (
	"math"
	"strconv"
	"strings"
)

// LooksLikeSupernode matches the supernode naming convention: alias "supernode.<NAME>.local.mesh".
func LooksLikeSupernode(parts ...string) bool {
	for _, p := range parts {
		if strings.Contains(strings.ToLower(p), "supernode") {
			return true
		}
	}
	return false
}

// Band keys follow the worldmap vocabulary: frequency first digit, "h" HaLow, plus pseudo-bands.
const (
	BandHaLow      = "h"
	Band900MHz     = "9"
	Band2_4GHz     = "2"
	Band3_4GHz     = "3"
	Band5GHz       = "5"
	BandSupernode  = "s"
	BandNoRF       = "n"
	BandUnresolved = "u"
	// BandUnscanned: this scanner never read the node's page, unlike BandUnresolved,
	// which claims the page was read and its radio could not be identified.
	BandUnscanned = "p"
)

// BandLabels maps a band key to the label used by the map legend and filter panel.
var BandLabels = map[string]string{
	BandHaLow:      "HaLow",
	Band900MHz:     "900 MHz",
	Band2_4GHz:     "2.4 GHz",
	Band3_4GHz:     "3.4 GHz",
	Band5GHz:       "5 GHz",
	BandSupernode:  "Supernode",
	BandNoRF:       "No RF",
	BandUnresolved: "Unknown",
	BandUnscanned:  "Unscanned",
}

// BandColors keeps marker colors aligned with worldmap.arednmesh.org.
var BandColors = map[string]string{
	BandHaLow:      "#c0c0c0",
	Band900MHz:     "#f97316",
	Band2_4GHz:     "#22c55e",
	Band3_4GHz:     "#eab308",
	Band5GHz:       "#3b82f6",
	BandSupernode:  "#a855f7",
	BandNoRF:       "#64748b",
	BandUnresolved: "#94a3b8",
	BandUnscanned:  "#78716c",
}

// BandOf is the one rule for a node's band key, and its fallbacks distinguish
// "page never read" from "page read but radio unclassified".
func BandOf(n *Node) string {
	// BandUnscanned is a statement about us, not the radio, so it is re-evaluated on every write.
	if n.Band != BandUnscanned {
		if _, ok := BandLabels[n.Band]; ok {
			return n.Band
		}
	}
	if !n.Crawled && !n.DetailFetched {
		return BandUnscanned
	}
	return BandUnresolved
}

// DeriveBand keys on the centre frequency (freqLow + chanBW/2), not the published low edge.
func DeriveBand(freqLow, chanBW, channel int, isSupernode bool, hasRF bool) string {
	if isSupernode {
		return BandSupernode
	}
	if !hasRF {
		return BandNoRF
	}
	if freqLow <= 0 {
		// freqLow <= 0 is an unnamed radio, not an absent one; the page omits disabled radios.
		return BandUnresolved
	}
	if isHaLow(freqLow, chanBW, channel) {
		return BandHaLow
	}
	center := float64(freqLow) + float64(chanBW)/2
	key := digitKey(int(math.Round(center)))
	switch key {
	case "2", "3", "5", "9":
		return key
	default:
		return BandUnresolved
	}
}

// isHaLow separates the two rasters sharing 9xx MHz: HaLow is 902.0 + 0.5*c, classic 887 + 5*c.
// The page publishes only the low edge, centre - chanBW/2, and chanBW defaults to 20.
func isHaLow(freqLow, chanBW, channel int) bool {
	center := float64(freqLow) + float64(chanBW)/2
	if center < 902 || center > 928 {
		return false
	}
	if channel > 0 {
		haLow := math.Abs(center - (902.0 + 0.5*float64(channel)))
		classic := math.Abs(center - (887.0 + 5.0*float64(channel)))
		// The rasters overlap, so the channel number wins; width is the fallback when it is absent.
		if haLow <= 0.6 && haLow < classic {
			return true
		}
		if classic <= 0.6 && classic < haLow {
			return false
		}
	}
	switch chanBW {
	case 1, 2, 4, 8:
		return true
	default:
		return false
	}
}

func digitKey(freq int) string {
	s := strconv.Itoa(freq)
	if s == "" {
		return ""
	}
	return s[:1]
}
