package sked

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// NOTE: These tests run against real wall-clock time.
//
// This was a pragmatic decision: implementing a reliable fake clock turned out
// to be surprisingly tricky, especially with the library's earlier fully
// asynchronous design.
//
// Real-time tests are slower and cover fewer scenarios than a mock-based
// approach, but they still serve as a useful high-level check that the
// scheduler's core timing loop behaves correctly.

func newTestScheduler(t *testing.T) (*Scheduler, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	return New(ctx), cancel
}

func TestScheduler_Every_RealTime(t *testing.T) {
	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	var runCount int32

	scheduler.Schedule(func(ctx context.Context) {
		atomic.AddInt32(&runCount, 1)
	}).Every(20 * time.Millisecond)

	go scheduler.Run()

	time.Sleep(105 * time.Millisecond)

	finalRunCount := atomic.LoadInt32(&runCount)
	if finalRunCount < 4 || finalRunCount > 6 {
		t.Errorf("expected 4-6 runs in ~100ms, but got %d", finalRunCount)
	}
}

// TestScheduler_Daily_MultipleTimes_At verifies that a job can be scheduled
// for multiple times in a single day.
func TestScheduler_Daily_MultipleTimes_At(t *testing.T) {
	t.Parallel()

	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	runCount := make(chan time.Time, 2)

	now := time.Now().Truncate(time.Second)
	time1 := now.Add(1 * time.Second)
	time2 := now.Add(2 * time.Second)

	scheduler.Schedule(func(ctx context.Context) {
		runCount <- time.Now()
	}).Daily().At(time1.Format("15:04:05"), time2.Format("15:04:05"))

	go scheduler.Run()

	var runs []time.Time
	for i := 0; i < 2; i++ {
		select {
		case runAt := <-runCount:
			runs = append(runs, runAt)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for runs; got %d runs, expected 2", len(runs))
		}
	}

	delta1 := runs[0].Sub(time1)
	delta2 := runs[1].Sub(time2)

	if delta1 < 0 || delta1 > 100*time.Millisecond {
		t.Errorf("first run has unexpected delay: %v", delta1)
	}
	if delta2 < 0 || delta2 > 100*time.Millisecond {
		t.Errorf("second run has unexpected delay: %v", delta2)
	}
}

// TestScheduler_Daily_FlexibleOrder_AtDaily verifies that the At().Daily()
// order of configuration works correctly.
func TestScheduler_Daily_FlexibleOrder_AtDaily(t *testing.T) {
	t.Parallel()

	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	runSignal := make(chan struct{}, 1)

	runTime := time.Now().Add(1 * time.Second)

	scheduler.Schedule(func(ctx context.Context) {
		close(runSignal)
	}).At(runTime.Format("15:04:05")).Daily()

	go scheduler.Run()

	select {
	case <-runSignal:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not run at the expected time using At().Daily() order")
	}
}

// TestScheduler_Daily_DefaultTimeIsMidnight verifies that Daily() without At()
// schedules the job for midnight.
func TestScheduler_Daily_DefaultTimeIsMidnight(t *testing.T) {
	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	job := scheduler.Schedule(func(ctx context.Context) {}).Daily()

	if job.schedule == nil {
		t.Fatal("job.schedule was not set after calling .Daily()")
	}

	// Calculate the expected next midnight.
	now := time.Now()
	year, month, day := now.Date()
	todayMidnight := time.Date(year, month, day, 0, 0, 0, 0, now.Location())

	var expectedNextRun time.Time
	// If it's already past midnight today, the next run is tomorrow's midnight.
	if now.After(todayMidnight) {
		expectedNextRun = todayMidnight.AddDate(0, 0, 1)
	} else {
		// If it's before midnight, the run is for today's midnight.
		expectedNextRun = todayMidnight
	}

	actualNextRun, _ := job.schedule.Next(now, now.Location())

	if !actualNextRun.Equal(expectedNextRun) {
		t.Errorf("expected next run to be at midnight (%v), but got %v", expectedNextRun, actualNextRun)
	}
}

// TestScheduler_OneOff_InZero_RunsImmediately verifies that a job scheduled with
// a zero duration runs as soon as the scheduler starts, without a significant delay.
func TestScheduler_OneOff_InZero_RunsImmediately(t *testing.T) {
	t.Parallel()

	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	runSignal := make(chan struct{}, 1)

	scheduler.Schedule(func(ctx context.Context) {
		runSignal <- struct{}{}
	}).In(0)

	go scheduler.Run()

	select {
	case <-runSignal:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("one-off job with In(0) did not run immediately")
	}
}

