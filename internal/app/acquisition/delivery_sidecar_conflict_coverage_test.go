package acquisition

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcceptedSidecarConflictPreservesBothUnrelatedTargetAndVerifiedStage(t *testing.T) {
	caption := []byte("accepted subtitle bytes")
	ctx, svc, _, _, _, receipt, media, sidecar, _ := prepareSidecarMedia(t, caption)
	target := acceptedSidecarTarget(t, media.Path, media.SourcePath, sidecar.relative)
	conflict := []byte("existing subtitle from another source")
	if err := os.WriteFile(target, conflict, 0o644); err != nil {
		t.Fatal(err)
	}
	stage := expectedCaptionStagePath(target)
	if err := os.WriteFile(stage, caption, 0o600); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Lstat(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	mediaInfo, err := os.Lstat(media.Path)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.publishAcceptedSidecar(ctx, receipt, sidecar, media.Path, target, stage, dirInfo, mediaInfo)
	if err == nil || !strings.Contains(err.Error(), "already exists with different content") {
		t.Fatalf("sidecar conflict was not surfaced as a refused publication: err=%v", err)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != string(conflict) {
		t.Fatalf("publication overwrote the unrelated library sidecar: got=%q err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(stage); readErr != nil || string(got) != string(caption) {
		t.Fatalf("refused publication did not retain the verified stage for review/retry: got=%q err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(sidecar.absolute); readErr != nil || string(got) != string(caption) {
		t.Fatalf("sidecar conflict changed the accepted source bytes: got=%q err=%v", got, readErr)
	}
	rows, err := svc.db.DeliveryFileOutcomes(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Path == sidecar.relative && row.Disposition == "placed" {
			t.Fatalf("conflicted sidecar was recorded as placed: %+v", row)
		}
	}
}

func TestAcceptedSidecarMatchingExistingTargetSettlesVerifiedStage(t *testing.T) {
	caption := []byte("caption from the accepted manifest")
	ctx, svc, _, _, _, receipt, media, sidecar, _ := prepareSidecarMedia(t, caption)
	target := acceptedSidecarTarget(t, media.Path, media.SourcePath, sidecar.relative)
	if err := os.WriteFile(target, caption, 0o644); err != nil {
		t.Fatal(err)
	}
	stage := expectedCaptionStagePath(target)
	if err := os.WriteFile(stage, caption, 0o600); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Lstat(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	mediaInfo, err := os.Lstat(media.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.publishAcceptedSidecar(ctx, receipt, sidecar, media.Path, target, stage, dirInfo, mediaInfo); err != nil {
		t.Fatalf("matching existing sidecar was not treated as a replay: %v", err)
	}
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatalf("verified duplicate stage was not settled: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(caption) {
		t.Fatalf("idempotent replay changed the existing target: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(sidecar.absolute); err != nil || string(got) != string(caption) {
		t.Fatalf("idempotent replay changed the accepted source: %q err=%v", got, err)
	}
}
