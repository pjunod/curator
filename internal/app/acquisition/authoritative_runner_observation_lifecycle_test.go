package acquisition

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestAuthoritativeRunnerLookupFailurePoliciesPreserveRequestCustody(t *testing.T) {
	for _, tt := range []struct {
		name        string
		intent      string
		phase       string
		status      ports.DownloadState
		found       bool
		lookupErr   error
		wantHold    bool
		wantMessage string
	}{
		{name: "automatic uncertain receipt missing", phase: "uncertain", status: ports.StateFailed, wantHold: true, wantMessage: "exact automatic submission replay is queued"},
		{name: "automatic uncertain lookup outage", phase: "uncertain", status: ports.StateFailed, lookupErr: errors.New("Runner unavailable"), wantHold: true, wantMessage: "exact automatic submission replay is queued"},
		{name: "manual missing receipt", intent: GrabIntentManualAdditional, phase: "uncertain", status: ports.StateRemoved, wantHold: true, wantMessage: "waiting for authoritative Runner request admission receipt"},
		{name: "manual lookup outage", intent: GrabIntentManualAdditional, phase: "uncertain", status: ports.StateFailed, lookupErr: errors.New("Runner unavailable"), wantHold: true, wantMessage: "waiting for authoritative Runner outcome"},
		{name: "automatic terminal failure lookup outage", phase: "submitted", status: ports.StateFailed, lookupErr: errors.New("Runner unavailable"), wantHold: true, wantMessage: "waiting for authoritative Runner outcome"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			base := &fakeClient{}
			svc, db, dl, cfg := runnerAdmissionObservationFixture(t, base, tt.intent, tt.phase, false)
			client := &runnerAdmissionReplyClient{fakeClient: base, admission: ports.RequestAdmission{RequestToken: dl.Transfer}, found: tt.found, lookupErr: tt.lookupErr}
			svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
			status := ports.DownloadStatus{State: tt.status, Handle: ports.Handle(dl.Handle), Message: "native status"}
			got, hold := svc.authoritativeRunnerObservation(ctx, dl, cfg, status)
			current, err := db.GetDownload(ctx, dl.ID)
			if err != nil {
				t.Fatal(err)
			}
			if hold != tt.wantHold || got.State != status.State {
				t.Fatalf("lookup policy returned status=%+v hold=%v, want original state=%q hold=%v", got, hold, status.State, tt.wantHold)
			}
			if !strings.Contains(current.Error, tt.wantMessage) || current.State != dl.State || current.Handle != dl.Handle || current.Transfer != dl.Transfer {
				t.Fatalf("missing/error lookup changed request custody unexpectedly: current=%+v want message containing %q", current, tt.wantMessage)
			}
			if client.lookups != 1 || len(base.added) != 0 || len(base.removed) != 0 {
				t.Fatalf("uncertain receipt lookup caused unrelated client mutation: lookups=%d adds=%v removes=%v", client.lookups, base.added, base.removed)
			}
			item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
			if err != nil || len(item.Files) != 0 {
				t.Fatalf("missing/error receipt changed library files: %+v err=%v", item.Files, err)
			}
		})
	}
}

func TestAuthoritativeRunnerReceiptFencesProtocolTokenRunnerAndHandleMismatches(t *testing.T) {
	for _, tt := range []struct {
		name              string
		mutate            func(*ports.RequestAdmission)
		pinRunnerInstance string
		returnMismatched  bool
		wantReason        string
	}{
		{name: "wrong protocol version", mutate: func(a *ports.RequestAdmission) { a.ProtocolVersion = 2 }, wantReason: "identity mismatch"},
		{name: "wrong request token", mutate: func(a *ports.RequestAdmission) { a.RequestToken = "other-token" }, returnMismatched: true, wantReason: "identity mismatch"},
		{name: "wrong Runner installation", mutate: func(a *ports.RequestAdmission) { a.RunnerInstanceID = "replacement-runner" }, pinRunnerInstance: "expected-runner", wantReason: "installation or admission generation changed"},
		{name: "wrong exact job handle", mutate: func(a *ports.RequestAdmission) { a.JobID = "different-job" }, wantReason: "job handle conflicts"},
		{name: "receipt lacks exact handle", mutate: func(a *ports.RequestAdmission) { a.JobID = "" }, wantReason: "no exact job handle"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			base := &fakeClient{}
			svc, db, dl, cfg := runnerAdmissionObservationFixture(t, base, "", "submitted", false)
			admission := validRunnerAdmission(dl, "failed")
			tt.mutate(&admission)
			if tt.pinRunnerInstance != "" {
				if _, err := db.W.ExecContext(ctx, `INSERT INTO runner_client_instances(client_id,runner_instance_id,delivery_v1_enabled,updated_at) VALUES(?,?,0,?)`, dl.ClientID, tt.pinRunnerInstance, time.Now().UnixMilli()); err != nil {
					t.Fatal(err)
				}
			}
			client := &runnerAdmissionReplyClient{fakeClient: base, admission: admission, found: true, returnMismatched: tt.returnMismatched}
			svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
			status := ports.DownloadStatus{State: ports.StateFailed, Handle: ports.Handle(dl.Handle)}
			_, hold := svc.authoritativeRunnerObservation(ctx, dl, cfg, status)
			current, err := db.GetDownload(ctx, dl.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !hold || current.ParkedAt.UnixMilli() == 0 || !strings.Contains(current.ParkedReason, tt.wantReason) {
				t.Fatalf("identity conflict was not parked for review: hold=%v row=%+v want reason %q", hold, current, tt.wantReason)
			}
			if current.State != dl.State || current.Handle != dl.Handle || len(base.added) != 0 || len(base.removed) != 0 {
				t.Fatalf("mismatched receipt mutated transfer state/client: row=%+v adds=%v removes=%v", current, base.added, base.removed)
			}
			item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
			if err != nil || len(item.Files) != 0 {
				t.Fatalf("mismatched receipt mutated library: files=%+v err=%v", item.Files, err)
			}
		})
	}
}

