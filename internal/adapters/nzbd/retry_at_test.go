package nzbd

import (
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

// A history row names the indexer's retry time only as its Failure:RetryAt
// marker; the typed field, when present, wins.
func TestHistoryRetryAtMarkerIsRead(t *testing.T) {
	w := terminalWire{Outcome: ports.OutcomeFailed, Reason: ports.ReasonFetch, FetchCause: ports.FetchCauseIndexerUnavailable}
	tm, ok := terminalOf(w, [][2]string{{"Failure:RetryAt", "1795700000"}}, nil)
	if !ok || tm.RetryAt.Unix() != 1795700000 {
		t.Fatalf("retry_at = %v ok=%v, want the marker's time", tm.RetryAt, ok)
	}
	w.RetryAtUnix = 1795800000
	if tm, _ = terminalOf(w, [][2]string{{"Failure:RetryAt", "1795700000"}}, nil); tm.RetryAt.Unix() != 1795800000 {
		t.Fatalf("retry_at = %v, want the typed field", tm.RetryAt)
	}
	if tm, _ = terminalOf(terminalWire{Outcome: ports.OutcomeFailed, Reason: ports.ReasonFetch}, [][2]string{{"Failure:RetryAt", "junk"}}, nil); !tm.RetryAt.IsZero() {
		t.Fatalf("retry_at = %v, want none from an unreadable marker", tm.RetryAt)
	}
}
