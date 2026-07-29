package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
)

// fixtureBytes is real container bytes borrowed from the prober's corpus.
// Using the same bytes both packages test against means these tests fail for a
// wiring reason or not at all — they cannot fail because someone hand-rolled
// an MKV wrong.
//
// Reach for wholeFixture, not one of the 64 KiB header windows, unless the
// test is specifically about a file being cut short. Every windowed fixture is
// a genuinely truncated file and the prober now says so, which lands it in
// "implausible" and out of the measured path most of these tests are checking.
// The corpus's two intact MKVs: the ordinary case, files that are exactly as
// long as they say they are. Everything else in testdata is a header window
// and therefore genuinely truncated.
const (
	whole1080 = "mkv-1080p-h264-ac3-whole.mkv"
	whole720  = "mkv-720p-h264-aac-whole.mkv"
)

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "domain", "mediainfo", "testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// TestScanProbesAdoptedFileWithNoTokens is the ADR 0013 headline, end to end:
// a file whose NAME says nothing about its quality — the normal state of a
// Plex-era library — comes out of a scan with a measured quality, because
// monarr now looks at the bytes instead of shrugging at the name.
//
// Before this, the same file produced an empty quality string, which
// FileQualities omitted, which BestQualityForItem reported as "nothing known",
// which buildWanted read as MISSING. A 17 GB file on disk, hunted.
func TestScanProbesAdoptedFileWithNoTokens(t *testing.T) {
	svc, db, b := newService(t)
	ctx := context.Background()
	probed, cancel := bus.Subscribe[FileProbed](b, 8)
	defer cancel()

	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(item.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	// The screenshot's filename shape: a title and nothing else.
	movie := filepath.Join(item.Path, "A Good Day to Die Hard.mkv")
	if err := os.WriteFile(movie, fixtureBytes(t, whole1080), 0o644); err != nil {
		t.Fatal(err)
	}

	// No queue configured, so EnqueueProbe runs the probe inline — the
	// documented degradation, and what makes this assertable in one call.
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	records, err := db.FileQualityRecords(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one file record, got %d", len(records))
	}
	rec := records[0]
	if !rec.Known {
		t.Fatal("quality still unknown after a scan that had the bytes to hand")
	}
	if rec.Quality.Resolution != 1080 {
		t.Errorf("resolution = %d, want 1080 (measured, not parsed)", rec.Quality.Resolution)
	}
	if rec.Provenance != mediainfo.ProvenanceProbe {
		t.Errorf("provenance = %q, want %q", rec.Provenance, mediainfo.ProvenanceProbe)
	}
	if !rec.Probed || rec.ProbedAt.IsZero() {
		t.Error("probe timestamp not recorded")
	}
	if rec.Info.Video == nil || rec.Info.Video.Codec != "h264" {
		t.Errorf("media info not persisted: %+v", rec.Info)
	}
	if got := rec.Info.Summary(); got == "" {
		t.Error("no facts summary to show the user")
	}

	// And the item now reads as "has files with a known quality" rather than
	// the ambiguous nothing that made it a hunt target.
	state, err := db.DiskStateForItem(ctx, item.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasFiles || state.Best == nil || state.Best.Resolution != 1080 {
		t.Errorf("disk state = %+v", state)
	}

	// Wait rather than poll once. bus.Subscribe forwards through a goroutine,
	// so Publish returning does not mean the typed channel has the event yet —
	// a non-blocking receive here raced the scheduler and failed roughly one
	// run in fifteen. Every other event assertion in this package already uses
	// this timeout; this one was the odd man out.
	select {
	case ev := <-probed:
		if ev.MediaItemID != item.ID || ev.Provenance != string(mediainfo.ProvenanceProbe) {
			t.Errorf("FileProbed = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Error("no FileProbed event published; the wanted index would never learn")
	}
}

// TestScanSkipsAlreadyProbedFiles pins the cache key. Probing is bounded I/O,
// but bounded is not free over NFS, and a nightly scan that re-probed every
// file in a 40 TB library would be a self-inflicted outage.
func TestScanSkipsAlreadyProbedFiles(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	root := t.TempDir()
	rf, _ := svc.AddRootFolder(ctx, root, domain.KindMixed)
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(item.Path, 0o755)
	movie := filepath.Join(item.Path, "Fight Club.mkv")
	if err := os.WriteFile(movie, fixtureBytes(t, whole720), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := db.FileQualityRecords(ctx, item.ID)
	if len(first) != 1 || !first[0].Probed {
		t.Fatalf("first scan did not probe: %+v", first)
	}
	if first[0].NeedsProbe(first[0].Size) {
		t.Error("a file just probed at this size still wants a probe")
	}

	// Same size, same file: nothing to redo.
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	second, _ := db.FileQualityRecords(ctx, item.ID)
	if !second[0].ProbedAt.Equal(first[0].ProbedAt) {
		t.Error("second scan re-probed an unchanged file")
	}

	// Different bytes at the same path IS a different file, and must be
	// re-measured — the user swapped in a 2160p copy by hand. The padding is
	// only there to make the swap change the size, which is the cache key;
	// both corpus fixtures are truncated to the same 64 KiB window.
	swapped := append(fixtureBytes(t, "mkv-2160p-hevc-hdr10-truehd.mkv"), make([]byte, 4096)...)
	if err := os.WriteFile(movie, swapped, 0o644); err != nil {
		t.Fatal(err)
	}
	if !second[0].NeedsProbe(int64(len(swapped))) {
		t.Fatal("a size change did not invalidate the probe")
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	third, _ := db.FileQualityRecords(ctx, item.ID)
	if third[0].Quality.Resolution != 2160 {
		t.Errorf("resolution after swap = %d, want 2160", third[0].Quality.Resolution)
	}
}

// TestProbeUnreadableFileIsAResultNotAFailure: a corrupt file is a fact about
// the library. It must be recorded once, marked unverified, and never retried
// on a loop — and above all it must not read as "missing", because the file is
// right there (ADR 0013 §5).
func TestProbeUnreadableFileIsAResultNotAFailure(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	root := t.TempDir()
	rf, _ := svc.AddRootFolder(ctx, root, domain.KindMixed)
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(item.Path, 0o755)
	junk := filepath.Join(item.Path, "Fight Club.mkv")
	if err := os.WriteFile(junk, []byte("this was a video file once"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	records, err := db.FileQualityRecords(ctx, item.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v err = %v", records, err)
	}
	rec := records[0]
	if rec.Provenance != mediainfo.ProvenanceFailed {
		t.Errorf("provenance = %q, want %q", rec.Provenance, mediainfo.ProvenanceFailed)
	}
	if rec.SourceVerified() {
		t.Error("an unreadable file must never count as source-verified")
	}
	state, err := db.DiskStateForItem(ctx, item.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasFiles {
		t.Error("the file is on disk; HasFiles must say so even though we cannot read it")
	}
	if state.Best != nil {
		t.Errorf("nothing was measured, so Best must be nil, got %v", state.Best)
	}
}

// TestProbeSkipsBookFiles: an EPUB has no container to walk, and its format is
// its quality (ADR 0006). Probing one would be a slow way to learn nothing.
func TestProbeSkipsBookFiles(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindBook, Title: "A Book", SortTitle: "a book",
	})
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, itemID, 0, filepath.Join(t.TempDir(), "A Book.epub"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ProbeFile(ctx, fileID); err != nil {
		t.Fatalf("ProbeFile on a book: %v", err)
	}
	rec, err := db.GetFileQuality(ctx, fileID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Probed {
		t.Error("a book file was probed")
	}
}

// TestProbeVanishedFileIsNotAnError: the row can outlive the file by exactly
// as long as it takes a queued job to reach the front. That is a race we lose
// gracefully, not a job to retry five times.
func TestProbeVanishedFileIsNotAnError(t *testing.T) {
	svc, _, _ := newService(t)
	if err := svc.ProbeFile(context.Background(), 99999); err != nil {
		t.Errorf("ProbeFile on a missing row = %v, want nil", err)
	}
}

// TestFailedProbeIsRetried: a probe that could not READ the file must try
// again on the next scan. A failure is usually about something outside the
// file — a permission monarr did not have, a mount that was not up — and those
// get fixed. Caching "we tried once" strands the file in "unreadable" forever
// with no way back short of touching it on disk.
func TestFailedProbeIsRetried(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	root := t.TempDir()
	rf, _ := svc.AddRootFolder(ctx, root, domain.KindMixed)
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(item.Path, 0o755)
	movie := filepath.Join(item.Path, "Fight Club.mkv")
	// Unreadable to the prober: recognisably not a container.
	if err := os.WriteFile(movie, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := db.FileQualityRecords(ctx, item.ID)
	if len(first) != 1 || first[0].Provenance != mediainfo.ProvenanceFailed {
		t.Fatalf("expected a failed probe, got %+v", first)
	}
	if !first[0].NeedsProbe(first[0].Size) {
		t.Error("a failed probe is cached forever; there would be no way back")
	}

	// Whatever was wrong is fixed — here, by the file becoming readable at the
	// same size it already had, so nothing but the retry rule can save it.
	real := fixtureBytes(t, whole1080)
	if err := os.WriteFile(movie, real, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	second, _ := db.FileQualityRecords(ctx, item.ID)
	if second[0].Provenance != mediainfo.ProvenanceProbe {
		t.Errorf("provenance = %q, want the retry to have succeeded", second[0].Provenance)
	}
	if second[0].Quality.Resolution != 1080 {
		t.Errorf("resolution = %d, want 1080", second[0].Quality.Resolution)
	}
}

// TestUnsupportedContainerIsNotRetriedForever is the other half of the rule:
// nothing about the next scan makes an AVI parseable, so re-reading one every
// sweep burns I/O to learn the same thing.
func TestUnsupportedContainerIsNotRetriedForever(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	root := t.TempDir()
	rf, _ := svc.AddRootFolder(ctx, root, domain.KindMixed)
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(item.Path, 0o755)
	avi := filepath.Join(item.Path, "Fight Club.avi")
	if err := os.WriteFile(avi, fixtureBytes(t, "avi-720p-mpeg4-ac3.avi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	records, _ := db.FileQualityRecords(ctx, item.ID)
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	if records[0].NeedsProbe(records[0].Size) {
		t.Error("an unsupported container wants re-probing on every scan")
	}
}

// TestReprobeItemForcesAMeasurement: the cache is right for a routine sweep,
// but "try again" has to be something a person can ask for.
func TestReprobeItemForcesAMeasurement(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	root := t.TempDir()
	rf, _ := svc.AddRootFolder(ctx, root, domain.KindMixed)
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(item.Path, 0o755)
	movie := filepath.Join(item.Path, "Fight Club.mkv")
	if err := os.WriteFile(movie, fixtureBytes(t, whole720), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	// Wipe what we learned, exactly as a bad first probe would have left it,
	// then ask for a re-measure. A scan would decline: same path, same size.
	records, _ := db.FileQualityRecords(ctx, item.ID)
	fileID := records[0].FileID
	if err := db.SetFileMediaInfo(ctx, fileID, mediainfo.Info{},
		mediainfo.ProvenanceManual, mediainfo.ConfidenceNone, records[0].ProbedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fileID, quality.Quality{Source: quality.SourceUnknown},
		mediainfo.ProvenanceManual, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}

	n, err := svc.ReprobeItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("queued %d files, want 1", n)
	}
	after, _ := db.FileQualityRecords(ctx, item.ID)
	if after[0].Quality.Resolution != 720 || after[0].Provenance != mediainfo.ProvenanceProbe {
		t.Errorf("re-measure did not take: %+v", after[0])
	}
}

// TestImplausibleFileStopsCountingAsSatisfied is the HIM case, end to end.
//
// A file lands, it measures as far too short for the feature it is supposed to
// be, and the question that matters is not what badge it gets — it is whether
// the movie is still being looked for. Before this, a file that passed the
// probe at all marked the item satisfied, so a fake copy retired the want and
// the search stopped forever. The assertions below are in that order: the row
// says we do not believe it, and the disk state says the item still needs one.
func TestImplausibleFileStopsCountingAsSatisfied(t *testing.T) {
	movieRuntime = 96 // a feature, against a one-second fixture
	t.Cleanup(func() { movieRuntime = 0 })

	svc, db, _ := newService(t)
	ctx := context.Background()

	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(item.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	// The name makes a strong claim. The bytes do not support it.
	movie := filepath.Join(item.Path, "Fight Club (1999) [Remux 2160p].mkv")
	if err := os.WriteFile(movie, fixtureBytes(t, whole1080), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	records, err := db.FileQualityRecords(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one file record, got %d", len(records))
	}
	if got := records[0].Provenance; got != mediainfo.ProvenanceImplausible {
		t.Errorf("provenance = %q, want %q", got, mediainfo.ProvenanceImplausible)
	}
	if records[0].SourceVerified() {
		t.Error("an implausible file must not read as source-verified")
	}

	state, err := db.DiskStateForItem(ctx, item.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if state.HasFiles || state.Best != nil {
		t.Errorf("disk state = %+v; a file we disbelieve must not satisfy the item, or the hunt stops", state)
	}

	// The row is still there. Not trusting a file is not the same as losing it.
	files, err := db.ListFilesForItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("file rows = %d, want 1 — the file stays visible, it just stops voting", len(files))
	}
}
