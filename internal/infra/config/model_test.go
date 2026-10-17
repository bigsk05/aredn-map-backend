package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"aredn-map/internal/meta"
)

// Tests here are offline: they read only the repo's config.yaml or temp-dir files.

// repoConfigPath resolves the tracked config.example.yaml relative to this file, never the
// working directory. The operator's config.yaml is ignored and cannot be checked in.
func repoConfigPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller gave no file for this test")
	}
	// The repo root is three directories up from this file.
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "config.example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("repo config %s: %v", path, err)
	}
	return path
}

// writeConfig drops body into a private temp dir and returns the file path.
func writeConfig(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// defaultsConfig returns the built-in defaults for the Validate cases to mutate.
func defaultsConfig(t *testing.T) *Config {
	t.Helper()
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") = %v, want the defaults to be valid", err)
	}
	return c
}

// Pins every section of the shipped config.yaml, including viper's duration-string decodes.
func TestLoadRepoConfig(t *testing.T) {
	path := repoConfigPath(t)
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s) = %v", path, err)
	}

	if c.Path != path {
		t.Fatalf("Path = %q, want %q", c.Path, path)
	}

	wantServer := Server{
		Host:         "0.0.0.0",
		AllowOrigins: []string{},
		Port:         8899,
		StaticDir:    "",
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	if !reflect.DeepEqual(c.Server, wantServer) {
		t.Fatalf("Server = %+v, want %+v", c.Server, wantServer)
	}

	if got := c.Scan.Seeds; len(got) != 1 || got[0] != "localnode.local.mesh" {
		t.Fatalf("Scan.Seeds = %q, want [localnode.local.mesh]", got)
	}
	// The shipped 6-field cron parses only because cron.New passes withSeconds=true.
	if c.Scan.Cron != "0 */30 * * * *" {
		t.Fatalf("Scan.Cron = %q, want %q", c.Scan.Cron, "0 */30 * * * *")
	}
	if c.Scan.CronZone != "Asia/Shanghai" {
		t.Fatalf("Scan.CronZone = %q, want Asia/Shanghai", c.Scan.CronZone)
	}
	if !c.Scan.RunOnStartup {
		t.Fatal("Scan.RunOnStartup = false, want the first sweep on boot")
	}
	if c.Scan.MaxHops != -1 {
		t.Fatalf("Scan.MaxHops = %d, want -1 (unlimited is the shipped default)", c.Scan.MaxHops)
	}
	if c.Scan.MaxNodes != 0 {
		t.Fatalf("Scan.MaxNodes = %d, want 0 (unlimited)", c.Scan.MaxNodes)
	}
	if c.Scan.WorkerGoroutines != 32 || c.Scan.DetailConcurrency != 6 {
		t.Fatalf("Scan concurrency = %d/%d, want 8 workers / 6 detail",
			c.Scan.WorkerGoroutines, c.Scan.DetailConcurrency)
	}
	if c.Scan.DeferDetailBeyondHops != 2 {
		t.Fatalf("Scan.DeferDetailBeyondHops = %d, want 2", c.Scan.DeferDetailBeyondHops)
	}
	if !c.Scan.AbsorbMeshHosts {
		t.Fatal("Scan.AbsorbMeshHosts = false, want host tables registered by default")
	}
	if c.Scan.NodeTimeout != 12*time.Second || c.Scan.SweepTimeout != 45*time.Minute ||
		c.Scan.RelazyAfter != 10*time.Minute {
		t.Fatalf("Scan durations = %v/%v/%v, want 12s/45m/10m",
			c.Scan.NodeTimeout, c.Scan.SweepTimeout, c.Scan.RelazyAfter)
	}

	wantHTTP := HTTP{
		Timeout:      12 * time.Second,
		ConnectTO:    4 * time.Second,
		MaxRetries:   2,
		RetryBackoff: 800 * time.Millisecond,
		UserAgent:    meta.UserAgent,
	}
	if c.HTTP != wantHTTP {
		t.Fatalf("HTTP = %+v, want %+v", c.HTTP, wantHTTP)
	}

	wantICMP := ICMP{
		Mode:           "auto",
		Count:          3,
		Interval:       600 * time.Millisecond,
		Timeout:        3 * time.Second,
		OverallTimeout: 8 * time.Second,
		Concurrency:    16,
	}
	if c.ICMP != wantICMP {
		t.Fatalf("ICMP = %+v, want %+v", c.ICMP, wantICMP)
	}

	wantLogging := Logging{
		Dir:        "logs",
		File:       "aredn-map.log",
		Level:      "info",
		Console:    true,
		MaxSizeMB:  20,
		MaxBackups: 10,
		MaxAgeDays: 30,
		Compress:   true,
	}
	if c.Logging != wantLogging {
		t.Fatalf("Logging = %+v, want %+v", c.Logging, wantLogging)
	}

	// The shipped config carries no AMap keys; the frontend asks /api/config for them at runtime.
	if c.Map.AmapKey != "" || c.Map.AmapSecurityCode != "" {
		t.Fatal("config.yaml must not ship map credentials")
	}
	if c.Map.Zoom != 8 {
		t.Fatalf("Map.Zoom = %d, want 8", c.Map.Zoom)
	}
	if c.Map.CenterLat != 31.2304 || c.Map.CenterLon != 121.4737 {
		t.Fatalf("Map centre = %v/%v, want 31.2304/121.4737 (Shanghai)",
			c.Map.CenterLat, c.Map.CenterLon)
	}
}

