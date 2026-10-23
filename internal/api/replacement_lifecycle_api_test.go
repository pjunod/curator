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

func TestReplacementLifecycleHTTPSavesBlacklistChoiceAcrossCreateReplayDetailAndHistory(t *testing.T) {
	for _, blacklist := range []bool{false, true} {
		t.Run(fmt.Sprintf("blacklist_%t", blacklist), func(t *testing.T) {
			e := newAPIEnv(t)
			ctx := context.Background()
			itemID := e.addMovie(t)
			item, err := e.db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(item.Path, 0o700); err != nil {
				t.Fatal(err)
			}
			contents := []byte("source bytes remain unchanged after queuing")
			path := filepath.Join(item.Path, "review-source.mkv")
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			fileID, err := e.db.UpsertFile(ctx, itemID, 0, path, int64(len(contents)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.db.W.ExecContext(ctx, `UPDATE media_files SET source_guid=?,source_indexer_id=? WHERE id=?`, "guid-api-blacklist", 73, fileID); err != nil {
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
			if generation == "" {
				t.Fatal("fixture media file has no durable generation")
			}

			postObservation := fmt.Sprintf("/api/v1/library/%d/files/%d/replacement-observations", itemID, fileID)
			var queued apigen.ReplacementObservation
			e.post(t, postObservation, fmt.Sprintf(`{"requestId":%q,"fileGeneration":%q}`, "observe-blacklist-choice", generation)).
				expect(t, http.StatusAccepted).into(t, &queued)
			workerCtx, cancel := context.WithCancel(ctx)
			e.acq.StartImporters(workerCtx)
			t.Cleanup(func() { cancel(); e.acq.WaitImporters() })
			var observed apigen.ReplacementObservation
			deadline := time.Now().Add(4 * time.Second)
			for time.Now().Before(deadline) {
				e.get(t, "/api/v1/replacement-observations/"+queued.ObservationId).
					expect(t, http.StatusOK).into(t, &observed)
				if observed.State == "ready" || observed.State == "stale" || observed.State == "failed" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			e.acq.WaitImporters()
			if observed.State != "ready" || observed.Sha256 == nil {
				t.Fatalf("observation was not ready for explicit decision: %+v", observed)
			}

			request := map[string]any{
				"requestId": "operation-blacklist-choice", "scope": "download", "copyId": 0,
				"sourceFileId": fileID, "intent": "replace_bad", "blacklistSourceRelease": blacklist,
				"reasonCode": "corrupt_media", "reason": "persist the explicit blacklist choice",
				"observedFiles": []map[string]any{{"id": fileID, "fileGeneration": generation, "observationId": queued.ObservationId}},
			}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			url := fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)
			var created apigen.ReplacementOperation
			e.post(t, url, string(body)).expect(t, http.StatusAccepted).into(t, &created)
			if created.BlacklistSourceRelease != blacklist {
				t.Fatalf("create response reported blacklist=%t, want saved choice %t", created.BlacklistSourceRelease, blacklist)
			}
			var replay apigen.ReplacementOperation
			e.post(t, url, string(body)).expect(t, http.StatusOK).into(t, &replay)
			if replay.OperationId != created.OperationId || replay.BlacklistSourceRelease != blacklist {
				t.Fatalf("exact replay lost saved choice: created=%+v replay=%+v", created, replay)
			}
			changed := map[string]any{}
			for k, v := range request {
				changed[k] = v
			}
			changed["blacklistSourceRelease"] = !blacklist
			changedBody, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			e.post(t, url, string(changedBody)).expect(t, http.StatusConflict)

			var detail apigen.ReplacementOperation
			e.get(t, "/api/v1/replacement-operations/"+created.OperationId).expect(t, http.StatusOK).into(t, &detail)
			if detail.BlacklistSourceRelease != blacklist {
				t.Fatalf("detail response reported blacklist=%t, want saved choice %t", detail.BlacklistSourceRelease, blacklist)
			}
			var history apigen.LibraryLifecycleReview
			e.get(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)).expect(t, http.StatusOK).into(t, &history)
			if len(history.Operations) != 1 || history.Operations[0].BlacklistSourceRelease != blacklist {
				t.Fatalf("history did not expose the saved blacklist choice: %+v", history.Operations)
			}
			persisted, err := e.db.GetReplacementOperation(ctx, created.OperationId)
			if err != nil || persisted.BlacklistSourceRelease != blacklist {
				t.Fatalf("SQLite operation choice = %t, err=%v; want %t", persisted.BlacklistSourceRelease, err, blacklist)
			}
			_, identityBlocked, err := e.db.ReleaseIdentityBlocklistReason(ctx, itemID, 73, "guid-api-blacklist")
			if err != nil || identityBlocked != blacklist {
				t.Fatalf("global blacklist state=%t err=%v; explicit operation choice=%t", identityBlocked, err, blacklist)
			}
			stillThere, err := os.ReadFile(path)
			if err != nil || string(stillThere) != string(contents) {
				t.Fatalf("queueing/replaying operation changed source bytes: got=%q err=%v", stillThere, err)
			}
		})
	}
}

func TestReplacementLifecycleHTTPRejectsBytesChangedAfterReadyObservationWithoutTouchingFile(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	itemID := e.addMovie(t)
	item, err := e.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(item.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "changed-after-observation.mkv")
	if err := os.WriteFile(path, []byte("observed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileID, err := e.db.UpsertFile(ctx, itemID, 0, path, int64(len("observed bytes")))
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
	postObservation := fmt.Sprintf("/api/v1/library/%d/files/%d/replacement-observations", itemID, fileID)
	var queued apigen.ReplacementObservation
	e.post(t, postObservation, fmt.Sprintf(`{"requestId":%q,"fileGeneration":%q}`, "observe-before-change", generation)).
		expect(t, http.StatusAccepted).into(t, &queued)
	workerCtx, cancel := context.WithCancel(ctx)
	e.acq.StartImporters(workerCtx)
	t.Cleanup(func() { cancel(); e.acq.WaitImporters() })
	var observed apigen.ReplacementObservation
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		e.get(t, "/api/v1/replacement-observations/"+queued.ObservationId).
			expect(t, http.StatusOK).into(t, &observed)
		if observed.State == "ready" || observed.State == "stale" || observed.State == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	e.acq.WaitImporters()
	if observed.State != "ready" {
		t.Fatalf("observation did not reach ready state before mutation: %+v", observed)
	}
	changedBytes := []byte("different bytes after the user reviewed the file")
	if err := os.WriteFile(path, changedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"requestId": "reject-changed-observation", "scope": "download", "copyId": 0,
		"sourceFileId": fileID, "intent": "replace_bad", "blacklistSourceRelease": false,
		"reasonCode": "corrupt_media", "reason": "attempt must be refused after mutation",
		"observedFiles": []map[string]any{{"id": fileID, "fileGeneration": generation, "observationId": queued.ObservationId}},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	e.post(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID), string(body)).expect(t, http.StatusConflict)
	var history apigen.LibraryLifecycleReview
	e.get(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)).expect(t, http.StatusOK).into(t, &history)
	if len(history.Operations) != 0 {
		t.Fatalf("stale observation reserved an operation: %+v", history.Operations)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(changedBytes) {
		t.Fatalf("rejected request changed current file: got=%q err=%v", actual, err)
	}
}
