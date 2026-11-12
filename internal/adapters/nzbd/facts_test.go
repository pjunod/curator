package nzbd

import (
	"context"
	"net/http"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

// Pipeline contract v2 adapter facts (review #4 and #13).

func oneFact(t *testing.T, name, data string) ports.ClientEvent {
	t.Helper()
	evs := translate(name, data, "41")
	if len(evs) != 1 || evs[0].Fact == nil {
		t.Fatalf("%s translated to %+v, want one event with a fact", name, evs)
	}
	return evs[0]
}

// job_status_changed{held} without a control object is held (contract
// section 2.4 makes control optional): OperationalState carries it.
func TestStatusChangedHeldWithoutControlIsHeld(t *testing.T) {
	ev := oneFact(t, "job_status_changed", `{"job":7,"transfer":"t-7","status":"held"}`)
	f := ev.Fact
	if f.Kind != ports.FactStatus || f.Transfer != "t-7" || f.Status == nil || f.Status.OperationalState != "held" {
		t.Fatalf("fact = %+v status %+v, want a held status with its token", f, f.Status)
	}
	ev = oneFact(t, "job_status_changed", `{"job":7,"transfer":"t-7","status":"fetching","stage":"unpack"}`)
	if st := ev.Fact.Status; st.OperationalState != "" || st.Stage != "unpack" || st.State != ports.StateQueued {
		t.Fatalf("fetching = %+v, want not held, carrying its stage", st)
	}
}

func TestControlChangedHeldIsHeld(t *testing.T) {
	ev := oneFact(t, "job_control_changed", `{"job":8,"control":{"version":1,"lifecycle":"held","message":"waiting for disk"}}`)
	if st := ev.Fact.Status; st.OperationalState != "held" || st.Message != "waiting for disk" || ev.Fact.Transfer != "" {
		t.Fatalf("control hold = %+v transfer %q, want held, no token (the reducer vouches by the queue)", st, ev.Fact.Transfer)
	}
}

func TestStatusesMarksHeldJobs(t *testing.T) {
	srv := fake(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/jobs" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":3,"name":"x","status":"downloading","status_projected":"held","size_bytes":10,"downloaded_bytes":5,"params":[["monarr-transfer","t-3"]]}]}`))
		return true
	})
	got, err := client(t, srv, "", "tok").Statuses(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("Statuses = %+v, %v", got, err)
	}
	if got[0].OperationalState != "held" || got[0].Transfer != "t-3" {
		t.Fatalf("held queue row = %+v, want OperationalState held", got[0])
	}
}

// Runner's per-job rate and remaining bytes are relayed as sent; an older
// Runner that sends neither leaves them unset rather than zero.
func TestStatusesRelayRunnersRateAndRemainingBytes(t *testing.T) {
	srv := fake(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/jobs" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":3,"name":"x","status":"downloading","size_bytes":100,"downloaded_bytes":40,"rate_bps":48200000,"remaining_bytes":60},` +
			`{"id":4,"name":"y","status":"downloading","size_bytes":100,"downloaded_bytes":40}]}`))
		return true
	})
	got, err := client(t, srv, "", "tok").Statuses(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("Statuses = %+v, %v", got, err)
	}
	if got[0].RateBps == nil || *got[0].RateBps != 48200000 || got[0].RemainingBytes == nil || *got[0].RemainingBytes != 60 {
		t.Fatalf("rate/remaining = %v/%v, want 48200000/60", got[0].RateBps, got[0].RemainingBytes)
	}
	if got[1].RateBps != nil || got[1].RemainingBytes != nil {
		t.Fatalf("older Runner row = %+v, want rate and remaining unset", got[1])
	}
}

func TestPublicationReadyIsASalvage(t *testing.T) {
	ev := oneFact(t, "job_publication_ready", `{"job":9,"transfer":"t-9","publication":{"path":"/completed/x.runner-g","generation":"g","manifest_digest":"d","files":2}}`)
	f := ev.Fact
	if f.Kind != ports.FactPublicationReady || f.Transfer != "t-9" || f.Publication == nil || f.Publication.Kind != "salvage" ||
		f.Publication.Path != "/completed/x.runner-g" || f.Publication.Generation != "g" || f.Publication.Files != 2 {
		t.Fatalf("publication_ready = %+v pub %+v", f, f.Publication)
	}
	if evs := translate("job_publication_ready", `{"job":9,"transfer":"t-9"}`, "42"); len(evs) != 0 {
		t.Fatalf("a publication_ready without a path made %+v", evs)
	}
}

