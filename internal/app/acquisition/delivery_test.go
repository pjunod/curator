package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type advancingDeliveryClient struct {
	fakeClient
	after []uint64
}

func (c *advancingDeliveryClient) ListDeliveries(_ context.Context, after uint64, _ int) (ports.RunnerDeliveryPage, error) {
	c.after = append(c.after, after)
	return ports.RunnerDeliveryPage{ProtocolVersion: 1, NextCursor: 5}, nil
}
func (c *advancingDeliveryClient) AcceptDelivery(context.Context, string, ports.RunnerDeliveryAcceptance) error {
	return nil
}

func receiptIDFor(t *testing.T, db *sqlite.DB, downloadID int64) int64 {
	t.Helper()
	receipts, _, err := db.DeliveryReceiptPageForDownload(context.Background(), downloadID, 0, 10)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt for download %d: %+v err=%v", downloadID, receipts, err)
	}
	return receipts[0].ID
}

type serializedAckDeliveryClient struct {
	deliveryImportClient
	count   atomic.Int32
	block   atomic.Bool
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *serializedAckDeliveryClient) AcceptDelivery(ctx context.Context, id string, acceptance ports.RunnerDeliveryAcceptance) error {
	n := c.count.Add(1)
	if n <= 2 {
		return errors.New("initial acknowledgement outage")
	}
	if c.block.Load() {
		c.once.Do(func() { close(c.entered) })
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestEmptyPublicationCleanupIsBoundToAcceptedDirectoryAcrossRetries(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := setup(t, nil, &fakeClient{})
	root := t.TempDir()
	publication := filepath.Join(root, "Release")
	if err := os.MkdirAll(filepath.Join(publication, "empty", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, CompletedRootsSetting, root); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	acceptedInfo, err := os.Stat(publication)
	if err != nil {
		t.Fatal(err)
	}
	acceptedIdentity := deliveryDirectoryObjectIdentity(acceptedInfo)
	if acceptedIdentity == "" {
		t.Fatal("platform did not provide a stable directory identity")
	}
	receipt := sqlite.DeliveryReceipt{MappedPath: publication, PublicationDirectoryIdentity: acceptedIdentity}
	// Nested directories emptied by this attempt should not invalidate the
	// accepted payload root; its name is removed after rechecking the same inode.
	removed, err := svc.removeEmptyDeliveryDirectories(ctx, receipt)
	if err != nil || !removed {
		t.Fatalf("empty nested publication cleanup removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(publication); !os.IsNotExist(err) {
		t.Fatalf("accepted empty publication was not removed: err=%v", err)
	}

	// A later empty directory at the same path is unrelated. Even though the
	// content is empty, a retry must retain it because it lacks the accepted
	// directory inode identity.
	oldPath := publication + ".old"
	if err := os.MkdirAll(publication, 0o755); err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(publication)
	if err != nil {
		t.Fatal(err)
	}
	if deliveryDirectoryObjectIdentity(newInfo) == acceptedIdentity {
		// Keep the old inode allocated if the filesystem rapidly recycled it.
		if err := os.Rename(publication, oldPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(publication, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(publication); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.removeEmptyDeliveryDirectories(ctx, receipt); err == nil {
		t.Fatal("same-path replacement directory inherited cleanup authority")
	}
	if info, err := os.Stat(publication); err != nil || !info.IsDir() {
		t.Fatalf("replacement directory was removed: info=%v err=%v", info, err)
	}
}

type deliveryImportClient struct {
	fakeClient
	accepts              int
	failCount            int
	acceptErr            error
	acceptEntered        chan struct{}
	acceptRelease        chan struct{}
	sourceCleanupRetries int
	sourceCleanupErr     error
	sourceCleanupEntered chan struct{}
	sourceCleanupRelease chan struct{}
	getDeliveryCalls     int
	conflict             bool
	deliveries           []ports.RunnerDelivery
	lookup               ports.RunnerDelivery
}

func (c *deliveryImportClient) ListDeliveries(_ context.Context, after uint64, limit int) (ports.RunnerDeliveryPage, error) {
	page := ports.RunnerDeliveryPage{ProtocolVersion: 1, NextCursor: after}
	for _, delivery := range c.deliveries {
		if delivery.Sequence <= after {
			continue
		}
		if len(page.Deliveries) == limit {
			break
		}
		page.Deliveries = append(page.Deliveries, delivery)
		page.NextCursor = delivery.Sequence
	}
	return page, nil
}
func (c *deliveryImportClient) AcceptDelivery(context.Context, string, ports.RunnerDeliveryAcceptance) error {
	c.accepts++
	if c.acceptEntered != nil {
		select {
		case c.acceptEntered <- struct{}{}:
		default:
		}
	}
	if c.acceptRelease != nil {
		<-c.acceptRelease
	}
	if c.acceptErr != nil {
		return c.acceptErr
	}
	if c.conflict {
		return ports.ErrDeliveryConflict
	}
	if c.accepts <= c.failCount {
		return errors.New("Runner temporarily unavailable")
	}
	return nil
}
func (c *deliveryImportClient) RetryDeliverySourceCleanup(context.Context, string) (ports.RunnerDelivery, error) {
	c.sourceCleanupRetries++
	if c.sourceCleanupEntered != nil {
		select {
		case c.sourceCleanupEntered <- struct{}{}:
		default:
		}
	}
	if c.sourceCleanupRelease != nil {
		<-c.sourceCleanupRelease
	}
	return c.lookup, c.sourceCleanupErr
}

type deliveryRetryAfterError struct{ deadline time.Time }

func (e deliveryRetryAfterError) Error() string            { return "Runner temporarily unavailable" }
func (e deliveryRetryAfterError) RetryDeadline() time.Time { return e.deadline }

func (c *deliveryImportClient) GetDelivery(context.Context, string) (ports.RunnerDelivery, error) {
	c.getDeliveryCalls++
	return c.lookup, nil
}

func sha256Hex(content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}

func importFailedRows(t *testing.T, db *sqlite.DB) int {
	t.Helper()
	events, err := db.ListHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == "import_failed" {
			n++
		}
	}
	return n
}

func TestReleaseSampleRecognition(t *testing.T) {
	for path, want := range map[string]bool{
		"/c/Show.S01E01-GRP/sample-show.s01e01-grp.mkv":   true,
		"/c/Show.S01E01-GRP/Sample/show.s01e01-grp.mkv":   true,
		"Samples/grp-show.s01e01.mkv":                     true,
		"/c/Show.S01E01-GRP/show.s01e01-grp.mkv":          false,
		"/c/Free.Samples.S01E01-GRP/show.s01e01-grp.mkv":  false,
		"/c/Show.S01E01.Blood.Sample-GRP/show.s01e01.mkv": false,
	} {
		if got := isReleaseSample(path); got != want {
			t.Errorf("isReleaseSample(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestPlacementRejectsSourceBytesThatChangedAfterManifestVerification(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "accepted.mkv")
	if err := os.WriteFile(src, []byte("changed after manifest verification"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(item.Path, "accepted.mkv")
	ctx = withExpectedSourceDigests(ctx, map[string]string{src: "0000000000000000000000000000000000000000000000000000000000000000"})
	if _, err := svc.commitPlacement(ctx, item, importScope{Release: "accepted"}, src, dest, quality.Quality{}, nil, false); err == nil {
		t.Fatal("changed source bytes passed the accepted manifest digest check")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("library destination was mutated: err=%v", err)
	}
	pending, err := db.PendingPlacements(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("source mismatch created placement journal: %+v err=%v", pending, err)
	}
}

func TestSafeDeliveryRelativePath(t *testing.T) {
	for _, path := range []string{"../escape.mkv", "/absolute.mkv", `dir\\file.mkv`, "dir/../file.mkv", "./file.mkv", "dir//file.mkv", ""} {
		if safeDeliveryRelativePath(path) {
			t.Errorf("accepted unsafe manifest path %q", path)
		}
	}
	for _, path := range []string{"movie.mkv", "Season 1/episode 01.mkv", "épisode.mkv"} {
		if !safeDeliveryRelativePath(path) {
			t.Errorf("rejected safe manifest path %q", path)
		}
	}
}
