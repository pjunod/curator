package backoff

import (
	"testing"
	"time"
)

// The reference functions below are the private ladders this helper replaced,
// copied verbatim. The table proves every caller's schedule is unchanged.

func oldSubmission(attempts int) time.Duration { // sqlite ScheduleRunnerSubmissionRetry / ScheduleManualAdmissionRetry
	backoff := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	index := attempts
	if index >= len(backoff) {
		index = len(backoff) - 1
	}
	return backoff[index]
}

func oldImport(attempts int) time.Duration { // acquisition importRetryDelay
	delays := [...]time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempts]
}

func oldOutbox(attempts int64) time.Duration { // notify backoffAfter
	schedule := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
	i := int(attempts) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[i]
}

func TestLaddersMatchTheSchedulesTheyReplaced(t *testing.T) {
	for attempts := 0; attempts <= 40; attempts++ {
		if got, want := Retry.After(attempts), oldSubmission(attempts); got != want {
			t.Errorf("submission attempts=%d: %v, was %v", attempts, got, want)
		}
		if got, want := Retry.After(attempts), oldImport(attempts); got != want {
			t.Errorf("import attempts=%d: %v, was %v", attempts, got, want)
		}
		// The outbox counts tries made (1 after the first), the others retries spent.
		if got, want := Outbox.After(attempts-1), oldOutbox(int64(attempts)); got != want {
			t.Errorf("outbox attempts=%d: %v, was %v", attempts, got, want)
		}
	}
}

func TestAfterLiteralSequences(t *testing.T) {
	s, m := time.Second, time.Minute
	for name, tc := range map[string]struct {
		ladder Ladder
		want   []time.Duration
	}{
		"retry":  {Retry, []time.Duration{30 * s, m, 2 * m, 5 * m, 5 * m, 5 * m}},
		"outbox": {Outbox, []time.Duration{5 * s, 30 * s, 2 * m, 5 * m, 15 * m, time.Hour, time.Hour, time.Hour}},
	} {
		for i, want := range tc.want {
			if got := tc.ladder.After(i); got != want {
				t.Errorf("%s rung %d = %v, want %v", name, i, got, want)
			}
		}
	}
	if got := Retry.After(-3); got != 30*s {
		t.Errorf("negative tries = %v, want the first rung", got)
	}
	if got := (Ladder{}).After(2); got != 0 {
		t.Errorf("empty ladder = %v, want 0", got)
	}
}
