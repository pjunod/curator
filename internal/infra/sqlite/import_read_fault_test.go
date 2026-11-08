package sqlite

import (
	"testing"
)

// Syntactically valid JSON with incompatible field types must not silently
// erase the receipt/custody inventory during restart or deletion review.
func TestImportRecoveryRejectsIncompatibleStoredReceipts(t *testing.T) {
	for _, column := range []string{"files_json", "file_outcomes_json", "lifecycle_timing_json", "runner_stages_json", "source_copies_json"} {
		t.Run(column, func(t *testing.T) {
			db, r, _ := atomicDeliveryFixture(t)
			if _, err := db.W.ExecContext(t.Context(), `UPDATE runner_delivery_receipts SET `+column+`='true',ack_state='accepted' WHERE id=?`, r.ID); err != nil {
				t.Fatal(err)
			}
			before := importDurableSnapshot(t, db)
			if _, err := db.DeliveryReceiptByID(t.Context(), r.ID); err == nil {
				t.Fatal("incompatible receipt decoded as usable")
			}
			if _, _, err := db.DeliveryReceiptPage(t.Context(), 0, 100); err == nil {
				t.Fatal("incompatible receipt disappeared from recovery page")
			}
			if _, err := db.DeliveryReceiptsForClient(t.Context(), r.ClientID, 100); err == nil {
				t.Fatal("incompatible receipt disappeared from recovery inventory")
			}
			if _, err := db.DeliveryReceiptsNeedingSourceRefresh(t.Context(), r.ClientID, 100); err == nil {
				t.Fatal("incompatible receipt authorized source refresh")
			}
			if after := importDurableSnapshot(t, db); after != before {
				t.Fatal("failed decode mutated custody")
			}
		})
	}
	db, dl, a := importControlFixture(t)
	p := controlPlacement(dl, a, "incompatible-placement", "/source/video", "/library/video")
	if err := db.PreparePlacement(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(t.Context(), `UPDATE import_placements SET data=json_set(data,'$.size','not-an-integer') WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetPlacement(t.Context(), p.ID); err == nil {
		t.Fatal("incompatible placement decoded")
	}
	if _, err := db.UnresolvedPlacementReferences(t.Context()); err == nil {
		t.Fatal("incompatible placement disappeared from custody")
	}
	if _, err := db.W.ExecContext(t.Context(), `UPDATE import_placements SET state='committed' WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommittedImportPlacements(t.Context(), dl.ID, dl.GenerationKey, p.Source); err == nil {
		t.Fatal("incompatible receipt became reuse evidence")
	}
}