// Load("") must hand back a working default config on the no-file boot path.
func TestLoadEmptyPathKeepsDefaults(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") = %v, want defaults", err)
	}
	if c.Path != "" {
		t.Fatalf("Path = %q, want the empty string it was called with", c.Path)
	}

	// A representative slice of every section, each value the code depends on.
	if c.Server.Host != "0.0.0.0" || c.Server.Port != 8899 {
		t.Fatalf("Server = %+v, want 0.0.0.0:8899", c.Server)
	}
	if c.Server.StaticDir != "" {
		t.Fatalf("Server.StaticDir = %q, want empty: the API does not look for the frontend", c.Server.StaticDir)
	}
	if c.Server.ReadTimeout != 15*time.Second || c.Server.WriteTimeout != 30*time.Second {
		t.Fatalf("Server timeouts = %v/%v, want 15s/30s", c.Server.ReadTimeout, c.Server.WriteTimeout)
	}
	if got := c.Scan.Seeds; len(got) != 1 || got[0] != "localnode.local.mesh" {
		t.Fatalf("Scan.Seeds = %q, want the localnode default", got)
	}
	if c.Scan.Cron != "0 */30 * * * *" || c.Scan.CronZone != "Asia/Shanghai" {
		t.Fatalf("Scan schedule = %q in %q, want \"0 */30 * * * *\" in Asia/Shanghai",
			c.Scan.Cron, c.Scan.CronZone)
	}
	if c.Scan.MaxHops != -1 || c.Scan.MaxNodes != 0 {
		t.Fatalf("Scan limits = %d/%d, want unlimited hops (-1) and unlimited nodes (0)",
			c.Scan.MaxHops, c.Scan.MaxNodes)
	}
	if !c.Scan.RunOnStartup || !c.Scan.AbsorbMeshHosts {
		t.Fatal("Scan booleans must default to true so a bare boot does real work")
	}
	if c.Scan.WorkerGoroutines != 32 || c.Scan.DetailConcurrency != 6 || c.Scan.DeferDetailBeyondHops != 2 {
		t.Fatalf("Scan counts = %d/%d/%d, want 32/6/2",
			c.Scan.WorkerGoroutines, c.Scan.DetailConcurrency, c.Scan.DeferDetailBeyondHops)
	}
	if c.Scan.NodeTimeout != 12*time.Second || c.Scan.SweepTimeout != 45*time.Minute ||
		c.Scan.RelazyAfter != 10*time.Minute {
		t.Fatalf("Scan durations = %v/%v/%v, want 12s/45m/10m",
			c.Scan.NodeTimeout, c.Scan.SweepTimeout, c.Scan.RelazyAfter)
	}
	if c.HTTP.Timeout != 12*time.Second || c.HTTP.ConnectTO != 4*time.Second ||
		c.HTTP.RetryBackoff != 800*time.Millisecond || c.HTTP.MaxRetries != 2 {
		t.Fatalf("HTTP = %+v, want 12s/4s/800ms and 2 retries", c.HTTP)
	}
	if c.HTTP.UserAgent != meta.UserAgent {
		t.Fatalf("HTTP.UserAgent = %q, want the default banner", c.HTTP.UserAgent)
	}
	if c.ICMP.Mode != "auto" || c.ICMP.Count != 3 || c.ICMP.Concurrency != 16 {
		t.Fatalf("ICMP = %+v, want auto/3/16", c.ICMP)
	}
	if c.ICMP.Interval != 600*time.Millisecond || c.ICMP.Timeout != 3*time.Second ||
		c.ICMP.OverallTimeout != 8*time.Second {
		t.Fatalf("ICMP durations = %v/%v/%v, want 600ms/3s/8s",
			c.ICMP.Interval, c.ICMP.Timeout, c.ICMP.OverallTimeout)
	}
	if c.Logging.Dir != "logs" || c.Logging.File != "aredn-map.log" || c.Logging.Level != "info" {
		t.Fatalf("Logging = %+v, want logs/aredn-map.log at info", c.Logging)
	}
	if !c.Logging.Console || !c.Logging.Compress {
		t.Fatalf("Logging.Console/Compress = %v/%v, want both true", c.Logging.Console, c.Logging.Compress)
	}
	if c.Logging.MaxSizeMB != 20 || c.Logging.MaxBackups != 10 || c.Logging.MaxAgeDays != 30 {
		t.Fatalf("Logging rotation = %d/%d/%d, want 20/10/30",
			c.Logging.MaxSizeMB, c.Logging.MaxBackups, c.Logging.MaxAgeDays)
	}
	if c.Map.AmapKey != "" || c.Map.AmapSecurityCode != "" {
		t.Fatalf("Map.Amap credentials = %q/%q, want no key baked into the defaults", c.Map.AmapKey, c.Map.AmapSecurityCode)
	}
	if c.Map.Zoom != 8 {
		t.Fatalf("Map.Zoom = %d, want 8", c.Map.Zoom)
	}
	if c.Map.CenterLat != 31.2304 || c.Map.CenterLon != 121.4737 {
		t.Fatalf("Map centre = %v/%v, want 31.2304/121.4737", c.Map.CenterLat, c.Map.CenterLon)
	}
}

