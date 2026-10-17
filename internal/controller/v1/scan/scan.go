// Package scan exposes sweep control: the enable flag, starting one, cancelling one, single probes.
package scan

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"

	"aredn-map/internal/controller"
	"aredn-map/internal/service/scanner"
)

func Enable(d controller.Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := c.Bind().JSON(&body); err != nil || body.Enabled == nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": `body {"enabled": bool} required`})
		}
		d.Engine.SetEnabled(*body.Enabled)
		d.Log.Info("scan enabled flag changed", zap.Bool("enabled", *body.Enabled))
		return c.JSON(fiber.Map{"scanning_enabled": *body.Enabled})
	}
}

// Run starts a sweep and answers once under way; progress comes via status or the push socket.
func Run(d controller.Deps, kind string) fiber.Handler {
	return func(c fiber.Ctx) error {
		done := make(chan error, 1)
		go func() { done <- d.Engine.Sweep(context.Background(), kind) }()
		select {
		case err := <-done:
			if errors.Is(err, scanner.ErrAlreadyRunning) {
				return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "sweep already running"})
			}
			return c.JSON(fiber.Map{"started": err == nil, "status": d.Engine.Status()})
		case <-time.After(600 * time.Millisecond):
			return c.JSON(fiber.Map{"started": true, "status": d.Engine.Status()})
		}
	}
}

func Stop(d controller.Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		d.Engine.Cancel()
		return c.JSON(fiber.Map{"status": d.Engine.Status()})
	}
}

// Probe answers a node card's re-probe request with the row as it stands afterwards.
func Probe(d controller.Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var body struct {
			Host string `json:"host"`
		}
		if err := c.Bind().JSON(&body); err != nil || strings.TrimSpace(body.Host) == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": `body {"host": string} required`})
		}
		ctx, cancel := context.WithTimeout(c.Context(), d.Cfg.ICMP.OverallTimeout+2*time.Second)
		defer cancel()
		n, ok := d.Engine.ProbeNow(ctx, body.Host)
		if !ok {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "unknown node"})
		}
		return c.JSON(fiber.Map{"node": n})
	}
}
