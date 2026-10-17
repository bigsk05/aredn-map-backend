// Package ping probes AREDN mesh hostnames over ICMP and reports liveness and latency.
// Unprivileged sockets are tried before raw ip4:icmp; New resolves, Mode reports the choice.
package ping

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	probing "github.com/prometheus-community/pro-bing"
	"go.uber.org/zap"
)

// Config is the icmp section of config.yaml; the zero value is valid and fills in defaults.
type Config struct {
	// Mode is "auto", "unprivileged" or "privileged"; auto keeps the first socket kind that opens.
	Mode string
	// Count is how many echo requests one session sends.
	Count int
	// Interval is the gap between two echo requests.
	Interval time.Duration
	// Timeout bounds a single reply and hostname resolution.
	Timeout time.Duration
	// OverallTimeout is the wall-clock ceiling of one session, including teardown.
	OverallTimeout time.Duration
	// Concurrency caps simultaneous sessions for the whole Pinger via a shared semaphore.
	Concurrency int
}

// Defaults mirror the icmp block of the shipped config.yaml.
const (
	defaultMode           = ModeAuto
	defaultCount          = 3
	defaultInterval       = 600 * time.Millisecond
	defaultTimeout        = 3 * time.Second
	defaultOverallTimeout = 8 * time.Second
	defaultConcurrency    = 16
)

// drainGrace is how long a stopped pro-bing gets to report statistics after the ceiling fires.
const drainGrace = 500 * time.Millisecond

// Sentinel errors: wrapped, never replaced, so callers can branch on errors.Is.
var (
	// ErrEmptyHost means no hostname was supplied.
	ErrEmptyHost = errors.New("ping: empty host")
	// ErrClosed means the Pinger was closed.
	ErrClosed = errors.New("ping: pinger closed")
	// ErrResolve means the hostname never made it to an IP: DNS, not ICMP.
	ErrResolve = errors.New("ping: hostname resolution failed")
	// ErrNoReply means echo requests went out but nothing came back.
	ErrNoReply = errors.New("ping: no reply")
	// ErrSessionTimeout means a session hit Config.OverallTimeout.
	ErrSessionTimeout = errors.New("ping: session exceeded overall timeout")
	// ErrIncomplete means PingMany stopped before every host was probed.
	ErrIncomplete = errors.New("ping: probing stopped early")
)

// Result is one bounded ping session; the zero value means not alive and 100% loss.
type Result struct {
	Host      string
	IP        string
	Sent      int
	Recv      int
	AvgMs     float64
	MinMs     float64
	MaxMs     float64
	MdevMs    float64
	TTL       int
	Mode      string
	Err       error
	ElapsedMs float64
}

// Alive is the stored verdict: at least one reply, even when Err says the session was cut short.
func (r Result) Alive() bool { return r.Recv > 0 }

// LossPct is 0..100 packet loss; a session that never transmitted is fully lost.
func (r Result) LossPct() float64 {
	if r.Sent <= 0 {
		return 100
	}
	loss := float64(r.Sent-r.Recv) / float64(r.Sent) * 100
	switch {
	case loss < 0:
		return 0
	case loss > 100:
		return 100
	default:
		return loss
	}
}

// withDefaults fills unset fields, leaving Mode as configured for resolveMode.
func (c Config) withDefaults() Config {
	if strings.TrimSpace(c.Mode) == "" {
		c.Mode = defaultMode
	}
	if c.Count <= 0 {
		c.Count = defaultCount
	}
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.OverallTimeout <= 0 {
		c.OverallTimeout = defaultOverallTimeout
	}
	if c.Concurrency <= 0 {
		c.Concurrency = defaultConcurrency
	}
	return c
}

// Pinger is a concurrency-safe ICMP prober; one is enough for the process.
type Pinger struct {
	cfg      Config
	log      *zap.Logger
	probe    socketProbe
	lookup   lookupFn
	decision decision
	mode     string

	sem chan struct{}

	// baseCtx is cancelled by Close; it only bounds the startup self-check.
	baseCtx    context.Context
	baseCancel context.CancelFunc

	mu      sync.Mutex
	closing bool
	closed  atomic.Bool
	wg      sync.WaitGroup
}

// New returns a ready Pinger: invalid modes error; an unavailable forced mode is kept.
func New(cfg Config, log *zap.Logger) (*Pinger, error) {
	return newPinger(cfg, log, openICMPSocket, defaultLookup, true)
}

