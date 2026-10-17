// Package system serves the read-only picture of the scanner and the settings the frontend needs.
package system

import (
	"time"

	"github.com/gofiber/fiber/v3"

	"aredn-map/internal/controller"
	"aredn-map/internal/meta"
	"aredn-map/internal/model"
)

func Health(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"ok": true})
}

// Payload is the whole status picture; HTTP route and WebSocket push serve the same shape.
func Payload(d controller.Deps) model.StatusResponse {
	st := d.Store
	status := d.Engine.Status()
	if status.Status == "running" && status.NodeTotal > 0 {
		status.Message = controller.ProgressText(&status)
	}
	return model.StatusResponse{
		Counts:          st.Counts(),
		Scan:            status,
		ScanningEnabled: d.Engine.Enabled(),
		Seeds:           st.Seeds(),
		BandCounts:      st.BandCounts(),
		LinkTypeCounts:  st.LinkTypeCounts(),
		ICMPMode:        d.Cfg.ICMP.Mode,
		ServerTime:      time.Now(),
		Cron:            d.Cfg.Scan.Cron,
		LastSweepAt:     controller.TimeFromRFC3339(status.CompletedAt),
		NextSweepAt:     d.NextSweep(),
		UptimeSeconds:   int64(time.Since(d.Started).Seconds()),
		Version:         meta.Version,
	}
}

func Status(d controller.Deps) fiber.Handler {
	return func(c fiber.Ctx) error { return c.JSON(Payload(d)) }
}

// Config serves runtime settings so no key or centre coordinate is baked into the static bundle.
func Config(d controller.Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		m := d.Cfg.Map
		return c.JSON(fiber.Map{
			"amap_key":           m.AmapKey,
			"amap_security_code": m.AmapSecurityCode,
			"center_lat":         m.CenterLat,
			"center_lon":         m.CenterLon,
			"zoom":               m.Zoom,
			"cron":               d.Cfg.Scan.Cron,
			"seeds":              d.Store.Seeds(),
			"max_hops":           d.Cfg.Scan.MaxHops,
			"icmp_mode":          d.Cfg.ICMP.Mode,
			"band_labels":        model.BandLabels,
			"band_colors":        model.BandColors,
			"version":            meta.Version,
		})
	}
}
