package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestDeliveryLifecycleRunnerCleanupRetryUpdatesProgressWhileAckIsPending(t *testing.T) {
	e := newAPIEnv(t)
	clientID := acqAddClient(t, e, "qbittorrent")
	itemID := e.addMovie(t)
	downloadID := acqGrab(t, e, itemID)
	r := apiDeliveryReceipt(t, e, clientID, downloadID, "delivery-api-cleanup-pending-ack")
	ctx := context.Background()
	if err := e.db.PrepareDeliveryAcceptance(ctx, sqlite.DeliveryReceipt{
		ID: r.ID, ConsumerReceiptID: "curator-local-pending", NextAttemptAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err := e.db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil || persisted.AckState != "pending" || persisted.ConsumerReceiptID != "curator-local-pending" {
		t.Fatalf("fixture failed to prepare a locally durable pending acknowledgement: %+v err=%v", persisted, err)
	}
	protocol := &apiDeliveryProtocolClient{
		fakeClient: fakeClient{}, delivery: r.Delivery,
		sourceProgress: `{"status":"succeeded","attempts":2}`,
	}
	service := acquisition.New(e.db, e.bus, nil, nil, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	e.srv.deps.Acquisition, e.acq = service, service
	e.h = e.srv.Handler()

	base := fmt.Sprintf("/api/v1/downloads/deliveries/%d", r.ID)
	e.post(t, base+"/retry-runner-cleanup", `{}`).expect(t, http.StatusAccepted)
	if protocol.sourceRetries != 1 {
		t.Fatalf("source-cleanup retry made %d Runner calls, want 1", protocol.sourceRetries)
	}
	var detail apigen.DeliveryLifecycleReceipt
	e.get(t, base).expect(t, http.StatusOK).into(t, &detail)
	if detail.Acceptance != "pending" {
		t.Fatalf("Runner cleanup retry changed local acceptance state: %+v", detail)
	}
	if detail.SourceCleanup == nil || (*detail.SourceCleanup)["status"] != "succeeded" || (*detail.SourceCleanup)["attempts"] != float64(2) {
		t.Fatalf("source-cleanup progress was not refreshed while ACK remained pending: %+v", detail.SourceCleanup)
	}
}
