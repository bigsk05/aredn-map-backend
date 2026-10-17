package model

import (
	"time"
)

// Counts is the summary card block.
type Counts struct {
	Total        int `json:"total"`
	Online       int `json:"online"`
	Offline      int `json:"offline"`
	Disconnected int `json:"disconnected"`
	Unprobed     int `json:"unprobed"`
	Located      int `json:"located"`
	Supernodes   int `json:"supernodes"`
	Pending      int `json:"pending"`
	// Failed is how many known nodes never gave up a status page, however often we looked.
	Links       int `json:"links"`
	Services    int `json:"services"`
	Failed      int `json:"failed"`
	Unreachable int `json:"unreachable"`
	Crawled     int `json:"crawled"`
}

// Phase describes what the engine is doing right now.
const (
	PhaseIdle      = "idle"
	PhaseDiscovery = "discovery"
	PhaseDetail    = "detail"
	PhaseProbe     = "probe"
	PhaseRevisit   = "revisit"
	PhaseCompleted = "completed"
	PhaseFailed    = "failed"
	PhaseStopped   = "stopped"
)

// ScanStatus reports the current and previous sweep.
type ScanStatus struct {
	ID            int    `json:"id"`
	Kind          string `json:"kind"`
	Phase         string `json:"phase"`
	Status        string `json:"status"`
	StartedAt     string `json:"started_at,omitempty"`
	CompletedAt   string `json:"completed_at,omitempty"`
	Total         int    `json:"total"`
	Completed     int    `json:"completed"`
	Succeeded     int    `json:"succeeded"`
	Failed        int    `json:"failed"`
	CurrentTarget string `json:"current_target"`
	Message       string `json:"message,omitempty"`
	NodeProgress  int    `json:"node_progress"`
	NodeTotal     int    `json:"node_total"`
	// Workers in flight against the configured ceiling; instantaneous, unlike the cumulative counters.
	WorkersBusy  int `json:"workers_busy"`
	WorkerSlots  int `json:"worker_slots"`
	ProbesActive int `json:"probes_active"`
	ProbeSlots   int `json:"probe_slots"`
}

// StatusResponse is what GET /api/status returns.
type StatusResponse struct {
	Counts          Counts         `json:"counts"`
	Scan            ScanStatus     `json:"scan"`
	ScanningEnabled bool           `json:"scanning_enabled"`
	Seeds           []string       `json:"seeds"`
	BandCounts      map[string]int `json:"band_counts"`
	LinkTypeCounts  map[string]int `json:"link_type_counts"`
	ICMPMode        string         `json:"icmp_mode"`
	ServerTime      time.Time      `json:"server_time"`
	Cron            string         `json:"cron"`
	LastSweepAt     time.Time      `json:"last_sweep_at"`
	NextSweepAt     time.Time      `json:"next_sweep_at"`
	UptimeSeconds   int64          `json:"uptime_seconds"`
	Version         string         `json:"version"`
}

// TopologyResponse feeds the mesh graph view.
