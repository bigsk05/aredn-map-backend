package aredn

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// The offline suite answers *.local.mesh from an httptest server holding recorded pages.
// Nothing touches the network, so these tests are hermetic; live checks need the "live" tag.

// fixture names files inside testdata/.
type fixture struct {
	routes    map[string]string // "<host><path>[?<query>]" -> testdata file
	redirects map[string]string // "<host>" -> canonical "<host>"

	// failing counts the 503 answers a key must produce before it succeeds.
	failing map[string]int
	// alwaysFailing keys answer 504 forever.
	alwaysFailing map[string]bool
}

// recorder counts how often each path was served, for the retry and concurrency budgets.
type recorder struct {
	mu     sync.Mutex
	counts map[string]int
	in     int32 // in-flight requests
	maxIn  int32 // observed maximum
}

func (r *recorder) enter() {
	if n := atomic.AddInt32(&r.in, 1); n > atomic.LoadInt32(&r.maxIn) {
		atomic.StoreInt32(&r.maxIn, n)
	}
}

func (r *recorder) leave() { atomic.AddInt32(&r.in, -1) }

func (r *recorder) hit(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = map[string]int{}
	}
	r.counts[key]++
}

func (r *recorder) count(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[key]
}

func (r *recorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, v := range r.counts {
		n += v
	}
	return n
}

// newFixtureServer serves recorded pages; unknown paths answer 401 like /a/status/e/* does.
func newFixtureServer(t *testing.T, f fixture) (*Client, *recorder) {
	t.Helper()

	rec := &recorder{}
	var srv *httptest.Server

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.enter()
		defer rec.leave()

		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		key := host + r.URL.Path
		// The dialog and JSON face are query-addressed, so their route keys carry the query.
		if (r.URL.Path == devicePath || r.URL.Path == sysinfoPath) && r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		rec.hit(key)

		if f.alwaysFailing[key] {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		}
		if n, ok := f.failing[key]; ok && n > 0 {
			f.failing[key] = n - 1
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		if target, ok := f.redirects[host]; ok {
			// Real nodes answer 307 to their canonical hostname, query kept.
			target := NodeURL(target) + r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
			return
		}
		file, ok := f.routes[key]
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		body, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatalf("read fixture %s: %v", file, err)
		}
		if strings.HasSuffix(file, ".json") {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		_, _ = w.Write(body)
	})

	srv = httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("server address: %v", err)
	}

	c := New(Config{
		Timeout:      5 * time.Second,
		ConnectTO:    time.Second,
		MaxRetries:   2,
		RetryBackoff: time.Millisecond,
		UserAgent:    "aredn-map-test",
	}, zap.NewNop())

	// Every hostname resolves to this server, keeping the crawler's URL building under test.
	c.http.Transport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, "127.0.0.1:"+port)
		},
	}
	return c, rec
}

// downFixture serves the tst16 status, adjacency fragment and one RF dialog.
func downFixture() fixture {
	return fixture{routes: map[string]string{
		"tst16.local.mesh" + statusPath:                          "status_tst16.html",
		"tst16.local.mesh" + neighborsPath:                       "neighbors_tst16.html",
		"tst16.local.mesh" + devicePath + "?m=02:00:00:00:00:03": "device_rf_tst09-ptp.html",
	}}
}

// homeNodeFixture serves TST06, directly and via the localnode redirect.
func homeNodeFixture() fixture {
	return fixture{
		routes: map[string]string{
			"tst06.local.mesh" + statusPath:                          "status_tst06.html",
			"tst06.local.mesh" + neighborsPath:                       "neighbors_localnode.html",
			"tst06.local.mesh" + sysinfoPath + "?nodes=1&services=1": "sysinfo_mesh_localnode.json",
			"tst06.local.mesh" + devicePath + "?m=02:00:00:00:00:04": "device_wireguard_tst05-tunnelserver.html",
		},
		redirects: map[string]string{"localnode.local.mesh": "TST06.local.mesh"},
	}
}

// meshLocalnodeFixture is the home access point reached through the "localnode" redirect.
func meshLocalnodeFixture() fixture {
	return homeNodeFixture()
}

// fixtureNow is a stable reference instant for the "last seen" prose tests.
func fixtureNow() time.Time {
	return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
}

// fixtureString loads a recorded page for the parser-level tests.
func fixtureString(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(body)
}

// almostEqual compares the float cells the dialogs print with one decimal.
func almostEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}
