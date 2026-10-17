// Package scanner walks the mesh outward from the seeds: discovery, probes, detail, and revisits.
package scanner

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"aredn-map/internal/infra/config"
	"aredn-map/internal/model"
	"aredn-map/internal/repository/aredn"
	"aredn-map/internal/repository/ping"
	"aredn-map/internal/repository/store"
)

const (
	KindFull        = "full"
	KindIncremental = "incremental"
)

// Pinger is the slice of internal/ping the engine depends on.
type Pinger interface {
	Ping(ctx context.Context, host string) ping.Result
	PingMany(ctx context.Context, hosts []string) map[string]ping.Result
	Mode() string
	Active() int
	Slots() int
}

// Fetcher is the slice of internal/aredn the engine depends on; tests substitute a scripted mesh.
type Fetcher interface {
	FetchSelf(ctx context.Context, hostname string) (*model.Node, error)
	FetchNeighbors(ctx context.Context, hostname string, withDetail bool, concurrency int) ([]model.Link, error)
	FetchMesh(ctx context.Context, hostname string) (*aredn.MeshData, error)
}

// ErrAlreadyRunning rejects an overlapping sweep.
var ErrAlreadyRunning = errors.New("scan already running")

type Engine struct {
	cfg *config.Config
	st  *store.Store
	cl  Fetcher
	pg  Pinger
	log *zap.Logger

	seq atomic.Int64
	// busy counts the scan tasks in flight now, against the configured worker ceiling.
	busy atomic.Int64

	mu     sync.Mutex
	status model.ScanStatus
	// sweepStart anchors the revisit pass's due times to this sweep's start instant.
	sweepStart time.Time
	running    bool
	cancel     context.CancelFunc
	enabled    bool
}

func New(cfg *config.Config, st *store.Store, cl Fetcher, pg Pinger, log *zap.Logger) *Engine {
	e := &Engine{cfg: cfg, st: st, cl: cl, pg: pg, log: log.Named("scanner")}
	e.enabled = cfg.Scan.RunOnStartup
	return e
}

// probePool pings nodes as discovery reaches them, not after the whole mesh is walked.

func (e *Engine) SetEnabled(on bool) {
	e.mu.Lock()
	e.enabled = on
	e.mu.Unlock()
}

func (e *Engine) Enabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enabled
}

func (e *Engine) Status() model.ScanStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.status
	s.WorkersBusy = int(e.busy.Load())
	s.WorkerSlots = e.cfg.Scan.WorkerGoroutines
	s.ProbesActive = e.pg.Active()
	s.ProbeSlots = e.pg.Slots()
	return s
}

// Cancel aborts a running sweep; the engine stays usable afterwards.
func (e *Engine) Cancel() {
	e.mu.Lock()
	c := e.cancel
	e.mu.Unlock()
	if c != nil {
		c()
	}
}

// patch mutates the published status and bumps the store counter the push loop watches.
func (e *Engine) patch(fn func(*model.ScanStatus)) {
	e.mu.Lock()
	fn(&e.status)
	e.mu.Unlock()
	e.st.BumpRev()
}

type task struct {
	host string
	hop  int
	// detail marks the second lane through the same frontier: the per-link dialog pass.
	detail bool
}

// Sweep phases: discovery -> probe -> detail -> revisit; KindIncremental refreshes in place.
