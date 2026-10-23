package acquisition

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestManualGrabCreatesIndependentUnmonitoredCopyAndIsIdempotent(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	req := GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:manual",
		Indexer: "idx", Protocol: "torrent", RequestID: "manual-action-1",
	}
	first, err := svc.GrabManual(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.GrabManual(ctx, req)
	if err != nil || second != first {
		t.Fatalf("same request = %d, %v; want original download %d", second, err, first)
	}
	if len(client.added) != 1 {
		t.Fatalf("external Add called %d times; want one", len(client.added))
	}
	dl, err := db.GetDownload(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if dl.Intent != GrabIntentManualAdditional || dl.RequestID != req.RequestID || dl.CopyID == 0 || len(dl.WantableIDs) != 1 {
		t.Fatalf("persisted manual intent = %+v", dl)
	}
	if len(dl.Handoff) < 2 || dl.Handoff[0].Step != stepManualGrabQueued {
		t.Fatalf("manual intent missing from durable handoff: %+v", dl.Handoff)
	}
	copies, err := db.ListMediaCopies(ctx, itemID)
	if err != nil || len(copies) != 1 {
		t.Fatalf("copies = %+v, err=%v", copies, err)
	}
	if copies[0].ID != dl.CopyID || copies[0].Monitored || copies[0].Path == "" {
		t.Fatalf("manual copy = %+v; want a distinct unmonitored folder", copies[0])
	}
	if filepath.Dir(copies[0].Path) != filepath.Dir(mustMediaPath(t, db, itemID)) {
		t.Fatalf("manual copy escaped the selected library location: %q", copies[0].Path)
	}

	// A new deliberate request gets a fresh identity and a second independent
	// target, even when it selects the same release and season.
	req.RequestID = "manual-action-2"
	third, err := svc.GrabManual(ctx, req)
	if err != nil || third == first {
		t.Fatalf("new manual action = %d, %v; want a new download", third, err)
	}
	if len(client.added) != 2 {
		t.Fatalf("new deliberate action Add count = %d, want 2", len(client.added))
	}
}

func TestManualGrabReusesRequestAfterUncertainAddAndRejectsIDConflict(t *testing.T) {
	client := &fakeClient{addErr: errors.New("connection lost after submission")}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	req := GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:uncertain",
		Indexer: "idx", Protocol: "torrent", RequestID: "manual-uncertain-1",
	}
	first, err := svc.GrabManual(ctx, req)
	if err == nil || first == 0 {
		t.Fatalf("first submission = %d, %v; want a persisted uncertain request", first, err)
	}
	client.addErr = nil
	second, err := svc.GrabManual(ctx, req)
	if err == nil || second != first {
		t.Fatalf("uncertain retry = %d, %v; want the same request held without unsafe duplicate Add", second, err)
	}
	if len(client.added) != 1 {
		t.Fatalf("uncertain submission retried external Add %d times", len(client.added))
	}
	if _, err := db.GetDownload(ctx, first); err != nil {
		t.Fatal(err)
	}
	req.DownloadURL = "magnet:different-request"
	if _, err := svc.GrabManual(ctx, req); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("reused request ID with different intent = %v", err)
	}
}

