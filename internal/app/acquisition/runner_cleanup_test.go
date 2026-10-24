package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type runnerSubmissionRetryClient struct {
	*fakeClient
	firstErr   error
	urls       []string
	options    []ports.AddOptions
	addEntered chan struct{}
	addRelease chan struct{}
}

func (c *runnerSubmissionRetryClient) ListDeliveries(context.Context, uint64, int) (ports.RunnerDeliveryPage, error) {
	return ports.RunnerDeliveryPage{ProtocolVersion: 1}, nil
}

func (c *runnerSubmissionRetryClient) AcceptDelivery(context.Context, string, ports.RunnerDeliveryAcceptance) error {
	return nil
}

func (c *runnerSubmissionRetryClient) SupportsRequestTokenIdempotency(context.Context) (bool, error) {
	return true, nil
}

func (c *runnerSubmissionRetryClient) AddWithOptions(_ context.Context, url, _ string, options ports.AddOptions) (ports.Handle, error) {
	if c.addEntered != nil {
		select {
		case c.addEntered <- struct{}{}:
		default:
		}
	}
	if c.addRelease != nil {
		<-c.addRelease
	}
	c.urls = append(c.urls, url)
	c.options = append(c.options, options)
	if c.firstErr != nil {
		err := c.firstErr
		c.firstErr = nil
		return "", err
	}
	return "runner-job-exact", nil
}

func TestAutomaticRunnerSubmissionRetrySkipsOperatorCancelledUncertainRow(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	client := &runnerSubmissionRetryClient{fakeClient: base, firstErr: errors.New("lost Add response")}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	id, err := svc.Grab(ctx, GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.cancel", DownloadURL: "https://indexer.example/cancel",
		Indexer: "idx", Protocol: "torrent"})
	if id == 0 || err == nil {
		t.Fatalf("Grab() id=%d err=%v, want uncertain admitted row", id, err)
	}
	if err = svc.RemoveDownload(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	before := len(client.urls)
	svc.processRunnerSubmissionRetries(ctx)
	row, err := db.GetDownload(ctx, id)
	if err != nil || row.ParkedAt.IsZero() || row.SubmissionPhase != "uncertain" || len(client.urls) != before {
		t.Fatalf("cancelled uncertain admission was replayed: row=%+v calls=%v err=%v", row, client.urls, err)
	}
}

func TestAutomaticSubmissionPendingAcceptsPersistedZeroParkedTimestamp(t *testing.T) {
	dl := sqlite.Download{Intent: GrabIntentAutomatic, SubmissionPhase: "submitting", State: "grabbed", ParkedAt: time.UnixMilli(0)}
	if !automaticSubmissionPending(dl) {
		t.Fatal("unparked database timestamp was treated as an operator cancellation")
	}
	dl.ParkedAt = time.Now()
	if automaticSubmissionPending(dl) {
		t.Fatal("persisted parked timestamp was treated as unparked")
	}
}

func TestAutomaticRunnerSubmissionCancellationRacesBlockedAddWithoutDuplicate(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	client := &runnerSubmissionRetryClient{fakeClient: base, addEntered: make(chan struct{}, 1), addRelease: make(chan struct{})}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	type grabResult struct {
		id  int64
		err error
	}
	grabbed := make(chan grabResult, 1)
	go func() {
		id, err := svc.Grab(ctx, GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1,
			Title: "Test.Show.S01E01.1080p.WEB-DL.cancel-race", DownloadURL: "https://indexer.example/cancel-race",
			Indexer: "idx", Protocol: "torrent"})
		grabbed <- grabResult{id: id, err: err}
	}()
	select {
	case <-client.addEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic Add did not reach the blocked client")
	}
	rows, err := db.ListActiveDownloads(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("admitted row before blocked Add=%+v err=%v", rows, err)
	}
	removed := make(chan error, 1)
	go func() { removed <- svc.RemoveDownload(ctx, rows[0].ID, false) }()
	close(client.addRelease)
	select {
	case result := <-grabbed:
		if result.id != rows[0].ID || result.err != nil {
			t.Fatalf("Grab() result=%+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Grab did not finish after the client acknowledged Add")
	}
	select {
	case err = <-removed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not finish after Add released the download lock")
	}
	row, err := db.GetDownload(ctx, rows[0].ID)
	if err != nil || row.ParkedAt.IsZero() || row.SubmissionPhase != "submitted" {
		t.Fatalf("cancellation did not retain the exact acknowledged request: row=%+v err=%v", row, err)
	}
	before := len(client.urls)
	svc.processRunnerSubmissionRetries(ctx)
	if len(client.urls) != before {
		t.Fatalf("cancelled admitted request was sent again: calls=%v", client.urls)
	}
}

