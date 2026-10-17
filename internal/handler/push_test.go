package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"

	"aredn-map/internal/controller"
	"aredn-map/internal/model"
	"aredn-map/internal/repository/ping"
	"aredn-map/internal/repository/store"
	"aredn-map/internal/service/scanner"
)

// Push contract: one frame per connection, only when it fell behind, carrying only new records.

type pushHarness struct {
	app   *fiber.App
	store *store.Store
}

// newPushApp wires a real app over the seeded contract graph.
func newPushApp(t *testing.T) pushHarness {
	t.Helper()
	cfg := testConfig()
	st := store.New()
	st.SetSeeds(cfg.Scan.Seeds)
	seedContractStore(st)
	eng := scanner.New(cfg, st, newFakeFetcher(map[string]scriptHost{}),
		&fakePinger{mode: cfg.ICMP.Mode, results: map[string]ping.Result{}}, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	app := New(controller.Deps{
		Ctx:     ctx,
		Cfg:     cfg,
		Store:   st,
		Engine:  eng,
		Log:     zap.NewNop(),
		Started: time.Now(),
		NextRun: func() time.Time { return fixedNextSweepTime() },
	})
	return pushHarness{app: app, store: st}
}

// serve uses Port 0 plus ListenerAddrFunc, fiber v3's only way to learn the bound address.
func (h pushHarness) serve(t *testing.T) string {
	t.Helper()
	addrs := make(chan string, 1)
	go func() {
		if err := h.app.Listen("127.0.0.1:0", fiber.ListenConfig{
			DisableStartupMessage: true,
			ListenerAddrFunc:      func(a net.Addr) { addrs <- a.String() },
		}); err != nil {
			t.Logf("test listener stopped: %v", err)
		}
	}()
	t.Cleanup(func() { _ = h.app.Shutdown() })

	select {
	case addr := <-addrs:
		return "ws://" + addr + "/api/ws"
	case <-time.After(5 * time.Second):
		t.Fatal("test listener never reported an address")
		return ""
	}
}

func dialWS(t *testing.T, rawURL string) *websocket.Conn {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("bad ws url %q: %v", rawURL, err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", u.Host, err)
	}
	ws, resp, err := websocket.NewClient(conn, u, http.Header{}, 1024, 1024)
	if err != nil {
		_ = conn.Close()
		if resp != nil {
			t.Fatalf("websocket handshake: %v (status %d)", err, resp.StatusCode)
		}
		t.Fatalf("websocket handshake: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func readFrame(t *testing.T, ws *websocket.Conn, within time.Duration) pushFrame {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(within))
	mt, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read pushed frame: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("frame type = %d, want a text frame", mt)
	}
	var out pushFrame
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("pushed frame is not a pushFrame: %v\n%s", err, data)
	}
	// Status rides along so counts and rows describe the same instant.
	if out.Status == nil {
		t.Fatalf("frame has no status block: %s", data)
	}
	return out
}

func hostnames(nodes []model.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Hostname
	}
	return out
}

// expectQuiet asserts no frame is sent while the store has not changed.
func expectQuiet(t *testing.T, ws *websocket.Conn, within time.Duration) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(within))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatalf("got a frame within %v while nothing had changed", within)
	}
}

func TestStatusPushPrimesThenFollowsChanges(t *testing.T) {
	h := newPushApp(t)
	ws := dialWS(t, h.serve(t))

	// A fresh connection gets a full-read instruction without waiting for a mutation.
	first := readFrame(t, ws, 5*time.Second)
	if !first.Full {
		t.Errorf("first frame to a new connection merged %d rows, want a re-read", len(first.Nodes))
	}
	if first.Status.Counts.Total == 0 {
		t.Fatal("priming frame reported no nodes; the seeded graph did not reach the socket")
	}
	if first.Rev == 0 {
		t.Errorf("priming frame carried rev 0; the connection has no usable cursor")
	}

	h.store.UpsertNode(&model.Node{Hostname: "push-probe-node", Name: "push-probe-node"})
	second := readFrame(t, ws, 5*time.Second)
	if second.Status.Counts.Total <= first.Status.Counts.Total {
		t.Errorf("total stayed at %d after a node was written, want an increase", second.Status.Counts.Total)
	}
	// One row moved, so one row goes out - not the graph the client already has.
	if len(second.Nodes) != 1 || second.Nodes[0].Hostname != "push-probe-node" {
		t.Errorf("delta nodes = %v, want exactly [push-probe-node]", hostnames(second.Nodes))
	}
	if second.Full {
		t.Error("a one-row change asked the client to re-read everything")
	}
	if second.Rev <= first.Rev {
		t.Errorf("revision went %d -> %d across a write", first.Rev, second.Rev)
	}

	expectQuiet(t, ws, 3*time.Second)
}

func TestStatusPushCarriesLinkWrites(t *testing.T) {
	h := newPushApp(t)
	ws := dialWS(t, h.serve(t))
	_ = readFrame(t, ws, 5*time.Second)

	h.store.UpsertLink(model.Link{From: "localnode", To: "push-probe-node", Type: model.LinkTypeRF, MAC: "aa:bb"})
	got := readFrame(t, ws, 5*time.Second)
	if len(got.Links) != 1 {
		t.Fatalf("delta links = %d, want the one edge that was written", len(got.Links))
	}
	if got.Links[0].From != "localnode" || got.Links[0].To != "push-probe-node" {
		t.Errorf("edge = %s -> %s, want localnode -> push-probe-node", got.Links[0].From, got.Links[0].To)
	}
	if len(got.Nodes) != 0 {
		t.Errorf("an edge write pushed %d nodes; only the edge moved", len(got.Nodes))
	}
}

