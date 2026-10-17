// Command aredn-map crawls an AREDN mesh, probes nodes over ICMP, and serves the result over HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"

	"aredn-map/internal/controller"
	"aredn-map/internal/cron"
	"aredn-map/internal/handler"
	"aredn-map/internal/infra/config"
	"aredn-map/internal/infra/logger"
	"aredn-map/internal/repository/aredn"
	"aredn-map/internal/repository/ping"
	"aredn-map/internal/repository/store"
	"aredn-map/internal/service/scanner"
)

func main() {
	cfgPath := flag.String("config", "", "path to config.yaml (default: search ./ and ../)")
	once := flag.Bool("once", false, "run a single full sweep, print the summary and exit")
	port := flag.Int("port", 0, "override server.port")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	if *port > 0 {
		cfg.Server.Port = *port
	}

	log, err := logger.New(cfg.Logging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logging: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	log.Info("starting", zap.String("config", cfg.Path), zap.Int("port", cfg.Server.Port),
		zap.String("cron", cfg.Scan.Cron), zap.Int("max_hops", cfg.Scan.MaxHops),
		zap.Int("max_nodes", cfg.Scan.MaxNodes), zap.Int("workers", cfg.Scan.WorkerGoroutines))

	st := store.New()
	st.SetSeeds(cfg.Scan.Seeds)

	client := aredn.New(aredn.Config{
		Timeout:      cfg.HTTP.Timeout,
		ConnectTO:    cfg.HTTP.ConnectTO,
		MaxRetries:   cfg.HTTP.MaxRetries,
		RetryBackoff: cfg.HTTP.RetryBackoff,
		UserAgent:    cfg.HTTP.UserAgent,
	}, log)

	pinger, err := ping.New(ping.Config{
		Mode:           cfg.ICMP.Mode,
		Count:          cfg.ICMP.Count,
		Interval:       cfg.ICMP.Interval,
		Timeout:        cfg.ICMP.Timeout,
		OverallTimeout: cfg.ICMP.OverallTimeout,
		Concurrency:    cfg.ICMP.Concurrency,
	}, log)
	if err != nil {
		log.Fatal("icmp init failed", zap.Error(err))
	}
	cfg.ICMP.Mode = pinger.Mode()

	eng := scanner.New(cfg, st, client, pinger, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		runOnce(ctx, eng, st, log)
		return
	}

	sched, err := cron.New(cfg.Scan.Cron, cfg.Scan.CronZone, func() {
		if !eng.Enabled() {
			log.Debug("cron tick skipped, scanning disabled")
			return
		}
		if err := eng.Sweep(context.Background(), scanner.KindIncremental); err != nil {
			log.Warn("scheduled sweep ended early", zap.Error(err))
		}
	}, log)
	if err != nil {
		log.Fatal("cron expression rejected", zap.String("cron", cfg.Scan.Cron), zap.Error(err))
	}
	sched.Start()
	defer func() { _ = sched.Stop() }()

	if cfg.Scan.RunOnStartup {
		go func() {
			if err := eng.Sweep(context.Background(), scanner.KindFull); err != nil {
				log.Warn("initial full sweep ended early", zap.Error(err))
			}
		}()
	}

	app := handler.Run(controller.Deps{
		Ctx:     ctx,
		Cfg:     cfg,
		Store:   st,
		Engine:  eng,
		Log:     log,
		Started: time.Now(),
		NextRun: sched.NextRun,
	})

	<-ctx.Done()
	log.Info("shutdown signal received")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
	eng.Cancel()
	_ = pinger.Close()
}

// runOnce is the CI and wiring-test mode: one sweep, one summary, no listener.
func runOnce(ctx context.Context, eng *scanner.Engine, st *store.Store, log *zap.Logger) {
	if err := eng.Sweep(ctx, scanner.KindFull); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("sweep failed", zap.Error(err))
	}
	c := st.Counts()
	log.Info("summary", zap.Int("total", c.Total), zap.Int("online", c.Online),
		zap.Int("offline", c.Offline), zap.Int("located", c.Located),
		zap.Int("supernodes", c.Supernodes), zap.Int("links", c.Links),
		zap.Any("bands", st.BandCounts()))
}

// loadConfig runs once at boot, so a config change needs a restart; a missing file means defaults.
func loadConfig(explicit string) (*config.Config, error) {
	if explicit != "" {
		return config.Load(explicit)
	}
	if env := os.Getenv("AREDNSCAN_CONFIG"); env != "" {
		return config.Load(env)
	}
	for _, cand := range []string{"config.yaml", "../config.yaml", "/etc/aredn-map/config.yaml"} {
		if abs, err := filepath.Abs(cand); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return config.Load(abs)
			}
		}
	}
	return config.Load("")
}