func TestAutomaticRunnerSubmissionReplaysExactPayloadAfterRestartAndLostAddAck(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("download clients=%+v err=%v", clients, err)
	}
	client := &runnerSubmissionRetryClient{fakeClient: base, firstErr: errors.New("Runner accepted request but Add response was lost")}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	id, grabErr := svc.Grab(ctx, GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL", DownloadURL: "https://indexer.example/release/one",
		Indexer: "idx", Protocol: "torrent"})
	if id == 0 || grabErr == nil {
		t.Fatalf("Grab() id=%d err=%v, want persisted uncertain admission after simulated lost response", id, grabErr)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	var payload manualSubmissionPayload
	if err := json.Unmarshal([]byte(row.ExecutionPayload), &payload); err != nil {
		t.Fatalf("automatic payload was not committed in the admission row: payload=%q err=%v", row.ExecutionPayload, err)
	}
	if row.SubmissionPhase != "uncertain" || row.Handle != "" || row.Transfer == "" || !row.RequestTokenIdempotent || payload.URL != "https://indexer.example/release/one" || len(client.urls) != 1 {
		t.Fatalf("lost Add response row=%+v calls=%v", row, client.urls)
	}
	var firstRetry int64
	if err := db.R.QueryRowContext(ctx, `SELECT runner_submission_retry_at FROM downloads WHERE id=?`, id).Scan(&firstRetry); err != nil {
		t.Fatal(err)
	}
	if time.UnixMilli(firstRetry).Before(time.Now().Add(25 * time.Second)) {
		t.Fatalf("initial automatic Add error did not use 30s backoff: %v", time.UnixMilli(firstRetry))
	}
	dbPath := db.Path
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Advance the persisted due time without sleeping through the production
	// backoff; the payload and generation remain those from before restart.
	if _, err := reopened.W.ExecContext(ctx, `UPDATE downloads SET runner_submission_retry_at=0 WHERE id=? AND generation_key=?`, dl.ID, dl.GenerationKey); err != nil {
		t.Fatal(err)
	}
	svc.db = reopened
	svc.processRunnerSubmissionRetries(ctx)
	settled, err := reopened.GetDownload(ctx, dl.ID)
	if err != nil || settled.SubmissionPhase != "submitted" || settled.Handle != "runner-job-exact" {
		t.Fatalf("replayed submission row=%+v err=%v", settled, err)
	}
	if len(client.urls) != 2 || client.urls[0] != client.urls[1] || client.options[0].Transfer != dl.Transfer || client.options[1].Transfer != dl.Transfer {
		t.Fatalf("retry changed request identity: urls=%v options=%+v", client.urls, client.options)
	}
	svc.processRunnerSubmissionRetries(ctx)
	if len(client.urls) != 2 {
		t.Fatalf("submitted request retried again: calls=%v", client.urls)
	}
}

func TestAutomaticRetryWorkerRechecksDeadlineAfterWaitingForInitialAdd(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	client := &runnerSubmissionRetryClient{fakeClient: base, firstErr: errors.New("lost Add ACK"), addEntered: make(chan struct{}, 2), addRelease: make(chan struct{})}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	grabDone := make(chan struct {
		id  int64
		err error
	}, 1)
	go func() {
		id, err := svc.Grab(ctx, GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1, Title: "Test.Show.S01E01.1080p.WEB-DL.race", DownloadURL: "https://indexer.example/race", Indexer: "idx", Protocol: "torrent"})
		grabDone <- struct {
			id  int64
			err error
		}{id, err}
	}()
	select {
	case <-client.addEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("initial Add did not start")
	}
	workerDone := make(chan struct{})
	go func() { svc.processRunnerSubmissionRetries(ctx); close(workerDone) }()
	// The row is in submitting with a due timestamp while the initial Add owns
	// its per-download lock. Let the retry scanner select it and wait there.
	time.Sleep(75 * time.Millisecond)
	close(client.addRelease)
	grab := <-grabDone
	if grab.id == 0 || grab.err == nil {
		t.Fatalf("initial Add=%d %v; want lost acknowledgement", grab.id, grab.err)
	}
	<-workerDone
	if len(client.urls) != 1 {
		t.Fatalf("retry worker bypassed the deadline written by initial Add: Add calls=%v", client.urls)
	}
	dl, err := db.GetDownload(ctx, grab.id)
	if err != nil || dl.SubmissionPhase != "uncertain" {
		t.Fatalf("lost response row=%+v err=%v", dl, err)
	}
}