// A node that leaves the store must leave the page; absent rows cannot express that.
func TestStatusPushReportsRemovals(t *testing.T) {
	h := newPushApp(t)
	ws := dialWS(t, h.serve(t))
	_ = readFrame(t, ws, 5*time.Second)

	h.store.DeleteNode("localnode")
	got := readFrame(t, ws, 5*time.Second)
	if len(got.Removed) != 1 || got.Removed[0] != "localnode" {
		t.Fatalf("removed = %v, want [localnode]", got.Removed)
	}
}

// store.Reset() is a new picture, so the frame sends the generation change instead of rows.
func TestStatusPushAsksForFullReadAfterReset(t *testing.T) {
	h := newPushApp(t)
	ws := dialWS(t, h.serve(t))
	first := readFrame(t, ws, 5*time.Second)

	h.store.Reset()
	got := readFrame(t, ws, 5*time.Second)
	if !got.Full {
		t.Errorf("frame after a reset is a merge (full=false, %d nodes), want a re-read", len(got.Nodes))
	}
	if got.Gen == first.Gen {
		t.Errorf("generation stayed %d across a reset", got.Gen)
	}
}

func TestStatusPushFansOutToEveryClient(t *testing.T) {
	h := newPushApp(t)
	wsURL := h.serve(t)
	a, b := dialWS(t, wsURL), dialWS(t, wsURL)
	_ = readFrame(t, a, 5*time.Second)
	_ = readFrame(t, b, 5*time.Second)

	h.store.MarkProbe("push-probe-node", store.ProbeOutcome{Online: true, LatencyMs: 12, Sent: 1, Recv: 1})
	ra, rb := readFrame(t, a, 5*time.Second), readFrame(t, b, 5*time.Second)
	if ra.Status.Counts.Online != rb.Status.Counts.Online {
		t.Errorf("the two connections disagree: online %d vs %d", ra.Status.Counts.Online, rb.Status.Counts.Online)
	}
	// Each connection merges from its own cursor, both primed from the same picture.
	if len(ra.Nodes) != 1 || len(rb.Nodes) != 1 {
		t.Errorf("deltas were %v and %v, want the probed node in both", hostnames(ra.Nodes), hostnames(rb.Nodes))
	}
}

// A status transition changes no rows, so the engine must bump the store counter.
func TestStatusPushFollowsEngineBumps(t *testing.T) {
	h := newPushApp(t)
	ws := dialWS(t, h.serve(t))
	_ = readFrame(t, ws, 5*time.Second)

	// Silence is asserted on the other connection; a read that expires kills this one.
	h.store.BumpRev()
	got := readFrame(t, ws, 5*time.Second)
	if got.Status.Version == "" {
		t.Error("a bumped revision produced no usable frame")
	}
	if len(got.Nodes) != 0 || len(got.Links) != 0 {
		t.Errorf("a bump with no writes pushed %d nodes and %d links", len(got.Nodes), len(got.Links))
	}
}

// countedConn totals the bytes that crossed it, weighing what the socket carried.
type countedConn struct {
	net.Conn
	rx *int64
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	*c.rx += int64(n)
	return n, err
}

// A delta that costs more than the full read it replaced is not worth sending.
// Frames go out deflated (RFC 7692) because JSON repeats its field names per row.
func TestPushNegotiatesPermessageDeflate(t *testing.T) {
	h := newPushApp(t)
	wsURL := h.serve(t)

	var rx int64
	d := &websocket.Dialer{
		EnableCompression: true,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &countedConn{Conn: c, rx: &rx}, nil
		},
	}
	conn, resp, err := d.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial with compression: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if resp == nil || !strings.Contains(resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
		t.Fatalf("handshake did not negotiate compression: %+v", resp)
	}

	_ = readFrame(t, conn, 5*time.Second)

	// The pacer splits this burst into two frames of full node rows.
	for i := range 500 {
		id := fmt.Sprintf("burst-%03d", i)
		h.store.UpsertNode(&model.Node{
			Hostname:    id,
			Name:        id,
			MeshIP:      "10.255.0.1",
			Description: "a repeated description across every row of the frame",
			Services:    []string{"ssh|10.255.0.1", "http|10.255.0.1", "ntp|10.255.0.1"},
			Neighbors:   []string{"localnode", "wg-remote", "supernode-hq"},
		})
	}

	before := rx
	var payload int64
	for payload < 120_000 {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read a burst frame: %v", err)
		}
		payload += int64(len(data))
	}
	wire := rx - before
	if wire == 0 {
		t.Fatal("nothing crossed the socket")
	}
	t.Logf("the socket carried %d bytes for %d bytes of frames", wire, payload)
	if wire*2 >= payload {
		t.Errorf("the socket carried %d bytes for %d bytes of frames: compression is not taking effect", wire, payload)
	}
}
