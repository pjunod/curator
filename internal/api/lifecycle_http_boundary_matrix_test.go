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
	"github.com/pjunod/monarr/internal/domain"
)

// prepareReviewableLibraryFile exercises the public observation route and its
// real worker, leaving a ready, generation-bound observation for operation
// admission tests.
func prepareReviewableLibraryFile(t *testing.T, e *apiEnv, contents string) (int64, int64, string, string) {
	t.Helper()
	ctx := context.Background()
	itemID := e.addMovie(t)
	item, err := e.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(item.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "review-boundary.mkv")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	fileID, err := e.db.UpsertFile(ctx, itemID, 0, path, int64(len(contents)))
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
		t.Fatal("library file has no generation")
	}
	observeURL := fmt.Sprintf("/api/v1/library/%d/files/%d/replacement-observations", itemID, fileID)
	var observation apigen.ReplacementObservation
	e.post(t, observeURL, fmt.Sprintf(`{"requestId":"boundary-observation-%d","fileGeneration":%q}`, fileID, generation)).
		expect(t, http.StatusAccepted).into(t, &observation)
	workerCtx, cancel := context.WithCancel(ctx)
	e.acq.StartImporters(workerCtx)
	t.Cleanup(func() { cancel(); e.acq.WaitImporters() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.get(t, "/api/v1/replacement-observations/"+observation.ObservationId).
			expect(t, http.StatusOK).into(t, &observation)
		if observation.State == "ready" || observation.State == "stale" || observation.State == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if observation.State != "ready" {
		t.Fatalf("review worker did not produce a ready observation: %+v", observation)
	}
	cancel()
	e.acq.WaitImporters()
	return itemID, fileID, generation, observation.ObservationId
}

func replacementBoundaryBody(itemID, fileID int64, generation, observationID, requestID string) string {
	return fmt.Sprintf(`{"requestId":%q,"scope":"download","copyId":0,"sourceFileId":%d,"intent":"replace_bad","blacklistSourceRelease":false,"reasonCode":"corrupt_media","reason":"boundary admission","observedFiles":[{"id":%d,"fileGeneration":%q,"observationId":%q}]}`,
		requestID, fileID, fileID, generation, observationID)
}

func TestReplacementHTTPMalformedAndOversizedRequestsDoNotReserveAuthority(t *testing.T) {
	e := newAPIEnv(t)
	itemID, fileID, generation, observationID := prepareReviewableLibraryFile(t, e, "stable source")
	url := fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)
	valid := replacementBoundaryBody(itemID, fileID, generation, observationID, "http-boundary-valid")
	invalid := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"blacklistSourceRelease":false`},
		{name: "wrong field type", body: `{"blacklistSourceRelease":false,"requestId":17}`},
		{name: "trailing JSON value", body: valid + `{}`},
		{name: "body exceeds bounded parser", body: strings.Repeat(" ", 1<<20) + valid},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			e.post(t, url, tc.body).expect(t, http.StatusBadRequest)
		})
	}
	var review apigen.LibraryLifecycleReview
	e.get(t, url).expect(t, http.StatusOK).into(t, &review)
	if len(review.Operations) != 0 {
		t.Fatalf("invalid request created durable replacement authority: %+v", review.Operations)
	}
}

func TestReplacementHTTPDatabaseFailuresRollbackAndPermitExactRetry(t *testing.T) {
	for _, failedTable := range []string{"replacement_operations", "replacement_operation_files"} {
		t.Run(failedTable, func(t *testing.T) {
			e := newAPIEnv(t)
			ctx := context.Background()
			itemID, fileID, generation, observationID := prepareReviewableLibraryFile(t, e, "reviewed bytes")
			requestID := "persist-failure-" + failedTable
			body := replacementBoundaryBody(itemID, fileID, generation, observationID, requestID)
			trigger := "fail_" + failedTable
			statement := fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT, 'injected lifecycle write failure'); END`, trigger, failedTable)
			if _, err := e.db.W.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
			url := fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)
			e.post(t, url, body).expect(t, http.StatusInternalServerError)
			var afterFailure apigen.LibraryLifecycleReview
			e.get(t, url).expect(t, http.StatusOK).into(t, &afterFailure)
			if len(afterFailure.Operations) != 0 {
				t.Fatalf("failed SQLite admission left a visible partial operation: %+v", afterFailure.Operations)
			}
			if _, err := e.db.W.ExecContext(ctx, `DROP TRIGGER `+trigger); err != nil {
				t.Fatal(err)
			}
			created := e.post(t, url, body).expect(t, http.StatusAccepted)
			var accepted apigen.ReplacementOperation
			created.into(t, &accepted)
			if accepted.RequestId != requestID || accepted.OperationId == "" {
				t.Fatalf("retry did not reserve the exact request after rollback: %+v", accepted)
			}
			replay := e.post(t, url, body).expect(t, http.StatusOK)
			var replayed apigen.ReplacementOperation
			replay.into(t, &replayed)
			if replayed.OperationId != accepted.OperationId {
				t.Fatalf("retry replay created a second operation: first=%+v replay=%+v", accepted, replayed)
			}
		})
	}
}

