package acquisition

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type workerAdmissionError struct {
	status   int
	deadline time.Time
	message  string
}

func (e workerAdmissionError) Error() string       { return e.message }
func (e workerAdmissionError) HTTPStatusCode() int { return e.status }
func (e workerAdmissionError) RetryDeadline() time.Time {
	return e.deadline
}

type workerAdmissionClient struct {
	*fakeClient
	err   error
	adds  int
	urls  []string
	trans []string
}

func (c *workerAdmissionClient) AddWithOptions(_ context.Context, url, _ string, options ports.AddOptions) (ports.Handle, error) {
	c.adds++
	c.urls = append(c.urls, url)
	c.trans = append(c.trans, options.Transfer)
	return "", c.err
}

func (c *workerAdmissionClient) ListDeliveries(context.Context, uint64, int) (ports.RunnerDeliveryPage, error) {
	return ports.RunnerDeliveryPage{ProtocolVersion: 1}, nil
}

func (c *workerAdmissionClient) SupportsRequestTokenIdempotency(context.Context) (bool, error) {
	return true, nil
}

func TestRunnerSubmissionRetryWorkerPermanentlyRejectsExactRequest(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("download clients=%+v err=%v", clients, err)
	}
	const url = "https://indexer.example/retry/permanent-refusal"
	const transfer = "retry-permanent-refusal-token"
	payload, err := json.Marshal(manualSubmissionPayload{URL: url, Category: "tv", Priority: 3})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{
		MediaItemID: itemID, ClientID: clients[0].ID, ReleaseTitle: "Retry.Show.S01E01.1080p.WEB-DL",
		Indexer: "idx", Protocol: "torrent", State: "grabbed", Transfer: transfer,
		Intent: GrabIntentAutomatic, SubmissionPhase: "uncertain", RequestTokenIdempotent: true,
		ExecutionPayload: string(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &workerAdmissionClient{fakeClient: base, err: workerAdmissionError{status: 422, message: "Runner refused this exact request"}}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }

	svc.processRunnerSubmissionRetries(ctx)
	rejected, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.SubmissionPhase != "rejected" || rejected.State != "failed" || !strings.Contains(rejected.Error, "permanently rejected") {
		t.Fatalf("definitive refusal was not atomically terminal: phase=%q state=%q error=%q", rejected.SubmissionPhase, rejected.State, rejected.Error)
	}
	if client.adds != 1 || len(client.urls) != 1 || client.urls[0] != url || client.trans[0] != transfer {
		t.Fatalf("worker did not send only the exact persisted request once: adds=%d urls=%v transfers=%v", client.adds, client.urls, client.trans)
	}
	svc.processRunnerSubmissionRetries(ctx)
	if client.adds != 1 {
		t.Fatalf("rejected request was replayed again: adds=%d", client.adds)
	}
}

func TestRunnerSubmissionRetryWorkerPersistsProviderDeadline(t *testing.T) {
	base := &fakeClient{}
	svc, db, itemID := setup(t, nil, base)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("download clients=%+v err=%v", clients, err)
	}
	const url = "https://indexer.example/retry/rate-limited"
	const transfer = "retry-provider-deadline-token"
	payload, err := json.Marshal(manualSubmissionPayload{URL: url, Category: "tv", Priority: 0})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{
		MediaItemID: itemID, ClientID: clients[0].ID, ReleaseTitle: "Retry.Show.S01E02.1080p.WEB-DL",
		Indexer: "idx", Protocol: "torrent", State: "grabbed", Transfer: transfer,
		Intent: GrabIntentAutomatic, SubmissionPhase: "uncertain", RequestTokenIdempotent: true,
		ExecutionPayload: string(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	providerDeadline := time.Now().Add(20 * time.Minute).Truncate(time.Millisecond)
	client := &workerAdmissionClient{fakeClient: base, err: workerAdmissionError{deadline: providerDeadline, message: "temporarily rate limited"}}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }

	svc.processRunnerSubmissionRetries(ctx)
	var persisted int64
	if err := db.R.QueryRowContext(ctx, `SELECT provider_retry_after FROM downloads WHERE id=? AND generation_key=?`, dl.ID, dl.GenerationKey).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if got := time.UnixMilli(persisted); !got.Equal(providerDeadline) {
		t.Fatalf("worker provider deadline=%v, want %v", got, providerDeadline)
	}
	before := client.adds
	svc.processRunnerSubmissionRetries(ctx)
	if client.adds != before {
		t.Fatalf("provider deadline was bypassed by a second worker pass: adds=%d", client.adds)
	}
	row, err := db.GetDownload(ctx, dl.ID)
	if err != nil || row.SubmissionPhase != "uncertain" || row.State == "failed" {
		t.Fatalf("retryable provider refusal became terminal: row=%+v err=%v", row, err)
	}
}
