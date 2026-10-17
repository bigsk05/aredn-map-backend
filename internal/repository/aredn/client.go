// Package aredn crawls AREDN 4.x nodes out of the unauthenticated htmx HTML fragments
// /a/status, /a/u-local-and-neighbor-devices, /a/neighbor-device?m=<mac> and /a/sysinfo.
package aredn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Endpoints and limits.
const (
	// MeshDomain is the mDNS domain every AREDN node answers on.
	MeshDomain = "local.mesh"

	statusPath    = "/a/status"
	neighborsPath = "/a/u-local-and-neighbor-devices"
	devicePath    = "/a/neighbor-device"

	// maxBodyBytes caps one response; a supernode's full /a/sysinfo dump is about 760 KB.
	maxBodyBytes = 16 << 20

	defaultTimeout      = 15 * time.Second
	defaultConnectTO    = 5 * time.Second
	defaultMaxRetries   = 2
	defaultRetryBackoff = 400 * time.Millisecond
	defaultUserAgent    = "aredn-scan/1.0 (mesh crawler)"
)

// Config tunes the HTTP client; zero values fall back to the defaults above.
type Config struct {
	Timeout      time.Duration
	ConnectTO    time.Duration
	MaxRetries   int
	RetryBackoff time.Duration
	UserAgent    string
}

// withDefaults returns the config with zero values replaced.
func (cfg Config) withDefaults() Config {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.ConnectTO <= 0 {
		cfg.ConnectTO = defaultConnectTO
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = defaultRetryBackoff
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	return cfg
}

// Client fetches and parses AREDN 4.x HTML fragments.
type Client struct {
	cfg     Config
	log     *zap.Logger
	http    *http.Client
	retrier *retrier
}

// New builds a crawler client. Redirects follow the stdlib default policy:
// /a/status answers 307 with the node's canonical host in Location.
func New(cfg Config, log *zap.Logger) *Client {
	if log == nil {
		log = zap.NewNop()
	}
	cfg = cfg.withDefaults()
	dialer := &net.Dialer{Timeout: cfg.ConnectTO, KeepAlive: 15 * time.Second}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Client{
		cfg: cfg,
		log: log,
		http: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: transport,
		},
		retrier: &retrier{attempts: cfg.MaxRetries + 1, backoff: cfg.RetryBackoff},
	}
}

// statusError marks a status the crawler must not retry: a 401 or the removed JSON API's 404.
type statusError struct {
	code int
	url  string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("aredn: unexpected status %d from %s", e.code, e.url)
}

func (e *statusError) Status() int { return e.code }

// retrier wraps the bounded retry loop so the policy lives in one place.
type retrier struct {
	attempts int
	backoff  time.Duration
}

// do runs fn until success, permanent failure, or an exhausted budget; backoff waits honor ctx.
func (r *retrier) do(ctx context.Context, fn func() error) error {
	var err error
	for i := 0; i < r.attempts; i++ {
		if i > 0 {
			wait := time.NewTimer(r.backoff * time.Duration(1<<uint(i-1)))
			select {
			case <-ctx.Done():
				wait.Stop()
				return ctx.Err()
			case <-wait.C:
			}
		}
		if err = fn(); err == nil {
			return nil
		}
		var perm *statusError
		if errors.As(err, &perm) && perm.code < 500 && perm.code != http.StatusTooManyRequests {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return err
}

// get returns the body plus the URL that finally answered, i.e. the node's canonical form.
func (c *Client) get(ctx context.Context, rawURL string) ([]byte, string, error) {
	var (
		body  []byte
		final string
	)
	err := c.retrier.do(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return fmt.Errorf("aredn: build request for %s: %w", rawURL, err)
		}
		req.Header.Set("User-Agent", c.cfg.UserAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		res, err := c.http.Do(req)
		if err != nil {
			c.log.Debug("aredn fetch failed", zap.String("url", rawURL), zap.Error(err))
			return fmt.Errorf("aredn: get %s: %w", rawURL, err)
		}
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
			_ = res.Body.Close()
		}()
		if res.StatusCode != http.StatusOK {
			return &statusError{code: res.StatusCode, url: rawURL}
		}
		buf, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
		if err != nil {
			return fmt.Errorf("aredn: read %s: %w", rawURL, err)
		}
		body, final = buf, res.Request.URL.String()
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return body, final, nil
}

// NodeURL lowercases the hostname and builds http://<host>.local.mesh.
// Mesh names are case-insensitive and uppercase forms intermittently fail to resolve.
func NodeURL(hostname string) string {
	host := strings.ToLower(strings.TrimSpace(hostname))
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimPrefix(host, "https://")
	host = strings.Trim(host, "/")
	if host == "" {
		return ""
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if ip := net.ParseIP(host); ip != nil {
		return "http://" + host
	}
	if !strings.HasSuffix(host, "."+MeshDomain) {
		host += "." + MeshDomain
	}
	return "http://" + host
}

// HostnameFromURL extracts the peer label from a link href; an IP form is returned unchanged.
// A relative href such as "neighbor-device?m=..." carries no host and yields "".
func HostnameFromURL(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "//") {
		// Relative href (hx-get="neighbor-device?m=..."): carries no host.
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Fall back to naive slicing for host-only strings.
		host := strings.TrimPrefix(strings.TrimPrefix(raw, "http://"), "https://")
		host = strings.Trim(host, "/")
		if i := strings.IndexAny(host, "/?#:"); i >= 0 {
			host = host[:i]
		}
		return strings.ToLower(host)
	}
	return strings.ToLower(u.Hostname())
}
