package handler

import (
	"github.com/gofiber/fiber/v3"

	"aredn-map/internal/controller"
	"aredn-map/internal/controller/v1/mesh"
	"aredn-map/internal/controller/v1/scan"
	"aredn-map/internal/controller/v1/system"
	"aredn-map/internal/service/scanner"
)

// Register is the whole API surface in one table.
func Register(api fiber.Router, d controller.Deps) {
	hub := newWSHub()
	go hub.run(d.Ctx, d)

	api.Get("/health", system.Health)
	api.Get("/status", system.Status(d))
	api.Get("/config", system.Config(d))

	api.Get("/nodes", mesh.Nodes(d))
	api.Get("/links", mesh.Links(d))
	api.Get("/topology", mesh.Topology(d))
	api.Get("/lookup", mesh.Lookup(d))

	api.Get("/ws", handleWS(d, hub))

	api.Post("/scan/enable", scan.Enable(d))
	api.Post("/scan/run", scan.Run(d, scanner.KindFull))
	api.Post("/scan/incremental", scan.Run(d, scanner.KindIncremental))
	api.Post("/scan/stop", scan.Stop(d))
}