// TestScheduler_LongRunningJob_DoesNotOverlap verifies that the synchronous scheduler
// waits for a long-running job to complete before scheduling the next one.
func TestScheduler_LongRunningJob_DoesNotOverlap(t *testing.T) {
	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	var mu sync.Mutex
	var runTimes []time.Time
	jobDuration := 50 * time.Millisecond

	jobFunc := func(ctx context.Context) {
		mu.Lock()
		runTimes = append(runTimes, time.Now())
		mu.Unlock()
		time.Sleep(jobDuration)
	}

	scheduler.Schedule(jobFunc).Every(20 * time.Millisecond)

	go scheduler.Run()
	// Allow enough time for 3 runs to complete (0ms, 60ms, 120ms)
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(runTimes) < 2 {
		t.Fatalf("expected at least 2 job runs, but got %d", len(runTimes))
	}

	for i := 1; i < len(runTimes); i++ {
		delta := runTimes[i].Sub(runTimes[i-1])
		if delta < jobDuration {
			t.Errorf("job overlap detected! Time between run %d and %d was %v, which is less than job duration %v",
				i-1, i, delta, jobDuration)
		}
	}
}

func TestScheduler_ContextCancellation_StopsJobs(t *testing.T) {
	scheduler, cancel := newTestScheduler(t)
	var runCount int32

	// Schedule a job that runs very frequently.
	scheduler.Schedule(func(ctx context.Context) {
		atomic.AddInt32(&runCount, 1)
	}).Every(10 * time.Millisecond)

	go scheduler.Run()
	time.Sleep(25 * time.Millisecond) // Allow it to run a couple of times.

	cancel()

	// Wait a bit longer to see if any more jobs run after cancellation.
	time.Sleep(50 * time.Millisecond)

	runsAfterCancel := atomic.LoadInt32(&runCount)
	if runsAfterCancel < 1 || runsAfterCancel > 3 {
		t.Errorf("expected 1-3 runs before context cancellation, got %d", runsAfterCancel)
	}

	// Wait even longer and check that the count has not changed.
	time.Sleep(50 * time.Millisecond)
	finalRunCount := atomic.LoadInt32(&runCount)

	if finalRunCount != runsAfterCancel {
		t.Errorf("job ran after context was cancelled. Count changed from %d to %d", runsAfterCancel, finalRunCount)
	}
}

// --- One-Off Job Tests ---

func TestScheduler_OneOff_In_RealTime(t *testing.T) {
	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	runSignal := make(chan struct{}, 1)

	scheduler.Schedule(func(ctx context.Context) {
		runSignal <- struct{}{}
	}).In(50 * time.Millisecond)

	start := time.Now()
	go scheduler.Run()

	select {
	case <-runSignal:
		duration := time.Since(start)
		// Check if it ran within a reasonable window of the expected time.
		if duration < 40*time.Millisecond || duration > 100*time.Millisecond {
			t.Errorf("job ran after %v, expected ~50ms", duration)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("one-off job scheduled with In() did not run within the expected time")
	}
}

// --- Filter Tests ---

func TestScheduler_Filter_Except(t *testing.T) {
	t.Parallel()

	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	var mu sync.Mutex
	var runs []int
	var runCounter int

	// Skip odd-numbered runs, expect only even ones recorded.
	scheduler.Schedule(func(ctx context.Context) {
		mu.Lock()
		defer mu.Unlock()
		runs = append(runs, runCounter)
	}).Every(20 * time.Millisecond).Except(func(t time.Time) bool {
		mu.Lock()
		defer mu.Unlock()

		runCounter++
		return runCounter%2 != 0
	})

	go scheduler.Run()
	time.Sleep(105 * time.Millisecond) // Should allow for 5 checks.

	mu.Lock()
	defer mu.Unlock()
	// Expected checks: 1 (skip), 2 (run), 3 (skip), 4 (run), 5 (skip)
	// So, the 'runs' slice should contain the values [2, 4]
	if len(runs) < 2 || runs[0] != 2 || runs[1] != 4 {
		t.Errorf("expected runs [2, 4] but got %v", runs)
	}
}

func TestScheduler_Filter_Between(t *testing.T) {
	t.Parallel()
	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	runSignal := make(chan struct{}, 1)

	currentHour := time.Now().Hour()
	// Create a window that definitely includes the current hour.
	fromHour := (currentHour - 1 + 24) % 24
	toHour := (currentHour + 2) % 24

	scheduler.Schedule(func(ctx context.Context) {
		runSignal <- struct{}{}
	}).Every(20*time.Millisecond).Between(fromHour, toHour)

	go scheduler.Run()

	select {
	case <-runSignal:
	case <-time.After(100 * time.Millisecond):
		t.Error("job did not run even though current hour is within the Between() window")
	}

	// Test the inverse: a window that excludes the current hour.
	scheduler2, cancel2 := newTestScheduler(t)
	defer cancel2()

	runSignal2 := make(chan struct{}, 1)

	fromHour = (currentHour + 2) % 24
	toHour = (currentHour + 4) % 24

	scheduler2.Schedule(func(ctx context.Context) {
		runSignal2 <- struct{}{}
	}).Every(20*time.Millisecond).Between(fromHour, toHour)

	go scheduler2.Run()

	select {
	case <-runSignal2:
		t.Error("job ran even though current hour is outside the Between() window")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestScheduler_InvalidConfigurations_Panics(t *testing.T) {
	t.Run("nil function", func(t *testing.T) {
		scheduler := New(context.Background())
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for nil function")
			}
		}()
		scheduler.Schedule(nil)
	})

	t.Run("schedule after run", func(t *testing.T) {
		scheduler := New(context.Background())
		scheduler.Schedule(func(ctx context.Context) {}).Every(time.Hour)
		scheduler.Run()

		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic when scheduling after Run()")
			}
		}()
		scheduler.Schedule(func(ctx context.Context) {}).Every(time.Hour)
	})
}