func TestAuthoritativeRunnerCompletionUsesDeliveryCustodyOnlyWhenExpected(t *testing.T) {
	for _, deliveryExpected := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy completion remains native status", true: "delivery completion stays on manifest path"}[deliveryExpected], func(t *testing.T) {
			ctx := context.Background()
			base := &fakeClient{}
			svc, db, dl, cfg := runnerAdmissionObservationFixture(t, base, "", "submitted", deliveryExpected)
			admission := validRunnerAdmission(dl, "completed")
			admission.PublicationPath = "/runner/completed/known-release"
			client := &runnerAdmissionReplyClient{fakeClient: base, admission: admission, found: true}
			svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
			status := ports.DownloadStatus{State: ports.StateFailed, Handle: ports.Handle(dl.Handle), Message: "generic native result"}
			got, hold := svc.authoritativeRunnerObservation(ctx, dl, cfg, status)
			current, err := db.GetDownload(ctx, dl.ID)
			if err != nil {
				t.Fatal(err)
			}
			if deliveryExpected {
				if !hold || got.State != ports.StateFailed || current.State != dl.State || current.ImportPath != "" || current.SavePath != "" {
					t.Fatalf("delivery-backed completion escaped native import fence: status=%+v hold=%v row=%+v", got, hold, current)
				}
			} else if hold || got.State != ports.StateCompleted || got.SavePath != admission.PublicationPath || got.Handle != ports.Handle(admission.JobID) || got.Transfer != dl.Transfer {
				t.Fatalf("native completion did not reconcile from exact receipt: status=%+v hold=%v admission=%+v", got, hold, admission)
			}
			if client.lookups != 1 || len(base.added) != 0 || len(base.removed) != 0 {
				t.Fatalf("completion reconciliation issued unrelated client actions: lookups=%d add=%v remove=%v", client.lookups, base.added, base.removed)
			}
			item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
			if err != nil || len(item.Files) != 0 {
				t.Fatalf("receipt observation bypassed the appropriate import pipeline: files=%+v err=%v", item.Files, err)
			}
		})
	}
}

func TestAuthoritativeRunnerDeletedOutcomeWithUnresolvedCustodyStaysReviewable(t *testing.T) {
	ctx := context.Background()
	base := &fakeClient{}
	svc, db, dl, cfg := runnerAdmissionObservationFixture(t, base, GrabIntentManualAdditional, "uncertain", false)
	admission := validRunnerAdmission(dl, "deleted")
	admission.CustodyState = "runner_owned"
	admission.SourceCleanupStatus = "pending"
	client := &runnerAdmissionReplyClient{fakeClient: base, admission: admission, found: true}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	status := ports.DownloadStatus{State: ports.StateRemoved, Handle: ports.Handle(dl.Handle)}
	_, hold := svc.authoritativeRunnerObservation(ctx, dl, cfg, status)
	current, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hold || current.ParkedAt.UnixMilli() == 0 || !strings.Contains(current.ParkedReason, "source custody is unresolved") {
		t.Fatalf("deleted queue row with Runner-owned bytes was not held for review: hold=%v row=%+v", hold, current)
	}
	if current.State != dl.State || current.Handle != dl.Handle || len(base.added) != 0 || len(base.removed) != 0 {
		t.Fatalf("unresolved deleted custody triggered a replacement or cleanup: row=%+v adds=%v removes=%v", current, base.added, base.removed)
	}
	item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil || len(item.Files) != 0 {
		t.Fatalf("unresolved deleted custody mutated library: files=%+v err=%v", item.Files, err)
	}
}

type inFlightAdmissionClient struct {
	*runnerAdmissionReplyClient
	afterLookup func()
}

