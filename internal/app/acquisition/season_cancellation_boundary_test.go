package acquisition

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

type cancellationStatusFailure struct{ *fakeClient }

func (c cancellationStatusFailure) Statuses(context.Context) ([]ports.DownloadStatus, error) {
	return nil, errors.New("inventory unavailable")
}

func TestPlannedCancellationRequiresConfirmedClientAbsenceAndRetainsBytes(t *testing.T) {
	for _, mode := range []string{"pending", "removed", "remove-error", "inventory-error", "still-present", "missing-client"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			client := &fakeClient{}
			s, db, item := setup(t, nil, client)
			plan, rows := admittedFixture(t, s, db, item, "single")
			id := rows[0].ID
			if mode != "pending" {
				if err := s.dispatchPlan(ctx, plan); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "remove-error":
				client.removeErr = errors.New("removal refused")
			case "inventory-error":
				s.newClient = func(ports.ClientConfig) ports.DownloadClient { return cancellationStatusFailure{client} }
			case "still-present":
				client.statuses = []ports.DownloadStatus{{Handle: "h1", Name: rows[0].ReleaseTitle}}
			case "missing-client":
				if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET client_id=999999 WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
			}
			err := s.RemoveDownload(ctx, id, true)
			failed := mode != "pending" && mode != "removed"
			if (err != nil) != failed {
				t.Fatalf("cancellation error %v", err)
			}
			row, err := db.GetDownload(ctx, id)
			if err != nil {
				t.Fatal("custody row deleted", err)
			}
			if mode == "pending" {
				if row.SubmissionPhase != "rejected" || !row.Superseded || len(row.ReservedEpisodes) != 0 || len(client.added) != 0 {
					t.Fatalf("unsubmitted cancellation %+v", row)
				}
			} else if mode == "removed" {
				if !strings.HasPrefix(row.ParkedReason, "operator cancellation") || len(row.ReservedEpisodes) == 0 {
					t.Fatalf("sent custody released %+v", row)
				}
			} else if row.ParkedReason != "" || row.Superseded || len(row.ReservedEpisodes) == 0 {
				t.Fatalf("failed cancellation changed custody %+v", row)
			}
			for _, call := range client.removed {
				if call.DeleteData {
					t.Fatal("cancellation deleted payload")
				}
			}
			p, err := db.GetAcquisitionPlan(ctx, plan)
			if err != nil {
				t.Fatal(err)
			}
			if (p.State == "cancel_requested") == failed {
				t.Fatalf("plan state after cancellation: %s", p.State)
			}
		})
	}
}
