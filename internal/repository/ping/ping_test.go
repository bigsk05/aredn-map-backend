package ping

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// frozenAPI pins the public surface the scan engine compiles against.
var (
	_ func(Config, *zap.Logger) (*Pinger, error)                 = New
	_ func(*Pinger) string                                       = (*Pinger).Mode
	_ func(*Pinger, context.Context, string) Result              = (*Pinger).Ping
	_ func(*Pinger, context.Context, []string) map[string]Result = (*Pinger).PingMany
	_ func(*Pinger) error                                        = (*Pinger).Close
	_ func(Result) bool                                          = Result.Alive
	_ func(Result) float64                                       = Result.LossPct
)

// errDenied is the EPERM the kernel answers when the caller lacks privilege.
var errDenied error = syscall.EPERM

// okSocket reports every socket kind as openable.
func okSocket(bool) error { return nil }

// dgramOnlySocket is a non-root host: the datagram socket opens, the raw one is denied.
func dgramOnlySocket(privileged bool) error {
	if privileged {
		return &net.OpError{Op: "listen", Net: rawNetwork, Err: os.NewSyscallError("socket", errDenied)}
	}
	return nil
}

// noSocket denies both kinds.
func noSocket(privileged bool) error {
	if privileged {
		return dgramOnlySocket(true)
	}
	return &net.OpError{Op: "listen", Net: dgramNetwork, Err: os.NewSyscallError("socket", errDenied)}
}