// A typed job_pp_finished (Runner M1a) is taken as Runner states it, not
// re-derived from pp_status.
func TestTypedPPFinishedIsTakenAsStated(t *testing.T) {
	f, ok := ppFinishedFact([]byte(`{"job":5,"transfer":"t-5","outcome":"failed","reason":"fetch","fetch_cause":"indexer_unavailable",
		"blames_release":false,"salvage":"none","message":"indexer 503","fetched":{"bytes":100},"pp_status":"FAILURE/FETCH","history_seq":77,"history_incarnation":"inc"}`), 12)
	if !ok || f.Kind != ports.FactTerminal || f.Terminal == nil {
		t.Fatalf("pp_finished = %+v ok=%v", f, ok)
	}
	tm := f.Terminal
	if tm.Outcome != ports.OutcomeFailed || tm.Reason != ports.ReasonFetch || tm.FetchCause != ports.FetchCauseIndexerUnavailable || tm.BlamesRelease {
		t.Fatalf("terminal = %+v, want failed/fetch{indexer_unavailable} not blaming the release", tm)
	}
	if f.Transfer != "t-5" || f.HistorySeq != 77 || f.HistoryIncarnation != "inc" || tm.Fetched.Bytes != 100 {
		t.Fatalf("fact = %+v fetched %+v", f, tm.Fetched)
	}
	// A typed completed carries its publication.
	f, ok = ppFinishedFact([]byte(`{"job":6,"transfer":"t-6","outcome":"completed","salvage":"none","publication":{"path":"/completed/y","generation":"gy","files":1}}`), 13)
	if !ok || f.Terminal.Outcome != ports.OutcomeCompleted || f.Terminal.Publication == nil || f.Terminal.Publication.Path != "/completed/y" {
		t.Fatalf("completed = %+v ok=%v", f.Terminal, ok)
	}
}

// A deliveries row without a kind (a pre-M1a Runner) is a salvage when its
// terminal was not a success, a completed terminal otherwise.
func TestDeliveryKindInference(t *testing.T) {
	cases := []struct {
		kind, outcome string
		want          ports.FactKind
	}{
		{"", "SUCCESS", ports.FactTerminal},
		{"", "", ports.FactTerminal},
		{"", "FAILURE/HEALTH", ports.FactPublicationReady},
		{"salvage", "SUCCESS", ports.FactPublicationReady},
		{"completed", "FAILURE/HEALTH", ports.FactTerminal},
	}
	for _, c := range cases {
		f := deliveryFact(ports.RunnerDelivery{Kind: c.kind, TerminalOutcome: c.outcome, RequestToken: "t-1", JobID: "1",
			PublicationPath: "/completed/p", PublicationGeneration: "g", Sequence: 3})
		if f.Kind != c.want || f.Transfer != "t-1" || f.Publication == nil || f.Publication.Manifest == nil {
			t.Errorf("kind %q outcome %q = %s (%+v), want %s", c.kind, c.outcome, f.Kind, f, c.want)
		}
		if f.Kind == ports.FactTerminal && (f.Terminal == nil || f.Terminal.Outcome != ports.OutcomeCompleted) {
			t.Errorf("kind %q outcome %q terminal = %+v", c.kind, c.outcome, f.Terminal)
		}
	}
}

// Continuity is lost when the incarnation changes, the cursor fell below
// what Runner retains, or the cursor is above Runner's head: no facts, and
// the next cursor is Runner's head.
func TestHistorySinceContinuityLost(t *testing.T) {
	srv := fake(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/history" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"incarnation":"inc-2","head_seq":50,"retained_min_seq":20,"entries":[{"job":1,"seq":49,"status":"SUCCESS","final_dir":"/c/a"}]}`))
		return true
	})
	c := client(t, srv, "", "tok")
	for name, cur := range map[string]ports.HistoryCursor{
		"incarnation changed": {Incarnation: "inc-1", SinceSeq: 30},
		"pruned gap":          {Incarnation: "inc-2", SinceSeq: 5},
		"above head":          {Incarnation: "inc-2", SinceSeq: 70},
	} {
		facts, next, lost, err := c.HistorySince(context.Background(), cur, 100)
		if err != nil || !lost || len(facts) != 0 || next.SinceSeq != 50 || next.Incarnation != "inc-2" {
			t.Errorf("%s: facts=%d next=%+v lost=%v err=%v, want lost with the cursor at head", name, len(facts), next, lost, err)
		}
	}
	facts, next, lost, err := c.HistorySince(context.Background(), ports.HistoryCursor{Incarnation: "inc-2", SinceSeq: 30}, 100)
	if err != nil || lost || len(facts) != 1 || next.SinceSeq != 49 || facts[0].HistoryIncarnation != "inc-2" {
		t.Fatalf("continuous read: facts=%+v next=%+v lost=%v err=%v", facts, next, lost, err)
	}
}
