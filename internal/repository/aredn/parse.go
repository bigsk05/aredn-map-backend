package aredn

import (
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.gh.ink/toolbox/data"
)

// The fragments mix quote styles and pretty-print across newlines, so every pattern allows both.
// Each is anchored on a label the firmware emits literally; testdata/ fixtures pin them down.
var (
	// tagRe removes anything that looks like a tag so a value cell collapses to text.
	tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

	// spaceRe collapses whitespace runs, including the U+00A0 &nbsp; in dns and load cells.
	spaceRe = regexp.MustCompile(`\s+`)

	// pairRe captures the repeating value/label pairs: class "t" is the term, "s" the description.
	pairRe = regexp.MustCompile(`(?s)<div\s+class=["']t["'][^>]*>(.*?)</div>\s*<div\s+class=["']s[^"']*["'][^>]*>(.*?)</div>`)

	// titleRe yields the node name from <title>TST16 status</title>.
	titleRe = regexp.MustCompile(`(?s)<title>\s*(.*?)\s*</title>`)

	// firmwareRe anchors the version cell whose value div nests an empty firmware-status div.
	firmwareRe = regexp.MustCompile(`(?s)<div\s+class=["']firmware["'].*?<div\s+class=["']t["'][^>]*>(.*?)<div\s+class=["']firmware-status`)

	// locationRe captures the "59.934773, -2.425156" cell plus the sibling grid-locator div.
	locationRe = regexp.MustCompile(`(?s)<div\s+class=["']t["'][^>]*>\s*([+-]?\d+(?:\.\d+)?)\s*,\s*([+-]?\d+(?:\.\d+)?)\s*</div>\s*<div\s+style=["']flex:0["'][^>]*>\s*([A-Za-z0-9]{4,8})\s*</div>`)

	// worldmapRe is the fallback coordinate source: worldmap.arednmesh.org/#12/59.934773/-2.425156.
	worldmapRe = regexp.MustCompile(`worldmap\.arednmesh\.org/#\d+/([+-]?\d+(?:\.\d+)?)/([+-]?\d+(?:\.\d+)?)`)

	// freqRangeRe reads "5670 - 5680 MHz", "5670 to 5680" and fractional 9xx cells.
	// Endpoints are truncated to whole MHz because model.Node stores integers.
	freqRangeRe = regexp.MustCompile(`(\d{3,6}(?:\.\d+)?)\s*(?:-|–|—|to)\s*(\d{3,6}(?:\.\d+)?)`)
	numberRe    = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)
	integerRe   = regexp.MustCompile(`[-+]?\d+`)
	ipRe        = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)

	// sectionTitleRe names a .noctrl block: "Local Nodes", "Neighborhood Nodes", "Radio", "Mesh".
	sectionTitleRe = regexp.MustCompile(`(?s)<div\s+class=["']section-title["'][^>]*>\s*(.*?)\s*</div>`)
)

// absent reports AREDN's "no value" strings ("-", "--", "—", "n/a", empty); never a 0.
func absent(s string) bool {
	s = text(s)
	return s == "" || s == "-" || s == "--" || s == "—" || s == "n/a" || s == "N/A"
}

// text strips tags, unescapes entities and collapses whitespace.
func text(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(html.UnescapeString(s), "\u00a0", " ")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// pairs indexes value/label pairs by label, first occurrence winning.
// Labels repeat across status page sections, so callers scope the block first.
func pairs(block string) map[string]string {
	out := make(map[string]string, 16)
	for _, m := range pairRe.FindAllStringSubmatch(block, -1) {
		label := strings.ToLower(text(m[2]))
		if label == "" {
			continue
		}
		if _, seen := out[label]; seen {
			continue
		}
		out[label] = text(m[1])
	}
	return out
}

// firstIP returns the dotted-quad in a cell such as "10.100.3.254 / 32".
func firstIP(s string) string {
	if s == "" {
		return ""
	}
	return ipRe.FindString(text(s))
}

// parseFloat pulls the leading decimal from a unit-bearing string; absence is reported, not 0.
func parseFloat(s string) (float64, bool) {
	if absent(s) {
		return 0, false
	}
	m := numberRe.FindString(text(s))
	if m == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// parseInt is parseFloat truncated toward the nearest integer, for cells like "27.7 Mbps".
func parseInt(s string) (int, bool) {
	if absent(s) {
		return 0, false
	}
	m := integerRe.FindString(text(s))
	if m == "" {
		return 0, false
	}
	// data.Atoi keeps the conversion identical to the rest of the backend.
	v, err := data.Atoi(m)
	if err != nil {
		return 0, false
	}
	return v, true
}

// roundInt converts a decimal cell to the int a model field wants (1.15 -> 1, 2.6 -> 3).
func roundInt(s string) (int, bool) {
	f, ok := parseFloat(s)
	if !ok {
		return 0, false
	}
	return int(math.Round(f)), true
}

// parseFreqs splits "5670 - 5680 MHz"; a single-frequency cell sets both ends.
func parseFreqs(s string) (low, high int, ok bool) {
	if absent(s) {
		return 0, 0, false
	}
	s = text(s)
	if m := freqRangeRe.FindStringSubmatch(s); m != nil {
		low, _ = parseInt(m[1])
		high, _ = parseInt(m[2])
		return low, high, low > 0
	}
	v, ok := parseInt(s)
	if !ok {
		return 0, 0, false
	}
	return v, v, true
}

// parseLastSeen turns "now", "47 hours", "5 minutes" prose into a timestamp, else fallback.
func parseLastSeen(s string, fallback time.Time) time.Time {
	if absent(s) {
		return fallback
	}
	s = strings.ToLower(text(s))
	if strings.Contains(s, "now") {
		return fallback
	}
	n, ok := parseInt(s)
	if !ok || n <= 0 {
		return fallback
	}
	switch {
	case strings.Contains(s, "second"):
		return fallback.Add(-time.Duration(n) * time.Second)
	case strings.Contains(s, "minute"):
		return fallback.Add(-time.Duration(n) * time.Minute)
	case strings.Contains(s, "hour"):
		return fallback.Add(-time.Duration(n) * time.Hour)
	case strings.Contains(s, "day"):
		return fallback.Add(-time.Duration(n) * 24 * time.Hour)
	default:
		return fallback
	}
}

// substringBetween returns the body from start to the first of ends, never past its own markup.
func substringBetween(h, start string, ends ...string) string {
	i := strings.Index(h, start)
	if i < 0 {
		return ""
	}
	i += len(start)
	stop := len(h)
	for _, e := range ends {
		if j := strings.Index(h[i:], e); j >= 0 && i+j < stop {
			stop = i + j
		}
	}
	return h[i:stop]
}