func TestLibraryReviewHTTPMissingOwnerAndDatabaseFailuresStayNonMutating(t *testing.T) {
	t.Run("missing item", func(t *testing.T) {
		e := newAPIEnv(t)
		e.get(t, "/api/v1/library/9223372036854775807/replacement-operations").expect(t, http.StatusNotFound)
	})

	t.Run("report another item's file", func(t *testing.T) {
		e := newAPIEnv(t)
		ctx := context.Background()
		firstItem := e.addMovie(t)
		roots, err := e.db.ListRootFolders(ctx)
		if err != nil || len(roots) == 0 {
			t.Fatalf("load library root: roots=%v err=%v", roots, err)
		}
		secondItem, err := e.db.CreateMediaItem(ctx, domain.MediaItem{
			Kind: domain.KindMovie, Title: "Different Ownership Item", SortTitle: "different ownership item",
			Year: 2001, RootFolderID: roots[0].ID, Path: filepath.Join(roots[0].Path, "Different Ownership Item"),
			IDs: domain.ExternalIDs{TMDB: 551}, Source: "tmdb",
		})
		if err != nil {
			t.Fatal(err)
		}
		if secondItem == 0 || secondItem == firstItem {
			t.Fatalf("fixture did not create a distinct second item: first=%d second=%d", firstItem, secondItem)
		}
		item, err := e.db.GetMediaItemFull(ctx, firstItem)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(item.Path, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(item.Path, "owned-by-first-item.mkv")
		contents := []byte("belongs to first item")
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		fileID, err := e.db.UpsertFile(ctx, firstItem, 0, path, int64(len(contents)))
		if err != nil {
			t.Fatal(err)
		}
		files, err := e.db.ListFilesForItem(ctx, firstItem)
		if err != nil {
			t.Fatal(err)
		}
		var generation string
		for _, file := range files {
			if file.ID == fileID {
				generation = file.FileGeneration
			}
		}
		url := fmt.Sprintf("/api/v1/library/%d/files/%d/report", secondItem, fileID)
		e.post(t, url, fmt.Sprintf(`{"fileGeneration":%q,"reasonCode":"corrupt_media","reason":"wrong owner"}`, generation)).expect(t, http.StatusNotFound)
		var firstReview apigen.LibraryLifecycleReview
		e.get(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", firstItem)).expect(t, http.StatusOK).into(t, &firstReview)
		if len(firstReview.OpenReports) != 0 {
			t.Fatalf("cross-item report created a review flag: %+v", firstReview.OpenReports)
		}
	})

	t.Run("review list table unavailable", func(t *testing.T) {
		e := newAPIEnv(t)
		itemID := e.addMovie(t)
		if _, err := e.db.W.ExecContext(context.Background(), `ALTER TABLE library_review_flags RENAME TO lifecycle_flags_temporarily_unavailable`); err != nil {
			t.Fatal(err)
		}
		response := e.get(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)).expect(t, http.StatusInternalServerError)
		if strings.TrimSpace(response.Body.String()) == "" {
			t.Fatal("database failure returned an empty lifecycle review error")
		}
	})
}

func TestLibraryReportHTTPWriteFailureCanRetryWithoutPhantomFlag(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	itemID, fileID, generation, _ := prepareReviewableLibraryFile(t, e, "reportable current bytes")
	if _, err := e.db.W.ExecContext(ctx, `CREATE TRIGGER fail_library_review_insert BEFORE INSERT ON library_review_flags BEGIN SELECT RAISE(ABORT, 'injected review write failure'); END`); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("/api/v1/library/%d/files/%d/report", itemID, fileID)
	body := fmt.Sprintf(`{"fileGeneration":%q,"reasonCode":"suspected_corruption","reason":"retry after storage recovers","evidence":{"probe":"fixture"}}`, generation)
	e.post(t, url, body).expect(t, http.StatusInternalServerError)
	var review apigen.LibraryLifecycleReview
	e.get(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)).expect(t, http.StatusOK).into(t, &review)
	if len(review.OpenReports) != 0 {
		t.Fatalf("failed report write left a phantom flag: %+v", review.OpenReports)
	}
	if _, err := e.db.W.ExecContext(ctx, `DROP TRIGGER fail_library_review_insert`); err != nil {
		t.Fatal(err)
	}
	e.post(t, url, body).expect(t, http.StatusNoContent)
	e.get(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)).expect(t, http.StatusOK).into(t, &review)
	if len(review.OpenReports) != 1 || review.OpenReports[0].FileGeneration != generation {
		t.Fatalf("successful retry did not persist one exact-generation flag: %+v", review.OpenReports)
	}
}