func TestManualGrabResumesDurablyReservedRequestBeforeFirstAdd(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:reserved",
		Indexer: "idx", Protocol: "torrent", RequestID: "manual-pending-1"}
	id, err := svc.GrabManual(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetSubmissionPhase(ctx, id, "pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET handle='' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	client.added = nil
	second, err := svc.GrabManual(ctx, req)
	if err != nil || second != id {
		t.Fatalf("reserved request resume = %d, %v; want download %d", second, err, id)
	}
	if len(client.added) != 1 || client.added[0] != "magnet:reserved" {
		t.Fatalf("resumed Add payload = %v; want persisted URL once", client.added)
	}
	resumed, err := db.GetDownload(ctx, id)
	if err != nil || resumed.Transfer != dl.Transfer || resumed.CopyID != dl.CopyID || resumed.SubmissionPhase != "submitted" {
		t.Fatalf("resumed identity = %+v err=%v; want original token/copy submitted", resumed, err)
	}
	copies, err := db.ListMediaCopies(ctx, itemID)
	if err != nil || len(copies) != 1 {
		t.Fatalf("resumed request created another copy: %+v err=%v", copies, err)
	}
}

type handlelessTransferClient struct {
	fakeClient
	transfer string
}

func (c *handlelessTransferClient) SupportsExactTransferCorrelation() bool { return true }

func (c *handlelessTransferClient) AddWithOptions(_ context.Context, downloadURL, _ string, options ports.AddOptions) (ports.Handle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.added = append(c.added, downloadURL)
	c.transfer = options.Transfer
	return "", c.addErr
}

func TestManualHandlelessAcceptanceWaitsForExactTransferWithoutReplay(t *testing.T) {
	ctx := context.Background()
	client := &handlelessTransferClient{}
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:handleless",
		Indexer: "idx", Protocol: "torrent", RequestID: "manual-handleless-accepted"}
	id, err := svc.GrabManual(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := db.GetDownload(ctx, id)
	if err != nil || accepted.SubmissionPhase != "submitted" || accepted.Handle != "" || accepted.Transfer == "" || accepted.GenerationKey == "" || !accepted.ExactTransferCorrelation {
		t.Fatalf("handleless accepted request=%+v err=%v", accepted, err)
	}
	if len(client.added) != 1 || client.transfer != accepted.Transfer {
		t.Fatalf("initial add=%v transfer=%q; want one add with persisted token %q", client.added, client.transfer, accepted.Transfer)
	}
	retries, err := db.ListManualAdmissionRetries(ctx, time.Now().Add(31*time.Second), 32)
	if err != nil || len(retries) != 1 || retries[0].DownloadID != id {
		t.Fatalf("accepted handleless request was not durably scheduled for exact-token reconciliation: %+v err=%v", retries, err)
	}
	// A replay while the exact status has not appeared returns the accepted
	// request and must not submit the same torrent a second time.
	if replay, replayErr := svc.GrabManual(ctx, req); replayErr != nil || replay != id {
		t.Fatalf("accepted handleless replay=%d err=%v", replay, replayErr)
	}
	if len(client.added) != 1 {
		t.Fatalf("accepted handleless replay duplicated Add: %v", client.added)
	}
	client.statuses = []ports.DownloadStatus{{Handle: "torrent-hash-42", Transfer: accepted.Transfer, State: ports.StateDownloading}}
	if replay, replayErr := svc.GrabManual(ctx, req); replayErr != nil || replay != id {
		t.Fatalf("exact-transfer reconciliation=%d err=%v", replay, replayErr)
	}
	settled, err := db.GetDownload(ctx, id)
	if err != nil || settled.SubmissionPhase != "submitted" || settled.Handle != "torrent-hash-42" || settled.Transfer != accepted.Transfer || settled.GenerationKey != accepted.GenerationKey {
		t.Fatalf("exact status did not bind the original request: %+v err=%v", settled, err)
	}
	if len(client.added) != 1 {
		t.Fatalf("exact status reconciliation re-submitted: %v", client.added)
	}
}

type manualStatusError int

func (e manualStatusError) Error() string       { return fmt.Sprintf("client status %d", int(e)) }
func (e manualStatusError) HTTPStatusCode() int { return int(e) }

type manualProviderRetryError struct{ deadline time.Time }

func (e manualProviderRetryError) Error() string            { return "temporarily rate limited" }
func (e manualProviderRetryError) HTTPStatusCode() int      { return 429 }
func (e manualProviderRetryError) RetryDeadline() time.Time { return e.deadline }

func TestManualProviderDeadlineSurvivesRestartAndCannotBeRetriedEarly(t *testing.T) {
	ctx := context.Background()
	provider := time.Now().Add(6 * time.Minute)
	client := &fakeClient{addErr: manualProviderRetryError{deadline: provider}}
	svc, db, itemID := setup(t, nil, client)
	req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1, Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:limited", Indexer: "idx", Protocol: "torrent", RequestID: "manual-provider-deadline"}
	id, err := svc.GrabManual(ctx, req)
	if err == nil {
		t.Fatal("expected provider-limited submission to remain retryable")
	}
	client.addErr = nil
	if replay, replayErr := svc.GrabManual(ctx, req); replay != id || replayErr == nil {
		t.Fatalf("same request replay bypassed provider deadline: id=%d err=%v", replay, replayErr)
	}
	if len(client.added) != 1 {
		t.Fatalf("provider deadline retry duplicated Add: %v", client.added)
	}
	dir := filepath.Dir(db.Path)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if rows, e := reopened.ListManualAdmissionRetries(ctx, provider.Add(-time.Second), 32); e != nil || len(rows) != 0 {
		t.Fatalf("manual retry became due before provider deadline: %+v err=%v", rows, e)
	}
	if rows, e := reopened.ListManualAdmissionRetries(ctx, provider.Add(time.Second), 32); e != nil || len(rows) != 1 || rows[0].DownloadID != id {
		t.Fatalf("manual retry was not restored at provider deadline: %+v err=%v", rows, e)
	}
}

func TestManualAdmissionStopsPermanentRefusalAndDoesNotReplayCancelledUncertain(t *testing.T) {
	ctx := context.Background()
	for _, status := range []int{409, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := &fakeClient{addErr: manualStatusError(status)}
			svc, db, itemID := setup(t, nil, client)
			req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1, Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:permanent", Indexer: "idx", Protocol: "torrent", RequestID: fmt.Sprintf("manual-permanent-%d", status)}
			id, err := svc.GrabManual(ctx, req)
			if err == nil {
				t.Fatalf("%d refusal was treated as a retryable uncertain submission", status)
			}
			dl, loadErr := db.GetDownload(ctx, id)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if dl.SubmissionPhase != "rejected" || dl.State != "failed" {
				t.Fatalf("permanent refusal state=%+v", dl)
			}
			if rows, e := db.ListManualAdmissionRetries(ctx, time.Now().Add(24*time.Hour), 32); e != nil || len(rows) != 0 {
				t.Fatalf("permanent refusal remains retryable: rows=%+v err=%v", rows, e)
			}
		})
	}

	client := &fakeClient{addErr: errors.New("lost acknowledgement")}
	svc, db, itemID := setup(t, nil, client)
	req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1, Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:cancel", Indexer: "idx", Protocol: "torrent", RequestID: "manual-cancel-uncertain"}
	id, err := svc.GrabManual(ctx, req)
	if err == nil {
		t.Fatal("expected uncertain request")
	}
	if _, e := db.W.ExecContext(ctx, `UPDATE downloads SET manual_retry_at=0 WHERE id=?`, id); e != nil {
		t.Fatal(e)
	}
	if e := db.ParkDownload(ctx, id, "operator cancellation; deliberate search required"); e != nil {
		t.Fatal(e)
	}
	rows, e := db.ListManualAdmissionRetries(ctx, time.Now().Add(time.Hour), 32)
	if e != nil || len(rows) != 0 {
		t.Fatalf("cancelled manual request remained runnable: %+v err=%v", rows, e)
	}
	if _, e = svc.resumeManualRequest(ctx, id); e == nil {
		t.Fatal("direct resume ignored cancellation")
	}
	if len(client.added) != 1 {
		t.Fatalf("cancelled request was resubmitted %d times", len(client.added))
	}
}

