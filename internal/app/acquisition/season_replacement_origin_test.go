package acquisition

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func insertSeasonReplacementParent(t *testing.T, ctx context.Context, db *sqlite.DB, itemID int64) sqlite.Download {
	t.Helper()
	id, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: itemID, Season: 1, State: "failed", Protocol: "torrent", ReleaseTitle: "Original failed release",
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return parent
}

func deleteSeasonReplacementParent(t *testing.T, ctx context.Context, db *sqlite.DB, parent sqlite.Download) {
	t.Helper()
	target := sqlite.ActivityIdentity{ID: parent.ID, GenerationKey: parent.GenerationKey, Epoch: "0"}
	if _, err := db.CreateActivityOperation(ctx, "delete-season-replacement-parent", "delete", target, []sqlite.ActivityTarget{{ActivityIdentity: target}}); err != nil {
		t.Fatal(err)
	}
	requested, err := db.DownloadDeletionRequested(ctx, parent.ID, parent.GenerationKey)
	if err != nil || !requested {
		t.Fatalf("parent disposition: requested=%v err=%v", requested, err)
	}
}

func TestSeasonReplacementOriginFencesQueueAndRestart(t *testing.T) {
	for _, stage := range []string{"before enqueue", "after enqueue", "after lease"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			client := &fakeClient{}
			svc, db, itemID := setup(t, []ports.Release{{
				Title: "Test.Show.S01.1080p.WEB-DL-GRP", DownloadURL: "pack", Indexer: "idx", Protocol: "torrent",
			}}, client)
			parent := insertSeasonReplacementParent(t, ctx, db, itemID)
			origin := sqlite.SeasonReplacementOrigin{DownloadID: parent.ID, GenerationKey: parent.GenerationKey}
			checkpoint := seasonCheckpoint{Version: 1, ItemID: itemID, Season: 1, Trigger: seasonTriggerFailedAcquisition, ReplacementOrigin: &origin}

			if stage == "before enqueue" {
				deleteSeasonReplacementParent(t, ctx, db, parent)
				status, err := svc.enqueueSeasonComparison(ctx, checkpoint, true)
				if err != nil || status != seasonNothingToDo {
					t.Fatalf("revoked comparison enqueue: status=%q err=%v", status, err)
				}
				// Exercise the write gate as well. This represents a caller
				// that read parent authority just before Delete committed.
				payload, _ := json.Marshal(checkpoint)
				_, err = db.EnqueueJob(ctx, domain.Job{Kind: SeasonSearchJobKind, DedupeKey: checkpoint.jobKey(), Payload: string(payload)})
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("write gate accepted the revoked origin: %v", err)
				}
				return
			}

			status, err := svc.enqueueSeasonComparison(ctx, checkpoint, true)
			if err != nil || status != seasonQueued {
				t.Fatalf("comparison enqueue: status=%q err=%v", status, err)
			}
			var staleLease domain.Job
			if stage == "after lease" {
				staleLease, err = db.ClaimJob(ctx, "old-worker", nil, time.Minute)
				if err != nil || staleLease.DedupeKey != checkpoint.jobKey() {
					t.Fatalf("replacement lease: %+v err=%v", staleLease, err)
				}
			}
			// An independent request owns a different queue key in the same season.
			if err = svc.enqueueSeason(WithManualSearch(ctx), itemID, 0, 1, "item_search", true); err != nil {
				t.Fatal(err)
			}
			deleteSeasonReplacementParent(t, ctx, db, parent)

			reopened, err := sqlite.Open(filepath.Dir(db.Path))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			restarted := New(reopened, svc.bus, svc.log, svc.newIndexer, svc.newClient)
			if staleLease.ID != 0 {
				if err = restarted.handleSeasonSearch(ctx, staleLease); err != nil {
					t.Fatalf("revoked lease must finish: %v", err)
				}
				if err = reopened.CompleteJob(ctx, staleLease.ID); err != nil {
					t.Fatal(err)
				}
			}
			var linked, independent int
			if err = reopened.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind=? AND dedupe_key=? AND state IN ('queued','leased')`, SeasonSearchJobKind, checkpoint.jobKey()).Scan(&linked); err != nil {
				t.Fatal(err)
			}
			if err = reopened.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind=? AND dedupe_key=? AND state='queued'`, SeasonSearchJobKind, seasonJobKey(itemID, 0, 1)).Scan(&independent); err != nil {
				t.Fatal(err)
			}
			if linked != 0 || independent != 1 || len(client.added) != 0 {
				t.Fatalf("linked=%d independent=%d client adds=%v", linked, independent, client.added)
			}
			manual, err := reopened.ClaimInteractiveJob(ctx, "new-worker", nil, time.Minute)
			if err != nil || manual.DedupeKey != seasonJobKey(itemID, 0, 1) {
				t.Fatalf("independent manual lease: %+v err=%v", manual, err)
			}
			if err = restarted.handleSeasonSearch(ctx, manual); err != nil {
				t.Fatalf("independent manual comparison: %v", err)
			}
			if len(client.added) != 1 || client.added[0] != "pack" {
				t.Fatalf("independent manual comparison did not submit: %v", client.added)
			}
		})
	}
}

