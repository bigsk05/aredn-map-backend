//go:build live

// Live tests talk to the real mesh over this box's tunnel; run with -tags live.
package ping

import (
	"context"
	"errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// liveHosts are AREDN nodes reachable from this machine.
var liveHosts = []string{
	"localnode.local.mesh",          // the gateway itself
	"tst04-supernode.local.mesh",    // supernode behind the tunnel
	"tst05-tunnelserver.local.mesh", // tunnel server
	"tst16.local.mesh",              // distant node through the tunnel chain
	"tst12-hap.local.mesh",          // HAAP across an RF hop
}

const meshIP = "10.100.1.65"

func liveLogger(t *testing.T) *zap.Logger {
	t.Helper()
	cfg := zap.NewDevelopmentConfig()
	cfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	cfg.Level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	log, err := cfg.Build(zap.AddCallerSkip(1))
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	return log
}

func liveConfig() Config {
	return Config{
		Mode:           ModeAuto,
		Count:          3,
		Interval:       600 * time.Millisecond,
		Timeout:        3 * time.Second,
		OverallTimeout: 8 * time.Second,
		Concurrency:    4,
	}
}

func TestLiveModeResolution(t *testing.T) {
	p, err := New(liveConfig(), liveLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close() }()

	t.Logf("requested=%s effective=%s uid=%d gid=%d goos=%s",
		liveConfig().Mode, p.Mode(), os.Geteuid(), os.Getegid(), runtime.GOOS)

	if os.Geteuid() == 0 {
		t.Skip("running as root: the unprivileged expectation does not apply")
	}
	if p.Mode() != ModeUnprivileged {
		t.Fatalf("effective mode = %q, want %q for a non-root user (reason=%q)",
			p.Mode(), ModeUnprivileged, p.decision.reason)
	}
	t.Logf("reason: %s", p.decision.reason)
}

func TestLivePingLocalnode(t *testing.T) {
	p, err := New(liveConfig(), liveLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close() }()

	// The pinger's startup self-check, asked explicitly here.
	ip, err := p.checkMeshResolution(context.Background(), meshCheckName)
	if err != nil {
		t.Fatalf("mesh hostname resolution failed: %v", err)
	}
	t.Logf("mesh dns check: %s -> %s", meshCheckName, ip)

	res := p.Ping(context.Background(), "localnode.local.mesh")
	report(t, res)

	if res.Err != nil {
		t.Fatalf("Ping(localnode) err = %v", res.Err)
	}
	if res.IP != meshIP {
		t.Fatalf("resolved IP = %q, want %q", res.IP, meshIP)
	}
	if res.Recv < 3 || res.Sent < 3 {
		t.Fatalf("sent=%d recv=%d, want 3/3", res.Sent, res.Recv)
	}
	if !res.Alive() || res.LossPct() != 0 {
		t.Fatalf("alive=%t loss=%v%%", res.Alive(), res.LossPct())
	}
	if res.AvgMs <= 0 || res.AvgMs > 20 {
		t.Fatalf("avg = %v ms, want low single-digit ms for the gateway", res.AvgMs)
	}
	if res.Mode != ModeUnprivileged {
		t.Fatalf("result mode = %q, want the effective %q", res.Mode, ModeUnprivileged)
	}
	if res.TTL <= 0 {
		t.Logf("TTL not reported on this platform (ttl=%d)", res.TTL)
	}
}

func TestLivePingDistantNode(t *testing.T) {
	p, err := New(liveConfig(), liveLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close() }()

	res := p.Ping(context.Background(), "tst16.local.mesh")
	report(t, res)

	if !res.Alive() {
		t.Fatalf("distant node unreachable: %v", res.Err)
	}
	if res.IP == "" {
		t.Fatal("resolved IP missing from the result")
	}
	// Distant node through the tunnel chain: 100-800ms, not local single digits.
	if res.AvgMs < 20 {
		t.Fatalf("avg = %v ms, expected the distant-node band of 100-800ms", res.AvgMs)
	}
	if res.ElapsedMs > 8000 {
		t.Fatalf("session took %v ms, over the 8s overall ceiling", res.ElapsedMs)
	}
	t.Logf("distant node avg=%.1fms min=%.1fms max=%.1fms mdev=%.1fms via %s",
		res.AvgMs, res.MinMs, res.MaxMs, res.MdevMs, res.IP)
}

func TestLivePingUnknownNodeFailsFast(t *testing.T) {
	p, err := New(liveConfig(), liveLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close() }()

	start := time.Now()
	res := p.Ping(context.Background(), "definitely-not-a-real-node.local.mesh")
	elapsed := time.Since(start)
	report(t, res)

	if res.Recv != 0 {
		t.Fatalf("recv = %d, want 0", res.Recv)
	}
	if res.Alive() {
		t.Fatal("a name that does not resolve must never look alive")
	}
	if res.Err == nil {
		t.Fatal("want a clean error")
	}
	if !errors.Is(res.Err, ErrResolve) {
		t.Fatalf("Err = %v, want it to wrap ErrResolve", res.Err)
	}
	if errors.Is(res.Err, errDenied) {
		t.Fatalf("Err = %v, must not be an ICMP permission problem", res.Err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("took %v, want a fast failure well under the 8s ceiling", elapsed)
	}
	t.Logf("failed fast in %v: %v", elapsed.Round(time.Millisecond), res.Err)
}

// TestLivePingManyVersusSequential times both paths over the same hosts and settings.
func TestLivePingManyVersusSequential(t *testing.T) {
	cfg := liveConfig()
	p, err := New(cfg, liveLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close() }()

	// Sequential pass first so the parallel pass runs on warm routes.
	seqStart := time.Now()
	sequential := make(map[string]Result, len(liveHosts))
	for _, h := range liveHosts {
		sequential[h] = p.Ping(context.Background(), h)
	}
	seqElapsed := time.Since(seqStart)

	parStart := time.Now()
	got := p.PingMany(context.Background(), liveHosts)
	parElapsed := time.Since(parStart)

	alive := 0
	for _, h := range liveHosts {
		res, ok := got[h]
		if !ok {
			t.Fatalf("PingMany returned no entry for %q", h)
		}
		report(t, res)
		if res.Alive() {
			alive++
		}
		if res.Mode != p.Mode() {
			t.Fatalf("%q: mode = %q, want %q", h, res.Mode, p.Mode())
		}
	}
	t.Logf("hosts=%d alive=%d concurrency=%d", len(liveHosts), alive, cfg.Concurrency)
	t.Logf("sequential: %v (%v/node)", seqElapsed.Round(time.Millisecond),
		seqElapsed.Round(time.Millisecond)/time.Duration(len(liveHosts)))
	t.Logf("PingMany  : %v (%v/node)", parElapsed.Round(time.Millisecond),
		parElapsed.Round(time.Millisecond)/time.Duration(len(liveHosts)))
	t.Logf("speedup   : %.2fx", float64(seqElapsed)/float64(parElapsed))

	if alive < 3 {
		t.Fatalf("only %d of %d hosts answered; the mesh or the tunnel is down, "+
			"so this run cannot prove anything about fan-out", alive, len(liveHosts))
	}
	if parElapsed >= seqElapsed {
		t.Fatalf("PingMany (%v) was not faster than sequential (%v)", parElapsed, seqElapsed)
	}
	// Parallel is bounded by the slowest session, so the bar is 3/4 of sequential, not 4x.
	if parElapsed > seqElapsed*3/4 {
		t.Fatalf("PingMany (%v) saved less than a quarter of sequential (%v) with concurrency %d",
			parElapsed, seqElapsed, cfg.Concurrency)
	}
}

// TestLiveForcedPrivilegedAsNonRoot: a forced mode is never downgraded, hosts error clearly.
func TestLiveForcedPrivilegedAsNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, raw sockets are expected to work")
	}
	cfg := liveConfig()
	cfg.Mode = ModePrivileged

	p, err := New(cfg, liveLogger(t))
	if err != nil {
		t.Fatalf("New(valid but unavailable forced mode) returned an error: %v", err)
	}
	defer func() { _ = p.Close() }()

	if p.Mode() != ModePrivileged {
		t.Fatalf("effective mode = %q, want %q: forcing must not fall back", p.Mode(), ModePrivileged)
	}
	if p.decision.available {
		t.Log("raw icmp was openable despite a non-root uid; nothing to assert")
		return
	}
	t.Logf("forced privileged, probe said: %s", p.decision.reason)
	if p.decision.hint != "" {
		t.Logf("hint (log only, never in Result.Err): %s", p.decision.hint)
	}

	res := p.Ping(context.Background(), "localnode.local.mesh")
	report(t, res)
	if res.Err == nil {
		t.Fatal("want a permission error in unprivileged-free mode")
	}
	if !noPermission(res.Err) {
		t.Fatalf("Err = %v, want the EPERM from the raw socket", res.Err)
	}
	if strings.Contains(res.Err.Error(), "ping_group_range") || strings.Contains(res.Err.Error(), "CAP_NET_RAW") {
		t.Fatalf("the operator hint must stay in the log, not the error: %v", res.Err)
	}
	t.Logf("clear per-host error: %v (mode=%s)", res.Err, res.Mode)
}

func report(t *testing.T, r Result) {
	t.Helper()
	t.Logf("%-38s ip=%-15s mode=%-13s sent=%d recv=%d loss=%5.1f%% avg=%8.3fms min=%8.3fms max=%8.3fms mdev=%6.3fms ttl=%d elapsed=%7.1fms err=%v",
		r.Host, orDash(r.IP), r.Mode, r.Sent, r.Recv, r.LossPct(), r.AvgMs, r.MinMs, r.MaxMs, r.MdevMs, r.TTL, r.ElapsedMs, r.Err)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
