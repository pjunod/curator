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

func TestReplacementAPIRequiresExplicitBooleanBlacklistDecision(t *testing.T) {
	e := newAPIEnv(t)
	itemID := e.addMovie(t)
	url := fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)
	base := `{"requestId":"explicit-decision","scope":"download","copyId":0,"sourceFileId":1,"intent":"replace_bad","reasonCode":"corrupt_media","reason":"reviewed replacement","observedFiles":[]`
	for _, body := range map[string]string{
		"omitted": base + `}`,
		"null":    base + `,"blacklistSourceRelease":null}`,
		"string":  base + `,"blacklistSourceRelease":"false"}`,
	} {
		e.post(t, url, body).expect(t, http.StatusBadRequest)
	}
	var review apigen.LibraryLifecycleReview
	e.get(t, url).expect(t, http.StatusOK).into(t, &review)
	if len(review.Operations) != 0 {
		t.Fatalf("request without an explicit boolean decision created an operation: %+v", review.Operations)
	}
}

func TestReplacementAPIReplayCannotChangeReviewedRequest(t *testing.T) {
	e := newAPIEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); e.acq.WaitImporters() })
	itemID := e.addMovie(t)
	item, err := e.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(item.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "request-bound.mkv")
	data := []byte("request-bound reviewed bytes")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fileID, err := e.db.UpsertFile(ctx, itemID, 0, path, int64(len(data)))
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
	if generation == "" {
		t.Fatal("fixture file has no immutable generation")
	}
	observeURL := fmt.Sprintf("/api/v1/library/%d/files/%d/replacement-observations", itemID, fileID)
	var observation apigen.ReplacementObservation
	e.post(t, observeURL, fmt.Sprintf(`{"fileGeneration":%q,"requestId":"request-bound-observation"}`, generation)).
		expect(t, http.StatusAccepted).into(t, &observation)
	e.acq.StartImporters(ctx)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		e.get(t, "/api/v1/replacement-observations/"+observation.ObservationId).expect(t, http.StatusOK).into(t, &observation)
		if observation.State == "ready" || observation.State == "stale" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if observation.State != "ready" {
		t.Fatalf("reviewed source observation did not become ready: %+v", observation)
	}
	operationURL := fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)
	body := fmt.Sprintf(`{"requestId":"immutable-review-request","scope":"download","copyId":0,"sourceFileId":%d,"intent":"replace_bad","blacklistSourceRelease":false,"reasonCode":"corrupt_media","reason":"replace the exact reviewed source","observedFiles":[{"id":%d,"fileGeneration":%q,"observationId":%q}]}`,
		fileID, fileID, generation, observation.ObservationId)
	var created apigen.ReplacementOperation
	e.post(t, operationURL, body).expect(t, http.StatusAccepted).into(t, &created)
	if created.OperationId == "" || created.RequestId != "immutable-review-request" || created.Intent != "replace_bad" || created.Scope != "download" {
		t.Fatalf("accepted operation omitted its reviewed identity/scope: %+v", created)
	}
	changed := strings.Replace(body, "replace the exact reviewed source", "retarget this source after approval", 1)
	e.post(t, operationURL, changed).expect(t, http.StatusConflict)
	var persisted apigen.ReplacementOperation
	e.get(t, "/api/v1/replacement-operations/"+created.OperationId).expect(t, http.StatusOK).into(t, &persisted)
	if persisted.OperationId != created.OperationId || persisted.RequestId != created.RequestId || persisted.Intent != created.Intent || persisted.Scope != created.Scope {
		t.Fatalf("changed replay modified or replaced the approved operation: created=%+v persisted=%+v", created, persisted)
	}
}

func TestLibraryReviewReportAndResolutionStayBoundToExactFileGeneration(t *testing.T) {
	e := newAPIEnv(t)
	itemID := e.addMovie(t)
	item, err := e.db.GetMediaItemFull(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(item.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "review-generation.mkv")
	data := []byte("current library generation")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fileID, err := e.db.UpsertFile(context.Background(), itemID, 0, path, int64(len(data)))
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
		t.Fatal("fixture file has no immutable generation")
	}
	before, err := e.db.GetFileQuality(context.Background(), fileID)
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("/api/v1/library/%d", itemID)
	reviewURL := base + "/replacement-operations"
	reportURL := fmt.Sprintf("%s/files/%d/report", base, fileID)
	e.post(t, reportURL, `{`).expect(t, http.StatusBadRequest)
	e.post(t, reportURL, fmt.Sprintf(`{"fileGeneration":"stale","reasonCode":"suspected_corruption","reason":"review bytes"}`)).expect(t, http.StatusConflict)
	var empty apigen.LibraryLifecycleReview
	e.get(t, reviewURL).expect(t, http.StatusOK).into(t, &empty)
	if len(empty.OpenReports) != 0 {
		t.Fatalf("stale report created review authority: %+v", empty.OpenReports)
	}

	valid := fmt.Sprintf(`{"fileGeneration":%q,"reasonCode":"suspected_corruption","reason":"review bytes","evidence":{"reportedBy":"integration-test"}}`, generation)
	e.post(t, reportURL, valid).expect(t, http.StatusNoContent)
	after, err := e.db.GetFileQuality(context.Background(), fileID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Provenance != after.Provenance || before.Confidence != after.Confidence || before.Known != after.Known {
		t.Fatalf("report changed quality evidence: before=%+v after=%+v", before, after)
	}
	var review apigen.LibraryLifecycleReview
	e.get(t, reviewURL).expect(t, http.StatusOK).into(t, &review)
	if len(review.OpenReports) != 1 || review.OpenReports[0].MediaFileId != fileID || review.OpenReports[0].FileGeneration != generation || review.OpenReports[0].ReasonCode != "suspected_corruption" {
		t.Fatalf("report was not exposed with exact file identity: %+v", review.OpenReports)
	}
	flagID := review.OpenReports[0].Id
	resolveURL := fmt.Sprintf("%s/review-flags/%d/resolve", base, flagID)
	e.post(t, resolveURL, `{`).expect(t, http.StatusBadRequest)
	e.post(t, resolveURL, `{"fileGeneration":"stale"}`).expect(t, http.StatusConflict)
	e.get(t, reviewURL).expect(t, http.StatusOK).into(t, &review)
	if len(review.OpenReports) != 1 {
		t.Fatalf("stale resolution cleared a current review flag: %+v", review.OpenReports)
	}
	e.post(t, resolveURL, fmt.Sprintf(`{"fileGeneration":%q}`, generation)).expect(t, http.StatusNoContent)
	e.get(t, reviewURL).into(t, &review)
	if len(review.OpenReports) != 0 {
		t.Fatalf("resolved review remains open: %+v", review.OpenReports)
	}
}