func TestStartImportersResumesDueAutomaticSubmission(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	client := &runnerSubmissionRetryClient{fakeClient: base}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	payload, err := json.Marshal(manualSubmissionPayload{URL: "https://indexer.example/startup", Category: "tv", Priority: 0})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{
		MediaItemID: itemID, WantableIDs: []string{"movie:" + itoa(itemID)},
		ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL.startup", Indexer: "idx", Protocol: "torrent",
		ClientID: 1, State: "grabbed", Transfer: "startup-replay-token", Intent: GrabIntentAutomatic,
		SubmissionPhase: "uncertain", ExecutionPayload: string(payload), RequestTokenIdempotent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	svc.StartImporters(workerCtx)
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			cancel()
			svc.WaitImporters()
			t.Fatal("StartImporters did not resume the persisted automatic submission")
		case <-ticker.C:
			fresh, loadErr := db.GetDownload(ctx, dl.ID)
			if loadErr == nil && fresh.SubmissionPhase == "submitted" && fresh.Handle == "runner-job-exact" {
				cancel()
				svc.WaitImporters()
				if len(client.urls) != 1 || client.urls[0] != "https://indexer.example/startup" {
					t.Fatalf("startup replay payloads=%v, want exact persisted URL", client.urls)
				}
				return
			}
		}
	}
}

func TestRunnerKeepIsSettledCustodyAndDoesNotBlockAutomaticReplacement(t *testing.T) {
	tests := []struct {
		name      string
		admission ports.RequestAdmission
		wantHold  bool
	}{
		{name: "unresolved runner custody", admission: ports.RequestAdmission{CustodyState: "runner_owned", SourceCleanupStatus: "pending"}, wantHold: true},
		{name: "runner cleanup complete", admission: ports.RequestAdmission{CustodyState: "runner_owned", SourceCleanupStatus: "removed"}, wantHold: false},
		{name: "explicit keep", admission: ports.RequestAdmission{CustodyState: "runner_owned", KeepSourceOnFailure: true}, wantHold: false},
		{name: "kept by user", admission: ports.RequestAdmission{CustodyState: "kept_by_user", KeepSourceOnFailure: true}, wantHold: false},
		{name: "no payload", admission: ports.RequestAdmission{CustodyState: "no_payload"}, wantHold: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runnerFailureNeedsCleanupBeforeReplacement(tt.admission); got != tt.wantHold {
				t.Fatalf("runnerFailureNeedsCleanupBeforeReplacement() = %v, want %v", got, tt.wantHold)
			}
		})
	}
}

type requestAdmissionFakeClient struct {
	*fakeClient
	admission ports.RequestAdmission
	found     bool
	lookups   int
	tokens    []string
}

func (c *requestAdmissionFakeClient) GetRequestAdmission(_ context.Context, token string) (ports.RequestAdmission, bool, error) {
	c.lookups++
	c.tokens = append(c.tokens, token)
	if token != c.admission.RequestToken {
		return ports.RequestAdmission{}, false, nil
	}
	return c.admission, c.found, nil
}

type statusHookClient struct {
	*fakeClient
	hook func()
}

func (c *statusHookClient) Statuses(ctx context.Context) ([]ports.DownloadStatus, error) {
	if c.hook != nil {
		c.hook()
	}
	return c.fakeClient.Statuses(ctx)
}