// newTestPinger builds a Pinger that cannot reach the network: scripted probe, no self-check.
func newTestPinger(t *testing.T, cfg Config, probe socketProbe, lookup lookupFn) *Pinger {
	t.Helper()
	p, err := newPinger(cfg, zap.NewNop(), probe, lookup, false)
	if err != nil {
		t.Fatalf("newPinger: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// failingLookup refuses every name, so Ping can only ever fail before ICMP.
func failingLookup(ctx context.Context, host string) ([]net.IPAddr, error) {
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestResolveModeAutoPrefersUnprivileged(t *testing.T) {
	var order []string
	probe := func(privileged bool) error {
		order = append(order, socketKind(privileged))
		return nil
	}
	d, err := resolveMode(ModeAuto, probe, "darwin")
	if err != nil {
		t.Fatalf("resolveMode: %v", err)
	}
	if d.mode != ModeUnprivileged {
		t.Fatalf("mode = %q, want %q", d.mode, ModeUnprivileged)
	}
	if !d.available || d.warn {
		t.Fatalf("decision = %+v, want available and not warned", d)
	}
	// The raw socket must not be probed once the datagram one works.
	if strings.Join(order, ",") != dgramNetwork {
		t.Fatalf("probe order = %v, want just %q", order, dgramNetwork)
	}
	if !strings.Contains(d.reason, "auto") {
		t.Fatalf("reason = %q, want it to name the auto strategy", d.reason)
	}
}

func TestResolveModeAutoFallsBackToPrivileged(t *testing.T) {
	var order []string
	probe := func(privileged bool) error {
		order = append(order, socketKind(privileged))
		if privileged {
			return nil
		}
		return errDenied
	}
	d, err := resolveMode("", probe, "linux")
	if err != nil {
		t.Fatalf("resolveMode: %v", err)
	}
	if d.mode != ModePrivileged {
		t.Fatalf("mode = %q, want %q", d.mode, ModePrivileged)
	}
	if want := strings.Join([]string{dgramNetwork, rawNetwork}, ","); strings.Join(order, ",") != want {
		t.Fatalf("probe order = %v, want %v", order, want)
	}
	if !strings.Contains(d.reason, dgramNetwork) || !strings.Contains(d.reason, rawNetwork) {
		t.Fatalf("reason = %q, want both socket kinds named", d.reason)
	}
}

func TestResolveModeAutoWithNothingOpenKeepsUnprivilegedAndWarns(t *testing.T) {
	d, err := resolveMode(ModeAuto, noSocket, "darwin")
	if err != nil {
		t.Fatalf("resolveMode must not fail for auto: %v", err)
	}
	if d.mode != ModeUnprivileged {
		t.Fatalf("mode = %q, want %q", d.mode, ModeUnprivileged)
	}
	if d.available || !d.warn {
		t.Fatalf("decision = %+v, want unavailable and warned", d)
	}
	if !strings.Contains(d.reason, rawNetwork) || !strings.Contains(d.reason, dgramNetwork) {
		t.Fatalf("reason = %q, want both socket kinds named", d.reason)
	}
	if !strings.Contains(d.hint, "sandbox") {
		t.Fatalf("hint = %q, want a darwin specific explanation", d.hint)
	}
}

func TestResolveModeForcedPrivilegedDoesNotFallBack(t *testing.T) {
	probed := 0
	probe := func(privileged bool) error {
		probed++
		if !privileged {
			t.Fatal("forced privileged must not probe the datagram socket")
		}
		return errDenied
	}
	d, err := resolveMode(ModePrivileged, probe, "darwin")
	if err != nil {
		t.Fatalf("a valid but unavailable forced mode must still construct: %v", err)
	}
	if d.mode != ModePrivileged {
		t.Fatalf("mode = %q, want %q with no fallback", d.mode, ModePrivileged)
	}
	if probed != 1 {
		t.Fatalf("probe calls = %d, want 1", probed)
	}
	if !d.warn || d.available {
		t.Fatalf("decision = %+v, want warned and unavailable", d)
	}
	if !strings.Contains(d.reason, "forced by config") {
		t.Fatalf("reason = %q, want it to say the mode was forced", d.reason)
	}
}

func TestResolveModeForcedUnprivilegedHasNoFallbackEither(t *testing.T) {
	d, err := resolveMode(ModeUnprivileged, func(privileged bool) error {
		if privileged {
			t.Fatal("forced unprivileged must not probe the raw socket")
		}
		return errDenied
	}, "darwin")
	if err != nil {
		t.Fatalf("resolveMode: %v", err)
	}
	if d.mode != ModeUnprivileged || !d.warn {
		t.Fatalf("decision = %+v", d)
	}
}

func TestNormalizeModeAndInvalidMode(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ModeAuto},
		{"  AUTO ", ModeAuto},
		{"privileged", ModePrivileged},
		{"Unprivileged", ModeUnprivileged},
		{"raw", ""},
		{"udp", ""},
	} {
		if got := normalizeMode(tc.in); got != tc.want {
			t.Fatalf("normalizeMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := resolveMode("raw", okSocket, "darwin"); !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("resolveMode(invalid) err = %v, want ErrInvalidMode", err)
	}
	// New must reject the invalid value before it opens anything.
	opened := 0
	if _, err := newPinger(Config{Mode: "raw"}, zap.NewNop(), func(bool) error {
		opened++
		return nil
	}, failingLookup, false); !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("New(invalid mode) err = %v, want ErrInvalidMode", err)
	} else if opened != 0 {
		t.Fatalf("invalid mode still probed %d sockets", opened)
	}
	if _, err := New(Config{Mode: "raw"}, zap.NewNop()); !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("New(invalid mode) err = %v, want ErrInvalidMode", err)
	}
}

func TestResultAliveAndLossPct(t *testing.T) {
	tests := []struct {
		name      string
		res       Result
		wantAlive bool
		wantLoss  float64
	}{
		{"zero value is safe", Result{}, false, 100},
		{"all replies", Result{Sent: 3, Recv: 3}, true, 0},
		{"one lost", Result{Sent: 4, Recv: 3}, true, 25},
		{"total blackout", Result{Sent: 3, Recv: 0}, false, 100},
		{"nothing sent", Result{Sent: 0, Recv: 0}, false, 100},
		{"replies above sends cannot go negative", Result{Sent: 1, Recv: 4}, true, 0},
		{"negative counters clamp and are not alive", Result{Sent: 2, Recv: -2}, false, 100},
		{"partial session still alive", Result{Sent: 3, Recv: 1, Err: ErrSessionTimeout}, true, 200.0 / 3.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.Alive(); got != tc.wantAlive {
				t.Fatalf("Alive() = %t, want %t", got, tc.wantAlive)
			}
			if got := tc.res.LossPct(); got < tc.wantLoss-1e-9 || got > tc.wantLoss+1e-9 {
				t.Fatalf("LossPct() = %v, want %v", got, tc.wantLoss)
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	want := Config{
		Mode:           ModeAuto,
		Count:          defaultCount,
		Interval:       defaultInterval,
		Timeout:        defaultTimeout,
		OverallTimeout: defaultOverallTimeout,
		Concurrency:    defaultConcurrency,
	}
	if got != want {
		t.Fatalf("withDefaults() = %+v, want %+v", got, want)
	}

	// Zero fields are replaced, real values survive, and Mode is echoed back untouched.
	got = Config{Mode: "  PRIVILEGED ", Count: -1, Interval: 0, Concurrency: -5}.withDefaults()
	if got.Mode != "  PRIVILEGED " {
		t.Fatalf("Mode must stay as configured, got %q", got.Mode)
	}
	if got.Count != defaultCount || got.Interval != defaultInterval || got.Concurrency != defaultConcurrency {
		t.Fatalf("withDefaults() = %+v", got)
	}
	explicit := Config{Mode: ModePrivileged, Count: 5, Interval: time.Second, Timeout: 2 * time.Second, OverallTimeout: 30 * time.Second, Concurrency: 7}
	if got = explicit.withDefaults(); got != explicit {
		t.Fatalf("explicit values were overwritten: %+v", got)
	}
}

func TestSessionBudget(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want time.Duration
	}{
		{"defaults fit under the ceiling",
			Config{Count: 3, Interval: 600 * time.Millisecond, Timeout: 3 * time.Second, OverallTimeout: 8 * time.Second},
			3*600*time.Millisecond + 3*time.Second},
		{"clamped to the ceiling",
			Config{Count: 20, Interval: time.Second, Timeout: 3 * time.Second, OverallTimeout: 8 * time.Second},
			8 * time.Second},
		{"missing ceiling falls back",
			Config{Count: 1, Interval: time.Second, Timeout: time.Second},
			2 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Pinger{cfg: tc.cfg.withDefaults()}
			if got := p.sessionBudget(); got != tc.want {
				t.Fatalf("sessionBudget() = %v, want %v", got, tc.want)
			}
			if ceiling := p.cfg.OverallTimeout; ceiling > 0 && p.sessionBudget() > ceiling {
				t.Fatalf("sessionBudget() = %v exceeds overall %v", p.sessionBudget(), ceiling)
			}
		})
	}
}

func TestPingHonoursContextCancellation(t *testing.T) {
	// A resolver that never answers: only cancellation can end the call.
	blocked := make(chan struct{})
	defer close(blocked)
	p := newTestPinger(t, Config{Concurrency: 1}, okSocket, func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-blocked:
			return nil, errDenied
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res := p.Ping(ctx, "never-resolves.local.mesh")
	elapsed := time.Since(start)

	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context canceled", res.Err)
	}
	if res.Recv != 0 || res.Sent != 0 || res.Alive() {
		t.Fatalf("result must stay zero-valued: %+v", res)
	}
	if res.Host != "never-resolves.local.mesh" || res.Mode != ModeUnprivileged {
		t.Fatalf("result lost its labels: %+v", res)
	}
	if elapsed > time.Second {
		t.Fatalf("cancellation took %v, want under a second", elapsed)
	}
	if res.ElapsedMs <= 0 {
		t.Fatalf("ElapsedMs = %v, want the wall clock recorded", res.ElapsedMs)
	}

	// An already-expired deadline short-circuits before any resolver work.
	dead, cancelDead := context.WithTimeout(context.Background(), 0)
	defer cancelDead()
	if res := p.Ping(dead, "x.local.mesh"); !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("expired context: Err = %v", res.Err)
	}
}

func TestPingFailsFastOnUnknownHost(t *testing.T) {
	p := newTestPinger(t, Config{Concurrency: 2}, okSocket, failingLookup)

	start := time.Now()
	res := p.Ping(context.Background(), "definitely-not-a-real-node.local.mesh")
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("unknown host took %v, want a fast failure", elapsed)
	}
	if !errors.Is(res.Err, ErrResolve) {
		t.Fatalf("Err = %v, want ErrResolve", res.Err)
	}
	var dnsErr *net.DNSError
	if !errors.As(res.Err, &dnsErr) || !dnsErr.IsNotFound {
		t.Fatalf("the DNS error must stay wrapped: %v", res.Err)
	}
	if res.Recv != 0 || res.Alive() || res.LossPct() != 100 {
		t.Fatalf("res = %+v", res)
	}
	if res.IP != "" {
		t.Fatalf("IP = %q, want empty when nothing resolved", res.IP)
	}
}

func TestPingRejectsEmptyHostAndIPShortcutsResolver(t *testing.T) {
	lookups := 0
	p := newTestPinger(t, Config{Concurrency: 1}, okSocket, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		lookups++
		return nil, errors.New("must not be consulted for a literal address")
	})

	if res := p.Ping(context.Background(), "   "); !errors.Is(res.Err, ErrEmptyHost) {
		t.Fatalf("empty host Err = %v, want ErrEmptyHost", res.Err)
	}
	if _, err := p.resolve(context.Background(), "10.100.1.65"); err != nil {
		t.Fatalf("literal IP must resolve without DNS: %v", err)
	}
	if _, err := p.resolve(context.Background(), "fe80::1"); err != nil {
		t.Fatalf("literal IPv6 must resolve without DNS: %v", err)
	}
	if lookups != 0 {
		t.Fatalf("resolver consulted %d times for literals", lookups)
	}
}

