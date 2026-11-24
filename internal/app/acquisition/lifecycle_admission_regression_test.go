package acquisition

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

func TestExpiredCandidateCannotDropBlacklistIdentity(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	release := ports.Release{Title: "Test.Show.S01E01.1080p.WEB-DL", DownloadURL: "magnet:expired", Indexer: "idx", GUID: "stable-guid", IndexerID: 7}
	token := svc.issueCandidateToken(itemID, 0, 1, 1, release, domain.MatchEvidence{Version: 1, Matched: true})
	record := svc.candidateTokens[token]
	record.expiresAt = time.Now().Add(-time.Second)
	svc.candidateTokens[token] = record
	_, err := svc.GrabManual(ctx, GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1, Title: release.Title, DownloadURL: release.DownloadURL, Indexer: release.Indexer, Protocol: "torrent", CandidateToken: token, RequestID: "expired-action"})
	if !errors.Is(err, ErrCandidateExpired) {
		t.Fatalf("expired candidate admitted: %v", err)
	}
	if len(client.added) != 0 {
		t.Fatal("expired identity reached Add")
	}
	rows, err := db.ListDownloadReservations(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("expired evidence reserved acquisition: %+v %v", rows, err)
	}
}
func TestPendingManualRequestCanCancelBeforeDispatch(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	req := GrabRequest{MediaItemID: itemID, Season: 1, Episode: 1, Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:cancel-pending", Indexer: "idx", Protocol: "torrent", RequestID: "cancel-pending-action"}
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
	client.added = nil
	if err := svc.RemoveDownload(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.resumeManualRequest(ctx, id)
	row, err := db.GetDownload(ctx, id)
	if err != nil || row.SubmissionPhase != "rejected" || !row.Superseded || len(client.added) != 0 || len(row.WantableIDs) != 0 {
		t.Fatalf("cancelled intent dispatched or lost: %+v adds=%v err=%v", row, client.added, err)
	}
}

func TestNonIdempotentAutomaticUncertaintyWarnsWithoutReplay(t *testing.T) {
	client := &fakeClient{addErr: errors.New("response lost after Add")}
	svc, db, item := setup(t, nil, client)
	ctx := context.Background()
	id, err := svc.Grab(ctx, GrabRequest{MediaItemID: item, Season: 1, Episode: 1, Title: epRelease, DownloadURL: "magnet:uncertain-automatic", Indexer: "idx", Protocol: "torrent"})
	if err == nil || id <= 0 {
		t.Fatalf("fixture did not leave uncertain intent: %d %v", id, err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET updated_at=? WHERE id=?`, time.Now().Add(-31*time.Minute).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	svc.processRunnerSubmissionRetries(ctx)
	svc.processRunnerSubmissionRetries(ctx)
	row, err := db.GetDownload(ctx, id)
	if err != nil || !strings.Contains(row.Error, "automatic replay is unsafe") || len(client.added) != 1 {
		t.Fatalf("uncertainty warning/replay: %+v adds=%v err=%v", row, client.added, err)
	}
	var warnings int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='submission_review_required'`).Scan(&warnings); err != nil || warnings != 1 {
		t.Fatalf("warning history=%d err=%v", warnings, err)
	}
}
