# sked

[![Go Reference](https://pkg.go.dev/badge/github.com/levmv/sked.svg)](https://pkg.go.dev/github.com/levmv/sked)
[![Go Report Card](https://goreportcard.com/badge/github.com/levmv/sked)](https://goreportcard.com/report/github.com/levmv/sked)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A lightweight, zero-dependency, and idiomatic job scheduler for Go.

## Features

-   **Chainable API**: Configure jobs with a simple, readable syntax: `s.Schedule(task).Every(5*time.Second)`.
-   **Scheduling Options**: Provides DST-aware schedules for intervals (`Every`), days of the week (`On`), and days of the month (`OnThe`). Supports one-off jobs via `In(duration)` or `At("15:04[:05]", ...)`.
-   **Context-Aware**: The scheduler's lifecycle is managed by a `context.Context`, eliminating the need for a `Stop()` method. Job functions receive this context for graceful cancellation.
-   **Panic Recovery**: Recovers from panics within a job's execution, logs the error, and allows other jobs to continue running unaffected.
-   **Job Timeouts**: Apply a per-job execution deadline using `.WithTimeout(duration)`.
-   **Timezone Support**: Configure schedules to run in specific timezones via `WithLocation(loc)`.
-   **Pluggable Logging**: Uses a minimal interface compatible with `slog` to integrate with your application's logger.



## Usage

A minimal example of setting up a few jobs.

```go
// Define your job functions. They must accept a context.Context.
func periodicReport(ctx context.Context) { /* ... */ }
func nightlyBackup(ctx context.Context)  { /* ... */ }
func weeklyCleanup(ctx context.Context)  { /* ... */ }

// In your application's setup code:
func setupScheduler(ctx context.Context) {
	// Create a new scheduler.
	sched := sked.New(ctx)

	// Schedule your functions with a simple, chainable API.
	sched.Schedule(periodicReport).Every(30 * time.Minute)
	sched.Schedule(nightlyBackup).At("03:00").EveryNDays(2)
	sched.Schedule(databaseSync).Daily().At("09:00", "21:00")
	sched.Schedule(weeklyCleanup).On(time.Sunday).At("04:00").WithTimeout(2 * time.Hour)

	// Run is non-blocking and starts the job workers.
	if err := sched.Run(); err != nil {		
    	log.Fatalf("Failed to start scheduler: %v", err)
	}
}
```

### API Reference

#### Scheduler Configuration

Options are passed to `sked.New` to configure the scheduler instance.

*   `sked.WithLocation(loc *time.Location)`: Sets the scheduler's timezone. This affects all calendar-based schedules like `.Daily()` and `.At()`. Defaults to `time.Local`.
*   `sked.WithLogger(logger SlogLogger)`: Integrates with any logger compatible with Go's `log/slog`. Defaults to a logger that discards all output.

#### Job Scheduling

All scheduling methods are chainable from `scheduler.Schedule(jobFunc)`.

#### Schedule Frequency

These methods define the core recurrence of a job. You can only use one per job.

*   `.Every(duration)`: Run at a fixed interval, aligned to the wall-clock (e.g., `Every(15*time.Minute)` runs at `xx:00`, `xx:15`, etc.).
*   `.Daily()`: Run once per day.
*   `.EveryNDays(n)`: Run every `n` days.
*   `.On(time.Weekday, ...)`: Run on specific days of the week (e.g., `On(time.Monday, time.Friday)`).
*   `.OnThe(day int)`: Run on a specific day of the month. `OnThe(-1)` targets the last day of the month.
*   `.In(duration)`: Run just once after a duration from when the scheduler starts.

#### Schedule and Execution Modifiers

These methods refine or modify a scheduled job.

*   `.At("15:04[:05]", ...)`: Specifies the time of day for calendar-based schedules (`Daily`, `On`, `OnThe`).  Can be called with multiple time strings.
*   `.AtOffset(duration)`: Modifies an `Every` schedule to shift its start time relative to midnight.
*   `.WithTimeout(duration)`: Sets a per-run deadline. The job's context is cancelled if it runs for too long.
*   `.Except(func(time.Time) bool)`: Provides a custom function to skip a scheduled run. If the function returns `true`, the job is skipped.
*   `.Between(from, to int)`: A helper to skip runs outside of a given hour range (e.g., `Between(9, 17)` for business hours).
*   `.WithName(string)`: Assign a custom name for better logging.


## Design Philosophy

Sked's goal is a small, clear API for recurring tasks.
This is achieved by a static design: all jobs are defined once at startup and cannot be added or changed at runtime. This trade-off keeps the API minimal and predictable.

Sked also uses a synchronous execution model: jobs never overlap themselves. If a run takes longer than its interval, the next run is skipped rather than queued.

It also handles common edge cases safely — time changes, missed runs after sleep, and panics in jobs.

These choices make Sked simple to use and reliable for in-process scheduling. If you need dynamic job management or parallel execution, other packages are a better fit.

## Installation

```sh
go get github.com/levmv/sked
```