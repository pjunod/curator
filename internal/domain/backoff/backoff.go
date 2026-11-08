// Package backoff is the one retry ladder helper behind every scheduler that
// repeats work on a fixed schedule: runner submission, manual admission, the
// blocked-import retry, and the notification outbox. A ladder is a list of
// waits; the last rung repeats for as long as the work stays retryable, so a
// schedule never ends by itself.
//
// It lives in the domain layer, not in internal/app/pipeline, because
// internal/infra/sqlite computes the submission and admission deadlines inside
// the same statement that reads the attempt counter, and pipeline imports
// sqlite.
package backoff

import "time"

// Ladder is an ordered list of waits.
type Ladder []time.Duration

// Retry is the ladder for work that repeats against a peer which may be
// briefly unavailable: runner submission, manual admission, and the import
// retry of a row that stopped on the machine.
var Retry = Ladder{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

// Outbox is the notification outbox ladder. A missed Cinema scan is a file
// that never appears, so its last rung is an hour and it repeats.
var Outbox = Ladder{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
}

// After is the wait following `tries` earlier tries: tries 0 is the first rung,
// and any count past the end (or below zero) clamps to the last (or first).
func (l Ladder) After(tries int) time.Duration {
	if len(l) == 0 {
		return 0
	}
	if tries < 0 {
		tries = 0
	}
	if tries >= len(l) {
		tries = len(l) - 1
	}
	return l[tries]
}

// Max is the last rung: the longest wait the ladder ever produces. Zero for an
// empty ladder, matching After.
func (l Ladder) Max() time.Duration {
	if len(l) == 0 {
		return 0
	}
	return l[len(l)-1]
}
