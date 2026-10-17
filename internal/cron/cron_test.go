package cron

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"
	"go.uber.org/zap"
)

// repoDefaultCron mirrors the 6-field default shipped in config.yaml.
const repoDefaultCron = "0 */30 * * * *"

// newForTest registers Stop as cleanup so a mid-test failure leaves no gocron goroutines.
func newForTest(t *testing.T, expr, zone string, fn func()) *Scheduler {
	t.Helper()
	s, err := New(expr, zone, fn, zap.NewNop())
	if err != nil {
		t.Fatalf("New(%q, %q) = %v", expr, zone, err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

func TestNewAcceptsEveryCronFormTheConfigAllows(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{"5 field crontab", "*/5 * * * *"},
		{"5 field time of day", "0 3 * * *"},
		{"6 field with seconds, the repo default", repoDefaultCron},
		{"6 field every second", "* * * * * *"},
		{"descriptor", "@every 30m"},
		{"descriptor with sub second precision", "@every 100ms"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newForTest(t, tc.expr, "Asia/Shanghai", func() {})

			// The job name is the string operators see in gocron logs.
			if got := s.job.Name(); got != "aredn-sweep" {
				t.Fatalf("job name = %q, want aredn-sweep", got)
			}
			if got := s.expr; got != tc.expr {
				t.Fatalf("expr = %q, want it kept verbatim as %q", got, tc.expr)
			}
			if sched, ok := s.job.Schedule().(gocron.CronJobSchedule); !ok {
				t.Fatalf("schedule = %T, want a gocron cron schedule", s.job.Schedule())
			} else if sched.Crontab != tc.expr {
				t.Fatalf("crontab = %q, want %q", sched.Crontab, tc.expr)
			}
		})
	}
}

// withSeconds=true is what makes the shipped 6-field config.yaml expression parse.
func TestWithSecondsIsWhatMakesTheSixFieldDefaultParse(t *testing.T) {
	noop := func() {}

	// Straight to gocron: withoutSeconds=false rejects the repo default outright.
	plain, err := gocron.NewScheduler(gocron.WithLocation(time.UTC))
	if err != nil {
		t.Fatalf("gocron.NewScheduler = %v", err)
	}
	t.Cleanup(func() { _ = plain.Shutdown() })

	_, err = plain.NewJob(gocron.CronJob(repoDefaultCron, false), gocron.NewTask(noop))
	if err == nil {
		t.Fatal("CronJob(6 field, withSeconds=false) parsed, expected it to be rejected")
	}
	if !errors.Is(err, gocron.ErrCronJobParse) {
		t.Fatalf("err = %v, want it to wrap gocron.ErrCronJobParse", err)
	}
	if !strings.Contains(err.Error(), "expected exactly 5 fields, found 6") {
		t.Fatalf("err = %v, want the field-count complaint the parser gives", err)
	}

	// The same expression parses with withSeconds=true.
	if _, err = plain.NewJob(gocron.CronJob(repoDefaultCron, true), gocron.NewTask(noop)); err != nil {
		t.Fatalf("CronJob(%q, withSeconds=true) = %v, want it to parse", repoDefaultCron, err)
	}
	newForTest(t, repoDefaultCron, "Asia/Shanghai", noop)
}

func TestNewRejectsAnExpressionThatIsNotCron(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want error
	}{
		{"prose", "every other tuesday", gocron.ErrCronJobParse},
		{"one field", "5", gocron.ErrCronJobParse},
		{"seven fields", "0 0 */30 * * * *", gocron.ErrCronJobParse},
		{"empty", "", gocron.ErrCronJobParse},
		// Parses, but can never fire: 30 February.
		{"impossible date", "0 0 0 30 2 *", gocron.ErrCronJobInvalid},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(tc.expr, "UTC", func() {}, zap.NewNop())
			if err == nil {
				_ = s.Stop()
				t.Fatalf("New(%q) = %v, want a rejection", tc.expr, err)
			}
			if s != nil {
				t.Fatalf("New(%q) returned %v alongside error %v, want a nil scheduler", tc.expr, s, err)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want it to wrap %v", err, tc.want)
			}
		})
	}
}

