package aredn

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.gh.ink/toolbox/expr"
	"go.uber.org/zap"

	"aredn-map/internal/model"
)

// Anchors and cells of the two htmx fragments that describe adjacencies.
var (
	// blockRe splits the fragment into its two .noctrl sections, in document order.
	blockRe = regexp.MustCompile(`(?s)<div\s+class=["']noctrl["']`)

	// rowRe matches a data row's opening div: quality word in class, peer MAC in hx-get.
	// Header rows carry no hx-get, so they can never match.
	rowRe = regexp.MustCompile(`(?s)<div\s+class=["']ctrl cols status ([a-zA-Z_-]+)["'][^>]*?hx-get=["']neighbor-device\?m=([0-9a-fA-F:.]{6,})["']`)

	// statsRe marks the start of a row's seven leaf cells.
	statsRe = regexp.MustCompile(`(?s)<div\s+class=['"]ts cols stats['"]\s*>`)

	// cellRe reads one leaf cell; the stats block has no nested divs, so a flat match works.
	cellRe = regexp.MustCompile(`(?s)<div[^>]*>(.*?)</div>`)

	// anchorRe reads a row's peer href plus label; the trailing class absorbs other attributes.
	anchorRe = regexp.MustCompile(`(?s)<a\b[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`)

	// detailPairRe reads the dialog's "i" value/label cells; text() flattens nested markup.
	detailPairRe = regexp.MustCompile(`(?s)<div\s+class=["']i["']\s*>\s*(.*?)\s*</div>\s*<div\s*>(.*?)</div>\s*</div>`)

	// dialogHeaderRe distinguishes the "Neighborhood Node" dialog from the "Local Node" one.
	dialogHeaderRe = regexp.MustCompile(`(?s)<div\s+class=["']t["']\s*>\s*(Local Node|Neighborhood Node)\s*</div>`)

	// peerAnchorRe is the authoritative peer link of a dialog, inside <div class="o">.
	peerAnchorRe = regexp.MustCompile(`(?s)<div\s+class=["']o["']\s*>(.*?)</div>`)

	// statLabels is the header order the cells follow; RF sections spell the first two lq|nlq.
	statLabels = []string{"rx", "rtt", "snr", "n snr", "errors", "mbps", "miles"}
)

// FetchNeighbors crawls the adjacency fragment for one link per data row of both sections.
// withDetail enriches each row from /a/neighbor-device?m=<mac>, at most concurrency in flight.
func (c *Client) FetchNeighbors(ctx context.Context, hostname string, withDetail bool, concurrency int) ([]model.Link, error) {
	host := trimMeshDomain(hostname)
	body, _, err := c.get(ctx, NodeURL(host)+neighborsPath)
	if err != nil {
		return nil, err
	}

	rows := parseNeighborFragment(string(body))
	if len(rows) == 0 {
		c.log.Debug("aredn node reports no neighbors", zap.String("hostname", host))
		return nil, nil
	}

	now := time.Now().UTC()
	links := make([]model.Link, len(rows))
	for i, r := range rows {
		l := model.Link{
			From:      host,
			To:        r.to,
			Container: r.container,
			MAC:       r.mac,
			Status:    r.status,
			Type:      r.hintType(),
			ScannedAt: now,
			LastSeen:  now,
		}
		applyFragmentStats(&l, r.stats)
		links[i] = l
	}

	if withDetail {
		c.enrich(ctx, host, rows, links, concurrency)
	}
	return links, nil
}

// enrich fetches one dialog per row; a failed dialog keeps the fragment's values.
func (c *Client) enrich(ctx context.Context, host string, rows []neighborRow, links []model.Link, concurrency int) {
	if concurrency <= 0 {
		concurrency = 1
	}
	// Never spawn more workers than there are rows to fetch.
	workers := expr.Ternary(concurrency > len(rows), len(rows), concurrency)

	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			d, err := c.fetchDevice(ctx, host, rows[i].mac)
			if err != nil {
				c.log.Warn("aredn neighbor detail unavailable",
					zap.String("hostname", host),
					zap.String("mac", rows[i].mac),
					zap.String("peer", rows[i].to),
					zap.Error(err))
				return
			}
			applyDetail(&links[i], d)
		}(i)
	}
	wg.Wait()
}

// fetchDevice pulls one /a/neighbor-device dialog fragment.
func (c *Client) fetchDevice(ctx context.Context, host, mac string) (map[string]string, error) {
	if mac == "" {
		return nil, fmt.Errorf("aredn: row at %s has no mac address", NodeURL(host))
	}
	body, _, err := c.get(ctx, NodeURL(host)+devicePath+"?m="+mac)
	if err != nil {
		return nil, err
	}
	return parseDeviceDialog(string(body)), nil
}

// neighborRow is one data row of the adjacency fragment.
type neighborRow struct {
	container string // model.ContainerLocal or model.ContainerNeighborhood
	status    string // excellent / okay / poor ...
	mac       string
	to        string
	stats     []string
	isLocal   bool // the row came from the Local Nodes section
	hasWiFi   bool // the row carries a wifi icon
	hasGlobe  bool // the row carries a globe (tunnel) icon
}