func TestLibraryReviewResolveHTTPKeepsFlagOpenAcrossWrongOwnerAndDatabaseFailure(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	itemID, fileID, generation, _ := prepareReviewableLibraryFile(t, e, "flag remains until authorized resolve")
	reportURL := fmt.Sprintf("/api/v1/library/%d/files/%d/report", itemID, fileID)
	e.post(t, reportURL, fmt.Sprintf(`{"fileGeneration":%q,"reasonCode":"suspected_corruption","reason":"resolve boundary"}`, generation)).
		expect(t, http.StatusNoContent)
	var review apigen.LibraryLifecycleReview
	reviewURL := fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)
	e.get(t, reviewURL).expect(t, http.StatusOK).into(t, &review)
	if len(review.OpenReports) != 1 {
		t.Fatalf("report setup did not create one open flag: %+v", review.OpenReports)
	}
	flagID := review.OpenReports[0].Id
	resolvePath := fmt.Sprintf("/api/v1/library/%d/review-flags/%d/resolve", itemID, flagID)
	resolveBody := fmt.Sprintf(`{"fileGeneration":%q}`, generation)
	// A different item cannot resolve this file's flag even when it knows the
	// exact generation token.
	e.post(t, fmt.Sprintf("/api/v1/library/%d/review-flags/%d/resolve", itemID+1000, flagID), resolveBody).
		expect(t, http.StatusNotFound)
	if _, err := e.db.W.ExecContext(ctx, `CREATE TRIGGER fail_library_review_resolve BEFORE UPDATE ON library_review_flags BEGIN SELECT RAISE(ABORT, 'injected resolve failure'); END`); err != nil {
		t.Fatal(err)
	}
	e.post(t, resolvePath, resolveBody).expect(t, http.StatusInternalServerError)
	e.get(t, reviewURL).expect(t, http.StatusOK).into(t, &review)
	if len(review.OpenReports) != 1 || review.OpenReports[0].State != apigen.Open {
		t.Fatalf("failed resolve changed the visible review state: %+v", review.OpenReports)
	}
	if _, err := e.db.W.ExecContext(ctx, `DROP TRIGGER fail_library_review_resolve`); err != nil {
		t.Fatal(err)
	}
	e.post(t, resolvePath, resolveBody).expect(t, http.StatusNoContent)
	// Resolution is idempotent only while open; a stale repeated button gets a
	// not-found response rather than pretending another transition occurred.
	e.post(t, resolvePath, resolveBody).expect(t, http.StatusNotFound)
	e.get(t, reviewURL).expect(t, http.StatusOK).into(t, &review)
	if len(review.OpenReports) != 0 {
		t.Fatalf("successful resolve left an open flag: %+v", review.OpenReports)
	}
}

