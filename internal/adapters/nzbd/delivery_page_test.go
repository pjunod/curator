package nzbd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func deliveryPageFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/curator_delivery_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Delivery map[string]any `json:"delivery"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	fixture.Delivery["sequence"] = float64(2)
	return fixture.Delivery
}
func TestDeliveryDiscoveryRetainsMalformedRowAndContinues(t *testing.T) {
	for _, kind := range []string{"bad file", "wrong ready type", "oversized field", "server stub", "excess fields"} {
		t.Run(kind, func(t *testing.T) {
			valid := deliveryPageFixture(t)
			bad := deliveryPageFixture(t)
			bad["sequence"] = 1
			bad["deliveryId"] = "bad"
			switch kind {
			case "bad file":
				bad["files"] = []any{map[string]any{"path": "bad", "sizeBytes": -1, "sha256": "oops"}}
			case "wrong ready type":
				bad["readyAt"] = map[string]any{"invalid": true}
			case "oversized field":
				bad["files"] = strings.Repeat("x", deliveryFieldBudget+1)
			case "server stub":
				bad = map[string]any{"protocolVersion": 1, "sequence": 1, "deliveryId": "bad", "discoveryError": "unsupported manifest retained"}
			case "excess fields":
				for i := 0; i < 40; i++ {
					bad[fmt.Sprintf("unknown%d", i)] = true
				}
			}
			raw, err := json.Marshal(map[string]any{"protocolVersion": 1, "deliveries": []any{bad, valid}, "nextCursor": 2})
			if err != nil {
				t.Fatal(err)
			}
			page, err := readDeliveryPage(strings.NewReader(string(raw)), 100)
			if err != nil || len(page.Deliveries) != 1 || len(page.Rejected) != 1 || page.Rejected[0].Sequence != 1 || page.Rejected[0].DeliveryID != "bad" || page.NextCursor != 2 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
		})
	}
}
func TestDeliveryPageRejectsMalformedEnvelopeWithoutProgress(t *testing.T) {
	for _, raw := range []string{
		`{"protocolVersion":1,"deliveries":[],"nextCursor":0,}`,
		`{"protocolVersion":1,"protocolVersion":1,"deliveries":[],"nextCursor":0}`,
		`{"protocolVersion":1,"deliveries":[{"sequence":1,}],"nextCursor":1}`,
		`{"protocolVersion":1,"deliveries":[{"sequence":1},],"nextCursor":1}`,
		`{"protocolVersion":1,"deliveries":[{"deliveryId":"missing-sequence"}],"nextCursor":1}`,
		`{"protocolVersion":1,"deliveries":[{"sequence":1,"files":[}] ,"nextCursor":1}`,
		`{"protocolVersion":2,"deliveries":[],"nextCursor":0}`,
		`{"protocolVersion":1,"deliveries":[],"nextCursor":0}garbage`,
	} {
		if _, err := readDeliveryPage(strings.NewReader(raw), 100); err == nil {
			t.Fatalf("accepted malformed page: %s", raw)
		}
	}
}
func TestDeliveryStatusNegotiationDoesNotReadPoisonedCollection(t *testing.T) {
	collectionCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/status" {
			_, _ = io.WriteString(w, `{"version":"test","capabilities":["deliveries-v1"]}`)
			return
		}
		collectionCalls++
		http.Error(w, "poisoned collection", 500)
	}))
	defer srv.Close()
	c := client(t, srv, "", "token")
	supported, err := c.SupportsDeliveryV1(context.Background())
	if err != nil || !supported || collectionCalls != 0 {
		t.Fatalf("supported=%v calls=%d err=%v", supported, collectionCalls, err)
	}
}
func TestDeliveryDiscoveryRetriesInvalidLargePageWithOneRow(t *testing.T) {
	var limits []string
	valid := deliveryPageFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limits = append(limits, r.URL.Query().Get("limit"))
		if r.URL.Query().Get("limit") != "1" {
			_, _ = io.WriteString(w, `{"protocolVersion":1,"deliveries":[`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"protocolVersion": 1, "deliveries": []any{valid}, "nextCursor": 2})
	}))
	defer srv.Close()
	page, err := client(t, srv, "", "token").ListDeliveries(context.Background(), 0, 100)
	if err != nil || len(page.Deliveries) != 1 || strings.Join(limits, ",") != "100,1" {
		t.Fatalf("page=%+v limits=%v err=%v", page, limits, err)
	}
}