// AREDNSCAN_<SECTION>_<KEY> beats the file for ints, strings and duration strings alike.
func TestEnvOverridesWinTheYAML(t *testing.T) {
	path := writeConfig(t, "config.yaml", `
server:
  port: 8899
scan:
  seeds: ["seed-a.local.mesh"]
  max_hops: 3
  node_timeout: "5s"
icmp:
  mode: "privileged"
  count: 4
`)

	t.Setenv("AREDNSCAN_SERVER_PORT", "9911")
	t.Setenv("AREDNSCAN_SCAN_MAX_HOPS", "7")
	t.Setenv("AREDNSCAN_ICMP_MODE", "unprivileged")
	t.Setenv("AREDNSCAN_ICMP_COUNT", "9")
	t.Setenv("AREDNSCAN_SCAN_NODE_TIMEOUT", "13s")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if c.Server.Port != 9911 {
		t.Fatalf("Server.Port = %d, want the env value 9911 rather than the YAML 8899", c.Server.Port)
	}
	if c.Scan.MaxHops != 7 {
		t.Fatalf("Scan.MaxHops = %d, want 7 from AREDNSCAN_SCAN_MAX_HOPS", c.Scan.MaxHops)
	}
	if c.ICMP.Mode != "unprivileged" {
		t.Fatalf("ICMP.Mode = %q, want unprivileged from AREDNSCAN_ICMP_MODE", c.ICMP.Mode)
	}
	if c.ICMP.Count != 9 {
		t.Fatalf("ICMP.Count = %d, want 9 from AREDNSCAN_ICMP_COUNT", c.ICMP.Count)
	}
	if c.Scan.NodeTimeout != 13*time.Second {
		t.Fatalf("Scan.NodeTimeout = %v, want 13s parsed from the env string", c.Scan.NodeTimeout)
	}
	// Untouched keys must still come from the file.
	if got := c.Scan.Seeds; len(got) != 1 || got[0] != "seed-a.local.mesh" {
		t.Fatalf("Scan.Seeds = %q, want the YAML list to survive the env overrides", got)
	}
}

