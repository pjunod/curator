package sqlite

import (
	"context"
	"errors"
	"fmt"
	"github.com/pjunod/monarr/internal/domain"
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
	return HouseholdApproval{RequestID: id, RequestRevision: request.Revision, Item: item, Evidence: evidence, Policy: domain.HouseholdPolicy{Kind: item.Kind, TMDBID: 550, IDs: item.IDs, RootID: evidence.RootID, ProfileID: item.QualityProfileID, RootGeneration: evidence.RootGeneration, SharedRevision: evidence.SharedRevision, RootPath: evidence.RootPath, RootIdentity: evidence.RootIdentity, Path: evidence.Path}, Missing: []domain.RequestEpisode{{}}}, id
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
	second, created, err := db.InsertHouseholdDownload(ctx, target.ID, target.ApprovalGeneration, intent)
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
		return HouseholdApproval{RequestID: rid, RequestRevision: r.Revision, Item: item, Evidence: evidence, Missing: episodes, Policy: domain.HouseholdPolicy{Kind: item.Kind, TMDBID: 100, IDs: item.IDs, RootID: evidence.RootID, ProfileID: item.QualityProfileID, RootGeneration: evidence.RootGeneration, SharedRevision: evidence.SharedRevision, RootPath: evidence.RootPath, RootIdentity: evidence.RootIdentity, Path: evidence.Path, Reacquire: true}}
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
		if ep.EpisodeNumber == 1 && ep.OffloadedAt != 0 {
			t.Fatal("approved leaf not cleared")
		}
		if ep.EpisodeNumber != 1 && ep.OffloadedAt == 0 {
			t.Fatal("unapproved leaf's offload flag cleared")
		}
	}
}
