package household

import (
	"context"
	"fmt"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/infra/sqlite/sqlitetest"
	"github.com/pjunod/monarr/internal/ports"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExistingOwnerTransferSettlesOffloadBeforeAttachmentWithoutIndexers(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(sqlitetest.Dir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	defer b.Close()
	lib := library.New(db, nil, b, nil)
	acq := acquisition.New(db, b, nil, nil, nil)
	service := New(db, lib, acq)
	root, err := lib.AddRootFolder(ctx, t.TempDir(), domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(root.Path, "Movie")
	if err = os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Movie", IDs: domain.ExternalIDs{TMDB: 550}, RootFolderID: root.ID, QualityProfileID: 1, Path: folder, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(folder, "movie.mkv")
	if err = os.WriteFile(file, []byte("primary"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = db.UpsertFile(ctx, itemID, 0, file, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "Cinema", Enabled: true, OnImport: true, Settings: map[string]string{"url": "http://cinema.invalid", "apiKey": "fixture"}}); err != nil {
		t.Fatal(err)
	}
	req := acquisition.OffloadRequest{ItemID: itemID, Scope: "item"}
	preview, err := acq.PreviewOffload(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.RequestID = "household-owner-transfer"
	req.PlanFingerprint = preview.Fingerprint
	op, err := acq.StartOffload(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, e := acq.GetOffload(ctx, op.ID)
		if e != nil {
			t.Fatal(e)
		}
		if status.State == "complete" {
			break
		}
		if status.State == "failed" || time.Now().After(deadline) {
			t.Fatal("offload did not finish", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	name, hash, required := "owner", "local-hash", true
	if err = db.ConfigureLocalOwner(ctx, &name, &hash, &required); err != nil {
		t.Fatal(err)
	}
	owner, err := db.AccountByUsername(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	digest := "owner-session"
	if err = db.CreateAccountSession(ctx, digest, owner, "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := db.SubmitHouseholdRequest(ctx, owner, digest, domain.HouseholdRequest{Kind: item.Kind, TMDBID: item.IDs.TMDB, IDs: item.IDs, Title: item.Title, Fingerprint: "movie"})
	if err != nil {
		t.Fatal(err)
	}
	if err = lib.VerifyPrimaryPresence(ctx, itemID); err != nil {
		t.Fatal(err)
	}
	evidence, err := db.HouseholdEvidence(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := db.HouseholdProfileFingerprint(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	request, err := db.HouseholdRequest(ctx, requestID, owner.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	policy := domain.HouseholdPolicy{Kind: item.Kind, TMDBID: item.IDs.TMDB, IDs: item.IDs, RootID: root.ID, ProfileID: 1, ProfileFingerprint: profile, RootPath: evidence.RootPath, RootIdentity: evidence.RootIdentity, RootGeneration: evidence.RootGeneration, SharedRevision: evidence.SharedRevision, Path: item.Path, Reacquire: true, OffloadOperations: []string{op.ID}}
	if err = db.ApproveHouseholdRequest(ctx, owner, digest, sqlite.HouseholdApproval{RequestID: requestID, RequestRevision: request.Revision, Item: item, Evidence: evidence, Policy: policy, Missing: []domain.RequestEpisode{{}}}); err != nil {
		t.Fatal(err)
	}
	ownerDownload, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: itemID, ClientID: 1, Protocol: "torrent", State: "grabbed", Intent: "automatic", Transfer: "owner-transfer", WantableIDs: []string{fmt.Sprintf("movie:%d", itemID)}})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.Process(ctx); err != nil {
		t.Fatal(err)
	}
	targets, err := db.HouseholdPendingTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = targets
	var bound int64
	if err = db.R.QueryRowContext(ctx, `SELECT download_id FROM household_request_targets WHERE request_id=?`, requestID).Scan(&bound); err != nil || bound != ownerDownload {
		t.Fatal("existing work not attached without an indexer", bound, err)
	}
	deliveries, err := db.OperationDeliveries(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range deliveries {
		if delivery.Status == "pending" {
			t.Fatal("existing work bypassed offload narrowing", delivery)
		}
	}
	ownerRow, e := db.GetDownload(ctx, ownerDownload)
	if e != nil {
		t.Fatal(e)
	}
	origin, err := db.HouseholdDownloadOrigin(ctx, ownerDownload, ownerRow.GenerationKey)
	if err != nil || origin != nil {
		t.Fatal("existing owner authority replaced", origin, err)
	}

	// A restart follows this exact durable transfer even with no indexers.
	var targetID, generation int64
	if err = db.R.QueryRowContext(ctx, `SELECT id,approval_generation FROM household_request_targets WHERE request_id=?`, requestID).Scan(&targetID, &generation); err != nil {
		t.Fatal(err)
	}
	target, err := db.HouseholdTarget(ctx, targetID)
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(db, lib, acq)
	if err = restarted.processTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	waiting, err := db.HouseholdRequest(ctx, requestID, owner.ID, true)
	if err != nil || waiting.State != "acquiring" {
		t.Fatal(waiting, err)
	}
	// Held/parked owner work needs review and cannot authorize replacement.
	if _, err = db.W.ExecContext(ctx, `UPDATE downloads SET parked_at=? WHERE id=?`, time.Now().UnixMilli(), ownerDownload); err != nil {
		t.Fatal(err)
	}
	if err = restarted.processTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	blocked, err := db.HouseholdRequest(ctx, requestID, owner.ID, true)
	if err != nil || blocked.State != "needs_review" || blocked.PublicNote == "" {
		t.Fatal(blocked, err)
	}
	if err = restarted.processTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	stable, err := db.HouseholdRequest(ctx, requestID, owner.ID, true)
	if err != nil || stable.Revision != blocked.Revision {
		t.Fatal("unchanged parked work churns review", stable, blocked, err)
	}
	// Only verified primary bytes complete the request; existing work is retained.
	if err = os.WriteFile(file, []byte("primary returns"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = db.UpsertFile(ctx, itemID, 0, file, 15); err != nil {
		t.Fatal(err)
	}
	if err = restarted.processTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	complete, err := db.HouseholdRequest(ctx, requestID, owner.ID, true)
	if err != nil || complete.State != "fulfilled" {
		t.Fatal(complete, err)
	}
	retained, err := db.GetDownload(ctx, ownerDownload)
	if err != nil || retained.ID != ownerDownload {
		t.Fatal("completion removed original owner work", retained, err)
	}
	var count int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM downloads`).Scan(&count); err != nil || count != 1 {
		t.Fatal("replacement work appeared", count, err)
	}
}

func TestHouseholdWorkerWaitsForSourcesAndFencesChangedLibrary(t *testing.T) {
	for _, scenario := range []string{"no indexers", "volume absent", "policy changed", "primary arrived"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			db, err := sqlite.Open(sqlitetest.Dir(t))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err = db.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			b := bus.New(nil)
			defer b.Close()
			lib := library.New(db, nil, b, nil)
			acq := acquisition.New(db, b, nil, nil, nil)
			service := New(db, lib, acq)
			root, err := lib.AddRootFolder(ctx, t.TempDir(), domain.KindMixed)
			if err != nil {
				t.Fatal(err)
			}
			folder := filepath.Join(root.Path, "Movie")
			if err = os.Mkdir(folder, 0700); err != nil {
				t.Fatal(err)
			}
			id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Movie", IDs: domain.ExternalIDs{TMDB: 550}, RootFolderID: root.ID, QualityProfileID: 1, Path: folder})
			if err != nil {
				t.Fatal(err)
			}
			if err = lib.VerifyPrimaryPresence(ctx, id); err != nil {
				t.Fatal(err)
			}
			item, err := db.GetMediaItemFull(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			name, hash, required := "owner", "hash", true
			if err = db.ConfigureLocalOwner(ctx, &name, &hash, &required); err != nil {
				t.Fatal(err)
			}
			owner, err := db.AccountByUsername(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			digest := "worker-session"
			if err = db.CreateAccountSession(ctx, digest, owner, "test", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			rid, err := db.SubmitHouseholdRequest(ctx, owner, digest, domain.HouseholdRequest{Kind: item.Kind, TMDBID: 550, IDs: item.IDs, Title: item.Title, Fingerprint: "worker"})
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
			profile, err := db.HouseholdProfileFingerprint(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			policy := domain.HouseholdPolicy{Kind: item.Kind, TMDBID: 550, IDs: item.IDs, RootID: root.ID, ProfileID: 1, ProfileFingerprint: profile, RootPath: evidence.RootPath, RootIdentity: evidence.RootIdentity, RootGeneration: evidence.RootGeneration, SharedRevision: evidence.SharedRevision, Path: item.Path}
			if err = db.ApproveHouseholdRequest(ctx, owner, digest, sqlite.HouseholdApproval{RequestID: rid, RequestRevision: request.Revision, Item: item, Evidence: evidence, Policy: policy, Missing: []domain.RequestEpisode{{}}}); err != nil {
				t.Fatal(err)
			}
			targets, err := db.HouseholdPendingTargets(ctx)
			if err != nil || len(targets) != 1 {
				t.Fatal(targets, err)
			}
			target := targets[0]
			switch scenario {
			case "volume absent":
				if err = os.Rename(root.Path, root.Path+"-offline"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if e := os.Rename(root.Path+"-offline", root.Path); e != nil {
						t.Error(e)
					}
				})
			case "policy changed":
				if _, err = db.W.ExecContext(ctx, `UPDATE quality_profiles SET name='Changed policy' WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			case "primary arrived":
				file := filepath.Join(folder, "arrived.mkv")
				if err = os.WriteFile(file, []byte("primary"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err = db.UpsertFile(ctx, id, 0, file, 7); err != nil {
					t.Fatal(err)
				}
			}
			// Exercise the actual startup wake, not a fake interval or a 30-second sleep.
			run, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); service.Run(run) }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				got, e := db.HouseholdRequest(ctx, rid, owner.ID, true)
				if e != nil {
					cancel()
					<-done
					t.Fatal(e)
				}
				if got.PublicNote != "" || got.State == "fulfilled" {
					break
				}
				if time.Now().After(deadline) {
					cancel()
					<-done
					t.Fatal("startup worker did not process durable request", got)
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			<-done
			got, err := db.HouseholdRequest(ctx, rid, owner.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "no indexers":
				if got.State != "approved" || !strings.Contains(got.PublicNote, "configured sources") {
					t.Fatal(got)
				}
			case "volume absent", "policy changed":
				if got.State != "needs_review" {
					t.Fatal(got)
				}
			case "primary arrived":
				if got.State != "fulfilled" {
					t.Fatal(got)
				}
			}
			// Unavailable dependencies and changed policy cannot create fresh work.
			var count int
			if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM downloads`).Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
			current, err := db.HouseholdTarget(ctx, target.ID)
			if err != nil || current.DownloadID != 0 {
				t.Fatal(current, err)
			}
			if err = New(db, lib, nil).Process(ctx); err != nil {
				t.Fatal("disabled acquisition should idle", err)
			}
		})
	}
}
