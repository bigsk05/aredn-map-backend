package ping

import (
	"context"
	"time"

	probing "github.com/prometheus-community/pro-bing"
)

// bounded runs a session under a wall-clock ceiling: past it, stop is called and run has grace to
// hand back statistics; after that window fallback is consulted directly.
func bounded(
	ctx context.Context,
	ceiling, grace time.Duration,
	run func(context.Context) (*probing.Statistics, error),
	stop func(),
	fallback func() *probing.Statistics,
) (*probing.Statistics, error) {
	if ceiling <= 0 {
		ceiling = defaultOverallTimeout
	}
	if grace < 0 {
		grace = 0
	}

	sctx, cancel := context.WithTimeout(ctx, ceiling)
	defer cancel()

	type outcome struct {
		stats *probing.Statistics
		err   error
	}
	done := make(chan outcome, 1) // buffered: an abandoned worker never leaks
	go func() {
		stats, err := run(sctx)
		done <- outcome{stats: stats, err: err}
	}()

	alarm := time.NewTimer(ceiling)
	defer alarm.Stop()

	select {
	case o := <-done:
		return collect(o.stats, o.err, fallback)
	case <-alarm.C:
		// pro-bing outstayed the ceiling: stop it and take what it saw.
	case <-ctx.Done():
		// The sweep was cancelled or a SIGTERM landed.
	}

	stop()

	drain := time.NewTimer(grace)
	defer drain.Stop()
	select {
	case o := <-done:
		if o.err != nil {
			return collect(o.stats, o.err, fallback)
		}
		// The ceiling already fired, so report the cut even on a clean-looking worker.
		return collect(o.stats, ceilingErr(ctx), fallback)
	case <-drain.C:
		// The graceful window lost the worker: use the fallback counters and name the ceiling.
		return collect(nil, ceilingErr(ctx), fallback)
	}
}

func collect(stats *probing.Statistics, err error, fallback func() *probing.Statistics) (*probing.Statistics, error) {
	if stats == nil && fallback != nil {
		stats = fallback()
	}
	return stats, err
}
