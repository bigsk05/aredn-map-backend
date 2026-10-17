// Package cron schedules sweeps by cron expression, on startup, and on demand from the API.
package cron

import (
	"fmt"
	"time"

	"github.com/go-co-op/gocron/v2"
	"go.uber.org/zap"
)

type Scheduler struct {
	s    gocron.Scheduler
	job  gocron.Job
	log  *zap.Logger
	expr string
}

// New builds a scheduler for expr; accepts 5- or 6-field crontabs and "@every" descriptors.
// The zone comes from config so "0 3 * * *" means the same instant wherever the daemon runs.
func New(expr string, zone string, fn func(), log *zap.Logger) (*Scheduler, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("scan.cron_zone %q: %w", zone, err)
	}
	s, err := gocron.NewScheduler(gocron.WithLocation(loc))
	if err != nil {
		return nil, err
	}
	// gocron v2 recovers panics only via an event listener, so the sweep guards its own.
	// A nil fn would reach NewTask and panic on invoke, so it is rejected.
	if fn == nil {
		_ = s.Shutdown()
		return nil, fmt.Errorf("scheduler: job function must not be nil")
	}
	safe := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("scheduled sweep panicked; cron stays armed",
					zap.Any("panic", r), zap.Stack("stack"))
			}
		}()
		fn()
	}
	j, err := s.NewJob(gocron.CronJob(expr, true), gocron.NewTask(safe),
		gocron.WithName("aredn-sweep"), gocron.WithSingletonMode(gocron.LimitModeReschedule))
	if err != nil {
		_ = s.Shutdown()
		return nil, err
	}
	return &Scheduler{s: s, job: j, log: log.Named("scheduler"), expr: expr}, nil
}

func (s *Scheduler) Start() {
	s.s.Start()
	s.log.Info("cron armed", zap.String("expr", s.expr), zap.Time("next_run", s.NextRun()))
}

func (s *Scheduler) Stop() error { return s.s.Shutdown() }

// NextRun reports the following scheduled sweep, or the zero time.
func (s *Scheduler) NextRun() time.Time {
	t, err := s.job.NextRun()
	if err != nil {
		return time.Time{}
	}
	return t
}

// RunNow triggers the configured sweep out of band.
func (s *Scheduler) RunNow() error { return s.job.RunNow() }