// A bad zone must name the offending config key instead of silently scheduling in the host zone.
func TestNewRejectsAnUnknownZone(t *testing.T) {
	s, err := New(repoDefaultCron, "Mars/Olympus_Mons", func() {}, zap.NewNop())
	if err == nil {
		_ = s.Stop()
		t.Fatal("New with a bogus zone succeeded, want it refused")
	}
	if s != nil {
		_ = s.Stop()
		t.Fatalf("New returned %v alongside error %v, want a nil scheduler", s, err)
	}
	if !strings.Contains(err.Error(), "scan.cron_zone") {
		t.Fatalf("err = %v, want it to name the scan.cron_zone key", err)
	}
	if !strings.Contains(err.Error(), "Mars/Olympus_Mons") {
		t.Fatalf("err = %v, want it to quote the bad zone", err)
	}

	// An empty zone is legal Go (time.LoadLocation("") is UTC) and must keep working.
	if got, err := New(repoDefaultCron, "", func() {}, zap.NewNop()); err != nil {
		t.Fatalf("New with the empty zone = %v, want UTC behaviour", err)
	} else {
		_ = got.Stop()
	}
}

// The panic test re-executes itself as a child so the parent survives to assert on the marker.
// gocron rounds sub-second descriptors to whole seconds, so "@every 100ms" fires at 1 Hz.
func TestTaskPanicIsContainedAndScheduleSurvives(t *testing.T) {
	const childEnv = "AREDNSCAN_TEST_PANIC_CHILD"
	const panicText = "sweep panicked"

	if os.Getenv(childEnv) == "1" {
		var runs atomic.Int64
		s, err := New("@every 1s", "UTC", func() {
			if runs.Add(1) == 1 {
				panic(panicText)
			}
		}, zap.NewNop())
		if err != nil {
			t.Fatalf("New in the child = %v", err)
		}
		s.Start()
		<-time.After(3500 * time.Millisecond)
		_ = s.Stop()
		// The parent asserts on this marker; a disarmed scheduler reports a low count.
		if got := runs.Load(); got >= 2 {
			fmt.Printf("CHILD-OK runs=%d\n", got)
		} else {
			fmt.Printf("CHILD-BAD runs=%d\n", got)
		}
		return
	}

	cmd := exec.Command(os.Args[0],
		"-test.run=TestTaskPanicIsContainedAndScheduleSurvives",
		"-test.timeout=30s")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		t.Fatalf("child died instead of containing the panic: %v\n%s", err, text)
	}
	if strings.Contains(text, "panic: "+panicText) && !strings.Contains(text, "CHILD-OK") {
		t.Fatalf("panic reached the top level:\n%s", text)
	}
	if !strings.Contains(text, "CHILD-OK") {
		t.Fatalf("child did not keep running the schedule after the panic:\n%s", text)
	}
}

// A nil closure must be rejected rather than panicking later inside the scheduler goroutine.
func TestNilJobFunctionRejected(t *testing.T) {
	if _, err := New("@every 1h", "UTC", nil, zap.NewNop()); err == nil {
		t.Fatal("New accepted a nil job function")
	}
}

