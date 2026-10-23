package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type apiDeliveryProtocolClient struct {
	fakeClient
	delivery       ports.RunnerDelivery
	accepted       int
	sourceRetries  int
	sourceProgress string
	sourceRetryErr error
}

type apiDeliveryRetryDeadlineError struct{ deadline time.Time }

func (e apiDeliveryRetryDeadlineError) Error() string {
	return "Runner cleanup service is temporarily unavailable"
}
func (e apiDeliveryRetryDeadlineError) RetryDeadline() time.Time { return e.deadline }

func (c *apiDeliveryProtocolClient) ListDeliveries(context.Context, uint64, int) (ports.RunnerDeliveryPage, error) {
	return ports.RunnerDeliveryPage{ProtocolVersion: 1}, nil
}

func (c *apiDeliveryProtocolClient) AcceptDelivery(context.Context, string, ports.RunnerDeliveryAcceptance) error {
	c.accepted++
	return nil
}

func (c *apiDeliveryProtocolClient) RetryDeliverySourceCleanup(_ context.Context, deliveryID string) (ports.RunnerDelivery, error) {
	c.sourceRetries++
	if deliveryID != c.delivery.DeliveryID {
		return ports.RunnerDelivery{}, fmt.Errorf("unexpected delivery %q", deliveryID)
	}
	if c.sourceRetryErr != nil {
		return ports.RunnerDelivery{}, c.sourceRetryErr
	}
	c.delivery.SourceCleanup = json.RawMessage(c.sourceProgress)
	return c.delivery, nil
}

// apiDeliveryReceipt seeds the durable handoff ledger and binds it to a real
// Activity row, as the background discovery worker does before advancing its
// Runner cursor.
func apiDeliveryReceipt(t *testing.T, e *apiEnv, clientID, downloadID int64, deliveryID string) sqlite.DeliveryReceipt {
	t.Helper()
	ctx := context.Background()
	dl, err := e.db.GetDownload(ctx, downloadID)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("verified episode payload")
	digest := sha256.Sum256(data)
	delivery := ports.RunnerDelivery{
		ProtocolVersion: 1, Sequence: uint64(downloadID), DeliveryID: deliveryID,
		PublicationGeneration: "publication-" + deliveryID,
		SourceArtifactID:      "source-" + deliveryID, SourceGeneration: "source-generation-" + deliveryID,
		ArtifactID: "artifact-" + deliveryID, JobID: dl.Handle, RequestToken: dl.Transfer,
		RunnerInstanceID: "runner-api-test", PublicationPath: "/runner/completed/" + deliveryID,
		ManifestDigest:  "manifest-" + deliveryID,
		Files:           []ports.RunnerFile{{Path: "Season 01/épisode.mkv", SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}},
		TerminalOutcome: "SUCCESS", ReadyAt: time.Now().UTC(),
	}
	r, err := e.db.SaveDiscoveredDelivery(ctx, clientID, delivery, filepath.Join(t.TempDir(), "completed", deliveryID))
	if err != nil {
		t.Fatal(err)
	}
	r.DownloadID, r.GenerationKey, r.DownloadHandle = dl.ID, dl.GenerationKey, dl.Handle
	r.MediaItemID, r.CopyID = dl.MediaItemID, dl.CopyID
	if err := e.db.AssociateDelivery(ctx, r); err != nil {
		t.Fatal(err)
	}
	persisted, err := e.db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return persisted
}