func TestResolvePrefersIPv4AndUsesTheInjectedResolver(t *testing.T) {
	calls := 0
	p := newTestPinger(t, Config{}, okSocket, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		calls++
		if host != "node.local.mesh" {
			t.Fatalf("looked up %q", host)
		}
		return []net.IPAddr{
			{IP: net.ParseIP("fd00::5")},
			{IP: net.ParseIP("10.100.1.65")},
		}, nil
	})
	ip, err := p.resolve(context.Background(), "node.local.mesh")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ip.String() != "10.100.1.65" {
		t.Fatalf("resolve picked %v, want the mesh IPv4 address", ip)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}

	// An empty answer is an error, not a nil IP handed to pro-bing.
	empty := newTestPinger(t, Config{}, okSocket, func(context.Context, string) ([]net.IPAddr, error) {
		return nil, nil
	})
	if _, err := empty.resolve(context.Background(), "ghost.local.mesh"); !errors.Is(err, ErrResolve) {
		t.Fatalf("empty answer Err = %v, want ErrResolve", err)
	}
}

// TestSemaphoreIsTheGlobalCeiling records the high-water mark inside the session.
func TestSemaphoreIsTheGlobalCeiling(t *testing.T) {
	const ceiling = 3
	const callers = 40

	var inFlight, peak atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }

	p := newTestPinger(t, Config{
		Count:          1,
		Interval:       time.Millisecond,
		Timeout:        10 * time.Millisecond,
		OverallTimeout: 50 * time.Millisecond,
		Concurrency:    ceiling,
	}, okSocket, func(ctx context.Context, _ string) ([]net.IPAddr, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-release:
			return nil, errDenied
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	defer unblock()

	var wg sync.WaitGroup
	failures := make(chan error, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := p.Ping(context.Background(), fmt.Sprintf("node%d.local.mesh", i))
			switch {
			case res.Err == nil:
				failures <- fmt.Errorf("node%d: expected a failure, got %+v", i, res)
			case res.Recv != 0 || res.Sent != 0:
				failures <- fmt.Errorf("node%d: counters leaked: %+v", i, res)
			case res.Mode != ModeUnprivileged:
				failures <- fmt.Errorf("node%d: Mode = %q", i, res.Mode)
			}
		}(i)
	}
	// Let the workers pile up against the semaphore, then let them all finish.
	time.Sleep(50 * time.Millisecond)
	unblock()
	wg.Wait()
	close(failures)

	if got := peak.Load(); got > ceiling {
		t.Fatalf("peak concurrent sessions = %d, over the ceiling of %d", got, ceiling)
	}
	if peak.Load() == 0 {
		t.Fatal("no session ever started")
	}
	for err := range failures {
		t.Fatal(err)
	}

	// A closed pinger must not accept new work, and Close is idempotent.
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	if res := p.Ping(context.Background(), "after-close.local.mesh"); !errors.Is(res.Err, ErrClosed) {
		t.Fatalf("after Close: Err = %v, want ErrClosed", res.Err)
	}
	if got := p.PingMany(context.Background(), []string{"a.local.mesh"}); !errors.Is(got["a.local.mesh"].Err, ErrClosed) {
		t.Fatalf("PingMany after Close: %+v", got["a.local.mesh"])
	}
}

