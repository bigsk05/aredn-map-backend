package aredn

import (
	"context"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"aredn-map/internal/coord"
	"aredn-map/internal/model"
)

// Anchors of the inlined blocks on the status page; everything under /a/status/e/* answers 401.
// The radio, location, mesh, network and services blocks are there without authentication.
const (
	radioAnchor    = `id="radio-and-antenna"`
	locationAnchor = `id="location"`
	servicesAnchor = `id="services"`

	// nextSection ends a scanned block so it never reads into its neighbour's cells.
	radioEnd    = `id="mesh-summary"`
	locationEnd = `<div id="c2c3"`
	servicesEnd = `<div id="local-and-neighbor-devices"`
	noneCell    = "None"
)

var (
	// channelRe pins the "135" / "channel" pair inside #radio-and-antenna.
	channelRe = regexp.MustCompile(`(?s)<div\s+class=["']t["'][^>]*>\s*(\d+)\s*</div>\s*<div\s+class=["']s["'][^>]*>\s*channel`)

	// serviceRe and deviceRe read the inlined Local Services and Local Devices lists.
	serviceRe = regexp.MustCompile(`(?s)<div\s+class=["']service["']\s*>(.*?)</div>`)
	deviceRe  = regexp.MustCompile(`(?s)<div\s+class=["']device["']\s*>(.*?)</div>`)
	hrefRe    = regexp.MustCompile(`(?i)href=["']([^"']+)["']`)
)

// FetchSelf crawls /a/status for the node's own record; every section is optional.
// Hostname is lowercased to match the store key; Name keeps the page title casing.
func (c *Client) FetchSelf(ctx context.Context, hostname string) (*model.Node, error) {
	base := NodeURL(hostname)
	body, answeredFrom, err := c.get(ctx, base+statusPath)
	if err != nil {
		return nil, err
	}

	host := strings.ToLower(strings.TrimSpace(hostname))
	now := time.Now().UTC()
	n := &model.Node{
		Hostname:     host,
		Name:         host,
		URL:          base,
		Band:         model.BandUnresolved,
		DiscoveredAt: now,
		UpdatedAt:    now,
		Source:       "status",
		// Hops is unknown (-1), not 0: the store reserves 0 hops for the crawl root.
		Hops: -1,
	}

	page := string(body)
	pagePairs := pairs(page)

	if name := parseNodeName(page); name != "" {
		n.Name = name
	}

	// Radio / antenna cells: <div class="t">VALUE</div><div class="s">LABEL</div>
	if block := substringBetween(page, radioAnchor, radioEnd, locationAnchor); block != "" {
		p := pairs(block)
		n.Hardware = clean(p["model"])
		n.Channel, _ = parseInt(p["channel"])
		if low, high, ok := parseFreqs(p["frequencies"]); ok {
			n.FreqLow, n.FreqHigh = low, high
		}
		n.ChanBW, _ = parseInt(p["bandwidth"])
		n.TxPowerDBM, _ = parseInt(p["tx power"])
		n.Antenna = clean(p["antenna"])
		n.Azimuth, _ = parseInt(p["azimuth"])
		n.HeightM, _ = parseInt(p["height"])
		n.MaxDistMiles, _ = parseFloat(p["maximum distance (actual)"])
	}

	n.Firmware = parseFirmware(page)
	n.Description = clean(pagePairs["description"])
	n.MeshIP = firstIP(pagePairs["mesh address"])
	parseLocation(page, n)

	n.IsSupernode = model.LooksLikeSupernode(n.Name, n.Hostname, answeredFrom)
	// The redirect target is the node's canonical label, e.g. "localnode" -> "<name>.local.mesh".
	if canon := trimMeshDomain(strings.ToLower(answeredFrom)); canon != "" && canon != n.Hostname {
		n.CanonicalHost = canon
	}
	// A radio exists when the page described any part of it.
	// Frequency alone misfiles 802.11j out-of-band channels (2387-2407 MHz, numbered -4..0) as No RF.
	hasRF := n.FreqLow > 0 || n.Channel != 0 || n.ChanBW > 0
	n.Band = model.DeriveBand(n.FreqLow, n.ChanBW, n.Channel, n.IsSupernode, hasRF)
	n.BandLabel = model.BandLabels[n.Band]

	services, devices := parseLocalBlocks(substringBetween(page, servicesAnchor, servicesEnd))
	n.LocalServices = services
	n.LocalDevices = devices

	c.log.Debug("aredn self record parsed",
		zap.String("hostname", n.Hostname),
		zap.String("name", n.Name),
		zap.String("band", n.Band),
		zap.String("mesh_ip", n.MeshIP),
		zap.String("answered_from", answeredFrom))
	return n, nil
}

// parseNodeName reads <title>TST16 status</title>.
func parseNodeName(page string) string {
	m := titleRe.FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimSuffix(text(m[1]), "status"))
}

// parseFirmware reads the version cell whose value div nests an empty firmware-status div.
func parseFirmware(page string) string {
	m := firmwareRe.FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	return clean(m[1])
}

// parseLocation fills both datums from #location, which states the position three times.
// The worldmap URL is the fallback when the cell markup moves.
func parseLocation(page string, n *model.Node) {
	block := substringBetween(page, locationAnchor, locationEnd, `<div id="c2"`)
	if block == "" {
		block = page
	}
	var lat, lon float64
	switch m := locationRe.FindStringSubmatch(block); {
	case m != nil:
		lat, _ = parseFloat(m[1])
		lon, _ = parseFloat(m[2])
		n.GridLocator = clean(m[3])
	default:
		m = worldmapRe.FindStringSubmatch(block)
		if m == nil {
			return
		}
		lat, _ = parseFloat(m[1])
		lon, _ = parseFloat(m[2])
	}
	if lat == 0 && lon == 0 {
		return
	}
	n.Lat, n.Lon = lat, lon
	n.HasLocation = true
	// coord leaves positions outside mainland China untouched, so a UK node keeps identical pairs.
	n.GCJLat, n.GCJLon = coord.WGS84ToGCJ02(lat, lon)
}

// parseLocalBlocks reads services/devices lists; an empty section says "None", not a one-item list.
func parseLocalBlocks(block string) (services, devices []string) {
	if block == "" {
		return nil, nil
	}
	for _, m := range serviceRe.FindAllStringSubmatch(block, -1) {
		if name := clean(m[1]); name != "" && name != noneCell {
			services = append(services, name)
		}
	}
	for _, m := range deviceRe.FindAllStringSubmatch(block, -1) {
		if name := clean(m[1]); name != "" && name != noneCell {
			devices = append(devices, name)
		}
	}
	return services, devices
}

// clean maps the firmware's "-" placeholder and stray whitespace to "".
func clean(s string) string {
	if absent(s) {
		return ""
	}
	return text(s)
}
