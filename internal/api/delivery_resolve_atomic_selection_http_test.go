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

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestDeliveryResolveRejectsMixedManifestSelectionWithoutPartialAdmission(t *testing.T) {
	e := newAPIEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.acq.StartImporters(ctx)
	t.Cleanup(func() { cancel(); e.acq.WaitImporters() })
	clientID := acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	downloadID := acqGrab(t, e, itemID)
	r := apiDeliveryReceipt(t, e, clientID, downloadID, "delivery-api-atomic-selection")
	selected := "Season 01/épisode.mkv"
	payload := []byte("accepted manifest payload is unchanged by a rejected mixed selection")
	manifestPath := filepath.Join(r.MappedPath, filepath.FromSlash(selected))
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.db.PrepareDeliveryAcceptance(ctx, sqlite.DeliveryReceipt{
		ID: r.ID, ConsumerReceiptID: "curator-receipt-atomic-selection", NextAttemptAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	prepared, err := e.db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetDeliveryAccepted(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	acceptedSnapshot, err := e.db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}

	endpoint := fmt.Sprintf("/api/v1/downloads/deliveries/%d/import", r.ID)
	mixed := fmt.Sprintf(`{"paths":[%q,"Season 01/unmanifested.mkv"]}`, selected)
	rejected := e.post(t, endpoint, mixed).expect(t, http.StatusConflict)
	if !strings.Contains(rejected.Body.String(), "exact relative path") {
		t.Fatalf("mixed selection conflict did not explain the accepted-manifest boundary: %s", rejected.Body.String())
	}
	if got, err := os.ReadFile(manifestPath); err != nil || string(got) != string(payload) {
		t.Fatalf("rejected mixed selection changed accepted source bytes: %q err=%v", got, err)
	}
	stored, err := e.db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil || stored.State != acceptedSnapshot.State || stored.AckState != acceptedSnapshot.AckState || stored.ConsumerReceiptID != acceptedSnapshot.ConsumerReceiptID {
		t.Fatalf("rejected mixed selection changed durable receipt state: before=%+v after=%+v err=%v", acceptedSnapshot, stored, err)
	}
	dl, err := e.db.GetDownload(ctx, downloadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range dl.Handoff {
		if entry.Step == "manual_import_queued" {
			t.Fatalf("mixed selection partially queued a valid manifest path: %+v", entry)
		}
	}

	// The same durable receipt remains resolvable when the operator selects
	// only the exact path accepted by Runner's manifest.
	e.post(t, endpoint, fmt.Sprintf(`{"paths":[%q]}`, selected)).expect(t, http.StatusAccepted)
	dl, err = e.db.GetDownload(ctx, downloadID)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range dl.Handoff {
		if entry.Step == "manual_import_queued" {
			if len(entry.Paths) != 1 || entry.Paths[0] != manifestPath {
				t.Fatalf("exact retry queued paths outside the operator's selection: %+v", entry)
			}
			return
		}
	}
	t.Fatalf("exact manifest-only retry was not durably queued: %+v", dl.Handoff)
}
