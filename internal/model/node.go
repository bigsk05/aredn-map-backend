package model

import (
	"time"
)

// Node is one AREDN mesh node as known to the scan.
type Node struct {
	// Identity
	Name        string   `json:"name"`
	Hostname    string   `json:"hostname"`
	URL         string   `json:"url"`
	MeshIP      string   `json:"mesh_ip"`
	Aliases     []string `json:"aliases,omitempty"`
	Description string   `json:"description,omitempty"`
	// CanonicalHost is the bare label the node answered under, recovered from the redirect target.
	CanonicalHost string `json:"canonical_host,omitempty"`

	// Radio / hardware
	Hardware     string  `json:"hardware,omitempty"`
	Firmware     string  `json:"firmware,omitempty"`
	Band         string  `json:"band"`
	BandLabel    string  `json:"band_label"`
	Channel      int     `json:"channel"`
	FreqLow      int     `json:"freq_low,omitempty"`
	FreqHigh     int     `json:"freq_high,omitempty"`
	ChanBW       int     `json:"chan_bw,omitempty"`
	TxPowerDBM   int     `json:"tx_power_dbm,omitempty"`
	Antenna      string  `json:"antenna,omitempty"`
	Azimuth      int     `json:"azimuth,omitempty"`
	HeightM      int     `json:"height_m,omitempty"`
	MaxDistMiles float64 `json:"max_dist_miles,omitempty"`
	SSID         string  `json:"ssid,omitempty"`

	// Geography (WGS-84 as published by the node, GCJ-02 for Tencent maps)
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	GCJLat      float64 `json:"gcj_lat"`
	GCJLon      float64 `json:"gcj_lon"`
	GridLocator string  `json:"grid_locator,omitempty"`
	HasLocation bool    `json:"has_location"`

	// Topology
	IsSupernode bool `json:"is_supernode"`
	// Hops is the radio-hop distance from the nearest seed reached this sweep; -1 means unknown.
	Hops        int `json:"hops"`
	ETX         int `json:"etx"`
	BabelMetric int `json:"babel_metric,omitempty"`
	BabelRoutes int `json:"babel_routes,omitempty"`
	OLSRRoutes  int `json:"olsr_routes,omitempty"`

	Services      []string       `json:"services,omitempty"`
	LocalServices []string       `json:"local_services,omitempty"`
	LocalDevices  []string       `json:"local_devices,omitempty"`
	Neighbors     []string       `json:"neighbors,omitempty"`
	LinkTypes     map[string]int `json:"link_types,omitempty"`
	RFTypeCounts  map[string]int `json:"rf_type_counts,omitempty"`

	// Liveness from ICMP probing
	Online bool `json:"online"`
	// Disconnected is the mesh's own report from a crawled peer, not an inference from probing.
	Disconnected  bool      `json:"disconnected"`
	MeshState     string    `json:"mesh_state,omitempty"`
	LinkDownSince time.Time `json:"link_down_since"`
	LatencyMs     float64   `json:"latency_ms"`
	PacketLossPct float64   `json:"packet_loss_pct"`
	PacketsSent   int       `json:"packets_sent"`
	PacketsRecv   int       `json:"packets_recv"`
	TTL           int       `json:"ttl,omitempty"`
	JitterMs      float64   `json:"jitter_ms,omitempty"`
	ProbedAt      time.Time `json:"probed_at"`
	ProbeError    string    `json:"probe_error,omitempty"`

	// Scan bookkeeping
	DiscoveredAt  time.Time `json:"discovered_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	DetailFetched bool      `json:"detail_fetched"`
	// Crawled marks nodes this scanner reached; host-table names are not crawled and are not probed.
	Crawled   bool   `json:"crawled"`
	Source    string `json:"source,omitempty"`
	LastError string `json:"last_error,omitempty"`
	// RevisitAt parks a node this scanner could not reach until that instant; zero means it is live.
	RevisitAt time.Time `json:"revisit_at"`
	FailCount int       `json:"fail_count,omitempty"`
}

// ID is the store key; mesh hostnames are case-insensitive, so the lowercase hostname is canonical.
func (n *Node) ID() string { return n.Hostname }
