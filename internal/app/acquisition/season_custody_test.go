package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain/acquisitionplan"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func admittedFixture(t *testing.T, s *Service, db *sqlite.DB, item int64, candidates ...string) (int64, []sqlite.Download) {
	t.Helper()
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatal(err)
	}
	eps := make([]int64, 2)
	eps[0], _ = db.GetEpisodeID(ctx, item, 1, 1)
	eps[1], _ = db.GetEpisodeID(ctx, item, 1, 2)
	facts := seasonDecision{Version: 1, ImportAllowlists: map[acquisitionplan.CandidateKey][]int64{}, Plan: acquisitionplan.Plan{Providers: map[acquisitionplan.EpisodeKey]acquisitionplan.CandidateKey{}}}
	var intents []sqlite.Download
	for i, key := range candidates {
		ep := eps[i]
		k := acquisitionplan.CandidateKey(key)
		facts.ImportAllowlists[k] = []int64{ep}
		facts.Plan.Providers[acquisitionplan.EpisodeKey(ep)] = k
		facts.Candidates = append(facts.Candidates, acquisitionplan.Candidate{Key: k, Pack: key == "pack", Payload: []acquisitionplan.EpisodeKey{acquisitionplan.EpisodeKey(eps[0]), acquisitionplan.EpisodeKey(eps[1])}})
		payload, _ := json.Marshal(plannedExecution{URL: key, Category: clients[0].Category, ClientFingerprint: clientFingerprint(clients[0])})
		title := fmt.Sprintf("Test.Show.S01E%02d.1080p.WEB-DL", i+1)
		if key == "pack" {
			title = "Test.Show.S01.1080p.WEB-DL"
		}
		intents = append(intents, sqlite.Download{MediaItemID: item, Season: 1, ClientID: clients[0].ID, Protocol: "torrent", ReleaseTitle: title, CandidateKey: key, WantableIDs: []string{fmt.Sprintf("episode:%d:1:%d", item, i+1)}, ReservedEpisodes: []int64{ep}, ExecutionPayload: string(payload)})
	}
	raw, _ := json.Marshal(facts)
	rev, _ := db.AcquisitionRevision(ctx)
	id, err := db.AdmitAcquisitionPlan(ctx, sqlite.AcquisitionPlan{MediaItemID: item, Season: 1, Decision: raw, SnapshotFingerprint: "test"}, rev, intents)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.PlanDownloads(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return id, rows
}
func TestPendingRestartDispatchIsOnceAndCancelledPlanDoesNotReopen(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	id, rows := admittedFixture(t, s, db, item, "single")
	if err := s.RecoverSeasonSubmissions(ctx); err != nil {
		t.Fatal(err)
	}
	s.reconcileSeasonPlans(ctx)
	s.reconcileSeasonPlans(ctx)
	if len(client.added) != 1 {
		t.Fatalf("%v", client.added)
	}
	dl, _ := db.GetDownload(ctx, rows[0].ID)
	if dl.SubmissionPhase != "submitted" || dl.Handle == "" {
		t.Fatal("dispatch not committed")
	}
	if err := s.RemoveDownload(ctx, dl.ID, false); err != nil {
		t.Fatal(err)
	}
	s.reconcileSeasonPlans(ctx)
	p, _ := db.GetAcquisitionPlan(ctx, id)
	if p.State != "cancelled" {
		t.Fatalf("%+v", p)
	}
	if err := s.dispatchPlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	p, _ = db.GetAcquisitionPlan(ctx, id)
	if p.State != "cancelled" || len(client.added) != 1 {
		t.Fatal("cancelled plan reopened")
	}
	var n int
	_ = db.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind=?`, SeasonSearchJobKind).Scan(&n)
	if n != 0 {
		t.Fatal("cancellation enqueued automatic replacement")
	}
}
func TestUncertainEvidenceResetsForOutageConfigurationAndBoundedInventory(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	_, rows := admittedFixture(t, s, db, item, "single")
	dl := rows[0]
	_, _ = db.ClaimPlannedSubmission(ctx, dl.ID)
	_ = db.SetSubmissionPhase(ctx, dl.ID, "uncertain")
	dl, _ = db.GetDownload(ctx, dl.ID)
	cfg, _ := db.GetDownloadClient(ctx, dl.ClientID)
	for _, tt := range []struct {
		name     string
		complete bool
		change   bool
		statuses []ports.DownloadStatus
	}{{"outage", false, false, nil}, {"configuration", true, true, nil}, {"ambiguous", true, false, []ports.DownloadStatus{{Name: dl.ReleaseTitle, Handle: "a"}, {Name: dl.ReleaseTitle, Handle: "b"}}}} {
		t.Run(tt.name, func(t *testing.T) {
			ob := executionObservation{Config: clientFingerprint(cfg), LastInventory: time.Now(), AbsenceSince: time.Now().Add(-8 * 24 * time.Hour), Inventories: 100}
			_ = db.UpdateDownloadObservation(ctx, dl.ID, ob)
			changed := cfg
			if tt.change {
				changed.Category += "changed"
			}
			s.observeUncertain(ctx, dl, changed, tt.statuses, tt.complete)
			fresh, _ := db.GetDownload(ctx, dl.ID)
			if fresh.Superseded {
				t.Fatal("absence reset released custody")
			}
			var got executionObservation
			_ = json.Unmarshal([]byte(fresh.Observation), &got)
			if !got.AbsenceSince.IsZero() && time.Since(got.AbsenceSince) > time.Minute {
				t.Fatal("old absence evidence survived")
			}
		})
	}
}
func TestFallbackRequiresFailedSingleAndExtendsExactCustody(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	_, rows := admittedFixture(t, s, db, item, "pack", "single")
	pack, single := rows[0], rows[1]
	allowed, err := s.effectiveImportAllowlist(ctx, pack)
	if err != nil || len(allowed) != 1 {
		t.Fatalf("%v %v", allowed, err)
	}
	_, _ = db.ClaimPlannedSubmission(ctx, single.ID)
	_ = db.SetSubmissionPhase(ctx, single.ID, "submitted")
	_ = db.UpdateDownloadState(ctx, single.ID, "failed", 0, "client failure")
	allowed, err = s.effectiveImportAllowlist(ctx, pack)
	if err != nil || len(allowed) != 2 {
		t.Fatalf("%v %v", allowed, err)
	}
	fresh, _ := db.GetDownload(ctx, pack.ID)
	if len(fresh.WantableIDs) != 2 || len(fresh.ReservedEpisodes) != 2 {
		t.Fatal("fallback lacks durable exact reservation")
	}
	_ = db.ParkDownload(ctx, single.ID, "operator cancellation; test")
	allowed, err = s.effectiveImportAllowlist(ctx, pack)
	if err != nil || len(allowed) != 1 {
		t.Fatal("cancelled single authorized fallback")
	}
}
func TestScopeSettlementKeepsOnlyAffectedEpisodes(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id, rows := admittedFixture(t, s, db, item, "a", "b")
	_, _ = db.ClaimPlannedSubmission(ctx, rows[0].ID)
	_ = db.SetSubmissionPhase(ctx, rows[0].ID, "uncertain")
	p, _ := db.GetAcquisitionPlan(ctx, id)
	if err := s.settleSeasonPlan(ctx, p, rows, "settled_with_reservations"); err != nil {
		t.Fatal(err)
	}
	reservations, _ := db.ListDownloadReservations(ctx)
	if len(reservations) != 1 || reservations[0].ID != rows[0].ID {
		t.Fatalf("%+v", reservations)
	}
	fresh, _ := db.GetDownload(ctx, rows[1].ID)
	if !fresh.Superseded || fresh.SubmissionPhase != "rejected" {
		t.Fatal("pending broad claim leaked")
	}
}
func TestTorrentRetirementNeedsOperationalObservationAndConfirmedRemoval(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	_, rows := admittedFixture(t, s, db, item, "single")
	dl := rows[0]
	_, _ = db.ClaimPlannedSubmission(ctx, dl.ID)
	_ = db.SetSubmissionPhase(ctx, dl.ID, "submitted")
	_ = db.SetDownloadHandle(ctx, dl.ID, "h1", dl.Transfer)
	dl, _ = db.GetDownload(ctx, dl.ID)
	cfg, _ := db.GetDownloadClient(ctx, dl.ClientID)
	bytes := int64(100)
	zero := 0
	avail := 0.5
	st := ports.DownloadStatus{Handle: "h1", Name: dl.ReleaseTitle, State: ports.StateDownloading, OperationalState: "stalledDL", BytesCompleted: &bytes, ConnectedSeeds: &zero, Availability: &avail}
	ob := executionObservation{BytesKnown: true, LastBytes: 100, ShortStall: 48 * time.Hour, LongStall: 48 * time.Hour, LastStallObservation: time.Now().Add(-time.Minute)}
	_ = db.UpdateDownloadObservation(ctx, dl.ID, ob)
	s.resetStallObservation(ctx, dl)
	fresh, _ := db.GetDownload(ctx, dl.ID)
	var got executionObservation
	_ = json.Unmarshal([]byte(fresh.Observation), &got)
	if !got.LastStallObservation.IsZero() {
		t.Fatal("outage counted")
	}
	client.statuses = []ports.DownloadStatus{st}
	client.removeErr = errors.New("timeout")
	_ = db.UpdateDownloadObservation(ctx, dl.ID, ob)
	dl, _ = db.GetDownload(ctx, dl.ID)
	s.observeTorrentStall(ctx, dl, cfg, st)
	fresh, _ = db.GetDownload(ctx, dl.ID)
	if fresh.State == "failed" || !strings.Contains(fresh.ParkedReason, "uncertain") {
		t.Fatal("uncertain removal charged failure")
	}
	client.removeErr = nil
	client.statuses = nil
	_ = db.UpdateDownloadObservation(ctx, dl.ID, ob)
	dl, _ = db.GetDownload(ctx, dl.ID)
	paused := st
	paused.OperationalState = "pausedDL"
	s.observeTorrentStall(ctx, dl, cfg, paused)
	if len(client.removed) != 1 {
		t.Fatal("paused torrent retired")
	}
}

func TestDeliberateReservationResolutionRetainsWarningAndCannotPublish(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	_, rows := admittedFixture(t, s, db, item, "single")
	_, _ = db.ClaimPlannedSubmission(ctx, rows[0].ID)
	_ = db.SetSubmissionPhase(ctx, rows[0].ID, "uncertain")
	if err := s.ResolveReservation(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, rows[0].ID)
	if !dl.Superseded || len(dl.WantableIDs) != 0 || !strings.Contains(dl.Error, "Possible duplicate") {
		t.Fatal("custody resolution erased evidence")
	}
	if _, err := s.effectiveImportAllowlist(ctx, dl); err == nil {
		t.Fatal("late original publication")
	}
	if err := s.ResolveReservation(ctx, dl.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationSurvivesAbsenceAndFencesLateCompletion(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	_, rows := admittedFixture(t, s, db, item, "single")
	_, _ = db.ClaimPlannedSubmission(ctx, rows[0].ID)
	_ = db.SetSubmissionPhase(ctx, rows[0].ID, "uncertain")
	_, _ = db.W.ExecContext(ctx, `UPDATE downloads SET added_at=? WHERE id=?`, time.Now().Add(-8*24*time.Hour).UnixMilli(), rows[0].ID)
	if err := s.RemoveDownload(ctx, rows[0].ID, false); err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, rows[0].ID)
	cfg, _ := db.GetDownloadClient(ctx, dl.ClientID)
	ob := executionObservation{Config: clientFingerprint(cfg), LastInventory: time.Now(), AbsenceSince: time.Now().Add(-8 * 24 * time.Hour), Inventories: 100}
	_ = db.UpdateDownloadObservation(ctx, dl.ID, ob)
	for n := 0; n < 2; n++ {
		s.observeUncertain(ctx, dl, cfg, nil, true)
		dl, _ = db.GetDownload(ctx, dl.ID)
	}
	if dl.Superseded || !strings.HasPrefix(dl.ParkedReason, "operator cancellation") {
		t.Fatal("cancellation erased or retried")
	}
	s.reconcileDownload(ctx, dl, cfg, ports.DownloadStatus{Handle: "late", Name: dl.ReleaseTitle, State: ports.StateCompleted, SavePath: "/late"}, "poll")
	fresh, _ := db.GetDownload(ctx, dl.ID)
	if fresh.State == "downloaded" || fresh.State == "importing" {
		t.Fatal("cancelled late submission imported")
	}
}
func TestSubmittedEmptyHandleLearnsUniqueClientIdentity(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	_, rows := admittedFixture(t, s, db, item, "single")
	_, _ = db.ClaimPlannedSubmission(ctx, rows[0].ID)
	_ = db.SetSubmissionPhase(ctx, rows[0].ID, "submitted")
	client.statuses = []ports.DownloadStatus{{Handle: "torrent-hash", Name: rows[0].ReleaseTitle, State: ports.StateDownloading, OperationalState: "downloading"}}
	if err := s.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, rows[0].ID)
	if dl.Handle != "torrent-hash" {
		t.Fatal("successful empty-handle submission never reconciled")
	}
}
func TestManualChoiceSupersedesPendingIntentBeforeAdd(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	plan, rows := admittedFixture(t, s, db, item, "single")
	id, err := s.Grab(ctx, GrabRequest{MediaItemID: item, Season: 1, Episode: 1, Protocol: "torrent", Title: "Test.Show.S01E01.1080p.BluRay-MANUAL", DownloadURL: "manual"})
	if err != nil || id == rows[0].ID {
		t.Fatal("manual selection not admitted", err)
	}
	s.reconcileSeasonPlans(ctx)
	original, _ := db.GetDownload(ctx, rows[0].ID)
	if !original.Superseded {
		t.Fatal("pending intent still authorized")
	}
	if err = s.dispatchPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 || client.added[0] != "manual" {
		t.Fatalf("duplicate dispatch: %v", client.added)
	}
}
func TestTerminalRowCannotDismissUnresolvedPlacement(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	_, rows := admittedFixture(t, s, db, item, "single")
	_, _ = db.ClaimPlannedSubmission(ctx, rows[0].ID)
	_ = db.SetSubmissionPhase(ctx, rows[0].ID, "submitted")
	_ = db.UpdateDownloadState(ctx, rows[0].ID, "failed", 0, "publication interrupted")
	if err := db.PreparePlacement(ctx, sqlite.Placement{ID: "unresolved", DownloadID: rows[0].ID, ItemID: item, Release: rows[0].ReleaseTitle, Target: "/pending", State: "prepared"}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteDownload(ctx, rows[0].ID); err == nil {
		t.Fatal("dismiss erased placement association")
	}
	if n, err := db.ClearFailedDownloads(ctx); err != nil || n != 0 {
		t.Fatalf("clear erased custody: %d %v", n, err)
	}
}

type capabilityAuthFailure struct{ fakeIndexer }

func (capabilityAuthFailure) Capabilities(context.Context) (ports.IndexerCapabilities, error) {
	return ports.IndexerCapabilities{}, &ports.RemoteError{Category: ports.RemoteAuth}
}
func TestCapabilityOutageDoesNotVetoHealthySupply(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	bad, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "bad", URL: "http://bad.invalid", Protocol: "torrent", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	s.newIndexer = func(cfg ports.IndexerConfig) ports.Indexer {
		if cfg.ID == bad {
			return capabilityAuthFailure{}
		}
		return fakeIndexer{releases: []ports.Release{{IndexerID: cfg.ID, Indexer: cfg.Name, Title: "Test.Show.S01E01.1080p.WEB-DL", Protocol: "torrent", DownloadURL: "one"}, {IndexerID: cfg.ID, Indexer: cfg.Name, Title: "Test.Show.S01E02.1080p.WEB-DL", Protocol: "torrent", DownloadURL: "two"}}}
	}
	if _, err = s.AutoSearchItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	j, err := db.ClaimJob(ctx, "caps-fixture", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	err = s.handleSeasonSearch(ctx, j)
	var deferred interface{ DeferredUntil() time.Time }
	if !errors.As(err, &deferred) {
		t.Fatalf("expected persisted partial epoch, got %v", err)
	}
	if len(client.added) != 2 {
		t.Fatalf("healthy supply vetoed: %v", client.added)
	}
}

func TestCheckpointOverflowIsExplicitPartialRatherThanFailedJob(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	oversized := ports.Release{Title: "Test.Show.S01E01.1080p.WEB-DL", Protocol: "torrent", DownloadURL: strings.Repeat("x", (2<<20)+1)}
	s.newIndexer = func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{releases: []ports.Release{oversized}} }
	if _, err := s.AutoSearchItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	j, err := db.ClaimJob(ctx, "overflow-fixture", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	err = s.handleSeasonSearch(ctx, j)
	var deferred interface{ DeferredUntil() time.Time }
	if !errors.As(err, &deferred) {
		t.Fatalf("overflow spent failure attempt: %v", err)
	}
	var payload string
	if err = db.R.QueryRowContext(ctx, `SELECT payload FROM jobs WHERE id=?`, j.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) > 2<<20 || len(client.added) > 0 {
		t.Fatal("truncated evidence authorized transfer")
	}
}

func TestManualEpisodeKeepsSubmittedSiblingAuthority(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	_, rows := admittedFixture(t, s, db, item, "a", "b")
	_, _ = db.ClaimPlannedSubmission(ctx, rows[1].ID)
	_ = db.SetSubmissionPhase(ctx, rows[1].ID, "submitted")
	if _, err := s.Grab(ctx, GrabRequest{MediaItemID: item, Season: 1, Episode: 1, Protocol: "torrent", Title: "Test.Show.S01E01.1080p.BluRay-MANUAL", DownloadURL: "manual"}); err != nil {
		t.Fatal(err)
	}
	allowed, err := s.effectiveImportAllowlist(ctx, rows[1])
	if err != nil || len(allowed) != 1 {
		t.Fatalf("submitted sibling lost authority: %v %v", allowed, err)
	}
}
func TestManualPackReconcilesExactEpisodeIntents(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	_, rows := admittedFixture(t, s, db, item, "a", "b")
	if _, err := s.Grab(ctx, GrabRequest{MediaItemID: item, Season: 1, Protocol: "torrent", Title: "Test.Show.S01.1080p.BluRay-MANUAL", DownloadURL: "manual-pack"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		fresh, _ := db.GetDownload(ctx, row.ID)
		if !fresh.Superseded {
			t.Fatal("manual pack missed exact intent overlap")
		}
	}
	s.reconcileSeasonPlans(ctx)
	if len(client.added) != 1 {
		t.Fatal("automatic intents also dispatched")
	}
}
