package model

import (
	"time"
)

// LinkType values observed on the AREDN 4.x neighbor detail dialog.
const (
	LinkTypeRF        = "RF"
	LinkTypeDtD       = "DtD"
	LinkTypeWireguard = "Wireguard"
	LinkTypeLocal     = "Local"
)

// MeshState values from the dialog's "state" / "routing state" label.
const (
	MeshStateRouting      = "routing"
	MeshStateDisconnected = "disconnected"
)

// NeighborContainer distinguishes the two sections of the neighborhood fragment.
// Both expose the same detail endpoint, but Local Nodes are reached over LAN/tunnel, not RF.
const (
	ContainerLocal        = "local"
	ContainerNeighborhood = "neighborhood"
)

// Link is an observed radio or tunnel adjacency between two nodes.
type Link struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Container string `json:"container"`
	MAC       string `json:"mac,omitempty"`
	IP        string `json:"ip,omitempty"`
	Type      string `json:"type"`
	Band      string `json:"band"`
	Status    string `json:"status,omitempty"`
	// State is the mesh-reported routing state; it, not a failed ICMP, justifies calling a peer down.
	State      string  `json:"state"`
	Routes     int     `json:"routes"`
	DistanceMI float64 `json:"distance_mi,omitempty"`

	// Dialog-reported for every link kind; no omitempty, as 0% loss is a reading, not absent data.
	RxSuccessPct   float64 `json:"rx_success_pct"`
	TxSuccessPct   float64 `json:"tx_success_pct"`
	ErrorsPct      float64 `json:"errors_pct"`
	RTTMs          float64 `json:"rtt_ms"`
	PingMs         float64 `json:"ping_ms"`
	NeighborPingMs float64 `json:"neighbor_ping_ms"`
	Metric         int     `json:"metric"`

	// Radio-only metrics, null unless the link is RF: "no SNR to report" is not the same fact as 0 dB.
	// ETX is a float because the dialog reads "1.0", "2.3".
	LocalSNR      *int     `json:"local_snr"`
	NeighborSNR   *int     `json:"neighbor_snr"`
	ETX           *float64 `json:"etx"`
	AvgTxPps      *float64 `json:"avg_tx_pps"`
	BitrateRxMbps *float64 `json:"bitrate_rx_mbps"`
	BitrateTxMbps *float64 `json:"bitrate_tx_mbps"`
	RetransmitPct *float64 `json:"retransmit_pct"`

	// Peer identity recovered from the neighbour dialog so a never-expanded node can still be plotted.
	PeerName     string  `json:"peer_name,omitempty"`
	PeerHardware string  `json:"peer_hardware,omitempty"`
	PeerFirmware string  `json:"peer_firmware,omitempty"`
	PeerIP       string  `json:"peer_ip,omitempty"`
	PeerLat      float64 `json:"peer_lat,omitempty"`
	PeerLon      float64 `json:"peer_lon,omitempty"`

	LastSeen  time.Time `json:"last_seen"`
	ScannedAt time.Time `json:"scanned_at"`
}

// Key identifies the edge for de-duplication during a sweep.
func (l *Link) Key() string { return l.From + "\x00" + l.To + "\x00" + l.MAC }