func TestRefreshQueueUsesDurableRunnerReceiptAfterHistoryDisappears(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("download clients=%v err=%v", clients, err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, ClientID: clients[0].ID,
		ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL", Protocol: "torrent", State: "downloading",
		Transfer: "transfer-after-history-clear", RequestTokenIdempotent: true, Intent: GrabIntentManualAdditional})
	if err != nil {
		t.Fatal(err)
	}
	receipt := ports.RequestAdmission{ProtocolVersion: 1, RequestToken: dl.Transfer, JobID: "removed-job", Generation: "runner-admission-generation",
		RunnerInstanceID: "runner-instance", Outcome: "failed", TerminalOutcome: "FAILURE/FETCH", CustodyState: "no_payload"}
	client := &requestAdmissionFakeClient{fakeClient: base, admission: receipt, found: true}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	if err = svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	settled, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != "failed" || client.lookups != 1 {
		t.Fatalf("durable receipt was not reconciled after history disappeared: state=%s lookups=%d", settled.State, client.lookups)
	}
}

func TestRunnerDeletedCustodyReviewAllowsExplicitKeep(t *testing.T) {
	for _, tt := range []struct {
		name       string
		admission  ports.RequestAdmission
		wantReview bool
	}{
		{name: "unresolved bytes", admission: ports.RequestAdmission{Outcome: "deleted", CustodyState: "runner_owned"}, wantReview: true},
		{name: "Runner confirmed deletion", admission: ports.RequestAdmission{Outcome: "deleted", CustodyState: "deleted_by_runner"}},
		{name: "kept after queue removal", admission: ports.RequestAdmission{Outcome: "deleted", CustodyState: "kept_by_user", KeepSourceOnFailure: true}},
		{name: "explicit keep flag", admission: ports.RequestAdmission{Outcome: "deleted", CustodyState: "runner_owned", KeepSourceOnFailure: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := runnerDeletedNeedsCustodyReview(tt.admission); got != tt.wantReview {
				t.Fatalf("runnerDeletedNeedsCustodyReview() = %v, want %v", got, tt.wantReview)
			}
		})
	}
}

func TestRunnerReceiptPollAbortsWhenActivityWasRemovedBeforeStatusReconcile(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("clients=%v err=%v", clients, err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, ClientID: clients[0].ID,
		ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL", Protocol: "torrent", State: "downloading", RequestTokenIdempotent: true})
	if err != nil {
		t.Fatal(err)
	}
	transfer := "removed-row-transfer"
	if err = db.SetDownloadHandle(ctx, dl.ID, "runner-job-removed", transfer); err != nil {
		t.Fatal(err)
	}
	hooked := &statusHookClient{fakeClient: base, hook: func() {
		if _, e := db.W.ExecContext(ctx, `DELETE FROM downloads WHERE id=? AND generation_key=?`, dl.ID, dl.GenerationKey); e != nil {
			t.Errorf("delete Activity row during Statuses: %v", e)
		}
	}}
	client := &requestAdmissionFakeClient{fakeClient: hooked.fakeClient, admission: ports.RequestAdmission{RequestToken: transfer}}
	// Keep the status mutation hook and the optional receipt endpoint on one
	// client object, matching the real adapter's two capabilities.
	combined := &statusHookAdmissionClient{statusHookClient: hooked, requestAdmissionFakeClient: client}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return combined }
	base.statuses = []ports.DownloadStatus{{Handle: "runner-job-removed", State: ports.StateFailed}}
	if err = svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if client.lookups != 0 {
		t.Fatalf("receipt lookup ran after Activity row removal: lookups=%d", client.lookups)
	}
	if _, err = db.GetDownload(ctx, dl.ID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("removed download reappeared: err=%v", err)
	}
}

