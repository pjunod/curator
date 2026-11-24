package sqlite

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// Pipeline contract v2 (M2) removed the v1 Runner delivery import path, so
// nothing in production writes runner_delivery_receipts any more. The rows
// still exist in upgraded databases, read-only, until M6 drops the tables
// (migration 0073), and the lifecycle readers still act on them. These
// fixtures write those legacy rows directly, exactly as a pre-M2 curator left
// them, so the surviving readers are tested against real stored state.

// seedLegacyReceipt inserts an unresolved v1 receipt as discovery stored it.
func seedLegacyReceipt(t testing.TB, d *DB, clientID int64, v ports.RunnerDelivery, mappedPath string) DeliveryReceipt {
	t.Helper()
	enc := func(x any) string {
		b, err := json.Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cleanup := string(v.SourceCleanup)
	if cleanup == "" {
		cleanup = "null"
	}
	now := time.Now().UnixMilli()
	res, err := d.W.ExecContext(context.Background(), `INSERT INTO runner_delivery_receipts
		(client_id, delivery_id, sequence, publication_generation, source_artifact_id, source_generation, manifest_digest,
		artifact_id, job_id, request_token, runner_instance_id, ready_at, publication_path,
		mapped_path, files_json, source_cleanup_json, terminal_outcome, failure_stage, failure_cause,
		keep_source_on_failure, file_outcomes_json, state, error, created_at, updated_at,runner_stages_json,source_copies_json,lifecycle_timing_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'unresolved','',?,?,?,?,?)`, clientID, v.DeliveryID,
		v.Sequence, v.PublicationGeneration, v.SourceArtifactID, v.SourceGeneration, v.ManifestDigest, v.ArtifactID, v.JobID,
		v.RequestToken, v.RunnerInstanceID, v.ReadyAt.UnixMilli(), v.PublicationPath, mappedPath, enc(v.Files), cleanup,
		v.TerminalOutcome, v.FailureStage, v.FailureCause, v.KeepSourceOnFailure, enc(v.FileOutcomes), now, now,
		enc(v.RunnerStages), enc(v.SourceCopies), enc(v.LifecycleTiming))
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.DeliveryReceiptByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// seedLegacyFileOutcomes journals manifest entries the way v1 acceptance did.
func seedLegacyFileOutcomes(t testing.TB, d *DB, receiptID int64, files []DeliveryManifestFile, selected map[string]bool) {
	t.Helper()
	for _, f := range files {
		disposition := "pending"
		if selected != nil && !selected[f.Path] {
			disposition = "unselected_recoverable"
		}
		if _, err := d.W.ExecContext(context.Background(), `INSERT INTO runner_delivery_file_outcomes
			(receipt_id,path,size_bytes,sha256,file_generation,disposition,cleanup_state,manual_additional,updated_at)
			VALUES(?,?,?,?,?,?,'protected',0,?)`, receiptID, f.Path, f.SizeBytes, f.SHA256, f.FileGeneration, disposition, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
}
