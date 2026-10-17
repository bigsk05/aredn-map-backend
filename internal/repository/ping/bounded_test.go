package ping

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	probing "github.com/prometheus-community/pro-bing"
)

// The ceiling is the part of the contract a sweep depends on, so it is tested
// with a scripted worker rather than a real pro-bing session.

func TestBoundedReturnsWhenTheWorkerFinishes(t *testing.T) {
	want := &probing.Statistics{PacketsSent: 3, PacketsRecv: 3}
	var stopped atomic.Bool

	start := time.Now()
	stats, err := bounded(context.Background(), time.Minute, drainGrace,
		func(context.Context) (*probing.Statistics, error) { return want, nil },
		stoppedTrue(&stopped),
		func() *probing.Statistics { t.Fatal("fallback must not be used"); return nil })
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if stats != want {
		t.Fatalf("stats = %v, want the worker's", stats)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v, want an immediate return", elapsed)
	}
	if stopped.Load() {
		t.Fatal("stop must not be called on the happy path")
	}
}

func TestBoundedNeverWaitsPastTheCeiling(t *testing.T) {
	const ceiling = 40 * time.Millisecond
	const grace = 15 * time.Millisecond

	// A worker that ignores its context and only ever comes back long after the
	// ceiling plus grace: this is the "pro-bing blocked" case.
	var stopped, ranFallback atomic.Bool
	release := make(chan struct{})
	defer close(release)

	start := time.Now()
	stats, err := bounded(context.Background(), ceiling, grace,
		func(context.Context) (*probing.Statistics, error) {
			<-release // never reaches the drain window
			return &probing.Statistics{PacketsSent: 9}, nil
		},
		stoppedTrue(&stopped),
		func() *probing.Statistics {
			ranFallback.Store(true)
			return &probing.Statistics{PacketsSent: 2, PacketsRecv: 1}
		})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrSessionTimeout) {
		t.Fatalf("err = %v, want the session ceiling to surface as the sentinel", err)
	}
	if !stopped.Load() {
		t.Fatal("the runaway worker must be told to stop")
	}
	if !ranFallback.Load() {
		t.Fatal("fallback statistics must have been taken")
	}
	if stats == nil || stats.PacketsSent != 2 || stats.PacketsRecv != 1 {
		t.Fatalf("stats = %+v, want the fallback counters", stats)
	}
	if elapsed < ceiling {
		t.Fatalf("returned after %v, before the %v ceiling", elapsed, ceiling)
	}
	if elapsed > ceiling+grace+150*time.Millisecond {
		t.Fatalf("returned after %v, ceiling+grace is %v", elapsed, ceiling+grace)
	}

	// The sentinel must survive the error normalisation the scanner sees.
	if got := (&Pinger{}).normalizeRunErr(err, context.Background()); !errors.Is(got, ErrSessionTimeout) {
		t.Fatalf("normalizeRunErr = %v, want ErrSessionTimeout", got)
	}
}

func TestBoundedKeepsTheWorkersStatisticsWhenStopUnblocksIt(t *testing.T) {
	const ceiling = 30 * time.Millisecond

	var stopped atomic.Bool
	released := make(chan struct{})
	stats, err := bounded(context.Background(), ceiling, drainGrace,
		func(context.Context) (*probing.Statistics, error) {
			<-released
			return &probing.Statistics{PacketsSent: 3, PacketsRecv: 2}, nil
		},
		func() {
			stopped.Store(true)
			close(released)
		},
		func() *probing.Statistics { return &probing.Statistics{PacketsSent: 99} })

	if !stopped.Load() {
		t.Fatal("stop must be called once the ceiling fires")
	}
	if stats == nil || stats.PacketsRecv != 2 {
		t.Fatalf("stats = %+v, want the partial session the worker reported", stats)
	}
	// The worker thinks it was fine; the ceiling still has to be reported.
	if !errors.Is(err, ErrSessionTimeout) {
		t.Fatalf("err = %v, want the ceiling error preserved", err)
	}
}

func TestBoundedHonoursCallerCancellation(t *testing.T) {
	var stopped atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	stats, err := bounded(ctx, time.Minute, drainGrace,
		func(sctx context.Context) (*probing.Statistics, error) {
			<-sctx.Done()
			return &probing.Statistics{PacketsSent: 1}, sctx.Err()
		},
		stoppedTrue(&stopped),
		func() *probing.Statistics { return &probing.Statistics{} })
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("cancellation took %v, want it to win immediately", elapsed)
	}
	if stats == nil || stats.PacketsSent != 1 {
		t.Fatalf("stats = %+v, want what the worker had collected", stats)
	}
	if !stopped.Load() {
		t.Fatal("stop must be called on cancellation as well")
	}

	if got := (&Pinger{}).normalizeRunErr(err, ctx); !errors.Is(got, context.Canceled) {
		t.Fatalf("normalizeRunErr must keep the caller's reason, got %v", got)
	}
}

func TestNormalizeRunErrPrecedence(t *testing.T) {
	p := newTestPinger(t, Config{}, okSocket, failingLookup)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A live caller cancellation outranks whatever pro-bing reported.
	cancel()
	if got := p.normalizeRunErr(errors.New("listen ip4:icmp: socket: operation not permitted"), ctx); !errors.Is(got, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", got)
	}

	// A bare permission error is passed through untouched: the operator hint
	// belongs to the log, not to the string the scanner stores per node.
	live := t.Context()
	if got := p.normalizeRunErr(errDenied, live); !errors.Is(got, errDenied) {
		t.Fatalf("got %v, want the raw error preserved", got)
	}

	// Ceiling without a parent deadline becomes the sentinel.
	if got := p.normalizeRunErr(context.DeadlineExceeded, live); !errors.Is(got, ErrSessionTimeout) {
		t.Fatalf("got %v, want ErrSessionTimeout", got)
	}
	if got := p.normalizeRunErr(nil, live); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func stoppedTrue(flag *atomic.Bool) func() {
	return func() { flag.Store(true) }
}