func TestRunnerReceiptPollCannotRetargetAReusedDownloadID(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("clients=%v err=%v", clients, err)
	}
	old, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, ClientID: clients[0].ID,
		ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL-old", Protocol: "torrent", State: "downloading", RequestTokenIdempotent: true})
	if err != nil {
		t.Fatal(err)
	}
	oldTransfer := "old-generation-transfer"
	if err = db.SetDownloadHandle(ctx, old.ID, "reused-handle", oldTransfer); err != nil {
		t.Fatal(err)
	}
	var replacement sqlite.Download
	hooked := &statusHookClient{fakeClient: base, hook: func() {
		var e error
		if _, e := db.W.ExecContext(ctx, `DELETE FROM downloads WHERE id=? AND generation_key=?`, old.ID, old.GenerationKey); e != nil {
			t.Errorf("remove old Activity generation: %v", e)
			return
		}
		replacement, e = db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, ClientID: clients[0].ID,
			ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL-new", Protocol: "torrent", State: "downloading", RequestTokenIdempotent: true})
		if e != nil {
			t.Errorf("insert replacement Activity generation: %v", e)
			return
		}
		if replacement.ID != old.ID {
			t.Errorf("fixture did not reuse the deleted integer ID: old=%d new=%d", old.ID, replacement.ID)
			return
		}
		if e = db.SetDownloadHandle(ctx, replacement.ID, "reused-handle", "new-generation-transfer"); e != nil {
			t.Errorf("set new generation handle: %v", e)
		}
	}}
	client := &requestAdmissionFakeClient{fakeClient: hooked.fakeClient, admission: ports.RequestAdmission{RequestToken: oldTransfer}}
	combined := &statusHookAdmissionClient{statusHookClient: hooked, requestAdmissionFakeClient: client}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return combined }
	base.statuses = []ports.DownloadStatus{{Handle: "reused-handle", State: ports.StateFailed}}
	if err = svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if client.lookups != 0 {
		t.Fatalf("stale queue observation was applied to the reused ID: receipt lookups=%d tokens=%v", client.lookups, client.tokens)
	}
	fresh, err := db.GetDownload(ctx, replacement.ID)
	if err != nil || fresh.GenerationKey != replacement.GenerationKey || fresh.Transfer != "new-generation-transfer" || fresh.State != "downloading" {
		t.Fatalf("reused ID was mutated by stale observation: row=%+v err=%v", fresh, err)
	}
}

type statusHookAdmissionClient struct {
	*statusHookClient
	*requestAdmissionFakeClient
}

func (c *statusHookAdmissionClient) GetRequestAdmission(ctx context.Context, token string) (ports.RequestAdmission, bool, error) {
	return c.requestAdmissionFakeClient.GetRequestAdmission(ctx, token)
}

func (c *statusHookAdmissionClient) Add(ctx context.Context, url, category string) (ports.Handle, error) {
	return c.statusHookClient.Add(ctx, url, category)
}

func (c *statusHookAdmissionClient) Statuses(ctx context.Context) ([]ports.DownloadStatus, error) {
	return c.statusHookClient.Statuses(ctx)
}

func (c *statusHookAdmissionClient) Remove(ctx context.Context, handle ports.Handle, deleteData bool) error {
	return c.statusHookClient.Remove(ctx, handle, deleteData)
}

func (c *statusHookAdmissionClient) Test(ctx context.Context) error {
	return c.statusHookClient.Test(ctx)
}

func TestRunnerReceiptPollingIsBoundedAndFairAcrossSweep(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("clients=%v err=%v", clients, err)
	}
	var statuses []ports.DownloadStatus
	var tokens []string
	for i := 0; i < 100; i++ {
		dl, e := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, ClientID: clients[0].ID,
			ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL", Protocol: "torrent", State: "downloading", RequestTokenIdempotent: true})
		if e != nil {
			t.Fatal(e)
		}
		transfer := fmt.Sprintf("fair-transfer-%d", i)
		handle := fmt.Sprintf("runner-job-%d", i)
		if e = db.SetDownloadHandle(ctx, dl.ID, handle, transfer); e != nil {
			t.Fatal(e)
		}
		tokens = append(tokens, transfer)
		statuses = append(statuses, ports.DownloadStatus{Handle: ports.Handle(handle), State: ports.StateFailed})
	}
	base.statuses = statuses
	client := &requestAdmissionFakeClient{fakeClient: base}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	for round := 0; round < 50; round++ {
		before := len(client.tokens)
		if err = svc.RefreshQueue(ctx); err != nil {
			t.Fatal(err)
		}
		if len(client.tokens)-before != 2 {
			t.Fatalf("queue refresh %d did %d receipt lookups; want bounded batch of two (cumulative=%d)", round, len(client.tokens)-before, len(client.tokens))
		}
	}
	seen := make(map[string]bool, len(client.tokens))
	for _, token := range client.tokens {
		if seen[token] {
			t.Fatalf("same due request was polled repeatedly before the tail: %v", client.tokens)
		}
		seen[token] = true
	}
	if len(seen) != 100 {
		t.Fatalf("bounded polling starved tail rows: got %d distinct tokens, want all 100; inserted=%v polled=%v", len(seen), tokens, client.tokens)
	}
}
