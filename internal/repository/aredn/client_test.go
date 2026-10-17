package aredn

import (
	"context"
	"strings"
	"testing"
)

func TestNodeURL(t *testing.T) {
	cases := map[string]string{
		"tst16":                        "http://tst16.local.mesh",
		"TST16":                        "http://tst16.local.mesh",
		"tst06.local.mesh":             "http://tst06.local.mesh",
		"http://TST09-PTP.local.mesh/": "http://tst09-ptp.local.mesh",
		"172.31.159.46":                "http://172.31.159.46",
		"  TST12-HAP ":                 "http://tst12-hap.local.mesh",
		"":                             "",
	}
	for in, want := range cases {
		if got := NodeURL(in); got != want {
			t.Errorf("NodeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostnameFromURL(t *testing.T) {
	cases := map[string]string{
		"http://tst09-ptp.local.mesh":         "tst09-ptp.local.mesh",
		"http://TST09-PTP.local.mesh/":        "tst09-ptp.local.mesh",
		"http://172.31.159.46/":               "172.31.159.46",
		"http://10.100.5.115/":                "10.100.5.115",
		"http://NASE038A6:80/":                "nase038a6",
		"neighbor-device?m=02:00:00:00:00:03": "",
		"/a/status":                           "",
		"":                                    "",
		"not a url at all":                    "",
	}
	for in, want := range cases {
		if got := HostnameFromURL(in); got != want {
			t.Errorf("HostnameFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrimMeshDomain(t *testing.T) {
	for in, want := range map[string]string{
		"TST09-PTP.local.mesh": "tst09-ptp",
		"tst16":                "tst16",
		"172.31.159.46":        "172.31.159.46",
		"mesh.local.mesh":      "mesh",
		"":                     "",
	} {
		if got := trimMeshDomain(in); got != want {
			t.Errorf("trimMeshDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

// Zero-value Config must fall back to the defaults the scan engine relies on.
func TestConfigDefaults(t *testing.T) {
	c := New(Config{}, nil)
	if c.cfg.Timeout != defaultTimeout || c.cfg.ConnectTO != defaultConnectTO {
		t.Errorf("timeouts = %v/%v", c.cfg.Timeout, c.cfg.ConnectTO)
	}
	if c.cfg.MaxRetries != defaultMaxRetries || c.cfg.RetryBackoff != defaultRetryBackoff {
		t.Errorf("retries = %d/%v", c.cfg.MaxRetries, c.cfg.RetryBackoff)
	}
	if c.cfg.UserAgent != defaultUserAgent {
		t.Errorf("UserAgent = %q", c.cfg.UserAgent)
	}
	if c.log == nil {
		t.Error("a nil logger must fall back to zap.NewNop()")
	}
}

// The crawler follows a node's 307 and reads the page of the canonical hostname it lands on.
func TestFetchSelfFollowsRedirect(t *testing.T) {
	c, rec := newFixtureServer(t, homeNodeFixture())

	n, err := c.FetchSelf(t.Context(), "localnode")
	if err != nil {
		t.Fatalf("FetchSelf(localnode): %v", err)
	}
	if n.MeshIP != "10.100.4.200" {
		t.Errorf("MeshIP = %q, want the canonical node's address", n.MeshIP)
	}
	if n.Hostname != "localnode" {
		t.Errorf("Hostname = %q, want the crawled label so Link.From matches", n.Hostname)
	}
	if n.Name != "TST06" {
		t.Errorf("Name = %q, want the title of the redirected page", n.Name)
	}
	if got := rec.count("localnode.local.mesh" + statusPath); got != 1 {
		t.Errorf("localnode answered %d times, want 1 redirect", got)
	}
	if got := rec.count("tst06.local.mesh" + statusPath); got != 1 {
		t.Errorf("canonical node served %d times, want 1", got)
	}
}

// /a/status/e/* answers 401 and stays that way, so it must not be hammered.
func TestNoRetryOn401(t *testing.T) {
	c, rec := newFixtureServer(t, fixture{})
	_, err := c.FetchSelf(t.Context(), "tst16")
	if err == nil {
		t.Fatal("401 must be an error")
	}
	if got := rec.count("tst16.local.mesh" + statusPath); got != 1 {
		t.Errorf("401 attempted %d times, want 1 (no retry)", got)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention the status: %v", err)
	}
}

// Transient transport failures are retried up to MaxRetries and the page still parses.
func TestRetryOn503(t *testing.T) {
	key := "flaky.local.mesh" + statusPath
	c, rec := newFixtureServer(t, fixture{
		routes:  map[string]string{key: "status_bare.html"},
		failing: map[string]int{key: 2},
	})
	n, err := c.FetchSelf(t.Context(), "flaky")
	if err != nil {
		t.Fatalf("FetchSelf: %v", err)
	}
	if n.Name != "BARE-TEST" {
		t.Errorf("Name = %q", n.Name)
	}
	if got := rec.count(key); got != 3 {
		t.Errorf("%d attempts, want 3 (2 retries)", got)
	}
}

// The give-up path once the retry budget is exhausted.
func TestRetryBudgetExhausted(t *testing.T) {
	key := "broken.local.mesh" + statusPath
	c, rec := newFixtureServer(t, fixture{
		routes:        map[string]string{key: "status_bare.html"},
		alwaysFailing: map[string]bool{key: true},
	})
	if _, err := c.FetchSelf(t.Context(), "broken"); err == nil {
		t.Fatal("an endpoint that never recovers must error")
	}
	if got := rec.count(key); got != 3 {
		t.Errorf("%d attempts, want 3", got)
	}
}

// A closed ctx must stop the crawl immediately.
func TestContextCancellation(t *testing.T) {
	c, _ := newFixtureServer(t, downFixture())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.FetchSelf(ctx, "tst16"); err == nil {
		t.Fatal("a cancelled context must fail the fetch")
	}
}