func TestManualAdmissionStopsTypedClientRejection(t *testing.T) {
	client := &fakeClient{addErr: fmt.Errorf("client refused request: %w", ports.ErrAdmissionRejected)}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:explicit-rejection",
		Indexer: "idx", Protocol: "torrent", RequestID: "manual-explicit-client-refusal"}
	id, err := svc.GrabManual(ctx, req)
	if !errors.Is(err, ports.ErrAdmissionRejected) {
		t.Fatalf("GrabManual() err=%v, want typed definite rejection", err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.SubmissionPhase != "rejected" || dl.State != "failed" {
		t.Fatalf("explicit refusal row=%+v, want terminal failed/rejected", dl)
	}
	if retries, err := db.ListManualAdmissionRetries(ctx, time.Now().Add(24*time.Hour), 32); err != nil || len(retries) != 0 {
		t.Fatalf("explicit refusal remained retryable: rows=%+v err=%v", retries, err)
	}
	if len(client.added) != 1 {
		t.Fatalf("explicit refusal caused %d client submissions, want one", len(client.added))
	}
}

func TestStartupWorkerDispatchesPendingManualRequestWithoutAnotherGrab(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:restart",
		Indexer: "idx", Protocol: "torrent", RequestID: "manual-restart-1"}
	id, err := svc.GrabManual(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetSubmissionPhase(ctx, id, "pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET handle='' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.added = nil
	client.mu.Unlock()

	// Construct a new service over the durable store and run its ordinary
	// startup workers. The recovery must use the persisted payload; no second
	// GrabManual call is made.
	fresh := New(db, nil, nil,
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} },
		func(ports.ClientConfig) ports.DownloadClient { return client },
	)
	workerCtx, cancel := context.WithCancel(ctx)
	fresh.StartImporters(workerCtx)
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			cancel()
			fresh.WaitImporters()
			t.Fatal("startup worker did not dispatch the reserved manual request")
		case <-ticker.C:
			dl, loadErr := db.GetDownload(ctx, id)
			client.mu.Lock()
			added := append([]string(nil), client.added...)
			client.mu.Unlock()
			if loadErr == nil && dl.SubmissionPhase == "submitted" && len(added) == 1 && added[0] == "magnet:restart" {
				cancel()
				fresh.WaitImporters()
				return
			}
		}
	}
}