// NextRun swallows gocron's error and reports the zero time until Start arms the schedule.
func TestNextRunIsZeroUntilStarted(t *testing.T) {
	s := newForTest(t, repoDefaultCron, "UTC", func() {})

	if got := s.NextRun(); !got.IsZero() {
		t.Fatalf("NextRun before Start = %v, want the zero time", got)
	}
	// The zero time comes from an empty schedule, not from the error path.
	if _, err := s.job.NextRun(); err != nil {
		t.Fatalf("job.NextRun before Start = %v, want no error, just an empty schedule", err)
	}

	s.Start()
	defer func() { _ = s.Stop() }()

	next := s.NextRun()
	if next.IsZero() {
		t.Fatal("NextRun after Start = zero, want the armed sweep time")
	}
	if now := time.Now(); !next.After(now) {
		t.Fatalf("NextRun = %v, want it in the future (now %v)", next, now)
	}
	// "0 */30 * * * *" fires on :00 and :30, so the wait is never over half an hour.
	if wait := next.Sub(time.Now()); wait > 30*time.Minute+time.Second {
		t.Fatalf("NextRun is %v away, want at most one 30 minute step", wait)
	}
	if sec := next.Second(); sec != 0 {
		t.Fatalf("NextRun = %v, want it on a whole second for %q", next, repoDefaultCron)
	}
	if m := next.Minute(); m != 0 && m != 30 {
		t.Fatalf("NextRun = %v, want minute 0 or 30", next)
	}
	if _, err := s.job.NextRun(); err != nil {
		t.Fatalf("job.NextRun after Start = %v, want the wrapper's value to be real", err)
	}

	// Start is not allowed to move the armed time backwards or double arm it.
	if again := s.NextRun(); !again.Equal(next) {
		t.Fatalf("NextRun changed from %v to %v, want a stable schedule", next, again)
	}
}

// The zone from config must make a time-of-day expression mean the same instant anywhere.
func TestZoneDecidesTheFiringInstant(t *testing.T) {
	const dailyAtThree = "0 3 * * *"
	const day = 24 * time.Hour

	nextInZone := func(t *testing.T, zone, wantOffset string) time.Time {
		t.Helper()
		s := newForTest(t, dailyAtThree, zone, func() {})
		s.Start()
		defer func() { _ = s.Stop() }()

		next := s.NextRun()
		if next.IsZero() {
			t.Fatalf("NextRun in %s = zero, want the armed time", zone)
		}
		if !next.After(time.Now()) {
			t.Fatalf("NextRun in %s = %v, want the future", zone, next)
		}
		// The expression's own clock, not the host's.
		if h, m, sec := next.Hour(), next.Minute(), next.Second(); h != 3 || m != 0 || sec != 0 {
			t.Fatalf("NextRun in %s = %v, want 03:00:00 on that zone's clock", zone, next)
		}
		if got := next.Format("-0700"); got != wantOffset {
			t.Fatalf("NextRun in %s has offset %s, want %s", zone, got, wantOffset)
		}
		return next
	}

	shanghai := nextInZone(t, "Asia/Shanghai", "+0800")
	utc := nextInZone(t, "UTC", "+0000")

	// Shanghai's 03:00 is 8 hours before UTC's; the invariant is the offset modulo a day.
	diff := (utc.Sub(shanghai)%day + day) % day
	if diff != 8*time.Hour {
		t.Fatalf("UTC next run %v minus Shanghai next run %v = %v mod 24h, want 8h",
			utc, shanghai, diff)
	}
}

// Waits on a channel rather than a sleep, and checks that Stop releases gocron's goroutines.
func TestTaskActuallyFires(t *testing.T) {
	baseline := runtime.NumGoroutine()

	type call struct {
		at time.Time
	}
	calls := make(chan call, 32)

	s := newForTest(t, "@every 100ms", "UTC", func() {
		// Non-blocking so a slow reader can never wedge the executor.
		select {
		case calls <- call{at: time.Now()}:
		default:
		}
	})
	s.Start()

	// Two firings prove a repeating schedule, not a one-shot.
	var stamps []time.Time
	for len(stamps) < 2 {
		select {
		case c := <-calls:
			if c.at.IsZero() {
				t.Fatal("task ran with a zero timestamp")
			}
			stamps = append(stamps, c.at)
		case <-time.After(15 * time.Second):
			t.Fatalf("task fired %d time(s) in 15s, want at least 2", len(stamps))
		}
	}
	if gap := stamps[1].Sub(stamps[0]); gap < 50*time.Millisecond {
		t.Fatalf("back to back firings %v apart, want the 100ms interval respected", gap)
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	// Stop must be idempotent: main.go defers it while the signal path may already have.
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop = %v, want idempotent shutdown", err)
	}

	// No calls may arrive after Stop; quiescence is bounded by a timer, not an unbounded sleep.
	select {
	case late := <-calls:
		t.Fatalf("task fired at %v after Stop, want the schedule stopped", late.at)
	case <-time.After(400 * time.Millisecond):
	}

	assertGoroutinesDrain(t, baseline)
}

