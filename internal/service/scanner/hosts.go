package scanner

import (
	"strings"
)

func NormalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimPrefix(h, "https://")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	h = strings.TrimSuffix(h, ".")
	h = strings.TrimSuffix(h, ".local.mesh")
	return h
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}