func TestPingManyCoversEveryHost(t *testing.T) {
	hosts := []string{
		"localnode.local.mesh",
		" tst04-supernode.local.mesh ",
		"localnode.local.mesh", // duplicate of the first
		"",                     // dropped
		"tst16.local.mesh",
	}
	var mu sync.Mutex
	seen := make(map[string]int)
	p := newTestPinger(t, Config{Concurrency: 2}, okSocket, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		mu.Lock()
		seen[host]++
		mu.Unlock()
		return failingLookup(ctx, host)
	})

	got := p.PingMany(context.Background(), hosts)
	want := []string{"localnode.local.mesh", "tst04-supernode.local.mesh", "tst16.local.mesh"}
	if len(got) != len(want) {
		t.Fatalf("results = %d entries, want %d", len(got), len(want))
	}
	for _, h := range want {
		res, ok := got[h]
		if !ok {
			t.Fatalf("missing result for %q", h)
		}
		if res.Host != h || res.Mode != ModeUnprivileged || !errors.Is(res.Err, ErrResolve) {
			t.Fatalf("res = %+v", res)
		}
		if res.Recv != 0 || res.Alive() || res.ElapsedMs <= 0 {
			t.Fatalf("res = %+v, want dead with a measured elapsed", res)
		}
	}
	if got := p.PingMany(context.Background(), nil); len(got) != 0 {
		t.Fatalf("empty host list returned %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["localnode.local.mesh"] != 1 {
		t.Fatalf("duplicate hostname probed %d times", seen["localnode.local.mesh"])
	}
}