func TestManualStatusMatchingRequiresExactTokenWhenAvailable(t *testing.T) {
	dl := sqlite.Download{Intent: GrabIntentManualAdditional, ReleaseTitle: "same release", Transfer: "token-a", Handle: "job-7"}
	statuses := []ports.DownloadStatus{{Name: "same release", Transfer: "token-b", Handle: "job-7"}}
	if _, ok := matchStatus(dl, statuses); ok {
		t.Fatal("a reused handle with a conflicting supplied transfer token matched")
	}
	statuses[0].Transfer = "token-a"
	if _, ok := matchStatus(dl, statuses); !ok {
		t.Fatal("the exact transfer token did not match")
	}
}

func TestExactTransferCorrelationDoesNotDowngradeButLegacyKeepsNativeHandle(t *testing.T) {
	exact := sqlite.Download{ExactTransferCorrelation: true, ReleaseTitle: "same release", Transfer: "token-a"}
	for _, statuses := range [][]ports.DownloadStatus{
		{{Name: "same release", Handle: "unrelated-handle"}},
		{{Name: "same release", Handle: "unrelated-handle", Transfer: "token-b"}},
	} {
		if _, ok := matchStatus(exact, statuses); ok {
			t.Fatalf("exact-correlated admission downgraded to title/foreign-token match: %+v", statuses)
		}
	}
	legacy := sqlite.Download{ReleaseTitle: "same release", Transfer: "old-token", Handle: "native-handle"}
	statuses := []ports.DownloadStatus{{Name: "same release", Handle: "native-handle", Transfer: "unrelated-token"}}
	if matched, ok := matchStatus(legacy, statuses); !ok || matched.Handle != "native-handle" {
		t.Fatalf("legacy exact native handle was suppressed by unrelated tagged row: %+v, ok=%v", matched, ok)
	}
	if _, ok := matchStatus(exact, []ports.DownloadStatus{{Name: exact.ReleaseTitle, Handle: "same-name-no-token"}}); ok {
		t.Fatal("accepted handleless exact-correlated request matched an untagged same-title row")
	}
	for name, admitted := range map[string]sqlite.Download{
		"native request-token protocol": {RequestTokenIdempotent: true, ReleaseTitle: "same release", Transfer: "runner-token", Handle: "runner-job"},
		"delivery protocol":             {DeliveryExpected: true, ReleaseTitle: "same release", Transfer: "delivery-token", Handle: "runner-job"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := matchStatus(admitted, []ports.DownloadStatus{{Name: admitted.ReleaseTitle, Handle: ports.Handle(admitted.Handle)}}); ok {
				t.Fatal("managed native admission downgraded to an untagged handle")
			}
			if _, ok := matchStatus(admitted, []ports.DownloadStatus{{Name: admitted.ReleaseTitle, Handle: ports.Handle(admitted.Handle), Transfer: admitted.Transfer}}); !ok {
				t.Fatal("the exact transfer token did not correlate")
			}
		})
	}
	duplicateTag := sqlite.Download{RequestTokenIdempotent: true, Transfer: "duplicate-token", Handle: "bound-job"}
	if _, ok := matchStatus(duplicateTag, []ports.DownloadStatus{
		{Transfer: duplicateTag.Transfer, Handle: ports.Handle(duplicateTag.Handle)},
		{Transfer: duplicateTag.Transfer, Handle: "other-job"},
	}); ok {
		t.Fatal("duplicate exact tags were treated as an unambiguous status")
	}
	if _, ok := matchStatus(duplicateTag, []ports.DownloadStatus{{Transfer: duplicateTag.Transfer, Handle: "reused-job"}}); ok {
		t.Fatal("exact token with a conflicting pinned handle was accepted")
	}
}