// newPinger is the testable core with the socket probe, resolver and mesh-DNS report injected.
func newPinger(cfg Config, log *zap.Logger, probe socketProbe, lookup lookupFn, reportMeshDNS bool) (*Pinger, error) {
	if log == nil {
		log = zap.NewNop()
	}
	if probe == nil {
		probe = openICMPSocket
	}
	if lookup == nil {
		lookup = defaultLookup
	}

	d, err := resolveMode(cfg.Mode, probe, runtime.GOOS)
	if err != nil {
		return nil, err
	}

	c := cfg.withDefaults()
	baseCtx, baseCancel := context.WithCancel(context.Background())
	p := &Pinger{
		cfg:        c,
		log:        log,
		probe:      probe,
		lookup:     lookup,
		decision:   d,
		mode:       d.mode,
		sem:        make(chan struct{}, c.Concurrency),
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
	}

	fields := []zap.Field{
		zap.String("mode", d.mode),
		zap.String("reason", d.reason),
		zap.String("requested", c.Mode),
		zap.String("goos", runtime.GOOS),
		zap.Bool("socket_available", d.available),
		zap.Int("count", c.Count),
		zap.Duration("interval", c.Interval),
		zap.Duration("timeout", c.Timeout),
		zap.Duration("overall_timeout", c.OverallTimeout),
		zap.Int("concurrency", c.Concurrency),
	}
	if d.hint != "" {
		fields = append(fields, zap.String("hint", d.hint))
	}
	if d.warn {
		p.log.Warn("icmp mode resolved", fields...)
	} else {
		p.log.Info("icmp mode resolved", fields...)
	}

	if reportMeshDNS {
		go p.reportMeshResolution(baseCtx, meshCheckName)
	}
	return p, nil
}

// Mode reports the effective ICMP mode, not the requested one.
func (p *Pinger) Mode() string { return p.mode }

// Ping runs one bounded session against host and always returns a usable Result.
func (p *Pinger) Ping(ctx context.Context, host string) (res Result) {
	start := time.Now()
	recordElapsed := func() { res.ElapsedMs = ms(time.Since(start)) }
	defer recordElapsed()

	res = Result{Host: strings.TrimSpace(host), Mode: p.mode}
	if ctx == nil {
		ctx = context.Background()
	}
	switch {
	case p.isClosed():
		res.Err = ErrClosed
		return res
	case res.Host == "":
		res.Err = ErrEmptyHost
		return res
	}
	if !p.track() {
		res.Err = ErrClosed
		return res
	}
	defer p.untrack()

	if err := p.acquire(ctx); err != nil {
		res.Err = err
		return res
	}
	defer p.release()

	ip, err := p.resolve(ctx, res.Host)
	if err != nil {
		res.Err = err
		return res
	}
	res.IP = ip.String()

	stats, runErr := p.runSession(ctx, ip)
	res.Sent = stats.PacketsSent
	res.Recv = stats.PacketsRecv
	res.AvgMs = ms(stats.AvgRtt)
	res.MinMs = ms(stats.MinRtt)
	res.MaxMs = ms(stats.MaxRtt)
	res.MdevMs = ms(stats.StdDevRtt)
	res.TTL = lastTTL(stats.TTLs)
	res.Err = runErr
	if res.Recv == 0 && res.Err == nil {
		res.Err = fmt.Errorf("%w: %s sent %d probe(s), none answered", ErrNoReply, res.Host, res.Sent)
	}
	// Stamp the wall clock before the log line so the two cannot disagree.
	recordElapsed()
	p.logOutcome(res)
	return res
}

