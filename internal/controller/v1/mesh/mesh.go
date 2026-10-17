// Package mesh serves the scanned nodes and links with the query filters a client pushes down.
package mesh

import (
	"sort"
	"strings"

	"github.com/gofiber/fiber/v3"

	"aredn-map/internal/controller"
	"aredn-map/internal/model"
	"aredn-map/internal/service/scanner"
)

func Nodes(d controller.Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		nodes := d.Store.Nodes()
		if q := strings.ToLower(strings.TrimSpace(c.Query("q"))); q != "" {
			nodes = controller.Keep(nodes, func(n *model.Node) bool { return controller.MatchNode(n, q) })
		}
		if b := c.Query("band"); b != "" && b != "all" {
			nodes = controller.Keep(nodes, func(n *model.Node) bool { return n.Band == b })
		}
		switch c.Query("state") {
		case "online":
			nodes = controller.Keep(nodes, func(n *model.Node) bool { return n.Online })
		case "offline":
			nodes = controller.Keep(nodes, func(n *model.Node) bool { return !n.Online })
		case "located":
			nodes = controller.Keep(nodes, func(n *model.Node) bool { return n.HasLocation })
		case "supernode":
			nodes = controller.Keep(nodes, func(n *model.Node) bool { return n.IsSupernode })
		case "disconnected":
			nodes = controller.Keep(nodes, func(n *model.Node) bool { return n.Disconnected })
		}
		return c.JSON(fiber.Map{"nodes": nodes, "total": len(nodes)})
	}
}

func Links(d controller.Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		links := d.Store.Links()
		if t := c.Query("type"); t != "" && t != "all" {
			filtered := links[:0:0]
			for _, l := range links {
				if l.Type == t {
					filtered = append(filtered, l)
				}
			}
			links = filtered
		}
		if from := scanner.NormalizeHost(c.Query("node")); from != "" {
			filtered := links[:0:0]
			for _, l := range links {
				if l.From == from || l.To == from {
					filtered = append(filtered, l)
				}
			}
			links = filtered
		}
		return c.JSON(fiber.Map{"links": links, "total": len(links)})
	}
}

func Topology(d controller.Deps) fiber.Handler {
	return func(c fiber.Ctx) error { return c.JSON(d.Store.Topology()) }
}

// Lookup answers which node owns an address: mesh address, published service address, or LAN alias.
func Lookup(d controller.Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		q := strings.TrimSpace(c.Query("ip"))
		if q == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "ip query parameter required"})
		}
		hits := make([]fiber.Map, 0, 4)
		for _, n := range d.Store.Nodes() {
			if n.MeshIP == q {
				hits = append(hits, fiber.Map{"match": "mesh_ip", "node": n})
				continue
			}
			for _, s := range n.Services {
				if before, ok := strings.CutSuffix(s, "|"+q); ok {
					hits = append(hits, fiber.Map{"match": "service", "service": before, "node": n})
				}
			}
			for _, a := range n.Aliases {
				if before, ok := strings.CutSuffix(a, "|"+q); ok {
					hits = append(hits, fiber.Map{"match": "alias", "alias": before, "node": n})
				}
			}
		}
		sort.Slice(hits, func(i, j int) bool {
			return hits[i]["node"].(*model.Node).Name < hits[j]["node"].(*model.Node).Name
		})
		return c.JSON(fiber.Map{"query": q, "results": hits})
	}
}
