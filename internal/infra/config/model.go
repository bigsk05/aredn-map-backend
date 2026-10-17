// Package config holds the operator's settings as one struct per section,
// with defaults for an empty file and validation that rejects an unworkable one.
package config

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/viper"

	"aredn-map/internal/meta"
)

type Server struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	// AllowOrigins names the frontends allowed to read this API cross-origin.
	// Empty allows any origin; requests carry no credentials.
	AllowOrigins []string      `mapstructure:"allow_origins"`
	StaticDir    string        `mapstructure:"static_dir"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

type Scan struct {
	Seeds                 []string      `mapstructure:"seeds"`
	Cron                  string        `mapstructure:"cron"`
	CronZone              string        `mapstructure:"cron_zone"`
	RunOnStartup          bool          `mapstructure:"run_on_startup"`
	MaxHops               int           `mapstructure:"max_hops"`
	MaxNodes              int           `mapstructure:"max_nodes"`
	WorkerGoroutines      int           `mapstructure:"worker_goroutines"`
	DetailConcurrency     int           `mapstructure:"detail_concurrency"`
	NodeTimeout           time.Duration `mapstructure:"node_timeout"`
	SweepTimeout          time.Duration `mapstructure:"sweep_timeout"`
	DeferDetailBeyondHops int           `mapstructure:"defer_detail_beyond_hops"`
	AbsorbMeshHosts       bool          `mapstructure:"absorb_mesh_hosts"`
	RelazyAfter           time.Duration `mapstructure:"relazy_after"`
	RevisitAfter          time.Duration `mapstructure:"revisit_after"`
}

type HTTP struct {
	Timeout      time.Duration `mapstructure:"timeout"`
	ConnectTO    time.Duration `mapstructure:"connect_timeout"`
	MaxRetries   int           `mapstructure:"max_retries"`
	RetryBackoff time.Duration `mapstructure:"retry_backoff"`
	UserAgent    string        `mapstructure:"user_agent"`
}

type ICMP struct {
	Mode           string        `mapstructure:"mode"`
	Count          int           `mapstructure:"count"`
	Interval       time.Duration `mapstructure:"interval"`
	Timeout        time.Duration `mapstructure:"timeout"`
	OverallTimeout time.Duration `mapstructure:"overall_timeout"`
	Concurrency    int           `mapstructure:"concurrency"`
}

type Logging struct {
	Dir        string `mapstructure:"dir"`
	File       string `mapstructure:"file"`
	Level      string `mapstructure:"level"`
	Console    bool   `mapstructure:"console"`
	MaxSizeMB  int    `mapstructure:"max_size_mb"`
	MaxBackups int    `mapstructure:"max_backups"`
	MaxAgeDays int    `mapstructure:"max_age_days"`
	Compress   bool   `mapstructure:"compress"`
}

// Map configures the basemap; AMap is the only supported provider.
type Map struct {
	AmapKey          string  `mapstructure:"amap_key"`
	AmapSecurityCode string  `mapstructure:"amap_security_code"`
	CenterLat        float64 `mapstructure:"center_lat"`
	CenterLon        float64 `mapstructure:"center_lon"`
	Zoom             int     `mapstructure:"zoom"`
}

type Config struct {
	Path    string  `mapstructure:"-"`
	Server  Server  `mapstructure:"server"`
	Scan    Scan    `mapstructure:"scan"`
	HTTP    HTTP    `mapstructure:"http"`
	ICMP    ICMP    `mapstructure:"icmp"`
	Logging Logging `mapstructure:"logging"`
	Map     Map     `mapstructure:"map"`
}

// defaults is the single source of truth for values an operator may leave out.
func defaults(v *viper.Viper) {
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 8899)
	v.SetDefault("server.allow_origins", []string{})
	v.SetDefault("server.static_dir", "")
	v.SetDefault("server.read_timeout", "15s")
	v.SetDefault("server.write_timeout", "30s")

	v.SetDefault("scan.seeds", []string{"localnode.local.mesh"})
	v.SetDefault("scan.cron", "0 */30 * * * *")
	v.SetDefault("scan.cron_zone", "Asia/Shanghai")
	v.SetDefault("scan.run_on_startup", true)
	v.SetDefault("scan.max_hops", -1)
	v.SetDefault("scan.max_nodes", 0)
	v.SetDefault("scan.worker_goroutines", 32)
	v.SetDefault("scan.detail_concurrency", 6)
	v.SetDefault("scan.node_timeout", "12s")
	v.SetDefault("scan.sweep_timeout", "45m")
	v.SetDefault("scan.defer_detail_beyond_hops", 2)
	v.SetDefault("scan.absorb_mesh_hosts", true)
	v.SetDefault("scan.relazy_after", "10m")
	v.SetDefault("scan.revisit_after", "30m")

	v.SetDefault("http.timeout", "12s")
	v.SetDefault("http.connect_timeout", "4s")
	v.SetDefault("http.max_retries", 2)
	v.SetDefault("http.retry_backoff", "800ms")
	v.SetDefault("http.user_agent", meta.UserAgent)

	v.SetDefault("icmp.mode", "auto")
	v.SetDefault("icmp.count", 3)
	v.SetDefault("icmp.interval", "600ms")
	v.SetDefault("icmp.timeout", "3s")
	v.SetDefault("icmp.overall_timeout", "8s")
	v.SetDefault("icmp.concurrency", 16)

	v.SetDefault("logging.dir", "logs")
	v.SetDefault("logging.file", "aredn-map.log")
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.console", true)
	v.SetDefault("logging.max_size_mb", 20)
	v.SetDefault("logging.max_backups", 10)
	v.SetDefault("logging.max_age_days", 30)
	v.SetDefault("logging.compress", true)

	v.SetDefault("map.amap_key", "")
	v.SetDefault("map.amap_security_code", "")
	v.SetDefault("map.center_lat", 31.2304)
	v.SetDefault("map.center_lon", 121.4737)
	v.SetDefault("map.zoom", 8)
}

func (c *Config) normalize() {
	for i, s := range c.Scan.Seeds {
		c.Scan.Seeds[i] = strings.ToLower(strings.TrimSpace(s))
	}
	c.ICMP.Mode = strings.ToLower(strings.TrimSpace(c.ICMP.Mode))
	if c.ICMP.Mode == "" {
		c.ICMP.Mode = "auto"
	}
}

func (c *Config) Validate() error {
	if len(c.Scan.Seeds) == 0 {
		return fmt.Errorf("scan.seeds must list at least one node")
	}
	if slices.Contains(c.Scan.Seeds, "") {
		return fmt.Errorf("scan.seeds contains an empty entry")
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d out of range", c.Server.Port)
	}
	if c.Scan.WorkerGoroutines <= 0 {
		return fmt.Errorf("scan.worker_goroutines must be positive")
	}
	if c.Scan.MaxHops < -1 {
		return fmt.Errorf("scan.max_hops must be -1 or a non-negative hop count")
	}
	switch c.ICMP.Mode {
	case "auto", "unprivileged", "privileged":
	default:
		return fmt.Errorf("icmp.mode %q must be auto, unprivileged or privileged", c.ICMP.Mode)
	}
	if c.ICMP.Count <= 0 {
		return fmt.Errorf("icmp.count must be positive")
	}
	if strings.TrimSpace(c.Scan.Cron) == "" {
		return fmt.Errorf("scan.cron must not be empty")
	}
	return nil
}
