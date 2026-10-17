package model

import "time"

type TopologyResponse struct {
	Nodes       []TopologyNode `json:"nodes"`
	Links       []Link         `json:"links"`
	GeneratedAt time.Time      `json:"generated_at"`
	// UnconnectedCount counts excluded directory-entry nodes with no observed edge on either endpoint.
	UnconnectedCount int `json:"unconnected_nodes"`
}

// TopologyNode is the trimmed node shape used by the graph renderer.
type TopologyNode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Band        string `json:"band"`
	BandLabel   string `json:"band_label"`
	Color       string `json:"color"`
	IsSupernode bool   `json:"is_supernode"`
	Hops        int    `json:"hops"`
	ETX         int    `json:"etx"`
	Online      bool   `json:"online"`
	// Probed says an ICMP verdict exists, so un-probed nodes are not shown as offline.
	Probed bool `json:"probed"`
	// Disconnected mirrors Node.Disconnected: the mesh's own verdict that the link is down.
	MeshState    string  `json:"mesh_state"`
	Disconnected bool    `json:"disconnected"`
	LatencyMs    float64 `json:"latency_ms"`
	// No omitempty: the frontend declares lat/lon required, and 0,0 is the honest "no position".
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	NeighborCnt int     `json:"neighbor_cnt"`
}