func TestDeliveryLifecycleHTTPPagesFiltersAndReadsDurableReceipt(t *testing.T) {
	e := newAPIEnv(t)
	clientID := acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	firstDownload := acqGrab(t, e, itemID)
	secondDownload := gapGrabTitled(t, e, itemID, "Fight.Club.1999.1080p.WEB-DL.x264-SECOND")
	first := apiDeliveryReceipt(t, e, clientID, firstDownload, "delivery-api-1")
	second := apiDeliveryReceipt(t, e, clientID, secondDownload, "delivery-api-2")

	var page apigen.DeliveryLifecyclePage
	e.get(t, "/api/v1/downloads/deliveries?limit=1").expect(t, http.StatusOK).into(t, &page)
	if len(page.Items) != 1 || page.Items[0].ReceiptId != second.ID || page.NextBeforeId == nil || *page.NextBeforeId != second.ID {
		t.Fatalf("first page = %+v, want newest durable receipt and its beforeId cursor", page)
	}
	var older apigen.DeliveryLifecyclePage
	e.get(t, fmt.Sprintf("/api/v1/downloads/deliveries?limit=1&beforeId=%d", *page.NextBeforeId)).expect(t, http.StatusOK).into(t, &older)
	if len(older.Items) != 1 || older.Items[0].ReceiptId != first.ID || older.NextBeforeId != nil {
		t.Fatalf("older page = %+v, want first receipt and no further cursor", older)
	}
	var filtered apigen.DeliveryLifecyclePage
	e.get(t, fmt.Sprintf("/api/v1/downloads/deliveries?downloadId=%d", firstDownload)).expect(t, http.StatusOK).into(t, &filtered)
	if len(filtered.Items) != 1 || filtered.Items[0].ReceiptId != first.ID {
		t.Fatalf("download-filtered page = %+v, want only receipt %d", filtered, first.ID)
	}

	var detail apigen.DeliveryLifecycleReceipt
	e.get(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d", first.ID)).expect(t, http.StatusOK).into(t, &detail)
	if detail.DeliveryId != "delivery-api-1" || detail.PublicationGeneration != "publication-delivery-api-1" || detail.JobId == nil || *detail.JobId == "" || detail.RequestToken == nil || *detail.RequestToken == "" {
		t.Fatalf("receipt detail omitted stable handoff identity: %+v", detail)
	}
	if len(detail.ManifestFiles) != 1 || detail.ManifestFiles[0].Path != "Season 01/épisode.mkv" || detail.ManifestFiles[0].SizeBytes == 0 {
		t.Fatalf("manifest detail = %+v, want exact Unicode relative path and size", detail.ManifestFiles)
	}
	var raw map[string]json.RawMessage
	e.get(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d", first.ID)).into(t, &raw)
	var attempts []json.RawMessage
	if err := json.Unmarshal(raw["attemptHistory"], &attempts); err != nil || attempts == nil {
		t.Fatalf("attemptHistory must be an array on the wire; raw=%s err=%v", raw["attemptHistory"], err)
	}
}

func TestDeliveryLifecycleHTTPRejectsBadLimitsMissingReceiptsAndInvalidResolvePath(t *testing.T) {
	e := newAPIEnv(t)
	clientID := acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	downloadID := acqGrab(t, e, itemID)
	r := apiDeliveryReceipt(t, e, clientID, downloadID, "delivery-api-unaccepted")

	e.get(t, "/api/v1/downloads/deliveries?limit=0").expect(t, http.StatusBadRequest)
	e.get(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d", r.ID+1000)).expect(t, http.StatusNotFound)
	e.get(t, "/api/v1/downloads/deliveries/not-an-id").expect(t, http.StatusBadRequest)
	e.post(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d/import", r.ID), `{`).expect(t, http.StatusBadRequest)
	e.post(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d/import", r.ID), `{"paths":[]}`).expect(t, http.StatusBadRequest)
	conflict := e.post(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d/import", r.ID), `{"paths":["../outside.mkv"]}`).expect(t, http.StatusConflict)
	if !strings.Contains(conflict.Body.String(), "relative path") && !strings.Contains(conflict.Body.String(), "manifest") {
		t.Fatalf("invalid selection conflict did not explain the manifest boundary: %s", conflict.Body.String())
	}
	var stored sqlite.DeliveryReceipt
	var err error
	stored, err = e.db.DeliveryReceiptByID(context.Background(), r.ID)
	if err != nil || stored.AckState != "not_accepted" {
		t.Fatalf("bad Resolve request changed acceptance state: receipt=%+v err=%v", stored, err)
	}
}

func TestDeliveryLifecycleHTTPReportsUnavailableDependency(t *testing.T) {
	e := newAPIEnv(t)
	e.srv.deps.Acquisition = nil
	e.h = e.srv.Handler()
	e.get(t, "/api/v1/downloads/deliveries").expect(t, http.StatusServiceUnavailable)
	e.get(t, "/api/v1/downloads/deliveries/1").expect(t, http.StatusServiceUnavailable)
	for _, suffix := range []string{"retry-handoff", "retry-runner-cleanup", "retry-curator-cleanup", "import"} {
		body := "{}"
		if suffix == "import" {
			body = `{"paths":["episode.mkv"]}`
		}
		e.post(t, "/api/v1/downloads/deliveries/1/"+suffix, body).expect(t, http.StatusServiceUnavailable)
	}
}

func TestDeliveryLifecycleRetryRoutesReturnConflictsForUnreadyOrUnsupportedWork(t *testing.T) {
	e := newAPIEnv(t)
	clientID := acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	downloadID := acqGrab(t, e, itemID)
	r := apiDeliveryReceipt(t, e, clientID, downloadID, "delivery-api-retry")
	base := fmt.Sprintf("/api/v1/downloads/deliveries/%d", r.ID)
	// The configured client is the ordinary fake download client, so it does
	// not implement Runner delivery extensions. Each retry route must refuse
	// with a conflict instead of reporting work as completed.
	e.post(t, base+"/retry-handoff", `{}`).expect(t, http.StatusConflict)
	e.post(t, base+"/retry-runner-cleanup", `{}`).expect(t, http.StatusConflict)
	e.post(t, base+"/retry-curator-cleanup", `{}`).expect(t, http.StatusConflict)
	for _, endpoint := range []string{"retry-handoff", "retry-runner-cleanup", "retry-curator-cleanup"} {
		e.post(t, "/api/v1/downloads/deliveries/999999/"+endpoint, `{}`).expect(t, http.StatusConflict)
	}
}

func TestDeliveryLifecycleRetryHandoffAndRunnerCleanupReturnAcceptedAfterProtocolCall(t *testing.T) {
	e := newAPIEnv(t)
	clientID := acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	firstDownload := acqGrab(t, e, itemID)
	secondDownload := gapGrabTitled(t, e, itemID, "Fight.Club.1999.1080p.WEB-DL.x264-CLEANUP")
	handoff := apiDeliveryReceipt(t, e, clientID, firstDownload, "delivery-api-retry-handoff")
	cleanup := apiDeliveryReceipt(t, e, clientID, secondDownload, "delivery-api-retry-source")
	if err := e.db.PrepareDeliveryAcceptance(context.Background(), sqlite.DeliveryReceipt{
		ID: cleanup.ID, ConsumerReceiptID: "curator-accepted-source-retry", NextAttemptAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	prepared, err := e.db.DeliveryReceiptByID(context.Background(), cleanup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetDeliveryAccepted(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}

	protocol := &apiDeliveryProtocolClient{fakeClient: fakeClient{}, delivery: cleanup.Delivery, sourceProgress: `{"status":"succeeded"}`}
	service := acquisition.New(e.db, e.bus, nil, nil, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	e.srv.deps.Acquisition = service
	e.acq = service
	e.h = e.srv.Handler()

	e.post(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d/retry-handoff", handoff.ID), `{}`).expect(t, http.StatusAccepted)
	if protocol.accepted != 0 {
		t.Fatalf("handoff retry acknowledged an unimported delivery unexpectedly: %d", protocol.accepted)
	}
	e.post(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d/retry-runner-cleanup", cleanup.ID), `{}`).expect(t, http.StatusAccepted)
	if protocol.sourceRetries != 1 {
		t.Fatalf("Runner source-cleanup endpoint made %d protocol calls, want 1", protocol.sourceRetries)
	}
	var detail apigen.DeliveryLifecycleReceipt
	e.get(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d", cleanup.ID)).expect(t, http.StatusOK).into(t, &detail)
	if detail.SourceCleanup == nil || (*detail.SourceCleanup)["status"] != "succeeded" {
		t.Fatalf("cleanup retry progress was not refreshed in the durable receipt: %+v", detail.SourceCleanup)
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	service.StartImporters(workerCtx)
	t.Cleanup(func() { cancel(); service.WaitImporters() })
	e.post(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d/retry-curator-cleanup", handoff.ID), `{}`).expect(t, http.StatusAccepted)
}

func TestDeliveryLifecycleRunnerCleanupHTTPPersistsFailureAndHonorsRetryAfter(t *testing.T) {
	e := newAPIEnv(t)
	clientID := acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	downloadID := acqGrab(t, e, itemID)
	r := apiDeliveryReceipt(t, e, clientID, downloadID, "delivery-api-retry-deadline")
	if err := e.db.PrepareDeliveryAcceptance(context.Background(), sqlite.DeliveryReceipt{
		ID: r.ID, ConsumerReceiptID: "curator-receipt-retry-deadline", NextAttemptAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	prepared, err := e.db.DeliveryReceiptByID(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetDeliveryAccepted(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	protocol := &apiDeliveryProtocolClient{fakeClient: fakeClient{}, delivery: r.Delivery, sourceRetryErr: apiDeliveryRetryDeadlineError{deadline}}
	service := acquisition.New(e.db, e.bus, nil, nil, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	e.srv.deps.Acquisition = service
	e.acq = service
	e.h = e.srv.Handler()

	base := fmt.Sprintf("/api/v1/downloads/deliveries/%d", r.ID)
	e.post(t, base+"/retry-runner-cleanup", `{}`).expect(t, http.StatusConflict)
	if protocol.sourceRetries != 1 {
		t.Fatalf("first cleanup retry made %d calls, want exactly one", protocol.sourceRetries)
	}
	var detail apigen.DeliveryLifecycleReceipt
	e.get(t, base).expect(t, http.StatusOK).into(t, &detail)
	if detail.SourceCleanupAttempts == nil || *detail.SourceCleanupAttempts != 1 || detail.SourceCleanupError == nil || *detail.SourceCleanupError != protocol.sourceRetryErr.Error() {
		t.Fatalf("failed cleanup attempt was not durably exposed: attempts=%v error=%v", detail.SourceCleanupAttempts, detail.SourceCleanupError)
	}
	if detail.SourceCleanupNextAttemptAt == nil || detail.SourceCleanupNextAttemptAt.Before(deadline) {
		t.Fatalf("persisted retry deadline %v precedes provider Retry-After %s", detail.SourceCleanupNextAttemptAt, deadline)
	}

	// An explicit second click must not retry ahead of Runner's deadline. This
	// verifies the HTTP route reads the durable deadline instead of relying on
	// an in-process timer or client-side button state.
	e.post(t, base+"/retry-runner-cleanup", `{}`).expect(t, http.StatusConflict)
	if protocol.sourceRetries != 1 {
		t.Fatalf("retry before Retry-After deadline reached Runner %d times", protocol.sourceRetries)
	}
}

func TestDeliveryLifecycleResolveQueuesOnlySelectedDurableManifestPath(t *testing.T) {
	e := newAPIEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.acq.StartImporters(ctx)
	t.Cleanup(func() { cancel(); e.acq.WaitImporters() })
	clientID := acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	downloadID := acqGrab(t, e, itemID)
	r := apiDeliveryReceipt(t, e, clientID, downloadID, "delivery-api-selected-resolve")
	selected := "Season 01/épisode.mkv"
	if err := os.MkdirAll(filepath.Join(r.MappedPath, "Season 01"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.MappedPath, filepath.FromSlash(selected)), []byte("verified episode payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.db.PrepareDeliveryAcceptance(ctx, sqlite.DeliveryReceipt{
		ID: r.ID, ConsumerReceiptID: "curator-receipt-selected", NextAttemptAt: time.Now(),
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
	response := e.post(t, fmt.Sprintf("/api/v1/downloads/deliveries/%d/import", r.ID), fmt.Sprintf(`{"paths":[%q]}`, selected)).expect(t, http.StatusAccepted)
	if response.Body.Len() != 0 {
		t.Fatalf("accepted selection response should be an asynchronous acknowledgement, got %q", response.Body.String())
	}
	dl, err := e.db.GetDownload(ctx, downloadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dl.Handoff) == 0 || dl.Handoff[len(dl.Handoff)-1].Step != "manual_import_queued" || len(dl.Handoff[len(dl.Handoff)-1].Paths) != 1 || dl.Handoff[len(dl.Handoff)-1].Paths[0] != filepath.Join(r.MappedPath, filepath.FromSlash(selected)) {
		t.Fatalf("Resolve did not persist exactly the selected manifest path: %+v", dl.Handoff)
	}
	persisted, err := e.db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil || persisted.AckState != "accepted" || persisted.ConsumerReceiptID != "curator-receipt-selected" {
		t.Fatalf("selected import disturbed durable Runner acceptance: %+v err=%v", persisted, err)
	}
	// The activity row records a bounded handoff, not a synchronous claim that
	// import completed successfully; the real worker will stamp its outcome.
}