func TestSeasonReplacementDeleteSeparatesUnsentInFlightAndAdmittedChildren(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	parent := insertSeasonReplacementParent(t, ctx, db, itemID)
	origin := sqlite.SeasonReplacementOrigin{DownloadID: parent.ID, GenerationKey: parent.GenerationKey}
	plan := sqlite.AcquisitionPlan{MediaItemID: itemID, Season: 1, Decision: []byte("{}"), SnapshotFingerprint: "test", ReplacementOrigin: &origin}
	revision, err := db.AcquisitionRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var intents []sqlite.Download
	for _, candidate := range []string{"acknowledged", "admitting", "pending"} {
		intents = append(intents, sqlite.Download{
			MediaItemID: itemID, Season: 1, Protocol: "torrent", CandidateKey: candidate,
			ReleaseTitle: "Test.Show.S01.1080p.WEB-DL-" + candidate, ExecutionPayload: "{}",
			WantableIDs: []string{fmt.Sprintf("attempt:%s", candidate)},
		})
	}
	id, err := db.AdmitAcquisitionPlan(ctx, plan, revision, intents)
	if err != nil {
		t.Fatal(err)
	}
	children, err := db.PlanDownloads(ctx, id)
	if err != nil || len(children) != len(intents) {
		t.Fatalf("plan children: %+v err=%v", children, err)
	}
	for _, child := range children {
		if child.CandidateKey == "pending" {
			continue
		}
		if claimed, err := db.ClaimPlannedSubmission(ctx, child.ID); err != nil || !claimed {
			t.Fatalf("claim %s: claimed=%v err=%v", child.CandidateKey, claimed, err)
		}
		if child.CandidateKey == "acknowledged" {
			if err := db.RecordDownloadSubmission(ctx, child.ID, child.GenerationKey, child.Transfer, "mock-client-handle"); err != nil {
				t.Fatal(err)
			}
		}
	}
	deleteSeasonReplacementParent(t, ctx, db, parent)
	if _, err := db.AdmitAcquisitionPlan(ctx, plan, revision, nil); !errors.Is(err, sqlite.ErrParentDispositionRequested) {
		t.Fatalf("plan admission did not check the parent disposition: %v", err)
	}

	reopened, err := sqlite.Open(filepath.Dir(db.Path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := New(reopened, svc.bus, svc.log, svc.newIndexer, svc.newClient)
	current, err := reopened.GetAcquisitionPlan(ctx, id)
	if err != nil || current.ReplacementOrigin == nil || *current.ReplacementOrigin != origin {
		t.Fatalf("persisted plan origin: %+v err=%v", current.ReplacementOrigin, err)
	}
	if err := restarted.dispatchPlan(ctx, id); err != nil || len(client.added) != 0 {
		t.Fatalf("dispatch after parent deletion: adds=%v err=%v", client.added, err)
	}
	for _, child := range children {
		disposed, err := reopened.DownloadDeletionRequested(ctx, child.ID, child.GenerationKey)
		if err != nil || disposed != (child.CandidateKey == "admitting") {
			t.Fatalf("%s child disposition: requested=%v err=%v", child.CandidateKey, disposed, err)
		}
		row, err := reopened.GetDownload(ctx, child.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch child.CandidateKey {
		case "acknowledged":
			if row.SubmissionPhase != "submitted" || row.State != "grabbed" || row.Handle != "mock-client-handle" {
				t.Fatalf("acknowledged independent child changed: %+v", row)
			}
		case "pending":
			if row.SubmissionPhase != "rejected" || !row.Superseded || len(row.WantableIDs) != 0 {
				t.Fatalf("unsent child retained reservation: %+v", row)
			}
		}
	}
}