func TestAutomaticHandlelessAdmissionCannotBindUntaggedSameTitle(t *testing.T) {
	ctx := context.Background()
	client := &handlelessTransferClient{}
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	title := "Test.Show.S01E01.1080p.WEB-DL.EXACT"
	id, err := svc.grab(ctx, GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1, Title: title,
		DownloadURL: "magnet:automatic-handleless", Indexer: "idx", Protocol: "torrent"}, false)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil || !dl.ExactTransferCorrelation || dl.Handle != "" || dl.SubmissionPhase != "submitted" {
		t.Fatalf("automatic handleless admission=%+v err=%v", dl, err)
	}
	client.statuses = []ports.DownloadStatus{{Name: title, Handle: "same-title-other-job", State: ports.StateDownloading}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := db.GetDownload(ctx, id)
	if err != nil || current.Handle != "" || current.State != "grabbed" || current.SubmissionPhase != "submitted" {
		t.Fatalf("untagged same-title status was bound to handleless exact request: %+v err=%v", current, err)
	}
}

func TestManualUncertainRequestDoesNotBindByTitleInventory(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	id, err := svc.GrabManual(ctx, GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1, Title: "Same.Show.S01E01.1080p.WEB-DL",
		DownloadURL: "magnet:uncertain", Indexer: "idx", Protocol: "torrent", RequestID: "manual-uncertain-title"})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetSubmissionPhase(ctx, id, "uncertain"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET handle='' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	dl.Handle = ""
	cfg := ports.ClientConfig{ID: dl.ClientID, Name: "client", URL: "http://client"}
	svc.observeUncertain(ctx, dl, cfg, []ports.DownloadStatus{{Name: dl.ReleaseTitle, Handle: "different-request"}}, true)
	current, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if current.Handle != "" || current.SubmissionPhase != "uncertain" {
		t.Fatalf("manual request was bound by a title-only inventory match: %+v", current)
	}
}