// hintType is the pre-dialog type: wifi rows are RF, globe rows tunnels, LAN peers Local.
func (r neighborRow) hintType() string {
	switch {
	case r.hasWiFi:
		return model.LinkTypeRF
	case r.hasGlobe:
		return model.LinkTypeWireguard
	case r.isLocal:
		return model.LinkTypeLocal
	default:
		// Iconless rows are DtD on 4.x and tunnels on older firmware; the dialog settles it.
		return model.LinkTypeDtD
	}
}

// parseNeighborFragment walks the .noctrl blocks in document order and returns their data rows.
func parseNeighborFragment(fragment string) []neighborRow {
	var rows []neighborRow
	bounds := blockRe.FindAllStringIndex(fragment, -1)
	for i := range bounds {
		start := bounds[i][0]
		end := len(fragment)
		if i+1 < len(bounds) {
			end = bounds[i+1][0]
		}
		block := fragment[start:end]
		container, isLocal := sectionKind(block)
		if container == "" {
			continue
		}
		rows = append(rows, parseRows(block, container, isLocal)...)
	}
	return rows
}

// sectionKind maps a section-title onto a container; unknown sections yield "".
func sectionKind(block string) (container string, isLocal bool) {
	m := sectionTitleRe.FindStringSubmatch(block)
	if m == nil {
		return "", false
	}
	switch strings.ToLower(text(m[1])) {
	case "local nodes":
		return model.ContainerLocal, true
	case "neighborhood nodes":
		return model.ContainerNeighborhood, false
	default:
		return "", false
	}
}

// parseRows extracts the data rows of one section.
func parseRows(block, container string, isLocal bool) []neighborRow {
	heads := rowRe.FindAllStringSubmatchIndex(block, -1)
	rows := make([]neighborRow, 0, len(heads))
	for i, loc := range heads {
		status := strings.ToLower(block[loc[2]:loc[3]])
		mac := strings.ToLower(block[loc[4]:loc[5]])
		start := loc[0]
		end := len(block)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		row := block[start:end]

		to := ""
		if a := anchorRe.FindStringSubmatch(row); a != nil {
			to = trimMeshDomain(HostnameFromURL(a[1]))
			if to == "" {
				to = trimMeshDomain(text(a[2]))
			}
		}

		var stats []string
		if m := statsRe.FindStringIndex(row); m != nil {
			tail := row[m[1]:]
			for _, cell := range cellRe.FindAllStringSubmatch(tail, len(statLabels)) {
				stats = append(stats, text(cell[1]))
			}
		}

		rows = append(rows, neighborRow{
			container: container,
			status:    status,
			mac:       mac,
			to:        to,
			stats:     stats,
			isLocal:   isLocal,
			hasWiFi:   strings.Contains(row, "icon wifi"),
			hasGlobe:  strings.Contains(row, "icon globe"),
		})
	}
	return rows
}

// applyFragmentStats maps the seven cells onto a link in header order.
// On RF rows cell 2 is nlq, so it becomes RTTMs only when it carries no percent sign.
func applyFragmentStats(l *model.Link, stats []string) {
	get := func(i int) string {
		if i < len(stats) {
			return stats[i]
		}
		return ""
	}
	if v, ok := parseFloat(get(0)); ok {
		l.RxSuccessPct = v
	}
	second := get(1)
	if strings.Contains(second, "%") {
		if v, ok := parseFloat(second); ok {
			l.TxSuccessPct = v
		}
	} else if v, ok := parseFloat(second); ok {
		l.RTTMs = v
	}
	if v, ok := parseInt(get(2)); ok {
		l.LocalSNR = new(v)
	}
	if v, ok := parseInt(get(3)); ok {
		l.NeighborSNR = new(v)
	}
	if v, ok := parseFloat(get(4)); ok {
		l.ErrorsPct = v
	}
	// Cell 5 is the aggregate mbps column, superseded by the dialog's per-direction bitrates.
	if v, ok := parseFloat(get(6)); ok {
		l.DistanceMI = v
	}
}

