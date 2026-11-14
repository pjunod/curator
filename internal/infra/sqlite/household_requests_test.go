package sqlite

import (
	"context"
	"errors"
	"fmt"
	"github.com/pjunod/monarr/internal/domain"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func householdMoviePlan(t *testing.T, db *DB, owner domain.Account, digest, fingerprint string) (HouseholdApproval, int64) {
	t.Helper()
	ctx := context.Background()
	itemID := availabilityFixture(t, db, domain.MediaItem{Kind: domain.KindMovie, Title: "Requested movie", IDs: domain.ExternalIDs{TMDB: 550}, QualityProfileID: 1})
	availabilityProof(t, db, itemID, time.Now())
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.SubmitHouseholdRequest(ctx, owner, digest, domain.HouseholdRequest{Kind: item.Kind, TMDBID: 550, IDs: item.IDs, Title: item.Title, Fingerprint: fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	request, err := db.HouseholdRequest(ctx, id, owner.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := db.HouseholdEvidence(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	return HouseholdApproval{RequestID: id, RequestRevision: request.Revision, Item: item, Evidence: evidence, Policy: domain.HouseholdPolicy{ProfileFingerprint: householdPolicyHash(t, db, item.QualityProfileID), Kind: item.Kind, TMDBID: 550, IDs: item.IDs, RootID: evidence.RootID, ProfileID: item.QualityProfileID, RootGeneration: evidence.RootGeneration, SharedRevision: evidence.SharedRevision, RootPath: evidence.RootPath, RootIdentity: evidence.RootIdentity, Path: evidence.Path}, Missing: []domain.RequestEpisode{{}}}, id
}
func TestHouseholdConcurrentSubmitJoinAndPrivateNotes(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	member, session := householdMember(t, db, owner, digest, "member")
	request := domain.HouseholdRequest{Kind: domain.KindMovie, TMDBID: 550, IDs: domain.ExternalIDs{TMDB: 550}, Title: "Shared movie", Fingerprint: "same-scope"}
	var wg sync.WaitGroup
	ids := make(chan int64, 2)
	errs := make(chan error, 2)
	for _, actor := range []struct {
		a domain.Account
		s string
	}{{owner, digest}, {member, session}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := db.SubmitHouseholdRequest(ctx, actor.a, actor.s, request)
			ids <- id
			errs <- err
		}()
	}
	wg.Wait()
	first := <-ids
	second := <-ids
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if first != second {
		t.Fatal("duplicate work created", first, second)
	}
	if err := db.SaveHouseholdRequestNote(ctx, owner, digest, first, "owner private"); err != nil {
		t.Fatal(err)
	}
	safe, err := db.HouseholdRequest(ctx, first, member.ID, false)
	if err != nil || safe.PrivateNote != "" || safe.SubscriberCount != 2 {
		t.Fatal("member receipt leaked private note or lost join", err)
	}
	if err = db.WithdrawHouseholdRequest(ctx, member, session, first); err != nil {
		t.Fatal(err)
	}
	safe, err = db.HouseholdRequest(ctx, first, owner.ID, true)
	if err != nil || safe.State != "pending" || safe.SubscriberCount != 1 || safe.PrivateNote != "owner private" {
		t.Fatal("one withdrawal ended another member's interest", err)
	}
	if err = db.WithdrawHouseholdRequest(ctx, owner, digest, first); err != nil {
		t.Fatal(err)
	}
	safe, err = db.HouseholdRequest(ctx, first, owner.ID, true)
	if err != nil || safe.State != "withdrawn" {
		t.Fatal("last interest did not withdraw pending request", err)
	}
}
func TestHouseholdApprovalRejectsChangedEvidenceAndWithdrawal(t *testing.T) {
	for _, change := range []string{"file arrived", "withdrawn", "volume changed"} {
		t.Run(change, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			owner, digest := householdOwner(t, db)
			plan, id := householdMoviePlan(t, db, owner, digest, change)
			switch change {
			case "file arrived":
				if _, err := db.UpsertFile(ctx, plan.Item.ID, 0, plan.Item.Path+"/arrived.mkv", 4); err != nil {
					t.Fatal(err)
				}
			case "withdrawn":
				if err := db.WithdrawHouseholdRequest(ctx, owner, digest, id); err != nil {
					t.Fatal(err)
				}
			case "volume changed":
				if _, err := db.W.ExecContext(ctx, `UPDATE library_presence_roots SET generation=generation+1 WHERE root_id=?`, plan.Policy.RootID); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.ApproveHouseholdRequest(ctx, owner, digest, plan); err == nil {
				t.Fatal("stale approval admitted work")
			}
			var count int
			if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM household_request_targets WHERE request_id=?`, id).Scan(&count); err != nil || count != 0 {
				t.Fatal("refusal left target intent", err)
			}
		})
	}
}
func TestHouseholdAdmissionPersistsOriginAndReusesExactGeneration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	plan, id := householdMoviePlan(t, db, owner, digest, "admission")
	if err := db.ApproveHouseholdRequest(ctx, owner, digest, plan); err != nil {
		t.Fatal(err)
	}
	targets, err := db.HouseholdPendingTargets(ctx)
	if err != nil || len(targets) != 1 {
		t.Fatal(targets, err)
	}
	target := targets[0]
	availabilityProof(t, db, plan.Item.ID, time.Now())
	intent := Download{MediaItemID: plan.Item.ID, ReleaseTitle: "Requested.Movie.1080p", Protocol: "nzb", ClientID: 1, State: "grabbed", SubmissionPhase: "submitting", Intent: "automatic", Transfer: "household-transfer", WantableIDs: []string{fmt.Sprintf("movie:%d", plan.Item.ID)}}
	first, created, err := db.InsertHouseholdDownload(ctx, target.ID, target.ApprovalGeneration, intent)
	if err != nil || !created {
		t.Fatal("admission failed", err)
	}
	restarted, e := Open(filepath.Dir(db.Path))
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = restarted.Close() }()
	second, created, err := restarted.InsertHouseholdDownload(ctx, target.ID, target.ApprovalGeneration, intent)
	if err != nil || created || second.ID != first.ID || second.GenerationKey != first.GenerationKey {
		t.Fatal("retry created another transfer", err)
	}
	origin, err := db.HouseholdDownloadOrigin(ctx, first.ID, first.GenerationKey)
	if err != nil || origin == nil || origin.ItemID != plan.Item.ID {
		t.Fatal("durable transfer lost publication scope", err)
	}
	wrong, err := db.HouseholdDownloadOrigin(ctx, first.ID, "another-generation")
	if err != nil || wrong != nil {
		t.Fatal("origin leaked to another generation", err)
	}
	if err = db.SetHouseholdTargetResult(ctx, target.ID, target.ApprovalGeneration, "needs_review", "review reason"); err != nil {
		t.Fatal(err)
	}
	current, e := db.HouseholdRequest(ctx, id, owner.ID, true)
	if e != nil {
		t.Fatal(e)
	}
	if err = db.DeclineHouseholdRequest(ctx, owner, digest, id, current.Revision, "cancel", "note"); err == nil {
		t.Fatal("owner decline discarded admitted custody")
	}
	if err = db.SetHouseholdTargetResult(ctx, target.ID, target.ApprovalGeneration, "fulfilled", ""); !errors.Is(err, ErrHouseholdVerification) {
		t.Fatal("queue state falsely established primary presence", err)
	}
}
func TestHouseholdPresentScopeFulfillsWithoutAcquisition(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	plan, id := householdMoviePlan(t, db, owner, digest, "present")
	fid, err := db.UpsertFile(ctx, plan.Item.ID, 0, plan.Item.Path+"/movie.mkv", 4)
	if err != nil {
		t.Fatal(err)
	}
	availabilityProof(t, db, plan.Item.ID, time.Now(), fid)
	plan.Evidence, err = db.HouseholdEvidence(ctx, plan.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan.Missing = nil
	if err = db.ApproveHouseholdRequest(ctx, owner, digest, plan); err != nil {
		t.Fatal(err)
	}
	request, err := db.HouseholdRequest(ctx, id, owner.ID, true)
	if err != nil || request.State != "fulfilled" {
		t.Fatal("present request did not fulfill", err)
	}
	targets, err := db.HouseholdPendingTargets(ctx)
	if err != nil || len(targets) != 0 {
		t.Fatal("present scope scheduled acquisition", err)
	}
	var downloads int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM downloads`).Scan(&downloads); err != nil || downloads != 0 {
		t.Fatal("present scope created download", err)
	}
}

func TestHouseholdEpisodeClaimsOverlapAndOffloadScope(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	id := availabilityFixture(t, db, domain.MediaItem{Kind: domain.KindSeries, Title: "Frozen show", IDs: domain.ExternalIDs{TMDB: 100}, QualityProfileID: 1, Seasons: []domain.Season{{Number: 1, OffloadedAt: 1, Episodes: []domain.Episode{{SeasonNumber: 1, EpisodeNumber: 1, AirDate: "2020-01-01", OffloadedAt: 1}, {SeasonNumber: 1, EpisodeNumber: 2, AirDate: "2020-01-02", OffloadedAt: 1}, {SeasonNumber: 1, EpisodeNumber: 3, AirDate: "2099-01-01", OffloadedAt: 1}}}}})
	// Offload is a local journaled intent, never accepted from provider metadata.
	if _, err := db.W.ExecContext(ctx, `UPDATE seasons SET offloaded_at=1 WHERE media_item_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE episodes SET offloaded_at=1 WHERE media_item_id=?`, id); err != nil {
		t.Fatal(err)
	}
	availabilityProof(t, db, id, time.Now())
	item, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := db.HouseholdEvidence(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	makePlan := func(fingerprint string, episodes []domain.RequestEpisode) HouseholdApproval {
		rid, e := db.SubmitHouseholdRequest(ctx, owner, digest, domain.HouseholdRequest{Kind: item.Kind, TMDBID: 100, IDs: item.IDs, Title: item.Title, Fingerprint: fingerprint, Scope: domain.RequestScope{Episodes: episodes}})
		if e != nil {
			t.Fatal(e)
		}
		r, e := db.HouseholdRequest(ctx, rid, owner.ID, true)
		if e != nil {
			t.Fatal(e)
		}
		return HouseholdApproval{RequestID: rid, RequestRevision: r.Revision, Item: item, Evidence: evidence, Missing: episodes, Policy: domain.HouseholdPolicy{ProfileFingerprint: householdPolicyHash(t, db, item.QualityProfileID), Kind: item.Kind, TMDBID: 100, IDs: item.IDs, RootID: evidence.RootID, ProfileID: item.QualityProfileID, RootGeneration: evidence.RootGeneration, SharedRevision: evidence.SharedRevision, RootPath: evidence.RootPath, RootIdentity: evidence.RootIdentity, Path: evidence.Path, Reacquire: true}}
	}
	first := makePlan("season-1-frozen", []domain.RequestEpisode{{Season: 1, Episode: 1}})
	overlap := makePlan("overlap", []domain.RequestEpisode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}})
	if err = db.ApproveHouseholdRequest(ctx, owner, digest, first); err != nil {
		t.Fatal(err)
	}
	availabilityProof(t, db, id, time.Now())
	overlap.Evidence, err = db.HouseholdEvidence(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ApproveHouseholdRequest(ctx, owner, digest, overlap); !errors.Is(err, ErrRequestOverlap) {
		t.Fatal("overlapping approval was not refused", err)
	}
	current, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if current.Monitored || current.Seasons[0].Monitored || current.Seasons[0].OffloadedAt == 0 {
		t.Fatal("approval changed monitoring or future offload inheritance")
	}
	for _, ep := range current.Seasons[0].Episodes {
		if ep.EpisodeNumber == 1 && ep.OffloadedAt == 0 {
			t.Fatal("approval cleared offload intent before primary bytes returned")
		}
		if ep.EpisodeNumber != 1 && ep.OffloadedAt == 0 {
			t.Fatal("unapproved leaf's offload flag cleared")
		}
	}
}

func householdPolicyHash(t *testing.T, db *DB, id int64) string {
	t.Helper()
	hash, err := db.HouseholdProfileFingerprint(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestHouseholdRetryFairnessSurvivesRestart(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	plan, id := householdMoviePlan(t, db, owner, digest, "fair")
	if err := db.ApproveHouseholdRequest(ctx, owner, digest, plan); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 10; n++ {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO household_request_targets(request_id,media_item_id,season,episode,created_at) VALUES(?,?,0,?,0)`, id, plan.Item.ID, n); err != nil {
			t.Fatal(err)
		}
	}
	targets, err := db.HouseholdPendingTargets(ctx)
	if err != nil || len(targets) != 11 {
		t.Fatal(targets, err)
	}
	for _, target := range targets[:10] {
		if err = db.ScheduleHouseholdTargetRetry(ctx, target.ID, target.ApprovalGeneration, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	restarted, e := Open(filepath.Dir(db.Path))
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = restarted.Close() }()
	// A new reader sees persisted due order; no in-memory round-robin state exists.
	due, err := restarted.HouseholdPendingTargets(ctx)
	if err != nil || len(due) != 1 || due[0].ID != targets[10].ID {
		t.Fatal("eleventh target starved", due, err)
	}
}
func TestHouseholdWithdrawAfterReviewKeepsClaimsAndAuthorizedWork(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	plan, id := householdMoviePlan(t, db, owner, digest, "review-withdraw")
	if err := db.ApproveHouseholdRequest(ctx, owner, digest, plan); err != nil {
		t.Fatal(err)
	}
	targets, err := db.HouseholdPendingTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := targets[0]
	if err = db.SetHouseholdTargetResult(ctx, target.ID, target.ApprovalGeneration, "needs_review", "changed policy"); err != nil {
		t.Fatal(err)
	}
	if err = db.WithdrawHouseholdRequest(ctx, owner, digest, id); err != nil {
		t.Fatal(err)
	}
	request, err := db.HouseholdRequest(ctx, id, owner.ID, true)
	if err != nil || request.State != "needs_review" || request.SubscriberCount != 0 {
		t.Fatal("withdraw terminalized authorized work", request, err)
	}
	var claims int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM household_scope_claims WHERE request_id=?`, id).Scan(&claims); err != nil || claims != 1 {
		t.Fatal("claim ownership lost", claims, err)
	}
}
func TestHouseholdExistingWorkAndFrozenOriginPolicy(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	plan, _ := householdMoviePlan(t, db, owner, digest, "existing")
	if err := db.ApproveHouseholdRequest(ctx, owner, digest, plan); err != nil {
		t.Fatal(err)
	}
	targets, err := db.HouseholdPendingTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := targets[0]
	downloadID, err := db.InsertDownload(ctx, Download{MediaItemID: plan.Item.ID, Protocol: "nzb", ClientID: 1, State: "grabbed", WantableIDs: []string{fmt.Sprintf("movie:%d", plan.Item.ID)}})
	if err != nil {
		t.Fatal(err)
	}
	found, err := db.FindHouseholdOwnerDownload(ctx, target)
	if err != nil || found != downloadID {
		t.Fatal("existing work requires an indexer", found, err)
	}
	// Runner payload identity differs from the local download generation.
	if _, err = db.W.ExecContext(ctx, `INSERT INTO publications(download_id,generation,kind,path,state,discovered_at,updated_at,holds) VALUES(?,'runner-payload-generation','completed','/held-payload','discovered',0,0,1)`, downloadID); err != nil {
		t.Fatal(err)
	}
	if err = db.BindExistingHouseholdDownload(ctx, target.ID, target.ApprovalGeneration, downloadID); !errors.Is(err, ErrRequestChanged) {
		t.Fatal("held payload attached through a different generation namespace", err)
	}
	if _, err = db.FindHouseholdOwnerDownload(ctx, target); !errors.Is(err, ErrRequestChanged) {
		t.Fatal("lookup failed to identify held work", err)
	}
	if _, err = db.W.ExecContext(ctx, `DELETE FROM publications WHERE download_id=?`, downloadID); err != nil {
		t.Fatal(err)
	}
	if err = db.BindExistingHouseholdDownload(ctx, target.ID, target.ApprovalGeneration, downloadID); err != nil {
		t.Fatal(err)
	}
	download, err := db.GetDownload(ctx, downloadID)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := db.HouseholdDownloadNeedsReview(ctx, downloadID, download.GenerationKey)
	if err != nil || stopped {
		t.Fatal(stopped, err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE downloads SET parked_at=1 WHERE id=?`, downloadID); err != nil {
		t.Fatal(err)
	}
	stopped, err = db.HouseholdDownloadNeedsReview(ctx, downloadID, download.GenerationKey)
	if err != nil || !stopped {
		t.Fatal("parked work followed forever", stopped, err)
	}
}

func TestHouseholdReReviewReleasesClaimsForArrivingPrimary(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	plan, id := householdMoviePlan(t, db, owner, digest, "present-again")
	if err := db.ApproveHouseholdRequest(ctx, owner, digest, plan); err != nil {
		t.Fatal(err)
	}
	targets, err := db.HouseholdPendingTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetHouseholdTargetResult(ctx, targets[0].ID, 1, "needs_review", "refresh"); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, plan.Item.ID, 0, plan.Item.Path+"/arrived.mkv", 4)
	if err != nil {
		t.Fatal(err)
	}
	availabilityProof(t, db, plan.Item.ID, time.Now(), fid)
	request, err := db.HouseholdRequest(ctx, id, owner.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	plan.RequestRevision = request.Revision
	plan.Missing = nil
	plan.Evidence, err = db.HouseholdEvidence(ctx, plan.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ApproveHouseholdRequest(ctx, owner, digest, plan); err != nil {
		t.Fatal(err)
	}
	var claims int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM household_scope_claims WHERE request_id=?`, id).Scan(&claims); err != nil || claims != 0 {
		t.Fatal("present leaf retained a permanent claim", claims, err)
	}
}
func TestHouseholdOriginRefusesPolicyDriftAndGenericReplacement(t *testing.T) {
	for _, change := range []string{"profile", "identity", "offload"} {
		t.Run(change, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			owner, digest := householdOwner(t, db)
			plan, _ := householdMoviePlan(t, db, owner, digest, "origin-policy")
			plan.Policy.IDs.IMDB = "tt123"
			plan.Item.IDs.IMDB = "tt123"
			if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET imdb_id='tt123' WHERE id=?`, plan.Item.ID); err != nil {
				t.Fatal(err)
			}
			availabilityProof(t, db, plan.Item.ID, time.Now())
			plan.Evidence, _ = db.HouseholdEvidence(ctx, plan.Item.ID)
			if err := db.ApproveHouseholdRequest(ctx, owner, digest, plan); err != nil {
				t.Fatal(err)
			}
			availabilityProof(t, db, plan.Item.ID, time.Now())
			targets, err := db.HouseholdPendingTargets(ctx)
			if err != nil {
				t.Fatal(err)
			}
			download, _, err := db.InsertHouseholdDownload(ctx, targets[0].ID, 1, Download{MediaItemID: plan.Item.ID, Protocol: "nzb", ClientID: 1, State: "grabbed", SubmissionPhase: "submitting", Intent: "automatic", Transfer: "transfer", WantableIDs: []string{fmt.Sprintf("movie:%d", plan.Item.ID)}})
			if err != nil {
				t.Fatal(err)
			}
			origin, err := db.HouseholdDownloadOrigin(ctx, download.ID, download.GenerationKey)
			if err != nil {
				t.Fatal(err)
			}
			placement := Placement{ItemID: plan.Item.ID, DownloadID: download.ID, SourceGeneration: download.GenerationKey, Target: plan.Item.Path + "/new.mkv"}
			if err = db.CheckHouseholdOrigin(ctx, *origin, placement); err != nil {
				t.Fatal("frozen policy refused unchanged import", err)
			}
			won, err := db.RecordRunnerTerminal(ctx, RunnerTerminalWrite{DownloadID: download.ID, Transfer: download.Transfer, Outcome: "failed", State: "failed", ReplacementGrab: &ReplacementGrabWrite{WantableIDs: download.WantableIDs}})
			if err != nil || !won {
				t.Fatal("terminal", won, err)
			}
			if _, err = db.W.ExecContext(ctx, `INSERT INTO replacement_grabs(download_id,wantable_ids,created_at,next_at) VALUES(?,'[]',0,0)`, download.ID); err != nil {
				t.Fatal(err)
			}
			var replacements int
			if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_grabs WHERE download_id=?`, download.ID).Scan(&replacements); err != nil || replacements != 0 {
				t.Fatal("generic replacement escaped household approval", replacements, err)
			}
			switch change {
			case "profile":
				_, err = db.W.ExecContext(ctx, `UPDATE quality_profiles SET upgrades_allowed=1-upgrades_allowed WHERE id=?`, plan.Policy.ProfileID)
			case "identity":
				_, err = db.W.ExecContext(ctx, `UPDATE media_items SET imdb_id='tt456' WHERE id=?`, plan.Item.ID)
			case "offload":
				_, err = db.W.ExecContext(ctx, `UPDATE media_items SET offloaded_at=999999 WHERE id=?`, plan.Item.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = db.CheckHouseholdOrigin(ctx, *origin, placement); !errors.Is(err, ErrRequestChanged) {
				t.Fatal("changed policy authorized publication", change, err)
			}
		})
	}
}
func TestCinemaRevocationsRetryFairlyAndForgetExpiredSecrets(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	connection := domain.CinemaConnection{BaseURL: "https://unavailable.example", ClientID: "client", ClientSecret: "secret"}
	for n := 0; n < 33; n++ {
		if err := db.QueueCinemaRevocation(ctx, connection, fmt.Sprint(n), time.Now().Add(time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.CinemaRevocations(ctx)
	if err != nil || len(rows) != 32 {
		t.Fatal(rows, err)
	}
	for _, row := range rows {
		if err = db.DeferCinemaRevocation(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = db.CinemaRevocations(ctx)
	if err != nil || len(rows) != 1 || rows[0].Grant != "32" {
		t.Fatal("failed old grants starved a reachable new grant", rows, err)
	}
	if err = db.QueueCinemaRevocation(ctx, connection, "expired", time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CinemaRevocations(ctx); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM cinema_revocations WHERE grant_secret='expired'`).Scan(&retained); err != nil || retained != 0 {
		t.Fatal("expired secret retained", retained, err)
	}
}

func TestHouseholdPrimaryProbePreservesAdditionalCopyEvidence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id := availabilityFixture(t, db, domain.MediaItem{Kind: domain.KindMovie, Title: "Copies", IDs: domain.ExternalIDs{TMDB: 7}})
	availabilityProof(t, db, id, time.Now())
	if _, err := db.W.ExecContext(ctx, `INSERT INTO library_presence_observations SELECT media_item_id,99,item_revision,presence_fingerprint,shared_revision,root_id,root_path,root_identity,root_generation,scope_path,checked_at,valid,present_file_ids FROM library_presence_observations WHERE media_item_id=? AND copy_id=0`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.InvalidatePrimaryPresence(ctx, id); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := db.R.QueryRowContext(ctx, `SELECT valid FROM library_presence_observations WHERE media_item_id=? AND copy_id=99`, id).Scan(&valid); err != nil || !valid {
		t.Fatal("primary check invalidated another copy", valid, err)
	}
}

func TestHouseholdPartialSeriesKeepsReviewReasonAndStablePollRevision(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	id := availabilityFixture(t, db, domain.MediaItem{Kind: domain.KindSeries, Title: "Partial", IDs: domain.ExternalIDs{TMDB: 100}, QualityProfileID: 1, Seasons: []domain.Season{{Number: 1, Episodes: []domain.Episode{{SeasonNumber: 1, EpisodeNumber: 1, AirDate: "2020-01-01"}, {SeasonNumber: 1, EpisodeNumber: 2, AirDate: "2020-01-02"}}}}})
	availabilityProof(t, db, id, time.Now())
	item, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	scope := []domain.RequestEpisode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}
	rid, err := db.SubmitHouseholdRequest(ctx, owner, digest, domain.HouseholdRequest{Kind: item.Kind, TMDBID: 100, IDs: item.IDs, Title: item.Title, Fingerprint: "two-leaf", Scope: domain.RequestScope{Episodes: scope}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := db.HouseholdRequest(ctx, rid, owner.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := db.HouseholdEvidence(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	plan := HouseholdApproval{RequestID: rid, RequestRevision: request.Revision, Item: item, Evidence: evidence, Missing: scope, Policy: domain.HouseholdPolicy{ProfileFingerprint: householdPolicyHash(t, db, item.QualityProfileID), Kind: item.Kind, TMDBID: 100, IDs: item.IDs, RootID: evidence.RootID, ProfileID: item.QualityProfileID, RootGeneration: evidence.RootGeneration, SharedRevision: evidence.SharedRevision, RootPath: evidence.RootPath, RootIdentity: evidence.RootIdentity, Path: evidence.Path}}
	if err = db.ApproveHouseholdRequest(ctx, owner, digest, plan); err != nil {
		t.Fatal(err)
	}
	targets, err := db.HouseholdPendingTargets(ctx)
	if err != nil || len(targets) != 2 {
		t.Fatal(targets, err)
	}
	first, second := targets[0], targets[1]
	if err = db.SetHouseholdTargetResult(ctx, second.ID, second.ApprovalGeneration, "needs_review", "Episode two is held. Review Activity."); err != nil {
		t.Fatal(err)
	}
	blocked, err := db.HouseholdRequest(ctx, rid, owner.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	// A different blocked leaf must not alternate the aggregate reason each poll.
	if err = db.SetHouseholdTargetResult(ctx, first.ID, first.ApprovalGeneration, "needs_review", "Episode one has failed."); err != nil {
		t.Fatal(err)
	}
	changed, err := db.HouseholdRequest(ctx, rid, owner.ID, true)
	if err != nil || changed.Revision != blocked.Revision+1 || changed.PublicNote != blocked.PublicNote {
		t.Fatal(changed, blocked, err)
	}
	for range 3 {
		for _, v := range []struct {
			target      domain.HouseholdTarget
			state, note string
		}{{first, "needs_review", "Episode one has failed."}, {second, "needs_review", "Episode two is held. Review Activity."}, {first, "waiting", "Still following work."}} {
			if err = db.SetHouseholdTargetResult(ctx, v.target.ID, v.target.ApprovalGeneration, v.state, v.note); err != nil {
				t.Fatal(err)
			}
		}
	}
	stable, err := db.HouseholdRequest(ctx, rid, owner.ID, true)
	if err != nil || stable.Revision != changed.Revision || stable.PublicNote != blocked.PublicNote || !stable.UpdatedAt.Equal(changed.UpdatedAt) {
		t.Fatal("identical polls changed owner confirmation", stable, changed, err)
	}
	files := []int64{}
	for i, target := range []domain.HouseholdTarget{first, second} {
		fid, e := db.UpsertFile(ctx, id, 0, fmt.Sprintf("%s/S01E%02d.mkv", item.Path, target.Episode), 10)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.W.ExecContext(ctx, `INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(?,?)`, fid, target.EpisodeID); e != nil {
			t.Fatal(e)
		}
		files = append(files, fid)
		availabilityProof(t, db, id, time.Now(), files...)
		if err = db.SetHouseholdTargetResult(ctx, target.ID, target.ApprovalGeneration, "fulfilled", ""); err != nil {
			t.Fatal(err)
		}
		got, e := db.HouseholdRequest(ctx, rid, owner.ID, true)
		if e != nil {
			t.Fatal(e)
		}
		if i == 0 && (got.State != "needs_review" || got.PublicNote != blocked.PublicNote || got.Revision != stable.Revision+1) {
			t.Fatal("partial fulfillment erased outstanding review", got)
		}
		if i == 1 && (got.State != "fulfilled" || got.PublicNote != "") {
			t.Fatal("complete primary scope not fulfilled", got)
		}
		if err = db.SetHouseholdTargetResult(ctx, target.ID, target.ApprovalGeneration, "fulfilled", ""); err != nil {
			t.Fatal(err)
		}
		same, e := db.HouseholdRequest(ctx, rid, owner.ID, true)
		if e != nil || same.Revision != got.Revision {
			t.Fatal("repeated proof churned revision", same, got, e)
		}
		var claims int
		if e = db.R.QueryRowContext(ctx, `SELECT count(*) FROM household_scope_claims WHERE request_id=?`, rid).Scan(&claims); e != nil || claims != 1-i {
			t.Fatal("wrong exact claims", claims, e)
		}
	}
	var downloads int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM downloads`).Scan(&downloads); err != nil || downloads != 0 {
		t.Fatal("result processing submitted new work", downloads, err)
	}
}

func TestHouseholdTransactionsRecheckActingIdentityAndKeepOtherMembersHistory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	owner, digest := householdOwner(t, db)
	member, memberDigest := householdMember(t, db, owner, digest, "member")
	plan, id := householdMoviePlan(t, db, owner, digest, "authority")
	forged := member
	forged.Role = "owner"
	stale := owner
	stale.CredentialGeneration++
	for _, actor := range []struct {
		name    string
		account domain.Account
		digest  string
	}{{"forged role", forged, memberDigest}, {"stale owner", stale, digest}, {"wrong session", owner, memberDigest}} {
		t.Run(actor.name, func(t *testing.T) {
			for _, operation := range []struct {
				name string
				run  func() error
			}{
				{"approve", func() error { return db.ApproveHouseholdRequest(ctx, actor.account, actor.digest, plan) }},
				{"decline", func() error {
					return db.DeclineHouseholdRequest(ctx, actor.account, actor.digest, id, plan.RequestRevision, "declined", "private")
				}},
				{"private note", func() error { return db.SaveHouseholdRequestNote(ctx, actor.account, actor.digest, id, "private") }},
				{"invite", func() error {
					return db.CreateHouseholdInvite(ctx, actor.account, actor.digest, "forbidden", time.Now().Add(time.Hour))
				}},
				{"connection", func() error { return db.SaveCinemaConnection(ctx, actor.account, actor.digest, nil) }},
				{"other account", func() error {
					return db.UpdateAccount(ctx, actor.account, actor.digest, owner.ID, owner.Revision, "owner", "active", "changed", "")
				}},
			} {
				if err := operation.run(); err == nil {
					t.Fatalf("%s bypassed transactional actor proof", operation.name)
				}
			}
		})
	}
	// Every member-facing mutation also rechecks account+session pairing, even
	// when an upstream HTTP handler authenticated an older request snapshot.
	for _, operation := range []func() error{
		func() error {
			_, err := db.SubmitHouseholdRequest(ctx, stale, digest, domain.HouseholdRequest{Kind: domain.KindMovie, TMDBID: 551, IDs: domain.ExternalIDs{TMDB: 551}, Title: "unauthorized", Fingerprint: "stale"})
			return err
		},
		func() error { return db.JoinHouseholdRequest(ctx, owner, memberDigest, id) },
		func() error { return db.WithdrawHouseholdRequest(ctx, owner, memberDigest, id) },
	} {
		if err := operation(); err == nil {
			t.Fatal("stale request snapshot changed subscriptions")
		}
	}
	saved, err := db.HouseholdRequest(ctx, id, owner.ID, true)
	if err != nil || saved.State != "pending" || saved.Revision != plan.RequestRevision || saved.PrivateNote != "" || saved.PublicNote != "" || saved.SubscriberCount != 1 {
		t.Fatal("rejected writer partially committed", saved, err)
	}
	current, err := db.AccountByID(ctx, owner.ID)
	if err != nil || current.DisplayName != owner.DisplayName || current.Revision != owner.Revision {
		t.Fatal("forged owner changed local account", current, err)
	}
	if _, err = db.AccountSession(ctx, digest); err != nil {
		t.Fatal("rejected mutation revoked unrelated owner", err)
	}
}