// An unusable env value must be rejected, not steer the scanner to an unbindable port.
func TestEnvOverrideIsRejectedWhenUnusable(t *testing.T) {
	t.Setenv("AREDNSCAN_SERVER_PORT", "70000")
	if c, err := Load(""); err == nil {
		t.Fatalf("Load = %+v, want the env port rejected by Validate", c)
	} else if !strings.Contains(err.Error(), "server.port") {
		t.Fatalf("err = %v, want it to name server.port", err)
	}
}

// An explicit path that does not exist must fall back to the defaults, not abort boot.
func TestAbsentExplicitFileFallsBackToDefaults(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yaml")

	c, err := Load(missing)
	if err != nil {
		t.Fatalf("Load(%s) = %v, want a not-yet-created file to fall back to defaults", missing, err)
	}
	if c == nil {
		t.Fatalf("Load(%s) returned nil config", missing)
	}
	if c.Path != missing {
		t.Errorf("Path = %q, want the requested path echoed for the boot log", c.Path)
	}
	if c.Server.Port != 8899 || c.ICMP.Mode != "auto" || len(c.Scan.Seeds) == 0 {
		t.Errorf("defaults = %+v / %+v / %v, want the shipped defaults", c.Server, c.ICMP, c.Scan.Seeds)
	}

	// A file that exists but cannot be decoded must still be fatal.
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("server: [this is not a mapping\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatalf("Load(%s) succeeded, want a malformed existing file to be reported", bad)
	} else if !strings.Contains(err.Error(), bad) {
		t.Fatalf("err = %v, want it to name %s", err, bad)
	}

	// Same keys when no path is given at all.
	if fallback, ferr := Load(""); ferr != nil {
		t.Fatalf("Load(\"\") = %v, want the defaults to stay reachable", ferr)
	} else if fallback.Server.Port != c.Server.Port || fallback.ICMP.Mode != c.ICMP.Mode {
		t.Fatalf("defaults differ between no-path and absent-path: %+v vs %+v", fallback, c)
	}
}

// A parse failure has to name the file to fix.
func TestMalformedYAMLErrorNamesTheFile(t *testing.T) {
	path := writeConfig(t, "broken.yaml", "server:\n  port: 8899\n oops: [unclosed\n")

	c, err := Load(path)
	if err == nil {
		t.Fatalf("Load = %+v, want a parse error", c)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want it to name %s", err, path)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "config") {
		t.Fatalf("err = %v, want it to say the config failed to load", err)
	}
}