func TestManualSubmittedPhaseCannotOutliveItsHandle(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:handle-commit",
		Indexer: "idx", Protocol: "torrent", RequestID: "manual-handle-commit"}
	id, err := svc.GrabManual(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET handle='' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GrabManual(ctx, req); err == nil {
		t.Fatal("replay treated a submitted row without a durable handle as complete")
	}
	if len(client.added) != 1 {
		t.Fatalf("ambiguous replay made another Add: %v", client.added)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil || dl.SubmissionPhase != "submitted" || dl.Handle != "" {
		t.Fatalf("ambiguous row changed phase or handle: %+v err=%v", dl, err)
	}
}

func TestManualRequestTombstoneSurvivesActivityRemoval(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:retained",
		Indexer: "idx", Protocol: "torrent", RequestID: "manual-retained-1"}
	id, err := svc.GrabManual(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDownloadState(ctx, id, "imported", 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPayloadRemoved(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClearImportedDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetDownload(ctx, id); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("dismissed Activity row still exists: %v", err)
	}
	if _, err := svc.GrabManual(ctx, req); !errors.Is(err, sqlite.ErrManualGrabRequestRetained) {
		t.Fatalf("replay after Activity removal = %v; want durable tombstone block", err)
	}
	if len(client.added) != 1 {
		t.Fatalf("replay after Activity removal added %d jobs", len(client.added))
	}
}

func TestManualAdditionalImportsDoNotMutatePrimaryAtAnyQuality(t *testing.T) {
	for _, tc := range []struct {
		name       string
		resolution int
	}{
		{name: "identical", resolution: 1080},
		{name: "worse", resolution: 720},
		{name: "better", resolution: 2160},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeClient{}
			svc, db, itemID := setup(t, nil, client)
			ctx := context.Background()
			item, err := db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			episodeID := item.Seasons[0].Episodes[0].ID
			primaryPath := filepath.Join(item.Path, "Test.Show.S01E01.1080p.WEB-DL.mkv")
			primaryBytes := []byte("primary-remains-byte-for-byte-identical")
			if err := os.WriteFile(primaryPath, primaryBytes, 0o644); err != nil {
				t.Fatal(err)
			}
			fileID, err := db.UpsertFile(ctx, itemID, 0, primaryPath, int64(len(primaryBytes)))
			if err != nil {
				t.Fatal(err)
			}
			primaryQuality := quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}
			if err := db.SetFileQuality(ctx, fileID, primaryQuality); err != nil {
				t.Fatal(err)
			}
			if err := db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{episodeID}); err != nil {
				t.Fatal(err)
			}
			beforeFiles, err := db.ListFilesForItem(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			var primaryBefore *domain.MediaFile
			for i := range beforeFiles {
				if beforeFiles[i].ID == fileID {
					primaryBefore = &beforeFiles[i]
					break
				}
			}
			if primaryBefore == nil || len(primaryBefore.EpisodeIDs) != 1 || primaryBefore.EpisodeIDs[0] != episodeID {
				t.Fatalf("primary episode association was not seeded: %+v", primaryBefore)
			}
			release := fmt.Sprintf("Test.Show.S01E01.%dp.WEB-DL.MANUAL", tc.resolution)
			id, err := svc.GrabManual(ctx, GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1,
				Title: release, DownloadURL: "magnet:" + tc.name, Indexer: "idx", Protocol: "torrent", RequestID: "manual-quality-" + tc.name})
			if err != nil {
				t.Fatal(err)
			}
			dl, err := db.GetDownload(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			payload := t.TempDir()
			payloadFile := filepath.Join(payload, release+".mkv")
			if err := os.WriteFile(payloadFile, []byte("manual-copy-"+tc.name), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.importDownload(ctx, dl, payload, true); err != nil {
				t.Fatalf("manual %s import: %v", tc.name, err)
			}
			primaryAfter, err := os.ReadFile(primaryPath)
			if err != nil || string(primaryAfter) != string(primaryBytes) {
				t.Fatalf("primary bytes changed for %s manual copy: %q err=%v", tc.name, primaryAfter, err)
			}
			files, err := db.ListFilesForItem(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			var primary, additional *domain.MediaFile
			for i := range files {
				if files[i].Path == primaryPath && files[i].CopyID == 0 {
					primary = &files[i]
				}
				if files[i].CopyID == dl.CopyID {
					additional = &files[i]
				}
			}
			if primary == nil || primary.Quality != primaryQuality || len(primary.EpisodeIDs) != 1 || primary.EpisodeIDs[0] != episodeID {
				t.Fatalf("primary record changed for %s: %+v", tc.name, primary)
			}
			if additional == nil || additional.Path == primaryPath || additional.CopyID != dl.CopyID {
				t.Fatalf("manual file was not attributed to independent copy: %+v", additional)
			}
			if _, err := os.Stat(additional.Path); err != nil {
				t.Fatalf("manual copy path not present: %+v err=%v", additional, err)
			}
			beforeHash := sha256.Sum256(primaryBytes)
			afterHash := sha256.Sum256(primaryAfter)
			if beforeHash != afterHash {
				t.Fatalf("primary digest changed for %s", tc.name)
			}
		})
	}
}

func TestManualBlacklistOverrideIsOneTimeAndManualFailureDoesNotRegrab(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	release := "Test.Show.S01E01.1080p.WEB-DL.BLOCKED"
	if err := db.AddBlocklist(ctx, itemID, release, "idx", "permanent parity failure"); err != nil {
		t.Fatal(err)
	}
	req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1, Title: release,
		DownloadURL: "magnet:blocked", Indexer: "idx", Protocol: "torrent", RequestID: "manual-override-1"}
	if _, err := svc.GrabManual(ctx, req); err == nil || !strings.Contains(err.Error(), "permanent parity failure") {
		t.Fatalf("blacklisted release without confirmation = %v", err)
	}
	req.AllowBlacklisted = true
	id, err := svc.GrabManual(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil || !dl.BlacklistOverride {
		t.Fatalf("persisted override = %v, download=%+v, err=%v", dl.BlacklistOverride, dl, err)
	}
	if blocked, err := db.IsBlocklisted(ctx, release, "idx"); err != nil || !blocked {
		t.Fatalf("manual override removed global automatic blacklist: blocked=%v err=%v", blocked, err)
	}
	svc.handleFailure(ctx, dl, 0, "permanent manual failure", false)
	if len(client.added) != 1 {
		t.Fatalf("permanent manual failure triggered automatic replacement; adds=%v", client.added)
	}
}

func TestManualEpisodeCopyImportStaysInsideRequestedEpisodeScope(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	release := "Test.Show.S01E01.1080p.WEB-DL.MANUAL-PACK"
	id, err := svc.GrabManual(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1, Title: release,
		DownloadURL: "magnet:manual-pack", Indexer: "idx", Protocol: "torrent", RequestID: "manual-episode-scope",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := t.TempDir()
	for _, n := range []string{"S01E01", "S01E02"} {
		name := "Test.Show." + n + ".1080p.WEB-DL.mkv"
		if err := os.WriteFile(filepath.Join(payload, name), corpusFile(t, wholeCorpus1080), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET save_path=?, import_path=?, state='downloaded' WHERE id=?`, payload, payload, id); err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.runImport(ctx, dl); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(files[0].EpisodeIDs) != 1 || files[0].EpisodeIDs[0] != 1 || files[0].CopyID != dl.CopyID {
		t.Fatalf("manual episode import widened its scope: %+v", files)
	}
}

func mustMediaPath(t *testing.T, db *sqlite.DB, itemID int64) string {
	t.Helper()
	item, err := db.GetMediaItemFull(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	return item.Path
}