// PingMany probes unique hosts, one entry each, bounded by the global semaphore shared with Ping.
func (p *Pinger) PingMany(ctx context.Context, hosts []string) map[string]Result {
	if ctx == nil {
		ctx = context.Background()
	}
	unique := make([]string, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		unique = append(unique, h)
	}

	out := make(map[string]Result, len(unique))
	if len(unique) == 0 {
		return out
	}

	workers := min(p.cfg.Concurrency, len(unique))

	jobs := make(chan string)
	results := make(chan Result, len(unique))

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for h := range jobs {
				results <- p.Ping(ctx, h)
			}
		})
	}

	go func() {
		defer close(jobs)
		for _, h := range unique {
			select {
			case jobs <- h:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
	close(results)
	for r := range results {
		out[r.Host] = r
	}

	if len(out) < len(unique) {
		err := ctx.Err()
		if err == nil {
			err = ErrIncomplete
		}
		for _, h := range unique {
			if _, ok := out[h]; !ok {
				out[h] = Result{Host: h, Mode: p.mode, Err: err}
			}
		}
	}
	return out
}

// Close stops new sessions, waits for running ones, and is safe to call twice.
func (p *Pinger) Close() error {
	p.mu.Lock()
	p.closing = true
	p.closed.Store(true)
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

// resolve turns host into an address once; the session then talks to the IP only.
func (p *Pinger) resolve(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	cctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	ips, err := p.lookup(cctx, host)
	if err != nil {
		if cerr := cctx.Err(); cerr != nil && errors.Is(err, cerr) {
			err = cerr
		}
		return nil, fmt.Errorf("%w %s: %w", ErrResolve, host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w %s: no addresses returned", ErrResolve, host)
	}
	for _, a := range ips {
		if v4 := a.IP.To4(); v4 != nil {
			return v4, nil
		}
	}
	return ips[0].IP, nil
}

// sessionBudget is Count*Interval plus one Timeout for the last reply, capped by the ceiling.
func (p *Pinger) sessionBudget() time.Duration {
	budget := min(time.Duration(p.cfg.Count)*p.cfg.Interval+p.cfg.Timeout, p.cfg.OverallTimeout)
	if budget <= 0 {
		budget = p.cfg.OverallTimeout
	}
	return budget
}

// newSession builds the one-shot pro-bing instance for an already resolved IP.
func (p *Pinger) newSession(ip net.IP) *probing.Pinger {
	pg := probing.New(ip.String())
	if ip.To4() != nil {
		pg.SetNetwork("ip4")
	} else {
		pg.SetNetwork("ip6")
	}
	pg.SetIPAddr(&net.IPAddr{IP: ip})
	pg.SetPrivileged(p.mode == ModePrivileged)
	// pro-bing's default logger writes to stderr and can Fatalf mid-loop; use the zap hooks.
	pg.SetLogger(probing.NoopLogger{})
	pg.RecordRtts = false
	pg.Count = p.cfg.Count
	pg.Interval = p.cfg.Interval
	pg.Timeout = p.sessionBudget()

	pg.OnRecv = func(pkt *probing.Packet) {
		p.log.Debug("icmp reply",
			zap.String("host", pkt.Addr),
			zap.Int("seq", pkt.Seq),
			zap.Float64("rtt_ms", ms(pkt.Rtt)),
			zap.Int("ttl", pkt.TTL),
			zap.String("mode", p.mode),
		)
	}
	pg.OnSendError = func(pkt *probing.Packet, err error) {
		p.noteSocketError(err, "icmp send error", pkt.Addr)
	}
	pg.OnRecvError = func(err error) {
		var ne *net.OpError
		if errors.As(err, &ne) && ne.Timeout() {
			return // the usual quiet gap between packets
		}
		p.noteSocketError(err, "icmp recv error", "")
	}
	return pg
}

// runSession drives pro-bing and guarantees it cannot outlive OverallTimeout.
func (p *Pinger) runSession(ctx context.Context, ip net.IP) (*probing.Statistics, error) {
	pg := p.newSession(ip)

	stats, err := bounded(ctx, p.cfg.OverallTimeout, drainGrace,
		func(sctx context.Context) (*probing.Statistics, error) {
			runErr := pg.RunWithContext(sctx)
			return pg.Statistics(), runErr
		},
		pg.Stop,
		pg.Statistics,
	)
	if stats == nil {
		stats = &probing.Statistics{}
	}
	if err != nil && noPermission(err) {
		p.log.Warn("icmp socket denied during session",
			zap.String("mode", p.mode),
			zap.String("reason", p.decision.reason),
			zap.String("hint", unprivHint(runtime.GOOS, p.mode == ModePrivileged, err)),
			zap.Error(err),
		)
	}
	return stats, p.normalizeRunErr(err, ctx)
}

// normalizeRunErr keeps caller cancellation first and maps a bare deadline to ErrSessionTimeout.
func (p *Pinger) normalizeRunErr(err error, ctx context.Context) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrSessionTimeout
	}
	return err
}

// ceilingErr names the reason a bounded session was cut short.
func ceilingErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrSessionTimeout
}

// noteSocketError logs a mid-session socket error, adding the permission hint on denials.
func (p *Pinger) noteSocketError(err error, msg, addr string) {
	fields := []zap.Field{
		zap.String("mode", p.mode),
		zap.String("addr", addr),
		zap.Error(err),
	}
	if noPermission(err) {
		fields = append(fields, zap.String("hint",
			unprivHint(runtime.GOOS, p.mode == ModePrivileged, err)))
		p.log.Warn(msg, fields...)
		return
	}
	p.log.Debug(msg, fields...)
}

func (p *Pinger) logOutcome(res Result) {
	fields := []zap.Field{
		zap.String("host", res.Host),
		zap.String("ip", res.IP),
		zap.Int("sent", res.Sent),
		zap.Int("recv", res.Recv),
		zap.Float64("avg_ms", res.AvgMs),
		zap.Float64("min_ms", res.MinMs),
		zap.Float64("max_ms", res.MaxMs),
		zap.Float64("mdev_ms", res.MdevMs),
		zap.Float64("loss_pct", res.LossPct()),
		zap.Int("ttl", res.TTL),
		zap.String("mode", res.Mode),
		zap.Float64("elapsed_ms", res.ElapsedMs),
	}
	if res.Err != nil {
		fields = append(fields, zap.Error(res.Err))
		p.log.Warn("icmp probe failed", fields...)
		return
	}
	p.log.Debug("icmp probe ok", fields...)
}

func (p *Pinger) acquire(ctx context.Context) error {
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Active is how many sessions hold a semaphore slot right now.
func (p *Pinger) Active() int { return len(p.sem) }

// Slots is the ceiling on simultaneous ping sessions.
func (p *Pinger) Slots() int { return cap(p.sem) }

func (p *Pinger) release() { <-p.sem }

func (p *Pinger) isClosed() bool { return p.closed.Load() }

// track registers an in-flight session unless Close has begun.
func (p *Pinger) track() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return false
	}
	p.wg.Add(1)
	return true
}

func (p *Pinger) untrack() { p.wg.Done() }

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func lastTTL(ttls []uint8) int {
	if len(ttls) == 0 {
		return 0
	}
	return int(ttls[len(ttls)-1])
}