// Unknown keys must not break the boot; operators keep notes in config.yaml.
func TestUnknownKeysAreIgnored(t *testing.T) {
	path := writeConfig(t, "extra.yaml", `
server:
  port: 9000
  somebody_elses_block:
    nested: true
scan:
  seeds: ["gate.local.mesh"]
  cron: "@every 15m"
  retired_setting: 42
telemetry:
  enabled: true
map:
  tencent_key: "KEY"
`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v, want unknown keys ignored", err)
	}
	if c.Server.Port != 9000 {
		t.Fatalf("Server.Port = %d, want 9000 from the file", c.Server.Port)
	}
	if got := c.Scan.Seeds; len(got) != 1 || got[0] != "gate.local.mesh" {
		t.Fatalf("Scan.Seeds = %q", got)
	}
	if c.Scan.Cron != "@every 15m" {
		t.Fatalf("Scan.Cron = %q, want the descriptor to pass through untouched", c.Scan.Cron)
	}
	// Keys the file leaves out still come from the defaults.
	if c.Scan.WorkerGoroutines != 32 || c.Map.Zoom != 8 {
		t.Fatalf("defaults lost beside unknown keys: %+v %+v", c.Scan, c.Map)
	}
}

// ext() hands the .yml type to viper's SetConfigType.
func TestYmlExtensionIsRecognised(t *testing.T) {
	path := writeConfig(t, "conf.yml", "server:\n  port: 9111\n")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s) = %v", path, err)
	}
	if c.Server.Port != 9111 {
		t.Fatalf("Server.Port = %d, want 9111 read through the .yml type", c.Server.Port)
	}
}

// normalize() trims and lowercases seeds and icmp mode.
func TestNormalizeTrimsAndLowercases(t *testing.T) {
	c := &Config{}
	c.Scan.Seeds = []string{"  LocalNode.Local.MESH  ", "Tail ", "already.lower"}
	c.ICMP.Mode = "  Privileged "
	c.normalize()

	want := []string{"localnode.local.mesh", "tail", "already.lower"}
	if strings.Join(c.Scan.Seeds, ",") != strings.Join(want, ",") {
		t.Fatalf("Seeds = %q, want %q", c.Scan.Seeds, want)
	}
	if c.ICMP.Mode != "privileged" {
		t.Fatalf("ICMP.Mode = %q, want lowercased and trimmed", c.ICMP.Mode)
	}

	// A blank mode is not an error, it means "let the pinger choose".
	c = &Config{ICMP: ICMP{Mode: "   "}}
	c.normalize()
	if c.ICMP.Mode != "auto" {
		t.Fatalf("ICMP.Mode = %q, want auto for a blank value", c.ICMP.Mode)
	}
	c = &Config{}
	c.normalize()
	if c.ICMP.Mode != "auto" {
		t.Fatalf("ICMP.Mode = %q, want auto when the key is absent", c.ICMP.Mode)
	}
}

// Both the file and the env route get normalised.
func TestNormalizeRunsDuringLoad(t *testing.T) {
	path := writeConfig(t, "case.yaml", `
scan:
  seeds:
    - "  GateNode.Local.MESH "
    - "ANOTHER.Node.Local.Mesh"
icmp:
  mode: "  UnPrivileged  "
`)

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if got := c.Scan.Seeds; len(got) != 2 || got[0] != "gatenode.local.mesh" || got[1] != "another.node.local.mesh" {
		t.Fatalf("Scan.Seeds = %q, want both trimmed and lowercased", got)
	}
	if c.ICMP.Mode != "unprivileged" {
		t.Fatalf("ICMP.Mode = %q, want unprivileged", c.ICMP.Mode)
	}

	// The env route as well.
	t.Setenv("AREDNSCAN_ICMP_MODE", "PRIVILEGED")
	c, err = Load(path)
	if err != nil {
		t.Fatalf("Load with env mode = %v", err)
	}
	if c.ICMP.Mode != "privileged" {
		t.Fatalf("ICMP.Mode = %q, want the normalised env value", c.ICMP.Mode)
	}
}

