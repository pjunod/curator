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
}
