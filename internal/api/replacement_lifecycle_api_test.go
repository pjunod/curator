package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
)

// These tests enter through HTTP and leave the durable SQLite records behind
// the API. The replacement observation worker is real; only the download
// client and external providers remain faked by apiEnv.
func TestReplacementObservationHTTPQueuesReplaysAndExposesWorkerResult(t *testing.T) {
	e := newAPIEnv(t)

	itemID := e.addMovie(t)
	item, err := e.db.GetMediaItemFull(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(item.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "reported-media.mkv")
	if err := os.WriteFile(path, []byte("known observed payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileID, err := e.db.UpsertFile(context.Background(), itemID, 0, path, int64(len("known observed payload")))
	if err != nil {
		t.Fatal(err)
	}
	files, err := e.db.ListFilesForItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	var generation string
	for _, file := range files {
		if file.ID == fileID {
			generation = file.FileGeneration
		}
	}
	if generation == "" {
		t.Fatal("fixture media file has no durable generation")
	}

	postPath := fmt.Sprintf("/api/v1/library/%d/files/%d/replacement-observations", itemID, fileID)
	body := fmt.Sprintf(`{"fileGeneration":%q,"requestId":"api-observe-replacement-1"}`, generation)
	var queued apigen.ReplacementObservation
	e.post(t, postPath, body).expect(t, http.StatusAccepted).into(t, &queued)
	if queued.ObservationId == "" || queued.RequestId != "api-observe-replacement-1" {
		t.Fatalf("queued observation omitted durable identity: %+v", queued)
	}
	var observationReplay apigen.ReplacementObservation
	e.post(t, postPath, body).expect(t, http.StatusOK).into(t, &observationReplay)
	if observationReplay.ObservationId != queued.ObservationId {
		t.Fatalf("idempotent POST changed observation id: first=%+v replay=%+v", queued, observationReplay)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.acq.StartImporters(ctx)
	t.Cleanup(func() { cancel(); e.acq.WaitImporters() })

	deadline := time.Now().Add(4 * time.Second)
	var observed apigen.ReplacementObservation
	for time.Now().Before(deadline) {
		response := e.get(t, "/api/v1/replacement-observations/"+queued.ObservationId).expect(t, http.StatusOK)
		response.into(t, &observed)
		if observed.State == "ready" || observed.State == "stale" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if observed.State != "ready" || observed.Sha256 == nil || *observed.Sha256 == "" || observed.Size == nil || *observed.Size != int64(len("known observed payload")) {
		t.Fatalf("real observation worker did not persist file evidence: %+v", observed)
	}
	if _, err := e.db.GetReplacementFileObservation(context.Background(), queued.ObservationId); err != nil {
		t.Fatalf("observation was only an in-memory HTTP response, not durable SQLite state: %v", err)
	}

	operationBody := fmt.Sprintf(`{"requestId":"api-replacement-operation-1","scope":"download","copyId":0,"sourceFileId":%d,"intent":"replace_bad","blacklistSourceRelease":false,"reasonCode":"corrupt_media","reason":"replace the reviewed damaged source","observedFiles":[{"id":%d,"fileGeneration":%q,"observationId":%q}]}`,
		fileID, fileID, generation, queued.ObservationId)
	var operation apigen.ReplacementOperation
	created := e.post(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID), operationBody).
		expect(t, http.StatusAccepted)
	created.into(t, &operation)
	if operation.OperationId == "" || operation.RequestId != "api-replacement-operation-1" || operation.State != "accepted" {
		t.Fatalf("replacement POST did not return the durable accepted operation: %+v", operation)
	}
	var replay apigen.ReplacementOperation
	e.post(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID), operationBody).
		expect(t, http.StatusOK).into(t, &replay)
	if replay.OperationId != operation.OperationId {
		t.Fatalf("same request replay created another operation: first=%+v replay=%+v", operation, replay)
	}
	var persisted apigen.ReplacementOperation
	e.get(t, "/api/v1/replacement-operations/"+operation.OperationId).
		expect(t, http.StatusOK).into(t, &persisted)
	if persisted.OperationId != operation.OperationId || persisted.RequestId != operation.RequestId {
		t.Fatalf("detail route returned a different persisted operation: %+v", persisted)
	}
}

func TestReplacementLifecycleHTTPRejectsInvalidStaleAndMissingRecords(t *testing.T) {
	e := newAPIEnv(t)
	itemID := e.addMovie(t)
	ctx := context.Background()
	item, err := e.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(item.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "generation-bound.mkv")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileID, err := e.db.UpsertFile(ctx, itemID, 0, path, 7)
	if err != nil {
		t.Fatal(err)
	}
	files, err := e.db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var generation string
	for _, file := range files {
		if file.ID == fileID {
			generation = file.FileGeneration
		}
	}
	postPath := fmt.Sprintf("/api/v1/library/%d/files/%d/replacement-observations", itemID, fileID)
	e.post(t, postPath, `{`).expect(t, http.StatusBadRequest)
	e.post(t, postPath, `{"requestId":"no-generation"}`).expect(t, http.StatusBadRequest)
	e.post(t, postPath, `{"fileGeneration":"stale-generation","requestId":"stale-observation"}`).expect(t, http.StatusConflict)
	e.get(t, "/api/v1/replacement-observations/missing-observation").expect(t, http.StatusNotFound)
	e.get(t, "/api/v1/replacement-operations/missing-operation").expect(t, http.StatusNotFound)

	// The request parser and acquisition service both reject incomplete
	// replacement requests before reserving an operation id.
	invalid := fmt.Sprintf(`{"requestId":"bad-replacement","scope":"download","copyId":0,"sourceFileId":%d,"intent":"replace_bad","reasonCode":"corrupt_media","reason":"bad bytes","observedFiles":[{"id":%d,"fileGeneration":%q,"observationId":"missing-observation"}],"blacklistSourceRelease":false}`, fileID, fileID, generation)
	var decoded any
	response := e.post(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID), invalid).expect(t, http.StatusConflict)
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("replacement refusal was not a structured API error: %s (%v)", response.Body.String(), err)
	}
	var review apigen.LibraryLifecycleReview
	e.get(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)).expect(t, http.StatusOK).into(t, &review)
	if len(review.Operations) != 0 {
		t.Fatalf("invalid/stale observation created durable replacement operations: %+v", review.Operations)
	}
}