// Every Validate rejection branch must name the offending key in its message.
func TestValidateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantMsg string
	}{
		{
			name:    "no seeds at all",
			mutate:  func(c *Config) { c.Scan.Seeds = nil },
			wantMsg: "scan.seeds must list at least one node",
		},
		{
			name:    "empty seed list",
			mutate:  func(c *Config) { c.Scan.Seeds = []string{} },
			wantMsg: "scan.seeds must list at least one node",
		},
		{
			name:    "blank seed entry",
			mutate:  func(c *Config) { c.Scan.Seeds = []string{"gate.local.mesh", ""} },
			wantMsg: "scan.seeds contains an empty entry",
		},
		{
			name:    "port zero",
			mutate:  func(c *Config) { c.Server.Port = 0 },
			wantMsg: "server.port 0 out of range",
		},
		{
			name:    "port negative",
			mutate:  func(c *Config) { c.Server.Port = -1 },
			wantMsg: "server.port -1 out of range",
		},
		{
			name:    "port above the range",
			mutate:  func(c *Config) { c.Server.Port = 65536 },
			wantMsg: "server.port 65536 out of range",
		},
		{
			name:    "workers zero",
			mutate:  func(c *Config) { c.Scan.WorkerGoroutines = 0 },
			wantMsg: "scan.worker_goroutines must be positive",
		},
		{
			name:    "workers negative",
			mutate:  func(c *Config) { c.Scan.WorkerGoroutines = -3 },
			wantMsg: "scan.worker_goroutines must be positive",
		},
		{
			name:    "hops below unlimited",
			mutate:  func(c *Config) { c.Scan.MaxHops = -2 },
			wantMsg: "scan.max_hops must be -1 or a non-negative hop count",
		},
		{
			name:    "unknown icmp mode",
			mutate:  func(c *Config) { c.ICMP.Mode = "raw" },
			wantMsg: `icmp.mode "raw" must be auto, unprivileged or privileged`,
		},
		{
			name:    "icmp count zero",
			mutate:  func(c *Config) { c.ICMP.Count = 0 },
			wantMsg: "icmp.count must be positive",
		},
		{
			name:    "icmp count negative",
			mutate:  func(c *Config) { c.ICMP.Count = -1 },
			wantMsg: "icmp.count must be positive",
		},
		{
			name:    "cron empty",
			mutate:  func(c *Config) { c.Scan.Cron = "" },
			wantMsg: "scan.cron must not be empty",
		},
		{
			name:    "cron only spaces",
			mutate:  func(c *Config) { c.Scan.Cron = "   " },
			wantMsg: "scan.cron must not be empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := defaultsConfig(t)
			tc.mutate(c)

			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate on %+v, want it rejected", c)
			}
			if err.Error() != tc.wantMsg {
				t.Fatalf("err = %q, want %q", err, tc.wantMsg)
			}
		})
	}
}

// Legal edges: -1 hops and 0 nodes both mean unlimited, and both port bounds bind.
func TestValidateAcceptsTheBoundaryValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"unlimited hops", func(c *Config) { c.Scan.MaxHops = -1 }},
		{"single hop", func(c *Config) { c.Scan.MaxHops = 0 }},
		{"deep hop count", func(c *Config) { c.Scan.MaxHops = 12 }},
		{"unlimited nodes", func(c *Config) { c.Scan.MaxNodes = 0 }},
		{"lowest bindable port", func(c *Config) { c.Server.Port = 1 }},
		{"highest bindable port", func(c *Config) { c.Server.Port = 65535 }},
		{"single worker", func(c *Config) { c.Scan.WorkerGoroutines = 1 }},
		{"auto icmp", func(c *Config) { c.ICMP.Mode = "auto" }},
		{"unprivileged icmp", func(c *Config) { c.ICMP.Mode = "unprivileged" }},
		{"privileged icmp", func(c *Config) { c.ICMP.Mode = "privileged" }},
		{"every cron form", func(c *Config) { c.Scan.Cron = "@every 30m" }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := defaultsConfig(t)
			tc.mutate(c)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate = %v, want %s accepted", err, tc.name)
			}
		})
	}
}