func (c *inFlightAdmissionClient) GetRequestAdmission(ctx context.Context, token string) (ports.RequestAdmission, bool, error) {
	admission, found, err := c.runnerAdmissionReplyClient.GetRequestAdmission(ctx, token)
	if c.afterLookup != nil {
		c.afterLookup()
	}
	return admission, found, err
}

func TestAuthoritativeRunnerReceiptCannotMutateReusedDownloadGenerationDuringLookup(t *testing.T) {
	ctx := context.Background()
	base := &fakeClient{}
	svc, db, old, cfg := runnerAdmissionObservationFixture(t, base, "", "submitted", false)
	oldAdmission := validRunnerAdmission(old, "failed")
	requestClient := &runnerAdmissionReplyClient{fakeClient: base, admission: oldAdmission, found: true}
	client := &inFlightAdmissionClient{runnerAdmissionReplyClient: requestClient}
	var replacement sqlite.Download
	client.afterLookup = func() {
		if _, err := db.W.ExecContext(ctx, `DELETE FROM downloads WHERE id=? AND generation_key=?`, old.ID, old.GenerationKey); err != nil {
			t.Errorf("delete prior request generation during receipt lookup: %v", err)
			return
		}
		var err error
		replacement, err = db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: old.MediaItemID, ClientID: old.ClientID,
			ReleaseTitle: "replacement request generation", Protocol: old.Protocol, State: "downloading", Transfer: "replacement-transfer", Handle: "replacement-job", RequestTokenIdempotent: true})
		if err != nil {
			t.Errorf("insert replacement generation during receipt lookup: %v", err)
		}
	}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	status := ports.DownloadStatus{State: ports.StateFailed, Handle: ports.Handle(old.Handle)}
	_, hold := svc.authoritativeRunnerObservation(ctx, old, cfg, status)
	if !hold || replacement.ID != old.ID || replacement.GenerationKey == old.GenerationKey {
		t.Fatalf("fixture did not create a reused Activity ID with a changed generation: hold=%v old=%+v replacement=%+v", hold, old, replacement)
	}
	current, err := db.GetDownload(ctx, replacement.ID)
	if err != nil || current.GenerationKey != replacement.GenerationKey || current.Transfer != "replacement-transfer" || current.Handle != "replacement-job" || current.State != "downloading" || current.ParkedAt.UnixMilli() != 0 {
		t.Fatalf("stale Runner receipt changed replacement request generation: row=%+v err=%v", current, err)
	}
	if len(base.added) != 0 || len(base.removed) != 0 || client.lookups != 1 {
		t.Fatalf("stale receipt caused unrelated client action: lookups=%d adds=%v removes=%v", client.lookups, base.added, base.removed)
	}
	item, err := db.GetMediaItemFull(ctx, old.MediaItemID)
	if err != nil || len(item.Files) != 0 {
		t.Fatalf("stale receipt changed replacement library: files=%+v err=%v", item.Files, err)
	}
}

type runnerAdmissionReplyClient struct {
	*fakeClient
	admission        ports.RequestAdmission
	found            bool
	lookupErr        error
	lookups          int
	tokens           []string
	returnMismatched bool
}

func (c *runnerAdmissionReplyClient) GetRequestAdmission(_ context.Context, token string) (ports.RequestAdmission, bool, error) {
	c.lookups++
	c.tokens = append(c.tokens, token)
	if c.lookupErr != nil {
		return ports.RequestAdmission{}, false, c.lookupErr
	}
	if token != c.admission.RequestToken && !c.returnMismatched {
		return ports.RequestAdmission{}, false, nil
	}
	return c.admission, c.found, nil
}

func runnerAdmissionObservationFixture(t *testing.T, base *fakeClient, intent, phase string, deliveryExpected bool) (*Service, *sqlite.DB, sqlite.Download, ports.ClientConfig) {
	t.Helper()
	svc, db, itemID := setup(t, nil, base)
	ctx := context.Background()
	cfg, err := db.GetDownloadClient(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, ClientID: cfg.ID,
		ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL.receipt", Protocol: "torrent", State: "downloading",
		Handle: "job-expected", Transfer: "token-expected", Intent: intent,
		RequestTokenIdempotent: true, DeliveryExpected: deliveryExpected})
	if err != nil {
		t.Fatal(err)
	}
	if phase != "" {
		if err := db.SetSubmissionPhase(ctx, dl.ID, phase); err != nil {
			t.Fatal(err)
		}
		dl.SubmissionPhase = phase
	}
	return svc, db, dl, cfg
}

func validRunnerAdmission(dl sqlite.Download, outcome string) ports.RequestAdmission {
	return ports.RequestAdmission{ProtocolVersion: 1, RequestToken: dl.Transfer, JobID: dl.Handle,
		Generation: "runner-admission-1", RunnerInstanceID: "runner-1", Outcome: outcome,
		TerminalOutcome: "FAILURE/FETCH", CustodyState: "no_payload"}
}