// Covers the boot failure path where New succeeds and Start never happens.
func TestStopWithoutStartIsSafe(t *testing.T) {
	baseline := runtime.NumGoroutine()

	s, err := New(repoDefaultCron, "Asia/Shanghai", func() {}, zap.NewNop())
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop without Start = %v, want nil", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop = %v, want nil", err)
	}
	assertGoroutinesDrain(t, baseline)
}

// LimitModeReschedule must forbid overlap and keep the schedule alive after an overrun.
func TestSingletonModeRescheduleKeepsOneSweepAlive(t *testing.T) {
	var running, concurrentPeak, invocations atomic.Int32
	entered := make(chan struct{})
	hold := make(chan struct{})
	completed := make(chan int32, 16)

	s := newForTest(t, "@every 100ms", "UTC", func() {
		n := running.Add(1)
		for {
			peak := concurrentPeak.Load()
			if n <= peak || concurrentPeak.CompareAndSwap(peak, n) {
				break
			}
		}
		seq := invocations.Add(1)
		if seq == 1 {
			close(entered)
			<-hold // stand in for a sweep that overruns its interval
		}
		running.Add(-1)
		completed <- n
	})
	s.Start()

	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the first run never started")
	}

	// Ticks during the overrun must be skipped, not overlapped or queued to fire at once.
	<-time.After(600 * time.Millisecond)
	if n := invocations.Load(); n != 1 {
		t.Fatalf("invocations during the overrun = %d, want 1", n)
	}
	if got := running.Load(); got != 1 {
		t.Fatalf("runs in flight during the overrun = %d, want the single held one", got)
	}

	close(hold)

	// The held run plus one rescheduled after it: the schedule continues past skipped ticks.
	for i := range 2 {
		select {
		case <-completed:
		case <-time.After(15 * time.Second):
			t.Fatalf("only %d run(s) completed after the overrun, want the schedule to continue", i)
		}
	}

	if peak := concurrentPeak.Load(); peak != 1 {
		t.Fatalf("peak concurrency = %d, want 1: WithSingletonMode(LimitModeReschedule) must forbid overlap", peak)
	}
	if n := invocations.Load(); n < 2 {
		t.Fatalf("invocations = %d, want the held run plus the one rescheduled after it", n)
	}
}

func TestRunNowFiresOutOfBand(t *testing.T) {
	ran := make(chan time.Time, 4)
	// A yearly cron so the only way the task can run is RunNow.
	s := newForTest(t, "0 0 0 1 1 *", "UTC", func() {
		select {
		case ran <- time.Now():
		default:
		}
	})
	s.Start()

	// A schedule this far out still reports an armed time.
	if next := s.NextRun(); next.IsZero() {
		t.Fatal("NextRun = zero for a yearly cron, want it armed")
	}

	if err := s.RunNow(); err != nil {
		t.Fatalf("RunNow = %v", err)
	}
	select {
	case at := <-ran:
		if wait := time.Since(at); wait > 10*time.Second {
			t.Fatalf("RunNow task ran %v ago, want it immediately", wait)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RunNow never invoked the task")
	}
}

// assertGoroutinesDrain polls with a deadline; a goroutine count has no channel to receive on.
func assertGoroutinesDrain(t *testing.T, baseline int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	count := runtime.NumGoroutine()
	for count > baseline && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		count = runtime.NumGoroutine()
	}
	if count > baseline {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("goroutines after Stop = %d, baseline %d, leaked scheduler workers:\n%s",
			count, baseline, buf)
	}
}
