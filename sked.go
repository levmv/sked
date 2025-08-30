// Package sked provides a small, zero-dependency job scheduler with a
// chainable API, context-aware execution, and DST-aware calendars.
package sked

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
)

// Scheduler manages jobs and runs each on its own background worker.
// Create it with New(ctx). Start it with Run().
type Scheduler struct {
	jobs []*Job

	ctx      context.Context
	location *time.Location
	logger   SlogLogger

	mu      sync.Mutex
	started bool
}

type Option func(*Scheduler)

func New(ctx context.Context, opts ...Option) *Scheduler {
	if ctx == nil {
		ctx = context.Background()
	}
	s := &Scheduler{
		ctx:      ctx,
		location: time.Local,
		logger:   discardLogger{},
		started:  false,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WithLocation sets the time.Location the scheduler uses for all calendar
// calculations (midnight alignment, At/Between, DST handling).
func WithLocation(loc *time.Location) Option {
	return func(s *Scheduler) {
		if loc != nil {
			s.location = loc
		}
	}
}

// WithLogger installs a slog-compatible logger used for debug/error messages.
func WithLogger(logger SlogLogger) Option {
	return func(s *Scheduler) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// Schedule registers a new job and returns a builder to configure its schedule.
// Panics if j is nil or if the scheduler has already started.
func (sh *Scheduler) Schedule(j func(context.Context)) *Job {
	if j == nil {
		panic("sked: cannot schedule a nil function")
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if sh.started {
		panic("sked: cannot schedule new jobs after scheduler has started")
	}

	job := &Job{fn: j}
	sh.jobs = append(sh.jobs, job)
	return job
}

// Run validates all scheduled jobs and starts their background workers.
// It returns immediately on success. However, if any job is misconfigured,
// Run will return an error and no jobs will be started.
// After a successful start, the scheduler runs until the context is canceled.
func (sh *Scheduler) Run() error {
	sh.mu.Lock()
	defer sh.mu.Unlock()

	if sh.started {
		return errors.New("scheduler already started")
	}
	if sh.ctx.Err() != nil {
		return fmt.Errorf("scheduler context error: %w", sh.ctx.Err())
	}

	for _, j := range sh.jobs {
		if err := j.prepare(); err != nil {
			return err
		}
	}

	sh.started = true

	for _, j := range sh.jobs {
		go sh.jobWorker(j)
	}
	return nil
}

// staleThreshold defines the maximum acceptable delay for a timer firing.
// If the actual delay is larger than this, we assume a significant event
// occurred (like system sleep) and that the original scheduled context for
// the job is no longer valid.
const staleThreshold = time.Second

func (sh *Scheduler) jobWorker(j *Job) {
	next, hasNext := sh.planNextRun(j, time.Now().In(sh.location))

	if next.IsZero() {
		sh.logger.Debug("job will not be scheduled for a future run", "name", j.name)
		return
	}

	t := time.NewTimer(time.Until(next))
	defer t.Stop()

	for {
		select {
		case <-t.C:
			if time.Since(next) < staleThreshold && !j.shouldSkip(next) {
				doJob(sh.ctx, sh.logger, j)
			} else {
				sh.logger.Debug("stale timer fired; job execution skipped",
					"name", j.name,
					"scheduled_for", next,
					"woke_at", time.Now().In(sh.location),
				)
			}
			if !hasNext {
				sh.logger.Debug("one-off job finished", "name", j.name)
				return
			}

			// Find the next candidate time.
			next, hasNext = sh.planNextRun(j, next)

			if next.IsZero() {
				sh.logger.Debug("job finished its schedule", "name", j.name)
				return
			}

			t.Reset(time.Until(next))

		case <-sh.ctx.Done():
			return
		}
	}
}

// planNextRun calculates the next valid execution time for a job, ensuring it's
// always in the future.
//
// It first determines the ideal next run time based on the previous one. If that
// time is in the past (due to system sleep or a long-running job), it asks the
// schedule to find the next valid time after the current moment.
//
// It returns a zero time and false if no future run is possible.
func (sh *Scheduler) planNextRun(j *Job, lastRun time.Time) (time.Time, bool) {
	next, hasNext := j.schedule.Next(lastRun, sh.location)

	if next.IsZero() || !hasNext {
		return next, hasNext
	}

	now := time.Now().In(sh.location)
	if next.Before(now) {
		next, hasNext = j.schedule.Next(now, sh.location)
		if next.IsZero() || !hasNext || !next.After(now) {
			sh.logger.Error("no future run found", "name", j.name)
			return time.Time{}, false
		}
	}

	return next, hasNext
}

// doJob executes a single run of a job's function.
func doJob(ctx context.Context, logger SlogLogger, j *Job) {
	jobCtx := ctx
	var cancel context.CancelFunc

	if j.timeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, j.timeout)
		defer cancel()
	}

	defer func() {
		if r := recover(); r != nil {
			logger.Error("job panicked", "name", j.name, "error", r, "stack", string(debug.Stack()))
		}
		if j.timeout > 0 && jobCtx.Err() == context.DeadlineExceeded {
			logger.Error("job timed out", "name", j.name, "timeout", j.timeout)
		}
	}()

	logger.Debug("executing job", "name", j.name)
	j.fn(jobCtx)
}

// Job represents a scheduled unit of work configured via the builder methods (Every, At, etc.).
type Job struct {
	name     string
	fn       func(context.Context)
	schedule Schedule
	tods     []time.Duration
	except   []func(time.Time) bool
	timeout  time.Duration
	err      error
}

// Every schedules a job to run at regular intervals. Intervals are aligned to
// midnight in the scheduler's location (see WithLocation); for example, a job
// with Every(30 * time.Minute) will run at 00:00, 00:30, 01:00, etc.,
// regardless of when the scheduler was started. Use AtOffset() to shift this alignment.
func (j *Job) Every(d time.Duration) *Job {
	if d <= 0 {
		j.err = errors.New("Every() interval must be a positive duration")
		return j
	}
	j.setSchedule(&IntervalSchedule{Interval: d})
	return j
}

// Daily is shorthand for EveryNDays(1). Combine with At to set the time of day.
func (j *Job) Daily() *Job { return j.EveryNDays(1) }

// EveryNDays schedules the job every n days. Combine with At to set the time
// of day; midnight is used if At is not specified.
func (j *Job) EveryNDays(n int) *Job {
	if n <= 0 {
		j.err = errors.New("EveryNDays() n must be positive")
		return j
	}
	j.setSchedule(&DailySchedule{
		atTimes: newAtTimes(j.tods),
		N:       n,
	})
	return j
}

// On sets a weekly schedule by specifying the weekdays the job should run.
// Combine with At to set the time of day.
func (j *Job) On(days ...time.Weekday) *Job {
	if len(days) == 0 {
		j.err = errors.New("On() requires at least one weekday")
		return j
	}
	var weekdays [7]bool
	for _, day := range days {
		if day < time.Sunday || day > time.Saturday {
			j.err = errors.New("On() received an invalid weekday")
			return j
		}
		weekdays[day] = true
	}
	j.setSchedule(&WeeklySchedule{
		atTimes:  newAtTimes(j.tods),
		Weekdays: weekdays,
	})
	return j
}

// OnThe sets a monthly schedule by day of month. Use -1 for the last day.
// If a day does not exist in a given month (e.g., 31 in April), the job
// will not run in that month. Combine with At to set the time of day.
func (j *Job) OnThe(day int) *Job {
	if (day < 1 || day > 31) && day != -1 {
		j.err = errors.New("OnThe() day is invalid")
		return j
	}
	j.setSchedule(&MonthlySchedule{
		atTimes:    newAtTimes(j.tods),
		DayOfMonth: day,
	})
	return j
}

// In schedules a one-off job to run once after the given delay. The delay is
// relative to when the scheduler is started via the Run() method.
func (j *Job) In(d time.Duration) *Job {
	if d < 0 {
		j.err = errors.New("In() duration cannot be negative")
		return j
	}
	j.setSchedule(&RelativeTimeSchedule{
		Duration: d,
	})
	return j
}

// At sets the time of day for a job to run, specified in "15:04" or
// "15:04:05" format. It is used to refine calendar-based schedules like
// Daily(), On(), and OnThe().
func (j *Job) At(times ...string) *Job {
	if len(times) == 0 {
		j.err = errors.New("At() requires at least one time string")
		return j
	}
	for _, timeStr := range times {
		timeStr = strings.TrimSpace(timeStr)

		var t time.Time
		var err error

		if t, err = time.Parse("15:04:05", timeStr); err != nil {
			if t, err = time.Parse("15:04", timeStr); err != nil {
				j.err = fmt.Errorf("invalid time string in At(): %w", err)
				return j
			}
		}
		tod := time.Duration(t.Hour())*time.Hour +
			time.Duration(t.Minute())*time.Minute +
			time.Duration(t.Second())*time.Second

		j.tods = append(j.tods, tod)
	}

	if j.schedule == nil {
		return j
	}

	switch s := j.schedule.(type) {
	case *DailySchedule:
		s.atTimes = newAtTimes(j.tods)
	case *WeeklySchedule:
		s.atTimes = newAtTimes(j.tods)
	case *MonthlySchedule:
		s.atTimes = newAtTimes(j.tods)
	default:
		j.err = errors.New("At() can only be used with calendar-based schedules (Daily, On, OnThe)")
	}
	return j
}

// AtOffset shifts the alignment of an Every(...) interval by the given
// duration (e.g., 1h to run at whole hours). Only valid with Every(...).
func (j *Job) AtOffset(d time.Duration) *Job {
	if d < 0 || d >= 24*time.Hour {
		j.err = errors.New("AtOffset() must be in [0, 24h)")
		return j
	}
	if s, ok := j.schedule.(*IntervalSchedule); ok && s != nil {
		s.Offset = d
	} else {
		j.err = errors.New("AtOffset() can only be used with an Every() schedule")
	}
	return j
}

// --- Execution Modifiers ---

// WithTimeout sets a per-run deadline. The job's context is canceled if
// execution exceeds d; your job function should observe ctx.Done().
func (j *Job) WithTimeout(d time.Duration) *Job {
	if d <= 0 {
		j.err = errors.New("WithTimeout() duration must be positive")
		return j
	}
	j.timeout = d
	return j
}

// Except allows for dynamically skipping a job run. The provided function is
// called with the potential run time, and if it returns true, the job is
// skipped for that occurrence.
func (j *Job) Except(f func(time.Time) bool) *Job {
	j.except = append(j.except, f)
	return j
}

// Between restricts execution to the [from, to) hourly window in local time.
// Hours are integers 0..23; a zero-length window (from == to) skips all runs.
func (j *Job) Between(from, to int) *Job {
	if from < 0 || from > 23 || to < 0 || to > 23 {
		j.err = errors.New("hours for Between() must be between 0 and 23")
		return j
	}

	// This filter uses the [from, to) interval convention, meaning it includes
	// the 'from' hour but excludes the 'to' hour.
	j.Except(func(t time.Time) bool {
		h := t.Hour()

		if from == to {
			return true
		}

		if from < to {
			return h < from || h >= to
		}

		// Overnight window (e.g., from=22, to=6)
		return h >= to && h < from
	})

	return j
}

// WithName sets a human-readable name for logging; if omitted, the function
// name is derived from the scheduled function.
func (j *Job) WithName(name string) *Job {
	j.name = name
	return j
}

func (j *Job) setSchedule(s Schedule) {
	if j.schedule != nil {
		panic("sked: a scheduler type (like Every, Daily, or On) has already been set")
	}
	j.schedule = s
}

// prepare finalizes the job's configuration and validates it.
// This should be called once before the scheduler starts.
func (j *Job) prepare() error {
	if j.name == "" {
		j.name = getFuncName(j.fn)
	}

	if j.err != nil {
		return fmt.Errorf("job %q configuration error: %w", j.name, j.err)
	}
	if j.schedule == nil {
		return fmt.Errorf("job %q has no schedule (call Every/Daily/On/OnThe/In)", j.name)
	}

	return nil
}

func (j *Job) shouldSkip(t time.Time) bool {
	for _, ex := range j.except {
		if ex(t) {
			return true
		}
	}
	return false
}

// Schedule is the core strategy interface for calculating run times.
type Schedule interface {
	// Next returns the first valid scheduled time after `t`.
	// The boolean return value indicates if the schedule is recurring.
	// It should return (zero time, false) if no future time is possible.
	Next(t time.Time, loc *time.Location) (time.Time, bool)
}

// RelativeTimeSchedule represents a job that runs only once at a specific time.
type RelativeTimeSchedule struct {
	Duration time.Duration
}

func (s *RelativeTimeSchedule) Next(t time.Time, loc *time.Location) (time.Time, bool) {
	return t.Add(s.Duration), false
}

// IntervalSchedule implements a simple, duration-based schedule (e.g., "every 30 minutes").
type IntervalSchedule struct {
	Interval time.Duration
	Offset   time.Duration
}

func (s *IntervalSchedule) Next(t time.Time, loc *time.Location) (time.Time, bool) {
	t = t.In(loc)
	year, month, day := t.Date()
	midnight := time.Date(year, month, day, 0, 0, 0, 0, loc)
	base := midnight.Add(s.Offset)
	if base.After(t) {
		return base, true
	}
	timeSinceBase := t.Sub(base)
	intervalsPassed := timeSinceBase / s.Interval
	return base.Add((intervalsPassed + 1) * s.Interval), true
}

// atTimes handles the "time of day" logic.
type atTimes struct {
	tods []time.Duration // Times of day from midnight
}

func newAtTimes(tods []time.Duration) atTimes {
	var todsCopy []time.Duration

	if len(tods) == 0 {
		// Default to midnight if no time is specified via At().
		todsCopy = []time.Duration{0}
	} else {
		todsCopy = make([]time.Duration, len(tods))
		copy(todsCopy, tods)
	}

	slices.Sort(todsCopy)
	todsCopy = slices.Compact(todsCopy)

	return atTimes{tods: todsCopy}
}

// findNextTime finds the first scheduled time that occurs on the same day as
// searchDate, but strictly after it.
func (as *atTimes) findNextTime(searchDate, t time.Time) time.Time {
	year, month, day := searchDate.Date()
	loc := searchDate.Location()

	for _, tod := range as.tods {
		hour := int(tod / time.Hour)
		minute := int((tod % time.Hour) / time.Minute)
		second := int((tod % time.Minute) / time.Second)

		next := time.Date(year, month, day, hour, minute, second, 0, loc)
		if next.After(t) {
			return next
		}
	}
	return time.Time{}
}

// findNextOnDay is a convenience wrapper for the common case of finding the
// next time on the same day as the last run.
func (as *atTimes) findNextOnDay(t time.Time) time.Time {
	return as.findNextTime(t, t)
}

// DailySchedule implements a DST-aware schedule for every N days.
type DailySchedule struct {
	atTimes
	N int
}

func (s *DailySchedule) Next(t time.Time, loc *time.Location) (time.Time, bool) {
	t = t.In(loc)

	if next := s.findNextOnDay(t); !next.IsZero() {
		return next, true
	}
	year, month, day := t.Date()
	nextRunDayBase := time.Date(year, month, day, 0, 0, 0, 0, loc).AddDate(0, 0, s.N)
	return s.findNextTime(nextRunDayBase, t), true
}

// WeeklySchedule implements a DST-aware schedule for specific days of the week.
type WeeklySchedule struct {
	atTimes
	Weekdays [7]bool
}

func (s *WeeklySchedule) Next(t time.Time, loc *time.Location) (time.Time, bool) {
	t = t.In(loc)

	if s.Weekdays[t.Weekday()] {
		if next := s.findNextOnDay(t); !next.IsZero() {
			return next, true
		}
	}

	year, month, day := t.Date()
	d := time.Date(year, month, day, 0, 0, 0, 0, loc)

	for i := 0; i < 7; i++ {
		d = d.AddDate(0, 0, 1)
		if s.Weekdays[d.Weekday()] {
			return s.findNextTime(d, t), true
		}
	}

	return time.Time{}, false
}

// MonthlySchedule implements a DST-aware schedule for a specific day of the month.
type MonthlySchedule struct {
	atTimes
	DayOfMonth int
}

func (s *MonthlySchedule) Next(t time.Time, loc *time.Location) (time.Time, bool) {
	t = t.In(loc)

	// Internal helper to find the valid run DATE (at midnight) for a given month.
	getTargetDateInMonth := func(y int, m time.Month) time.Time {
		day := s.DayOfMonth
		if day == -1 {
			// Last day of the month
			lastDay := time.Date(y, m+1, 1, 0, 0, 0, 0, loc).AddDate(0, 0, -1)
			day = lastDay.Day()
		}
		// Return a zero time for invalid dates like Feb 30.
		targetDate := time.Date(y, m, day, 0, 0, 0, 0, loc)
		if targetDate.Month() != m {
			return time.Time{}
		}
		return targetDate
	}

	targetThisMonth := getTargetDateInMonth(t.Year(), t.Month())
	// Only proceed if a valid schedule day exists in this month (e.g., April 31st is invalid).
	if !targetThisMonth.IsZero() {
		if next := s.findNextTime(targetThisMonth, t); !next.IsZero() {
			return next, true
		}
	}
	nextMonthBase := time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
	for i := 0; i < 10000; i++ {
		targetNextMonth := getTargetDateInMonth(nextMonthBase.Year(), nextMonthBase.Month())

		if !targetNextMonth.IsZero() {
			// Get the first time slot of the valid future day.
			return s.findNextTime(targetNextMonth, t), true
		}
		nextMonthBase = nextMonthBase.AddDate(0, 1, 0)
	}
	return time.Time{}, false
}

// SlogLogger is a minimal logging interface used by the scheduler.
type SlogLogger interface {
	Debug(msg string, args ...any)
	Error(msg string, args ...any)
}

// discardLogger is a no-op logger used by default.
type discardLogger struct{}

func (d discardLogger) Debug(msg string, args ...any) {}
func (d discardLogger) Error(msg string, args ...any) {}

// getFuncName attempts to retrieve the name of a function using reflection.
// This is used to provide a default name for logging when a job is not
// explicitly named with WithName().
func getFuncName(fn any) string {
	v := reflect.ValueOf(fn)
	if v.Kind() == reflect.Func {
		if pc := v.Pointer(); pc != 0 {
			if f := runtime.FuncForPC(pc); f != nil {
				return f.Name()
			}
		}
	}
	return "unnamed-job"
}