func TestPingManyStopsOnContextCancel(t *testing.T) {
	p := newTestPinger(t, Config{Concurrency: 2}, okSocket, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := p.PingMany(ctx, []string{"a.local.mesh", "b.local.mesh"})
	if len(got) != 2 {
		t.Fatalf("every host needs an entry even when cancelled, got %d", len(got))
	}
	for _, res := range got {
		if !errors.Is(res.Err, context.Canceled) {
			t.Fatalf("%q: Err = %v, want context canceled", res.Host, res.Err)
		}
	}
}

func TestModeResolutionIsLoggedAtInfoWithReason(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	p, err := newPinger(Config{Mode: ModeAuto}, zap.New(core), dgramOnlySocket, defaultLookup, false)
	if err != nil {
		t.Fatalf("newPinger: %v", err)
	}
	defer func() { _ = p.Close() }()

	lines := logs.FilterMessage("icmp mode resolved").All()
	if len(lines) != 1 {
		t.Fatalf("want exactly one decision line, got %v", logs.All())
	}
	entry := lines[0]
	if entry.Level != zapcore.InfoLevel {
		t.Fatalf("level = %v, want info", entry.Level)
	}
	fields := entry.ContextMap()
	if fields["mode"] != ModeUnprivileged {
		t.Fatalf("mode field = %v, want %q", fields["mode"], ModeUnprivileged)
	}
	reason, _ := fields["reason"].(string)
	if !strings.Contains(reason, dgramNetwork) || !strings.Contains(reason, "auto") {
		t.Fatalf("reason = %q, want the strategy and the socket kind", reason)
	}
	if fields["goos"] != runtime.GOOS {
		t.Fatalf("goos field = %v, want %v", fields["goos"], runtime.GOOS)
	}
	if fields["socket_available"] != true {
		t.Fatalf("socket_available = %v", fields["socket_available"])
	}
	if p.Mode() != ModeUnprivileged {
		t.Fatalf("Mode() = %q", p.Mode())
	}
}

func TestUnavailableModeIsLoggedAsWarnWithHint(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	p, err := newPinger(Config{Mode: ModePrivileged}, zap.New(core), noSocket, defaultLookup, false)
	if err != nil {
		t.Fatalf("newPinger must not fail on an unavailable forced mode: %v", err)
	}
	defer func() { _ = p.Close() }()

	lines := logs.FilterMessage("icmp mode resolved").All()
	if len(lines) != 1 {
		t.Fatalf("lines = %v", logs.All())
	}
	if lines[0].Level != zapcore.WarnLevel {
		t.Fatalf("level = %v, want warn", lines[0].Level)
	}
	fields := lines[0].ContextMap()
	if fields["mode"] != ModePrivileged {
		t.Fatalf("mode = %v, want the forced value echoed back", fields["mode"])
	}
	if fields["hint"] == nil {
		t.Fatalf("hint missing from %v", fields)
	}
	if p.Mode() != ModePrivileged {
		t.Fatalf("Mode() = %q, want no silent fallback", p.Mode())
	}
}

func TestMeshResolutionCheckReportsBothOutcomes(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	p, err := newPinger(Config{}, zap.New(core), okSocket, func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.100.1.65")}}, nil
	}, false)
	if err != nil {
		t.Fatalf("newPinger: %v", err)
	}
	defer func() { _ = p.Close() }()

	if ip, err := p.checkMeshResolution(context.Background(), meshCheckName); err != nil || ip != "10.100.1.65" {
		t.Fatalf("checkMeshResolution = %q, %v", ip, err)
	}
	p.reportMeshResolution(context.Background(), meshCheckName)
	if len(logs.FilterMessage("mesh hostname resolution ok").All()) != 1 {
		t.Fatalf("missing success line: %v", logs.All())
	}

	// The failing side must be a warn that points at the resolver, not at ICMP.
	failCore, failLogs := observer.New(zapcore.DebugLevel)
	fp, err := newPinger(Config{}, zap.New(failCore), okSocket, failingLookup, false)
	if err != nil {
		t.Fatalf("newPinger: %v", err)
	}
	defer func() { _ = fp.Close() }()

	fp.reportMeshResolution(context.Background(), meshCheckName)
	entries := failLogs.FilterMessage("mesh hostname resolution unavailable").All()
	if len(entries) != 1 {
		t.Fatalf("missing failure line: %v", failLogs.All())
	}
	if entries[0].Level != zapcore.WarnLevel {
		t.Fatalf("level = %v, want warn", entries[0].Level)
	}
	if hint, _ := entries[0].ContextMap()["hint"].(string); !strings.Contains(hint, "resolver") {
		t.Fatalf("hint = %q, want it to point at the resolver", hint)
	}
}

