// WebSocket push: one frame per change, carrying status plus rows written since that cursor.
package handler

import (
	"context"
	"sync"
	"time"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"
	gojson "go.gh.ink/json"
	"go.uber.org/zap"

	"aredn-map/internal/controller"
	"aredn-map/internal/controller/v1/system"
	"aredn-map/internal/model"
)

const (
	// wsTick: how often the push loop compares each connection's cursor with the store.
	wsTick = time.Second
	// wsPing: keepalive interval; the browser's automatic pong ends a half-open connection.
	wsPing = 20 * time.Second
	// wsWrite: one frame's deadline; a client slower than this is gone.
	wsWrite = 5 * time.Second
	// wsReadIdle: allowed silence before a drop; spans several ping intervals.
	wsReadIdle = 3 * wsPing
)

// wsClient owns one upgraded connection; mu serialises its concurrent frame writes.
type wsClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
	// rev/gen is this connection's cursor into the store; the push loop is its only writer.
	rev      uint64
	gen      uint64
	primed   bool
	lastPing time.Time
}

func (cl *wsClient) write(msgType int, data []byte) error {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	_ = cl.conn.SetWriteDeadline(time.Now().Add(wsWrite))
	return cl.conn.WriteMessage(msgType, data)
}

// pushFrame is one delivery: the status block plus the rows written since this cursor.
type pushFrame struct {
	Rev uint64 `json:"rev"`
	Gen uint64 `json:"gen"`
	// Full means the cursor was useless; the client re-reads the inventories over HTTP.
	Full   bool                  `json:"full,omitempty"`
	Status *model.StatusResponse `json:"status,omitempty"`

	Nodes   []model.Node `json:"nodes,omitempty"`
	Links   []model.Link `json:"links,omitempty"`
	Removed []string     `json:"removed,omitempty"`
}

// behind is true when the store wrote past this cursor; an unprimed client is always behind.
func (cl *wsClient) behind(rev, gen uint64) bool {
	return !cl.primed || cl.rev != rev || cl.gen != gen
}

type wsHub struct {
	mu   sync.Mutex
	live map[*wsClient]struct{}
}

func newWSHub() *wsHub {
	return &wsHub{live: make(map[*wsClient]struct{})}
}

func (h *wsHub) add(cl *wsClient) {
	cl.lastPing = time.Now()
	h.mu.Lock()
	h.live[cl] = struct{}{}
	h.mu.Unlock()
}

func (h *wsHub) remove(cl *wsClient) {
	h.mu.Lock()
	delete(h.live, cl)
	h.mu.Unlock()
}

func (h *wsHub) snapshot() []*wsClient {
	h.mu.Lock()
	out := make([]*wsClient, 0, len(h.live))
	for cl := range h.live {
		out = append(out, cl)
	}
	h.mu.Unlock()
	return out
}

// drop closes a failed client; its read loop unregisters itself, so remove() happens once.
func (h *wsHub) drop(cl *wsClient, why error, log *zap.Logger) {
	log.Debug("dropping websocket client", zap.Error(why))
	_ = cl.conn.Close()
}

// run delivers to every connection behind the store until ctx, the process shutdown signal.
func (h *wsHub) run(ctx context.Context, d controller.Deps) {
	tick := time.NewTicker(wsTick)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			h.closeAll()
			return
		case <-tick.C:
		}

		clients := h.snapshot()
		if len(clients) == 0 {
			continue
		}

		rev, gen := d.Store.Rev(), d.Store.Generation()
		// The status block is built once per pass and shared by pointer; marshalling is read-only.
		var status *model.StatusResponse
		for _, cl := range clients {
			if !cl.behind(rev, gen) {
				continue
			}
			if status == nil {
				s := system.Payload(d)
				status = &s
			}
			var frame pushFrame
			if !cl.primed {
				frame = pushFrame{Rev: rev, Gen: gen, Full: true, Status: status}
			} else {
				delta := d.Store.Delta(cl.rev, cl.gen)
				frame = pushFrame{Rev: delta.Rev, Gen: delta.Gen, Full: delta.Full, Status: status,
					Nodes: delta.Nodes, Links: delta.Links, Removed: delta.Removed}
			}
			blob, err := gojson.Marshal(frame)
			if err != nil {
				d.Log.Warn("push frame marshal failed", zap.Error(err))
				continue
			}
			if err := cl.write(websocket.TextMessage, blob); err != nil {
				h.drop(cl, err, d.Log)
				continue
			}
			// The cursor moves only after the frame is on the wire, so a missed frame re-sends.
			cl.primed = true
			cl.rev, cl.gen = frame.Rev, frame.Gen
		}

		// Only a pong refreshes the read deadline, so a busy sweep must still ping.
		now := time.Now()
		for _, cl := range clients {
			if now.Sub(cl.lastPing) < wsPing {
				continue
			}
			cl.lastPing = now
			if err := cl.write(websocket.PingMessage, nil); err != nil {
				h.drop(cl, err, d.Log)
			}
		}
	}
}

func (h *wsHub) closeAll() {
	for _, cl := range h.snapshot() {
		_ = cl.conn.Close()
	}
}

// handleWS upgrades the request; the frame is one-way, so the read loop only learns about closes.
func handleWS(d controller.Deps, h *wsHub) fiber.Handler {
	return websocket.New(func(c *websocket.Conn) {
		cl := &wsClient{conn: c}
		h.add(cl)
		defer func() {
			h.remove(cl)
			_ = c.Close()
		}()

		// The push loop owns the first frame; writing it here would race the same cursor.

		c.SetReadLimit(1 << 10)
		c.SetPongHandler(func(string) error {
			return c.SetReadDeadline(time.Now().Add(wsReadIdle))
		})
		for {
			_ = c.SetReadDeadline(time.Now().Add(wsReadIdle))
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}, websocket.Config{
		// Empty Origins matches the bundle's own host whether reached by name, IP or port-forward.
		HandshakeTimeout: 10 * time.Second,
		// permessage-deflate is negotiated per connection and keeps a live delta small.
		EnableCompression: true,
	})
}
