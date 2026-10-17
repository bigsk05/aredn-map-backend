package store

import (
	"maps"
	"slices"
	"strings"
	"time"

	"aredn-map/internal/model"
)

func orTime(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}

// mergeInto folds in over cur; zeros never erase, sets only grow, stamps only move forward.
func mergeInto(cur, in *model.Node) {
	// Identity
	setWhenSet(&cur.Name, in.Name)
	setWhenSet(&cur.URL, in.URL)
	setWhenSet(&cur.MeshIP, in.MeshIP)
	setWhenSet(&cur.Description, in.Description)
	cur.Aliases = unionStrings(cur.Aliases, in.Aliases, true)

	// Radio / hardware
	setWhenSet(&cur.Hardware, in.Hardware)
	setWhenSet(&cur.Firmware, in.Firmware)
	setWhenSet(&cur.Band, in.Band)
	setWhenSet(&cur.BandLabel, in.BandLabel)
	setWhenSet(&cur.Channel, in.Channel)
	setWhenSet(&cur.FreqLow, in.FreqLow)
	setWhenSet(&cur.FreqHigh, in.FreqHigh)
	setWhenSet(&cur.ChanBW, in.ChanBW)
	setWhenSet(&cur.TxPowerDBM, in.TxPowerDBM)
	setWhenSet(&cur.Antenna, in.Antenna)
	setWhenSet(&cur.Azimuth, in.Azimuth)
	setWhenSet(&cur.HeightM, in.HeightM)
	setWhenSet(&cur.MaxDistMiles, in.MaxDistMiles)
	setWhenSet(&cur.SSID, in.SSID)

	// Geography
	if in.DetailFetched || in.HasLocation {
		cur.Lat, cur.Lon = in.Lat, in.Lon
		cur.GCJLat, cur.GCJLon = in.GCJLat, in.GCJLon
		cur.GridLocator, cur.HasLocation = in.GridLocator, in.HasLocation
	} else {
		setWhenSet(&cur.GridLocator, in.GridLocator)
	}

	// Topology
	setWhenSet(&cur.IsSupernode, in.IsSupernode)
	// Crawled only goes one way: a node this sweep reached stays reached.
	wasCrawled := cur.Crawled
	setWhenSet(&cur.Crawled, in.Crawled)
	mergeHops(&cur.Hops, in.Hops, in.Crawled, wasCrawled)
	setWhenSet(&cur.ETX, in.ETX)
	setWhenSet(&cur.BabelMetric, in.BabelMetric)
	setWhenSet(&cur.BabelRoutes, in.BabelRoutes)
	setWhenSet(&cur.OLSRRoutes, in.OLSRRoutes)

	cur.Services = unionStrings(cur.Services, in.Services, false)
	cur.LocalServices = unionStrings(cur.LocalServices, in.LocalServices, false)
	cur.LocalDevices = unionStrings(cur.LocalDevices, in.LocalDevices, false)
	// Neighbors are hostnames: lowercase so they match node IDs directly.
	cur.Neighbors = unionStrings(cur.Neighbors, in.Neighbors, true)
	cur.LinkTypes = unionCounts(cur.LinkTypes, in.LinkTypes)
	cur.RFTypeCounts = unionCounts(cur.RFTypeCounts, in.RFTypeCounts)

	// Liveness: the latest probe verdict wins, zeros included.
	if hasProbeVerdict(in) {
		cur.Online = in.Online
		cur.LatencyMs = in.LatencyMs
		cur.PacketLossPct = in.PacketLossPct
		cur.PacketsSent = in.PacketsSent
		cur.PacketsRecv = in.PacketsRecv
		cur.JitterMs = in.JitterMs
		cur.ProbeError = in.ProbeError
		setWhenSet(&cur.TTL, in.TTL)
		if !in.ProbedAt.IsZero() {
			cur.ProbedAt = in.ProbedAt
		}
	}

	// Scan bookkeeping
	setWhenSet(&cur.DetailFetched, in.DetailFetched)
	setWhenSet(&cur.Source, in.Source)
	setWhenSet(&cur.LastError, in.LastError)
	if cur.DiscoveredAt.IsZero() {
		cur.DiscoveredAt = orTime(in.DiscoveredAt, time.Now())
	}
	if in.UpdatedAt.After(cur.UpdatedAt) {
		cur.UpdatedAt = in.UpdatedAt
	}
}

// setWhenSet copies src over dst only when src is not the zero value.
func setWhenSet[T comparable](dst *T, src T) {
	var zero T
	if src != zero {
		*dst = src
	}
}

// mergeHops keeps the shortest known path; hops -1 means unknown and 0 is the measured crawl root.
func mergeHops(cur *int, in int, inCrawled, wasCrawled bool) {
	switch {
	case *cur < 0 || (*cur == 0 && !wasCrawled):
		*cur = in // nothing known yet, so whatever arrived is an improvement
	case in < 0:
		// "We do not know the distance yet" never replaces a number.
	case in == 0 && inCrawled:
		*cur = 0 // we reached it ourselves; nothing is closer than the root
	case in > 0 && in < *cur:
		*cur = in
	}
}

// MergeHops applies the store's hop rule to a node a caller is editing in place.
func MergeHops(n *model.Node, hop int) {
	mergeHops(&n.Hops, hop, false, n.Crawled)
}

// hasProbeVerdict means the node carries an ICMP answer: probe stamp, verdict or probe error.
func hasProbeVerdict(n *model.Node) bool {
	return !n.ProbedAt.IsZero() || n.Online || n.ProbeError != ""
}

// normaliseSets canonicalises the set fields: case-insensitive dedup, sorted, hostnames lowercase.
func normaliseSets(n *model.Node) {
	n.Aliases = unionStrings(n.Aliases, nil, true)
	n.Neighbors = unionStrings(n.Neighbors, nil, true)
	n.Services = unionStrings(n.Services, nil, false)
	n.LocalServices = unionStrings(n.LocalServices, nil, false)
	n.LocalDevices = unionStrings(n.LocalDevices, nil, false)
}

// unionStrings merges b into a, deduping case-insensitively, sorted by the folded key.
// lower stores the lowercase form (hostnames); else the first casing seen is kept.
func unionStrings(a, b []string, lower bool) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	type entry struct {
		key string
		val string
	}
	out := make([]entry, 0, len(a)+len(b))
	seen := make(map[string]int, len(a)+len(b))
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		key := lowerASCII(v)
		if i, ok := seen[key]; ok {
			if lower {
				out[i].val = key
			}
			return
		}
		val := v
		if lower {
			val = key
		}
		seen[key] = len(out)
		out = append(out, entry{key: key, val: val})
	}
	for _, v := range a {
		add(v)
	}
	for _, v := range b {
		add(v)
	}
	slices.SortFunc(out, func(x, y entry) int { return strings.Compare(x.key, y.key) })
	if len(out) == 0 {
		return nil
	}
	vals := make([]string, len(out))
	for i, e := range out {
		vals[i] = e.val
	}
	return vals
}

// unionCounts folds b over a; keys b did not report keep their previous value.
func unionCounts(a, b map[string]int) map[string]int {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]int, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}
