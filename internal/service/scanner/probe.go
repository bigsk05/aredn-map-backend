package scanner

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"aredn-map/internal/repository/ping"
	"aredn-map/internal/repository/store"
)

type probePool struct {
	e      *Engine
	ch     chan string
	wg     sync.WaitGroup
	seen   sync.Map
	queued atomic.Int64
}

func (e *Engine) newProbePool(ctx context.Context) *probePool {
	n := max1(e.cfg.ICMP.Concurrency)
	p := &probePool{
		e:  e,
		ch: make(chan string, n*4),
	}
	for range n {
		p.wg.Go(func() {
			for host := range p.ch {
				p.e.probeOne(ctx, host)
			}
		})
	}
	return p
}

// enqueue never blocks; a dropped host is picked up by the sweep-up probe pass.
func (p *probePool) enqueue(host string) {
	if host == "" {
		return
	}
	if _, dup := p.seen.LoadOrStore(host, true); dup {
		return
	}
	select {
	case p.ch <- host:
		p.queued.Add(1)
	default:
		p.seen.Delete(host)
	}
}

func (p *probePool) close() {
	close(p.ch)
	p.wg.Wait()
}

// recordProbe stores an ICMP verdict and parks the node in the revisit lane if nothing answered.
func (e *Engine) recordProbe(host string, r ping.Result) {
	online := r.Alive()
	e.st.MarkProbe(host, store.ProbeOutcome{
		Online:     online,
		LatencyMs:  r.AvgMs,
		PacketLoss: r.LossPct(),
		Sent:       r.Sent,
		Recv:       r.Recv,
		TTL:        r.TTL,
		JitterMs:   r.MdevMs,
		IP:         r.IP,
		ProbedAt:   time.Now(),
		Err:        errText(r.Err),
	})
	if !online {
		e.parkUnanswered(host)
	}
}

func (e *Engine) probeOne(ctx context.Context, host string) {
	e.recordProbe(host, e.pg.Ping(ctx, host))
}

// parkAfterFailure records why a fetch failed plus the revisit time, even with the lane off.
