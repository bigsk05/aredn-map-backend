package coord

import (
	"math"
	"testing"
)

// metresBetween is the great-circle distance used to bound the datum shift.
func metresBetween(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return r * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func TestOutsideMainlandPassesThroughUnchanged(t *testing.T) {
	for _, p := range [][2]float64{
		{51.5074, -0.1278},  // London, outside the box
		{35.6810, 139.7671}, // Tokyo, outside the box
		{55.7558, 37.6173},  // Moscow, outside the box
		{0, 0},              // unset position
	} {
		lat, lon := WGS84ToGCJ02(p[0], p[1])
		if lat != p[0] || lon != p[1] {
			t.Errorf("expected exact pass-through for %v, got (%f,%f)", p, lat, lon)
		}
	}
}

func TestInsideMainlandShiftsByPlausibleDatumOffset(t *testing.T) {
	// The GCJ-02 shift inside China is a few hundred metres; 20-2000 m bounds a correct formula.
	cases := [][2]float64{
		{31.2304, 121.4737}, // Shanghai
		{39.90850, 116.39720},
		{22.5431, 114.0579}, // Shenzhen
		{30.2741, 120.1551}, // Hangzhou
	}
	for _, p := range cases {
		lat, lon := WGS84ToGCJ02(p[0], p[1])
		d := metresBetween(p[0], p[1], lat, lon)
		if d < 20 || d > 2000 {
			t.Errorf("(%f,%f): datum shift %.1f m outside plausible band", p[0], p[1], d)
		}
		if math.Abs(lat-p[0]) > 0.02 || math.Abs(lon-p[1]) > 0.02 {
			t.Errorf("(%f,%f): shift %.5f degrees too large", p[0], p[1], math.Max(math.Abs(lat-p[0]), math.Abs(lon-p[1])))
		}
	}
}

func TestShiftIsLocallyContinuous(t *testing.T) {
	// Two neighbouring nodes must not get wildly different corrections.
	aLat, aLon := WGS84ToGCJ02(31.2304, 121.4737)
	bLat, bLon := WGS84ToGCJ02(31.2314, 121.4747)
	sep := metresBetween(aLat, aLon, bLat, bLon)
	raw := metresBetween(31.2304, 121.4737, 31.2314, 121.4747)
	if math.Abs(sep-raw) > raw*0.5 {
		t.Errorf("discontinuity: raw %.1f m vs converted %.1f m", raw, sep)
	}
}

func TestBoundingBoxEdges(t *testing.T) {
	if InChinaMainland(31.23, 60.0) {
		t.Error("lon west of the box reported as mainland")
	}
	if InChinaMainland(60.0, 100.0) {
		t.Error("lat north of the box reported as mainland")
	}
	if !InChinaMainland(31.23, 121.47) {
		t.Error("Shanghai reported as outside the mainland box")
	}
}
