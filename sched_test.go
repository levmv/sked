package sked

import (
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
