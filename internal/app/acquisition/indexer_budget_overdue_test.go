package acquisition

import (
	"errors"
	"testing"
	"time"
)

func TestRSSPacingDelayRetriesOneOverdueDeadlineWithoutSpinning(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	const idleSweep = 15 * time.Minute
	const overdueAttempt = 100 * time.Millisecond
	tests := []struct {
		name             string
		due              int64
		queryErr         error
		lastAttemptedDue int64
		want             time.Duration
	}{
		{name: "no persisted schedule uses idle sweep", want: idleSweep},
		{name: "database read failure uses idle sweep", queryErr: errors.New("database unavailable"), want: idleSweep},
		{name: "future deadline sleeps only until due", due: now.Add(4 * time.Second).UnixMilli(), want: 4 * time.Second},
		{name: "new overdue deadline gets prompt attempt", due: now.Add(-time.Second).UnixMilli(), want: overdueAttempt},
		{name: "unchanged overdue deadline cannot spin", due: now.Add(-time.Second).UnixMilli(), lastAttemptedDue: now.Add(-time.Second).UnixMilli(), want: idleSweep},
		{name: "new overdue indexer deadline is not hidden", due: now.Add(-2 * time.Second).UnixMilli(), lastAttemptedDue: now.Add(-time.Second).UnixMilli(), want: overdueAttempt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rssPacingDelay(tt.due, tt.queryErr, tt.lastAttemptedDue, now); got != tt.want {
				t.Fatalf("rssPacingDelay() = %s, want %s", got, tt.want)
			}
		})
	}
}