func TestReplacementObservationHTTPUnknownFileDoesNotCreateObservation(t *testing.T) {
	e := newAPIEnv(t)
	itemID := e.addMovie(t)
	body := `{"fileGeneration":"unknown-generation","requestId":"unknown-observation-target"}`
	for _, target := range []string{
		"/api/v1/library/9223372036854775807/files/1/replacement-observations",
		fmt.Sprintf("/api/v1/library/%d/files/9223372036854775807/replacement-observations", itemID),
	} {
		status := http.StatusNotFound
		if strings.Contains(target, "/files/9223372036854775807/") {
			status = http.StatusConflict // the item exists, but its file generation is stale
		}
		e.post(t, target, body).expect(t, status)
	}
	e.get(t, "/api/v1/replacement-observations/unknown-observation-target").expect(t, http.StatusNotFound)
	var review apigen.LibraryLifecycleReview
	e.get(t, fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)).expect(t, http.StatusOK).into(t, &review)
	if len(review.Operations) != 0 {
		t.Fatalf("unknown observation target created a replacement operation: %+v", review.Operations)
	}
}

func TestReplacementObservationHTTPStaleResultHasActionableErrorAndFreshRequestIdentity(t *testing.T) {
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
	path := filepath.Join(item.Path, "observation-refresh.mkv")
	data := []byte("review once, then bind a fresh identity")
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
	var originalGeneration string
	for _, file := range files {
		if file.ID == fileID {
			originalGeneration = file.FileGeneration
		}
	}
	observeURL := fmt.Sprintf("/api/v1/library/%d/files/%d/replacement-observations", itemID, fileID)
	var stale apigen.ReplacementObservation
	e.post(t, observeURL, fmt.Sprintf(`{"requestId":"observation-stale-first","fileGeneration":%q}`, originalGeneration)).
		expect(t, http.StatusAccepted).into(t, &stale)
	movedPath := path + ".temporarily-moved"
	if err := os.Rename(path, movedPath); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	e.acq.StartImporters(workerCtx)
	t.Cleanup(func() { cancel(); e.acq.WaitImporters() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.get(t, "/api/v1/replacement-observations/"+stale.ObservationId).expect(t, http.StatusOK).into(t, &stale)
		if stale.State == "stale" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stale.State != "stale" || stale.Error == nil || *stale.Error == "" || stale.Sha256 != nil || stale.Size != nil {
		t.Fatalf("stale observation did not expose an actionable error without stale digest evidence: %+v", stale)
	}
	if err := os.Rename(movedPath, path); err != nil {
		t.Fatal(err)
	}
	var retry apigen.ReplacementObservation
	e.post(t, observeURL, fmt.Sprintf(`{"requestId":"observation-stale-refresh","fileGeneration":%q}`, originalGeneration)).
		expect(t, http.StatusAccepted).into(t, &retry)
	cancel()
	e.acq.WaitImporters()
	if retry.State != "pending" || retry.RequestId != "observation-stale-refresh" || retry.ObservationId == stale.ObservationId {
		t.Fatalf("fresh generation-bound request did not create a new review identity: %+v", retry)
	}
}

func TestReplacementHTTPRejectsInconsistentEpisodeScopeBeforeReservation(t *testing.T) {
	e := newAPIEnv(t)
	itemID, fileID, generation, observationID := prepareReviewableLibraryFile(t, e, "movie has no episode scope")
	url := fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)
	for name, body := range map[string]string{
		"episode scope without selected episodes": fmt.Sprintf(`{"requestId":"scope-missing-episodes","scope":"episodes","copyId":0,"sourceFileId":%d,"intent":"replace_bad","blacklistSourceRelease":false,"reasonCode":"corrupt_media","reason":"missing episode set","observedFiles":[{"id":%d,"fileGeneration":%q,"observationId":%q}]}`, fileID, fileID, generation, observationID),
		"download scope with episode set":         fmt.Sprintf(`{"requestId":"scope-extra-episodes","scope":"download","copyId":0,"sourceFileId":%d,"episodeIds":[1],"intent":"replace_bad","blacklistSourceRelease":false,"reasonCode":"corrupt_media","reason":"unexpected episode set","observedFiles":[{"id":%d,"fileGeneration":%q,"observationId":%q}]}`, fileID, fileID, generation, observationID),
	} {
		t.Run(name, func(t *testing.T) { e.post(t, url, body).expect(t, http.StatusBadRequest) })
	}
	var review apigen.LibraryLifecycleReview
	e.get(t, url).expect(t, http.StatusOK).into(t, &review)
	if len(review.Operations) != 0 {
		t.Fatalf("inconsistent scope created operation authority: %+v", review.Operations)
	}
}

func TestReplacementDTOExposesDurableBlockedRetryAndFileSnapshotState(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	itemID, fileID, generation, observationID := prepareReviewableLibraryFile(t, e, "blocked-but-reviewable")
	url := fmt.Sprintf("/api/v1/library/%d/replacement-operations", itemID)
	var operation apigen.ReplacementOperation
	e.post(t, url, replacementBoundaryBody(itemID, fileID, generation, observationID, "dto-blocked-retry-state")).
		expect(t, http.StatusAccepted).into(t, &operation)
	blockedAt := time.Now().Add(-45 * time.Minute).UnixMilli()
	nextAt := time.Now().Add(90 * time.Minute).UnixMilli()
	if _, err := e.db.W.ExecContext(ctx, `UPDATE replacement_operations SET state='cleanup_blocked',blocked_since=?,next_attempt_at=?,retry_attempts=4,last_error='storage mount unavailable' WHERE operation_id=?`, blockedAt, nextAt, operation.OperationId); err != nil {
		t.Fatal(err)
	}
	var detail apigen.ReplacementOperation
	e.get(t, "/api/v1/replacement-operations/"+operation.OperationId).expect(t, http.StatusOK).into(t, &detail)
	if detail.State != "cleanup_blocked" || detail.BlockedSince == nil || detail.NextAttemptAt == nil || detail.RetryAttempts == nil || *detail.RetryAttempts != 4 || detail.LastError == nil || *detail.LastError != "storage mount unavailable" || detail.BlockedWarning == nil || !*detail.BlockedWarning {
		t.Fatalf("operation detail lost durable retry/review state: %+v", detail)
	}
	if detail.Files == nil || len(*detail.Files) != 1 || (*detail.Files)[0]["fileGeneration"] != generation || (*detail.Files)[0]["path"] != filepath.Join(mustItemPath(t, e, itemID), "review-boundary.mkv") {
		t.Fatalf("operation DTO lost the exact selected file snapshot: %+v", detail.Files)
	}
}

func mustItemPath(t *testing.T, e *apiEnv, itemID int64) string {
	t.Helper()
	item, err := e.db.GetMediaItemFull(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	return item.Path
}
