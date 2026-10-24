package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
)

func TestDeliveryLifecycleResolveRefusesUnpreparedPendingAcceptance(t *testing.T) {
	e := newAPIEnv(t)
	clientID := acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	downloadID := acqGrab(t, e, itemID)
	r := apiDeliveryReceipt(t, e, clientID, downloadID, "delivery-api-pending-ack-unprepared")
	ctx := context.Background()

	// Model an incomplete durable receipt in which acceptance is pending but
	// no consumer receipt id is present. Resolve must fail closed until the
	// handoff can be recovered.
	if _, err := e.db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET ack_state='pending', consumer_receipt_id='' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	before, err := e.db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil || before.AckState != "pending" || before.ConsumerReceiptID != "" {
		t.Fatalf("fixture did not preserve an unresolved acknowledgement: receipt=%+v err=%v", before, err)
	}

	endpoint := fmt.Sprintf("/api/v1/downloads/deliveries/%d/import", r.ID)
	response := e.post(t, endpoint, `{"paths":["Season 01/épisode.mkv"]}`).expect(t, http.StatusConflict)
	if !strings.Contains(response.Body.String(), "acceptance is not durably prepared") {
		t.Fatalf("Resolve conflict did not explain the missing durable acceptance: %s", response.Body.String())
	}

	var detail apigen.DeliveryLifecycleReceipt
	e.get(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d", r.ID)).expect(t, http.StatusOK).into(t, &detail)
	if detail.Acceptance != "pending" || detail.ConsumerReceiptId != nil {
		t.Fatalf("rejected Resolve changed the durable acceptance: %+v", detail)
	}
	after, err := e.db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil || after.AckState != "pending" || after.ConsumerReceiptID != "" {
		t.Fatalf("rejected Resolve changed the receipt ledger: receipt=%+v err=%v", after, err)
	}
	download, err := e.db.GetDownload(ctx, downloadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range download.Handoff {
		if entry.Step == "manual_import_queued" {
			t.Fatalf("rejected Resolve created an Activity import handoff: %+v", download.Handoff)
		}
	}
}

func TestQueueImportHTTPRecoversFailedImportAfterPayloadPathIsRepaired(t *testing.T) {
	e := newAPIEnv(t)
	acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	downloadID := acqGrab(t, e, itemID)
	ctx := context.Background()

	item, err := e.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(item.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	dl, err := e.db.GetDownload(ctx, downloadID)
	if err != nil {
		t.Fatal(err)
	}
	expectedDirectory := item.Path
	if dl.CopyID != 0 {
		copy, err := e.db.GetMediaCopy(ctx, itemID, dl.CopyID)
		if err != nil {
			t.Fatal(err)
		}
		if copy.Path != "" {
			expectedDirectory = copy.Path
		}
	}
	completed := t.TempDir()
	payload := filepath.Join(completed, "Fight.Club.1999.1080p.WEB-DL.x264-TEST.mkv")
	if err := os.WriteFile(payload, []byte("recovered completed movie payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Make the target read-only so the real importer records an environmental
	// failure. Activity should expose its durable retry deadline; after the
	// operator repairs the target, the same row can be retried through HTTP.
	if err := os.MkdirAll(expectedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(expectedDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	dl.SavePath, dl.ImportPath = completed, completed
	if err := e.db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}

	failed := e.post(t, fmt.Sprintf("/api/v1/queue/%d/import", downloadID), "").expect(t, http.StatusBadRequest)
	if !strings.Contains(failed.Body.String(), "import stopped") {
		t.Fatalf("failed import did not identify the environmental blocker: %s", failed.Body.String())
	}
	var retryQueue []apigen.QueueItem
	e.get(t, "/api/v1/queue?filter=failed").expect(t, http.StatusOK).into(t, &retryQueue)
	if len(retryQueue) != 1 || retryQueue[0].Id != downloadID || retryQueue[0].ImportRetryAttempts == nil || *retryQueue[0].ImportRetryAttempts != 0 || retryQueue[0].ImportBlockedSince == nil || retryQueue[0].ImportRetryAt == nil {
		t.Fatalf("Activity omitted durable blocked-import retry details: %+v", retryQueue)
	}
	if retryQueue[0].ImportRetryAt.Before(retryQueue[0].ImportBlockedSince.Add(29 * time.Second)) {
		t.Fatalf("retry deadline %s is not based on the blocked-since time %s", retryQueue[0].ImportRetryAt, retryQueue[0].ImportBlockedSince)
	}
	if err := os.Chmod(expectedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}

	e.post(t, fmt.Sprintf("/api/v1/queue/%d/import", downloadID), "").expect(t, http.StatusNoContent)

	var queue []apigen.QueueItem
	e.get(t, "/api/v1/queue?filter=imported").expect(t, http.StatusOK).into(t, &queue)
	if len(queue) != 1 || queue[0].Id != downloadID || queue[0].State != "imported" || queue[0].Error != nil {
		t.Fatalf("successful import retry was not durable in Activity: %+v", queue)
	}
	files, err := e.db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("recovered import recorded %d library files, want one: %+v", len(files), files)
	}
	placed := files[0].Path
	if filepath.Dir(placed) != expectedDirectory {
		t.Fatalf("recovered payload was not placed under its requested target: %q (target %q)", placed, expectedDirectory)
	}
	if contents, err := os.ReadFile(placed); err != nil || string(contents) != "recovered completed movie payload" {
		t.Fatalf("placed library file does not contain the completed payload: %q err=%v", contents, err)
	}
	updated, err := e.db.GetDownload(ctx, downloadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Handoff) == 0 || updated.Handoff[len(updated.Handoff)-1].Step != "imported" {
		t.Fatalf("successful retry did not append its durable import outcome: %+v", updated.Handoff)
	}
}
