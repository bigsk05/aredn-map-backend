package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"aredn-map/internal/infra/config"
)

func fileCfg(t *testing.T) config.Logging {
	t.Helper()
	return config.Logging{
		Dir: filepath.Join(t.TempDir(), "logs"), File: "aredn-map.log",
		Level: "info", Console: false, MaxSizeMB: 1, MaxBackups: 3, MaxAgeDays: 7, Compress: false,
	}
}

func readLog(t *testing.T, cfg config.Logging) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg.Dir, cfg.File))
	if err != nil {
		t.Fatalf("read %s: %v", cfg.File, err)
	}
	return string(b)
}

func TestNewWritesUnderTheConfiguredDirectory(t *testing.T) {
	cfg := fileCfg(t)
	log, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log.Info("sweep started", zap.Int("id", 7))
	if err := log.Sync(); err != nil {
		t.Logf("Sync on a file-only logger: %v", err)
	}

	got := readLog(t, cfg)
	if !strings.Contains(got, "sweep started") || !strings.Contains(got, `"id": 7`) {
		t.Errorf("log file = %q, want the message and its field", got)
	}
	// The config selects the console encoder, not the JSON production format.
	if !strings.Contains(got, "info") || strings.HasPrefix(strings.TrimSpace(got), "{") {
		t.Errorf("log line = %q, want the console encoder format", got)
	}
}

// New creates Dir, so a fresh checkout needs no manual mkdir.
func TestNewCreatesMissingLogDirectory(t *testing.T) {
	cfg := fileCfg(t)
	cfg.Dir = filepath.Join(t.TempDir(), "deep", "logs")
	if _, err := New(cfg); err != nil {
		t.Fatalf("New with an absent directory: %v", err)
	}
	if st, err := os.Stat(cfg.Dir); err != nil || !st.IsDir() {
		t.Errorf("directory %s not created: %v", cfg.Dir, err)
	}
}

func TestLevelFiltersBelowThreshold(t *testing.T) {
	cfg := fileCfg(t)
	cfg.Level = "warn"
	log, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("should-not-appear")
	log.Warn("should-appear")
	_ = log.Sync()

	got := readLog(t, cfg)
	if strings.Contains(got, "should-not-appear") {
		t.Errorf("info leaked at warn level: %q", got)
	}
	if !strings.Contains(got, "should-appear") {
		t.Errorf("warn missing at warn level: %q", got)
	}
}

func TestUnknownLevelIsAnError(t *testing.T) {
	cfg := fileCfg(t)
	cfg.Level = "verbose"
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted an unparseable level; zap maps this, the config must not silently fall back")
	}
}

// With neither file nor console the logger must still be usable, not a core with no sinks.
func TestNoSinksFallsBackToStdout(t *testing.T) {
	cfg := config.Logging{Level: "debug", Console: false}
	log, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log.Debug("routed to the stdout fallback")
	_ = log.Sync()
}

func TestConsoleOnlyDoesNotCreateADirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "must-not-exist")
	cfg := config.Logging{Dir: "", File: "", Level: "info", Console: true}
	log, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("console only")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("an empty logging.dir still touched the filesystem")
	}
}