// applyDetail overlays the dialog's authoritative values on a link.
func applyDetail(l *model.Link, d map[string]string) {
	if len(d) == 0 {
		return
	}
	if t := clean(d["type"]); t != "" {
		l.Type = normalizeLinkType(t)
	}
	switch d[kindKey] {
	case "local node":
		l.Container = model.ContainerLocal
	case "neighborhood node":
		l.Container = model.ContainerNeighborhood
	}
	if l.To == "" {
		l.To = trimMeshDomain(HostnameFromURL(d[peerHrefKey]))
	}
	l.PeerName = clean(d[peerNameKey])
	l.IP = firstIP(d["ip address"])
	l.PeerIP = l.IP
	if v, ok := parseFloat(d["latitude"]); ok {
		l.PeerLat = v
	}
	if v, ok := parseFloat(d["longitude"]); ok {
		l.PeerLon = v
	}
	if hw := clean(d["model"]); hw != "" {
		l.PeerHardware = hw
	}
	if fw := clean(d["firmware"]); fw != "" {
		l.PeerFirmware = fw
	}
	if v, ok := parseFloat(d["distance"]); ok {
		l.DistanceMI = v
	}
	// RF dialogs label the success cells "lq | rx success" and "nlq | tx success"; others are bare.
	if v, ok := parseFloat(firstPresent(d, "lq | rx success", "rx success")); ok {
		l.RxSuccessPct = v
	}
	if v, ok := parseFloat(firstPresent(d, "nlq | tx success", "tx success")); ok {
		l.TxSuccessPct = v
	}
	// ETX is a float ("1.0", "2.3"); rounding it would merge distinct link qualities.
	if v, ok := parseFloat(d["etx"]); ok {
		l.ETX = new(v)
	}
	if v, ok := parseFloat(d["ping time"]); ok {
		l.PingMs = v
	}
	if v, ok := parseFloat(d["round trip time"]); ok {
		l.RTTMs = v
	}
	if v, ok := parseFloat(d["neighbor ping time"]); ok {
		l.NeighborPingMs = v
	}
	if v, ok := parseFloat(d["neighbor errors"]); ok {
		l.ErrorsPct = v
	}
	if v, ok := parseFloat(d["avg tx"]); ok {
		l.AvgTxPps = new(v)
	}
	if v, ok := parseInt(d["local snr"]); ok {
		l.LocalSNR = new(v)
	}
	if v, ok := parseInt(d["neighbor snr"]); ok {
		l.NeighborSNR = new(v)
	}
	if v, ok := parseFloat(d["physical rx bitrate"]); ok {
		l.BitrateRxMbps = new(v)
	}
	if v, ok := parseFloat(d["physical tx bitrate"]); ok {
		l.BitrateTxMbps = new(v)
	}
	if v, ok := parseFloat(d["tx retransmissions"]); ok {
		l.RetransmitPct = new(v)
	}
	if v, ok := firstInt(d, "metric", "babel metric", "rx cost"); ok {
		l.Metric = v
	}
	// "state" on RF dialogs, "routing state" on the others; the mesh verdict decides offline.
	l.State = text(firstPresent(d, "state", "routing state"))
	if v, ok := firstInt(d, "routes", "babel routes", "olsr routes"); ok {
		l.Routes = v
	}
	if ls := d["last seen"]; ls != "" {
		l.LastSeen = parseLastSeen(ls, time.Now().UTC())
	}
	// Band is left to the caller: neither row nor dialog states the peer's mesh frequency.
}

// normalizeLinkType maps the dialog's spelling onto the model vocabulary.
func normalizeLinkType(s string) string {
	// Some builds spell the radio kind "RF (5GHz)"; topology dedupes by (endpoints, type).
	if i := strings.IndexByte(s, '('); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	switch strings.ToLower(s) {
	case "rf":
		return model.LinkTypeRF
	case "dtd":
		return model.LinkTypeDtD
	case "wireguard", "wiregurd", "wg":
		return model.LinkTypeWireguard
	default:
		return s
	}
}

// Pseudo-labels for non-cell facts; the NUL prefix prevents collision with a real label.
const (
	peerHrefKey = "\x00peer"
	peerNameKey = "\x00peer-name"
	kindKey     = "\x00kind"
)

// parseDeviceDialog flattens a dialog into label -> value, plus the peer anchor and section kind.
func parseDeviceDialog(dialog string) map[string]string {
	out := make(map[string]string, 40)
	for _, m := range detailPairRe.FindAllStringSubmatch(dialog, -1) {
		label := strings.ToLower(text(m[2]))
		if label == "" {
			continue
		}
		if _, seen := out[label]; !seen {
			out[label] = text(m[1])
		}
	}
	if h := peerAnchorRe.FindStringSubmatch(dialog); h != nil {
		switch a := anchorRe.FindStringSubmatch(h[1]); {
		case a != nil:
			out[peerHrefKey] = a[1]
			out[peerNameKey] = text(a[2])
		default:
			if href := hrefRe.FindStringSubmatch(h[1]); href != nil {
				out[peerHrefKey] = href[1]
			}
			out[peerNameKey] = text(h[1])
		}
	}
	if k := dialogHeaderRe.FindStringSubmatch(dialog); k != nil {
		out[kindKey] = strings.ToLower(text(k[1]))
	}
	return out
}

// firstPresent returns the first non-empty value among keys.
func firstPresent(d map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := d[k]; !absent(v) {
			return v
		}
	}
	return ""
}

// firstInt returns the first key that carries a number.
func firstInt(d map[string]string, keys ...string) (int, bool) {
	for _, k := range keys {
		if v, ok := parseInt(d[k]); ok {
			return v, true
		}
	}
	return 0, false
}

// trimMeshDomain reduces a label to the bare node name used by Hostname, From and To.
func trimMeshDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, "."+MeshDomain)
	return s
}
