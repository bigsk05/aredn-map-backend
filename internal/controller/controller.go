// Package controller holds the HTTP layer's dependencies and shared endpoint helpers.
package controller

import (
	"context"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"aredn-map/internal/infra/config"
	"aredn-map/internal/model"
	"aredn-map/internal/repository/store"
	"aredn-map/internal/service/scanner"
)

type Deps struct {
	// Ctx is the process lifetime; on cancel the push loop lets its connections go.
	Ctx     context.Context
	Cfg     *config.Config
	Store   *store.Store
	Engine  *scanner.Engine
	Log     *zap.Logger
	Started time.Time
	NextRun func() time.Time
}

// Keep filters nodes by predicate without touching the input slice.
func Keep(in []*model.Node, fn func(*model.Node) bool) []*model.Node {
	out := in[:0:0]
	for _, n := range in {
		if fn(n) {
			out = append(out, n)
		}
	}
	return out
}

// MatchNode is the free-text search over the fields an operator recognizes.
func MatchNode(n *model.Node, q string) bool {
	if contains(n.Name, q) || contains(n.Hostname, q) || contains(n.MeshIP, q) ||
		contains(n.Description, q) || contains(n.Hardware, q) {
		return true
	}
	for _, s := range n.Services {
		if contains(s, q) {
			return true
		}
	}
	return false
}

func contains(hay, needle string) bool {
	return strings.Contains(strings.ToLower(hay), needle)
}

// ProgressText renders the running phase as a counter line for the status payload.
func ProgressText(s *model.ScanStatus) string {
	switch s.Phase {
	case model.PhaseDiscovery:
		return "discovered " + strconv.Itoa(s.Completed)
	case model.PhaseProbe:
		return "icmp " + counter(s)
	case model.PhaseDetail:
		return "detail " + counter(s)
	case model.PhaseRevisit:
		return "revisit " + counter(s)
	default:
		return s.Message
	}
}

func counter(s *model.ScanStatus) string {
	return strconv.Itoa(s.NodeProgress) + "/" + strconv.Itoa(s.NodeTotal)
}

// TimeFromRFC3339 parses a wire timestamp, treating anything unreadable as zero.
func TimeFromRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// NextSweep reports the scheduled sweep, or the zero time when there is none.
func (d Deps) NextSweep() time.Time {
	if d.NextRun == nil {
		return time.Time{}
	}
	return d.NextRun()
}
