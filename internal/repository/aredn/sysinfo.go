package aredn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"go.uber.org/zap"
)

// sysinfoPath is the JSON face of a 4.x node, parsed with encoding/json.
// The ?sections query picks parts; unrequested sections are absent, keeping a crawl near 10 KB.
const sysinfoPath = "/a/sysinfo"

// ErrNoSysinfo means the node serves no JSON face, in practice firmware 3.x.
// 3.x answers 200 with an HTML error page, so callers branch on content type, not status code.
var ErrNoSysinfo = errors.New("aredn: node serves no /a/sysinfo JSON")

// SysinfoHost is one entry of the mesh node table or the wider host table.
type SysinfoHost struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// SysinfoLink names the peer on the other end of an interface.
// The LinkInfo key is the link-local address, which is how the status page labels the row.
type SysinfoLink struct {
	Hostname  string `json:"hostname"`
	LinkType  string `json:"linkType"`
	Interface string `json:"interface"`
}

// SysinfoTracker is one babelmesh neighbour; pointer metrics keep null apart from a measured 0.
type SysinfoTracker struct {
	Type      string   `json:"type"`
	Device    string   `json:"device"`
	MAC       string   `json:"mac"`
	IPv6Link  string   `json:"ipv6ll"`
	LastSeen  *int     `json:"lastseen"`
	LastUp    *int     `json:"lastup"`
	Quality   *float64 `json:"quality"`
	LQ        *float64 `json:"lq"`
	RevLQ     *float64 `json:"rev_lq"`
	TxPrice   *float64 `json:"txcost"`
	RxPrice   *float64 `json:"rxcost"`
	AvgLQ     *float64 `json:"avg_lq"`
	RTT       *float64 `json:"rtt"`
	Metric    *float64 `json:"metric"`
	Routable  *bool    `json:"routable"`
	RouteCnt  *int     `json:"route_count"`
	TxPackets *float64 `json:"tx_packets"`
	TxFail    *float64 `json:"tx_fail"`
	SNR       *float64 `json:"snr"`
	RevSNR    *float64 `json:"rev_snr"`
	MeshVLAN  *int     `json:"meshvlan"`
	RemoteRF  *bool    `json:"remoterf"`
}

// Sysinfo is the subset of /a/sysinfo read here; an absent section means "not requested".
type Sysinfo struct {
	APIVersion string                 `json:"api_version"`
	Node       string                 `json:"node"`
	IP         string                 `json:"ip"`
	Lat        *float64               `json:"lat"`
	Lon        *float64               `json:"lon"`
	GridSquare string                 `json:"gridsquare"`
	LinkInfo   map[string]SysinfoLink `json:"link_info"`
	Nodes      []SysinfoHost          `json:"nodes"`
	Hosts      []SysinfoHost          `json:"hosts"`
	Services   []struct {
		Name     string `json:"name"`
		IP       string `json:"ip"`
		Link     string `json:"link"`
		Protocol string `json:"protocol"`
	} `json:"services"`
	LQM struct {
		Enabled bool `json:"enabled"`
		Info    struct {
			Trackers map[string]SysinfoTracker `json:"trackers"`
		} `json:"info"`
	} `json:"lqm"`
}

// sectionQuery names the parts of the payload a caller wants.
func sectionQuery(sections ...string) string {
	v := url.Values{}
	for _, s := range sections {
		v.Set(s, "1")
	}
	return v.Encode()
}

// FetchSysinfo reads the JSON face; firmware without one returns a wrapped ErrNoSysinfo.
func (c *Client) FetchSysinfo(ctx context.Context, hostname string, sections ...string) (*Sysinfo, error) {
	base := NodeURL(trimMeshDomain(hostname))
	target := base + sysinfoPath
	if q := sectionQuery(sections...); q != "" {
		target += "?" + q
	}
	body, _, err := c.get(ctx, target)
	if err != nil {
		return nil, err
	}
	// A 3.x node answers 200 with an HTML page, so the first byte is the honest discriminator.
	if trimmed := strings.TrimLeft(string(body), " \t\r\n"); !strings.HasPrefix(trimmed, "{") {
		return nil, fmt.Errorf("aredn: %s: %w", target, ErrNoSysinfo)
	}
	var out Sysinfo
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("aredn: decode %s: %w", target, err)
	}
	c.log.Debug("aredn sysinfo fetched",
		zap.String("hostname", trimMeshDomain(hostname)),
		zap.String("api_version", out.APIVersion),
		zap.Int("bytes", len(body)),
		zap.Int("nodes", len(out.Nodes)),
		zap.Int("trackers", len(out.LQM.Info.Trackers)))
	return &out, nil
}
