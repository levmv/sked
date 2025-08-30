package sked

import (
	"context"
	"testing"
	"time"
)

func mustTime(t *testing.T, layout, value string, loc *time.Location) time.Time {
	t.Helper()
	parsedTime, err := time.ParseInLocation(layout, value, loc)
	if err != nil {
		t.Fatalf("Failed to parse time: %v", err)
	}
	return parsedTime
}

func TestDailySchedule_IsDSTAware(t *testing.T) {
	// We use a timezone with a 30-minute DST shift (because - why not?)
	loc, err := time.LoadLocation("Australia/Lord_Howe")
	if err != nil {
		t.Fatal(err)
	}

	schedule := &DailySchedule{
		atTimes: atTimes{tods: []time.Duration{9 * time.Hour}},
		N:       1,
	}

	// Lord Howe DST "springs forward" on the first Sunday of October.
	// On Oct 6, 2024, 2:00 AM becomes 2:30 AM.
	// We check that a 9:00 AM job schedules correctly across this boundary.
	after := time.Date(2024, 10, 5, 10, 0, 0, 0, loc)   // 10:00 LHST (+10:30)
	expected := time.Date(2024, 10, 6, 9, 0, 0, 0, loc) // 09:00 LHDT (+11:00)

	next, ok := schedule.Next(after, loc)
	if !ok || !next.Equal(expected) {
		t.Errorf("schedule is not DST-aware:\n got: %v (offset %s)\nwant: %v (offset %s)",
			next, next.Format("-0700"), expected, expected.Format("-0700"))
	}
}

func TestMonthlySchedule_Next_EdgeCases(t *testing.T) {
	loc := time.UTC
	layout := "2006-01-02 15:04:05"

	testCases := []struct {
		name     string
		schedule Schedule
		after    time.Time
		expected time.Time
	}{
		{
			name:     "last day of leap year February",
			schedule: &MonthlySchedule{DayOfMonth: -1, atTimes: atTimes{tods: []time.Duration{10 * time.Hour}}},
			after:    mustTime(t, layout, "2024-02-15 11:00:00", loc),
			expected: mustTime(t, layout, "2024-02-29 10:00:00", loc),
		},
		{
			name:     "day 31 in a month with 30 days (should be skipped)",
			schedule: &MonthlySchedule{DayOfMonth: 31, atTimes: atTimes{tods: []time.Duration{10 * time.Hour}}},
			after:    mustTime(t, layout, "2024-04-15 11:00:00", loc), // April has 30 days
			expected: mustTime(t, layout, "2024-05-31 10:00:00", loc), // Next run is in May
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			next, ok := tc.schedule.Next(tc.after, loc)
			if !ok || !next.Equal(tc.expected) {
				t.Errorf("mismatch:\n got: %v\nwant: %v", next, tc.expected)
			}
		})
	}
}

func TestScheduler_At_TimeParsing(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		times       []string
		expectError bool
	}{
		// --- Valid Cases ---
		{name: "HH:MM", times: []string{"9:30"}, expectError: false},
		{name: "Zero-padded HH:MM", times: []string{"09:30"}, expectError: false},
		{name: "HH:MM:SS", times: []string{"9:30:00"}, expectError: false},
		{name: "With seconds", times: []string{"09:30:45"}, expectError: false},
		{name: "Midnight", times: []string{"0:00"}, expectError: false},
		{name: "End of day", times: []string{"23:59"}, expectError: false},
		{name: "End of day with seconds", times: []string{"23:59:59"}, expectError: false},
		{name: "Multiple valid times", times: []string{"09:00", "12:30", "18:45"}, expectError: false},
		{name: "Duplicate valid times", times: []string{"09:00", "09:00", "12:30"}, expectError: false},

		// --- Invalid Cases ---
		{name: "Invalid hour", times: []string{"25:00"}, expectError: true},
		{name: "Invalid minute", times: []string{"9:60"}, expectError: true},
		{name: "Invalid second", times: []string{"9:30:60"}, expectError: true},
		{name: "Too many components", times: []string{"9:30:45:67"}, expectError: true},
		{name: "Non-numeric", times: []string{"abc"}, expectError: true},
		{name: "Empty string", times: []string{""}, expectError: true},
		{name: "Missing minutes", times: []string{"9"}, expectError: true},
		{name: "Trailing colon", times: []string{"9:"}, expectError: true},
		{name: "Missing hour", times: []string{":30"}, expectError: true},
		{name: "One invalid among many valid", times: []string{"09:00", "bogus", "18:45"}, expectError: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheduler, cancel := newTestScheduler(t)
			defer cancel()

			job := scheduler.Schedule(func(ctx context.Context) {}).Daily().At(tc.times...)

			err := job.err

			if tc.expectError && err == nil {
				t.Error("expected a configuration error, but got nil")
			}
			if !tc.expectError && err != nil {
				t.Errorf("did not expect a configuration error, but got: %v", err)
			}
		})
	}
}