func TestLinuxHintNamesTheSysctlAndTheCapability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ping_group_range")
	restore := setPingGroupRange(t, path)
	defer restore()

	gid := os.Getegid()
	// The sysctl is unreadable on macOS, so the hint still has to cope.
	if h := unprivHint("linux", false, errDenied); !strings.Contains(h, "ping_group_range") {
		t.Fatalf("unreadable sysctl: hint = %q", h)
	}

	for _, tc := range []struct {
		name       string
		contents   string
		wantResult string
	}{
		{"gid inside the range", fmt.Sprintf("0 %d", gid), "includes=true"},
		{"gid outside the range", fmt.Sprintf("%d %d", gid+1, gid+2), "includes=false"},
		{"malformed sysctl", "nonsense", "includes=false"},
	} {
		if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		hint := unprivHint("linux", false, errDenied)
		if !strings.Contains(hint, tc.wantResult) {
			t.Fatalf("%s: hint = %q, want %q", tc.name, hint, tc.wantResult)
		}
		if !strings.Contains(hint, fmt.Sprintf("%d", gid)) {
			t.Fatalf("%s: hint %q must mention gid %d", tc.name, hint, gid)
		}
	}

	// The privileged failure on Linux must mention CAP_NET_RAW as well.
	if err := os.WriteFile(path, []byte(fmt.Sprintf("0 %d", gid)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	priv := unprivHint("linux", true, errDenied)
	if !strings.Contains(priv, "CAP_NET_RAW") {
		t.Fatalf("privileged hint = %q, want CAP_NET_RAW", priv)
	}
	if !strings.Contains(priv, "ping_group_range") {
		t.Fatalf("privileged hint = %q, want the sysctl too", priv)
	}
	// A non-permission error must not invent a permissions lecture.
	if h := unprivHint("linux", true, errors.New("network is down")); !strings.Contains(h, "not a permissions problem") {
		t.Fatalf("unexpected-error hint = %q", h)
	}
	// The same code on macOS talks about macOS.
	if h := unprivHint("darwin", true, errDenied); !strings.Contains(h, "macOS") {
		t.Fatalf("darwin hint = %q", h)
	}
}

func TestGroupAllowedRangeAndSocketKind(t *testing.T) {
	for _, tc := range []struct {
		spec       string
		gid        int
		allowed    bool
		couldParse bool
	}{
		{"0 2147483647", 20, true, true},
		{"1 1", 20, false, true},
		{"20 20", 20, true, true},
		{"", 20, false, false},
		{"0", 20, false, false},
		{"x y", 20, false, false},
	} {
		allowed, parsed := groupAllowedRange(tc.spec, tc.gid)
		if allowed != tc.allowed || parsed != tc.couldParse {
			t.Fatalf("groupAllowedRange(%q, %d) = %t/%t, want %t/%t",
				tc.spec, tc.gid, allowed, parsed, tc.allowed, tc.couldParse)
		}
	}
	if socketKind(true) != rawNetwork || socketKind(false) != dgramNetwork {
		t.Fatal("socket kinds mapped wrong")
	}
	if got := readPingGroupRange(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("readPingGroupRange of a missing file = %q", got)
	}
	if !noPermission(errDenied) || noPermission(nil) || noPermission(errors.New("boom")) {
		t.Fatal("noPermission misclassified")
	}
	if !noPermission(&net.OpError{Op: "listen", Err: os.NewSyscallError("socket", errEPERM)}) {
		t.Fatal("EPERM from a socket error must count as a permission failure")
	}
	if !noPermission(&net.OpError{Op: "listen", Err: os.NewSyscallError("socket", errEACCES)}) {
		t.Fatal("EACCES must count as a permission failure too")
	}
}

// setPingGroupRange points the sysctl reader at a temp file; callers must not be parallel.
func setPingGroupRange(t *testing.T, path string) func() {
	t.Helper()
	old := pingGroupRangePath
	pingGroupRangePath = path
	return func() { pingGroupRangePath = old }
}

// TestNewAgainstTheRealKernel talks to the OS and only asserts Mode is one of the two keys.
func TestNewAgainstTheRealKernel(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	p, err := New(Config{Count: 1, Interval: 10 * time.Millisecond, Timeout: 50 * time.Millisecond, OverallTimeout: 200 * time.Millisecond, Concurrency: 2}, zap.New(core))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close() }()

	if m := p.Mode(); m != ModeUnprivileged && m != ModePrivileged {
		t.Fatalf("Mode() = %q", m)
	}
	lines := logs.FilterMessage("icmp mode resolved").All()
	if len(lines) != 1 {
		t.Fatalf("decision line missing: %v", logs.All())
	}
	// The startup self-check only logs; it must not change the resolved mode.
	if p.Mode() != lines[0].ContextMap()["mode"] {
		t.Fatalf("Mode() = %q but the log said %v", p.Mode(), lines[0].ContextMap()["mode"])
	}
	t.Logf("real socket probe: %v", lines[0].ContextMap())
}