func TestScheduler_InvalidConfigurations_Errors(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(*Scheduler)
		expectError bool
	}{
		{
			name: "invalid duration",
			setup: func(s *Scheduler) {
				s.Schedule(func(ctx context.Context) {}).Every(-1 * time.Hour)
			},
			expectError: true,
		},
		{
			name: "invalid time format",
			setup: func(s *Scheduler) {
				s.Schedule(func(ctx context.Context) {}).Daily().At("25:00")
			},
			expectError: true,
		},
		{
			name: "invalid timeout",
			setup: func(s *Scheduler) {
				s.Schedule(func(ctx context.Context) {}).Every(time.Hour).WithTimeout(-1 * time.Hour)
			},
			expectError: true,
		},
		{
			name: "invalid day of month",
			setup: func(s *Scheduler) {
				s.Schedule(func(ctx context.Context) {}).OnThe(32)
			},
			expectError: true,
		},
		{
			name: "no schedule configured",
			setup: func(s *Scheduler) {
				s.Schedule(func(ctx context.Context) {})
			},
			expectError: true,
		},
		{
			name: "valid configuration",
			setup: func(s *Scheduler) {
				s.Schedule(func(ctx context.Context) {}).Every(time.Hour)
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheduler, cancel := newTestScheduler(t)
			defer cancel()

			tt.setup(scheduler)

			err := scheduler.Run()
			if tt.expectError && err == nil {
				t.Errorf("expected error for %s, got nil", tt.name)
			}
			if !tt.expectError && err != nil {
				t.Errorf("unexpected error for %s: %v", tt.name, err)
			}
		})
	}
}

// TestScheduler_PanicRecovery verifies that if a job function panics, the
// scheduler recovers from it and continues to run other scheduled jobs.
func TestScheduler_PanicRecovery(t *testing.T) {
	t.Parallel()
	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	survivorJobRan := make(chan struct{})

	panickingJob := func(ctx context.Context) {
		panic("a deliberate panic for testing recovery")
	}

	survivorJob := func(ctx context.Context) {
		close(survivorJobRan)
	}

	scheduler.Schedule(panickingJob).In(10 * time.Millisecond)
	scheduler.Schedule(survivorJob).In(50 * time.Millisecond)

	go scheduler.Run()

	select {
	case <-survivorJobRan:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for survivor job to run; the scheduler likely crashed")
	}
}

// TestScheduler_RecurringJob_RecoversFromPanic verifies that if a recurring
// job panics, it is still scheduled for its subsequent runs.
func TestScheduler_RecurringJob_RecoversFromPanic(t *testing.T) {
	t.Parallel()

	scheduler, cancel := newTestScheduler(t)
	defer cancel()

	var runCount atomic.Int32
	secondRunSucceeded := make(chan struct{})

	job := func(ctx context.Context) {
		count := runCount.Add(1)

		if count == 1 {
			panic("deliberate panic on first run")
		}

		if count == 2 {
			close(secondRunSucceeded)
		}
	}

	scheduler.Schedule(job).Every(20 * time.Millisecond)

	go scheduler.Run()

	select {
	case <-secondRunSucceeded:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for the second run; the job was not rescheduled after panicking")
	}
}
