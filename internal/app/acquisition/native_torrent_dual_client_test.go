package acquisition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	runner "github.com/pjunod/monarr/internal/adapters/nzbd"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestDualRunnerModesKeepStatusFallbackAndProvenDeliveryOwnershipSeparate(t *testing.T) {
	ctx := t.Context()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("payload"))
	instance := "same-runner"
	afterQueries := []string{}
	v := map[string]any{"protocolVersion": 1, "sequence": 1, "deliveryId": "owned-usenet-delivery", "publicationGeneration": "publication", "artifactId": "artifact", "sourceArtifactId": "source", "sourceGeneration": "source-generation", "jobId": 17, "runnerInstanceId": instance, "requestToken": "usenet-token", "publicationPath": t.TempDir(), "manifestDigest": hex.EncodeToString(digest[:]), "readyAt": time.Now().Unix(), "terminalOutcome": "SUCCESS", "files": []any{map[string]any{"path": "movie.mkv", "sizeBytes": 7, "sha256": hex.EncodeToString(digest[:])}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobs":
			_, _ = w.Write([]byte(`{"jobs":[{"id":17,"kind":"nzb","name":"Usenet.Release","status":"downloading","params":[["monarr-transfer","usenet-token"]]},{"id":18,"kind":"torrent","torrent_role":"seed","name":"Native.Same.Title","status":"completed"},{"id":19,"kind":"torrent","torrent_role":"acquisition","name":"Native.Same.Title","status":"downloading","params":[["monarr-transfer","native-token"]]}]}`))
		case "/api/v1/history":
			_, _ = w.Write([]byte(`{"entries":[]}`))
		case "/api/v1/deliveries":
			after := r.URL.Query().Get("after")
			afterQueries = append(afterQueries, after)
			if after == "0" {
				_ = json.NewEncoder(w).Encode(map[string]any{"protocolVersion": 1, "deliveries": []any{v}, "nextCursor": v["sequence"]})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"protocolVersion": 1, "deliveries": []any{}, "nextCursor": v["sequence"]})
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	usenet := ports.ClientConfig{Type: "nzbd", Name: "Usenet", URL: server.URL, Enabled: true, ManualApproval: true}
	usenet.ID, err = db.AddDownloadClient(ctx, usenet)
	if err != nil {
		t.Fatal(err)
	}
	native := ports.ClientConfig{Type: "runner-torrent", Name: "Native", URL: server.URL, Enabled: true, ManualApproval: true}
	native.ID, err = db.AddDownloadClient(ctx, native)
	if err != nil {
		t.Fatal(err)
	}
	usenetClient, nativeClient := runner.New(usenet), runner.NewTorrent(native)
	usenetStatuses, err := usenetClient.Statuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := matchStatus(sqlite.Download{ReleaseTitle: "Native.Same.Title"}, usenetStatuses); ok {
		t.Fatal("Usenet legacy title fallback matched native child/donor")
	}
	nativeStatuses, err := nativeClient.Statuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := matchStatus(sqlite.Download{ReleaseTitle: "Usenet.Release"}, nativeStatuses); ok {
		t.Fatal("native status fallback matched Usenet")
	}
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Dual mode", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: item, ClientID: usenet.ID, Handle: "17", Transfer: "usenet-token", DeliveryExpected: true, Protocol: "usenet", State: "grabbed", ReleaseTitle: "Usenet.Release"})
	if err != nil {
		t.Fatal(err)
	}
	delivery := ports.RunnerDelivery{JobID: "17", RequestToken: dl.Transfer, RunnerInstanceID: instance}
	if skip, err := db.DeliveryBelongsToOtherRunnerMode(ctx, native, delivery); err != nil || skip {
		t.Fatalf("unpinned admission skipped: %v %v", skip, err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(db, bus.New(log), log, nil, nil)
	svc.discoverRunnerDeliveries(ctx, native, nativeClient)
	page, _, err := db.DeliveryReceiptPage(ctx, 0, 100)
	if err != nil || len(page) != 1 || page[0].OwnedByClientID != 0 || page[0].State != "unresolved" {
		t.Fatalf("unbound first discovery=%+v %v", page, err)
	}
	duplicateID := page[0].ID
	if err := db.BindRunnerAdmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, instance, "admission-generation"); err != nil {
		t.Fatal(err)
	}
	svc.discoverRunnerDeliveries(ctx, native, nativeClient)
	duplicate, err := db.DeliveryReceiptByID(ctx, duplicateID)
	if err != nil || duplicate.OwnedByClientID != usenet.ID || duplicate.State != "conflict" || duplicate.AckState != "not_accepted" || duplicate.DownloadID != 0 {
		t.Fatalf("resolved duplicate view=%+v %v", duplicate, err)
	}
	projected, err := svc.DeliveryLifecycleByID(ctx, duplicateID)
	if err != nil || projected.State != "owned_by_other_client" || projected.OwnedByClientID != usenet.ID {
		t.Fatalf("public duplicate=%+v %v", projected, err)
	}
	for _, action := range []func() error{func() error { return svc.RetryDeliveryHandoff(ctx, duplicateID) }, func() error { return svc.RetryRunnerSourceCleanup(ctx, duplicateID) }, func() error { return svc.RetryCuratorDeliveryCleanup(ctx, duplicateID) }, func() error { return svc.QueueDeliveryImport(ctx, duplicateID, []string{"movie.mkv"}) }, func() error { return svc.AbandonDeliveryDeleteRequest(ctx, duplicateID, "anything") }} {
		if err := action(); err == nil || !strings.Contains(err.Error(), "another configured Runner client") {
			t.Fatalf("read-only direct action=%v", err)
		}
	}
	var count int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_delivery_receipts WHERE client_id=?`, native.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("view not retained exactly once: %d %v", count, err)
	}
	if len(afterQueries) < 2 || afterQueries[0] != "0" || afterQueries[1] != "1" {
		t.Fatalf("discovery did not progress trusted cursor: %v", afterQueries)
	}
	v["runnerInstanceId"] = "unknown-runner"
	v["deliveryId"] = "unknown-delivery"
	v["sequence"] = 2
	svc.discoverRunnerDeliveries(ctx, native, nativeClient)
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_delivery_receipts WHERE client_id=? AND owned_by_client_id=0 AND state IN ('unresolved','conflict')`, native.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unknown ownership not retained for review: %d %v", count, err)
	}
	v["runnerInstanceId"] = instance
	v["deliveryId"] = "owned-usenet-delivery"
	v["sequence"] = 1
	svc.discoverRunnerDeliveries(ctx, usenet, usenetClient)
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_delivery_receipts WHERE client_id=? AND job_id=?`, usenet.ID, strconv.Itoa(17)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("owning protocol did not retain its receipt: %d %v", count, err)
	}
	// A proven association or accepted custody is never rewritten as a
	// duplicate view, even when the other mode's admission is now pinned.
	for _, proof := range []struct{ name, sql string }{
		{"associated", "UPDATE runner_delivery_receipts SET download_id=?,generation_key=?,download_handle=? WHERE id=?"},
		{"accepted", "UPDATE runner_delivery_receipts SET ack_state='accepted',consumer_receipt_id='durable-consumer' WHERE id=?"},
	} {
		t.Run(proof.name, func(t *testing.T) {
			protectedDelivery := duplicate.Delivery
			protectedDelivery.DeliveryID += "-" + proof.name
			protected, err := db.SaveDiscoveredDelivery(ctx, native.ID, protectedDelivery, protectedDelivery.PublicationPath)
			if err != nil {
				t.Fatal(err)
			}
			if proof.name == "associated" {
				_, err = db.W.ExecContext(ctx, proof.sql, dl.ID, dl.GenerationKey, dl.Handle, protected.ID)
			} else {
				_, err = db.W.ExecContext(ctx, proof.sql, protected.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := db.DeliveryReceiptByID(ctx, protected.ID)
			if err != nil {
				t.Fatal(err)
			}
			skip, err := db.ReconcileOtherRunnerModeView(ctx, native, protectedDelivery)
			if err != nil || skip {
				t.Fatalf("protected receipt superseded: %v %v", skip, err)
			}
			after, err := db.DeliveryReceiptByID(ctx, protected.ID)
			if err != nil || after.OwnedByClientID != 0 || after.State != before.State || after.AckState != before.AckState || after.ConsumerReceiptID != before.ConsumerReceiptID || after.DownloadID != before.DownloadID || after.GenerationKey != before.GenerationKey {
				t.Fatalf("custody changed: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}
