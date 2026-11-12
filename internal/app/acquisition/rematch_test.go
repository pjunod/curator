package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type rematchFixture struct {
	svc  *Service
	db   *sqlite.DB
	show int64 // setup's series, Test Show (2020), S01E01-E02
	root domain.RootFolder
}

func newRematchFixture(t *testing.T) rematchFixture {
	t.Helper()
	svc, db, show := setup(t, nil, nil)
	root, err := db.AddRootFolder(context.Background(), filepath.Join(t.TempDir(), "movies"), domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	return rematchFixture{svc: svc, db: db, show: show, root: root}
}

func (f rematchFixture) movie(t *testing.T, title string, year int, tmdb int64, origin string) domain.MediaItem {
	t.Helper()
	ctx := context.Background()
	item := domain.MediaItem{Kind: domain.KindMovie, Title: title, SortTitle: strings.ToLower(title), Year: year,
		IDs: domain.ExternalIDs{TMDB: tmdb}, Monitored: true, RootFolderID: f.root.ID,
		Path: filepath.Join(f.root.Path, title+" ("+strconv.Itoa(year)+")")}
	id, err := f.db.CreateMediaItem(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		if err := f.db.SetMediaItemOrigin(ctx, id, origin); err != nil {
			t.Fatal(err)
		}
	}
	item.ID = id
	return item
}

// file records a library file with a quality and, optionally, its source
// release.
func (f rematchFixture) file(t *testing.T, itemID int64, path, body, guid string, eps ...int64) int64 {
	t.Helper()
	ctx := context.Background()
	writeFile(t, path, body)
	id, err := f.db.UpsertFile(ctx, itemID, 0, path, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetFileQuality(ctx, id, quality.Quality{Source: quality.SourceBluray, Resolution: 2160}); err != nil {
		t.Fatal(err)
	}
	if guid != "" {
		if _, err := f.db.W.ExecContext(ctx, `UPDATE media_files SET source_guid=?, source_indexer_id=1, source_release=? WHERE id=?`, guid, "Dune.1984.2160p.BluRay-GRP", id); err != nil {
			t.Fatal(err)
		}
	}
	if len(eps) > 0 {
		if err := f.db.ReplaceFileEpisodeLinks(ctx, id, eps); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// run executes a preview the way the API does and waits for the end.
func (f rematchFixture) run(t *testing.T, req RematchRequest, p RematchPreview) RematchStatus {
	t.Helper()
	ctx := context.Background()
	req.RequestID = "req-" + p.Fingerprint
	req.SourceRevision, req.TargetRevision, req.PlanFingerprint = p.SourceRevision, p.TargetRevision, p.Fingerprint
	op, err := f.svc.StartRematch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	return f.wait(t, op.ID)
}

func (f rematchFixture) wait(t *testing.T, id string) RematchStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := f.svc.GetRematch(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		switch st.State {
		case "complete", "cancelled", "failed":
			// The goroutine releases the leases just after.
			time.Sleep(20 * time.Millisecond)
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("re-match %s did not finish", id)
	return RematchStatus{}
}

// The wrong title: Dune (1984) was adopted for a folder that is Dune
// (2021). The file moves to a new item in a folder with the {tmdb-N} hint,
// the emptied adopted item is removed, history records it on the target, and
// Cinema gets one operation-keyed sub-delivery naming the removed path.
func TestRematchToNewTitleRenamesAndRemovesAdoptedSource(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	if _, err := f.db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "cinema", Settings: map[string]string{"url": "http://c", "apiKey": "k"}, OnImport: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	src := f.movie(t, "Dune", 1984, 841, "adopted")
	oldPath := filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv")
	fileID := f.file(t, src.ID, oldPath, "dune bytes", "")
	writeFile(t, strings.TrimSuffix(oldPath, ".mkv")+".en.srt", "subs")

	newTitle := domain.MediaItem{Kind: domain.KindMovie, Title: "Dune", Year: 2021, IDs: domain.ExternalIDs{TMDB: 438631}, RootFolderID: f.root.ID}
	req := RematchRequest{SourceItemID: src.ID, NewTarget: &newTitle}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Moves) != 1 || p.Moves[0].Mode != "rename" || !strings.Contains(p.Moves[0].To, "Dune (2021) {tmdb-438631}") {
		t.Fatalf("moves = %+v, want one rename into the tagged folder", p.Moves)
	}
	if p.Target.InLibrary || !p.Target.FolderChosen || p.SourceOutcome != "removed" {
		t.Fatalf("preview = %+v, want a new title, a chosen folder and the adopted source removed", p)
	}
	if len(p.Moves[0].Sidecars) != 1 || p.Moves[0].Sidecars[0].To != strings.TrimSuffix(p.Moves[0].To, ".mkv")+".en.srt" {
		t.Fatalf("sidecars = %+v, want the subtitle renamed with the video", p.Moves[0].Sidecars)
	}
	if !strings.Contains(p.Cinema, "does not support removed paths") {
		t.Fatalf("cinema = %q, want the old-cinema warning before it runs", p.Cinema)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatal("the preview touched the file")
	}

	// The API adds the title through the normal add path, then executes.
	targetID, err := f.db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Dune", SortTitle: "dune", Year: 2021,
		IDs: domain.ExternalIDs{TMDB: 438631}, Monitored: true, RootFolderID: f.root.ID, Path: filepath.Join(f.root.Path, "Dune (2021)")})
	if err != nil {
		t.Fatal(err)
	}
	req.NewTarget, req.TargetItemID = nil, targetID
	st := f.run(t, req, p)
	if st.State != "complete" {
		t.Fatalf("state = %s (%s), want complete", st.State, st.LastError)
	}
	newPath := p.Moves[0].To
	if b, err := os.ReadFile(newPath); err != nil || string(b) != "dune bytes" {
		t.Fatalf("new file: %q %v", b, err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("the old path still exists")
	}
	if _, err := os.Stat(p.Moves[0].Sidecars[0].To); err != nil {
		t.Fatal("the subtitle did not travel with the video")
	}
	target, err := f.db.GetMediaItemFull(ctx, targetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Files) != 1 || target.Files[0].ID != fileID || target.Files[0].Path != newPath || !strings.HasSuffix(target.Path, "{tmdb-438631}") {
		t.Fatalf("target = %+v / %+v, want the file row moved and the tagged folder", target.Path, target.Files)
	}
	if _, err := f.db.GetMediaItemFull(ctx, src.ID); err == nil {
		t.Fatal("the emptied adopted item was kept")
	}
	if _, err := os.Stat(src.Path); !os.IsNotExist(err) {
		t.Fatal("the emptied folder was kept")
	}
	var n int
	_ = f.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_events WHERE type='rematched' AND media_item_id=?`, targetID).Scan(&n)
	if n != 1 {
		t.Fatalf("rematched history on the target = %d, want 1", n)
	}
	deliveries, err := f.db.OperationDeliveries(ctx, st.ID)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %+v %v, want one sub-delivery", deliveries, err)
	}
	d := deliveries[0]
	if !strings.HasPrefix(d.IdempotencyKey, "op/"+st.ID+"/") || d.Event != "moved" || d.Directory != f.root.Path || len(d.ExpectedFiles) != 1 {
		t.Fatalf("delivery = %+v, want op-keyed moved request at the root with the placed file", d)
	}
	var n2 ports.Notification
	_ = json.Unmarshal([]byte(d.Payload), &n2)
	if n2.Import == nil || len(n2.Import.RemovedPaths) != 1 || n2.Import.RemovedPaths[0] != oldPath || n2.Import.FromItemID != src.ID {
		t.Fatalf("payload = %+v, want the removed path and the source item", n2.Import)
	}
}

// The right title is already in the library: the file merges into it by
// import's rule — it replaces the file there when it ranks higher in the
// target's profile (removed once the move is committed, through import's
// removeExistingFiles), else it is kept beside it, with the reason. The
// user-added source stays wanted, and the release that produced the file is
// blocklisted for it through the replacement path (intent rematch, complete).
func TestRematchMergesIntoExistingItemAndBlocklistsVacatedRelease(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	src := f.movie(t, "Dune", 1984, 841, "")
	f.file(t, src.ID, filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv"), "4k", "guid-1")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	oldTarget := filepath.Join(dst.Path, "Dune (2021) [WEBDL-1080p].mkv")
	old := f.file(t, dst.ID, oldTarget, "1080", "")
	if err := f.db.SetFileQuality(ctx, old, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}); err != nil {
		t.Fatal(err)
	}

	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.SourceOutcome != "stays" || !strings.Contains(p.SourceReason, "you added it") || len(p.Blocklist) != 1 || !p.BlocklistOn {
		t.Fatalf("preview = %+v, want the source kept and the release blocklisted", p)
	}
	m := p.Moves[0]
	if m.Action != "replace" || len(m.Replaces) != 1 || m.Replaces[0].FileID != old || !strings.Contains(m.Reason, "ranks above") || p.Target.FolderChosen {
		t.Fatalf("move = %+v, want it to replace the lower-ranked file in the existing folder", m)
	}
	st := f.run(t, req, p)
	if st.State != "complete" {
		t.Fatalf("state = %s (%s)", st.State, st.LastError)
	}
	target, _ := f.db.GetMediaItemFull(ctx, dst.ID)
	if len(target.Files) != 1 || target.Files[0].Path != m.To {
		t.Fatalf("target files = %+v, want only the moved file", target.Files)
	}
	if _, err := os.Stat(oldTarget); !os.IsNotExist(err) {
		t.Fatal("the replaced file is still on disk")
	}
	var result rematchResult
	_ = json.Unmarshal(st.Result, &result)
	if len(result.Replaced) != 1 || result.Replaced[0] != oldTarget {
		t.Fatalf("result = %+v, want the replaced file recorded", result)
	}
	source, err := f.db.GetMediaItemFull(ctx, src.ID)
	if err != nil || len(source.Files) != 0 {
		t.Fatalf("source = %+v %v, want it kept, with no files", source.Files, err)
	}
	if listed, _ := f.db.ListReplacementOperationsForItem(ctx, src.ID, 10); len(listed) != 0 {
		t.Fatalf("replacement panel lists %+v, want the re-match's blocklist record kept out of it", listed)
	}
	var blocked, ops int
	_ = f.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_blacklist_identity WHERE media_item_id=? AND guid='guid-1'`, src.ID).Scan(&blocked)
	_ = f.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM replacement_operations WHERE intent='rematch' AND state='complete' AND media_item_id=?`, src.ID).Scan(&ops)
	if blocked != 1 || ops != 1 {
		t.Fatalf("blocklist = %d, replacement ops = %d; want the release blocklisted for the source", blocked, ops)
	}

	// A lower-ranked file is kept beside, and nothing is removed.
	worse := f.movie(t, "Arrival", 2016, 329865, "")
	w := f.file(t, worse.ID, filepath.Join(worse.Path, "Arrival (2016) [HDTV-720p].mkv"), "720", "")
	if err := f.db.SetFileQuality(ctx, w, quality.Quality{Source: quality.SourceHDTV, Resolution: 720}); err != nil {
		t.Fatal(err)
	}
	req = RematchRequest{SourceItemID: worse.ID, TargetItemID: dst.ID}
	p, err = f.svc.PreviewRematch(ctx, req)
	if err != nil || p.Moves[0].Action != "beside" || len(p.Moves[0].Replaces) != 0 || !strings.Contains(p.Moves[0].Reason, "kept beside") {
		t.Fatalf("preview = %+v %v, want the worse file kept beside with the reason", p.Moves, err)
	}
	if st := f.run(t, req, p); st.State != "complete" {
		t.Fatalf("state = %s (%s)", st.State, st.LastError)
	}
	target, _ = f.db.GetMediaItemFull(ctx, dst.ID)
	if len(target.Files) != 2 {
		t.Fatalf("target files = %d, want both kept", len(target.Files))
	}
}

// Series: Move to… one file to another episode of the same show; and to
// another show that lacks the episode, which needs a pick and is left out
// until given one. A file covering two episodes maps to both.
func TestRematchSeriesEpisodeMapAndPicker(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	show, _ := f.db.GetMediaItemFull(ctx, f.show)
	ep := func(item domain.MediaItem, s, e int) int64 {
		got, ok := findEpisode(item, s, e)
		if !ok {
			t.Fatalf("no S%dE%d", s, e)
		}
		return got.ID
	}
	e1, e2 := ep(show, 1, 1), ep(show, 1, 2)
	one := f.file(t, f.show, filepath.Join(show.Path, "Season 01", "Test Show - S01E01 - Pilot [Bluray-2160p].mkv"), "e1", "guid-e1", e1)

	// Same show, different episode.
	req := RematchRequest{SourceItemID: f.show, TargetItemID: f.show, FileIDs: []int64{one}, Choices: []RematchChoice{{FileID: one, Season: 1, Episodes: []int{2}}}}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Moves) != 1 || !strings.Contains(p.Moves[0].To, "S01E02 - Finale") || p.SourceOutcome != "stays" || len(p.Blocklist) != 1 {
		t.Fatalf("preview = %+v, want S01E01 renamed to S01E02, the show kept, the release blocklisted", p)
	}
	if st := f.run(t, req, p); st.State != "complete" {
		t.Fatalf("state = %s (%s)", st.State, st.LastError)
	}
	show, _ = f.db.GetMediaItemFull(ctx, f.show)
	if len(show.Files) != 1 || len(show.Files[0].EpisodeIDs) != 1 || show.Files[0].EpisodeIDs[0] != e2 {
		t.Fatalf("files = %+v, want the file linked to S01E02 only", show.Files)
	}

	// Another show with only S01E01: a double-episode file needs a pick.
	other := domain.MediaItem{Kind: domain.KindSeries, Title: "Other Show", SortTitle: "other show", Year: 2021, IDs: domain.ExternalIDs{TMDB: 200}, Monitored: true,
		Path: filepath.Join(f.root.Path, "Other Show (2021)"), Seasons: []domain.Season{{Number: 1, Episodes: []domain.Episode{
			{SeasonNumber: 1, EpisodeNumber: 1, Title: "One"}, {SeasonNumber: 1, EpisodeNumber: 3, Title: "Three"}, {SeasonNumber: 1, EpisodeNumber: 4, Title: "Four"}}}}}
	otherID, err := f.db.CreateMediaItem(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	double := f.file(t, f.show, filepath.Join(show.Path, "Season 01", "Test Show - S01E01-E02 [Bluray-2160p].mkv"), "e1e2", "", e1, e2)
	req = RematchRequest{SourceItemID: f.show, TargetItemID: otherID, FileIDs: []int64{double}}
	p, err = f.svc.PreviewRematch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Moves) != 0 || len(p.LeftOut) != 1 || !p.LeftOut[0].NeedsPick || !strings.Contains(p.LeftOut[0].Problem, "S01E02") || len(p.Episodes) != 3 || p.Unresolved != 1 {
		t.Fatalf("preview = %+v, want the double file unresolved with a picker of 3 episodes", p)
	}
	// Unresolved is not left out: nothing moves and no row leaves.
	blockedReq := req
	blockedReq.RequestID, blockedReq.SourceRevision, blockedReq.TargetRevision, blockedReq.PlanFingerprint = "unresolved", p.SourceRevision, p.TargetRevision, p.Fingerprint
	if _, err := f.svc.StartRematch(ctx, blockedReq); !errors.Is(err, ErrInvalidRematch) || !strings.Contains(err.Error(), "need a choice") {
		t.Fatalf("err = %v, want Move refused until the file is picked or left out", err)
	}
	req.Choices = []RematchChoice{{FileID: double, Season: 1, Episodes: []int{3, 4}}}
	p, err = f.svc.PreviewRematch(ctx, req)
	if err != nil || len(p.Moves) != 1 || !strings.Contains(p.Moves[0].To, "S01E03-E04") {
		t.Fatalf("preview = %+v %v, want the file mapped to S01E03-E04", p, err)
	}
	if st := f.run(t, req, p); st.State != "complete" {
		t.Fatalf("state = %s (%s)", st.State, st.LastError)
	}
	moved, _ := f.db.GetMediaItemFull(ctx, otherID)
	if len(moved.Files) != 1 || len(moved.Files[0].EpisodeIDs) != 2 {
		t.Fatalf("other show files = %+v, want one file on two episodes", moved.Files)
	}
}

// Fences: a download in flight blocks the preview (named, retryable) and the
// execution (409 rematch_blocked); a library change since the preview is
// refused as stale.
func TestRematchFenceRefusals(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	src := f.movie(t, "Dune", 1984, 841, "")
	f.file(t, src.ID, filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv"), "x", "")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	now := time.Now().UnixMilli()
	res, err := f.db.W.ExecContext(ctx, `INSERT INTO downloads(media_item_id,release_title,protocol,state,added_at,updated_at) VALUES(?,?,?,?,?,?)`, dst.ID, "Dune.2021", "torrent", "downloading", now, now)
	if err != nil {
		t.Fatal(err)
	}
	dlID, _ := res.LastInsertId()
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Blocked == nil || p.Blocked.Reason != "download" || p.Blocked.ItemID != dst.ID || !p.Blocked.Retry || !strings.Contains(p.Blocked.Message, "Dune") {
		t.Fatalf("blocked = %+v, want the target's download named", p.Blocked)
	}
	req.RequestID, req.SourceRevision, req.TargetRevision, req.PlanFingerprint = "r1", p.SourceRevision, p.TargetRevision, p.Fingerprint
	var blocked *RematchBlockedError
	if _, err := f.svc.StartRematch(ctx, req); !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want rematch_blocked", err)
	}
	if _, err := f.db.W.ExecContext(ctx, `UPDATE downloads SET state='imported' WHERE id=?`, dlID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(ctx, `UPDATE media_items SET title='Dune!' WHERE id=?`, src.ID); err != nil {
		t.Fatal(err)
	}
	req.RequestID = "r2"
	if _, err := f.svc.StartRematch(ctx, req); !errors.Is(err, ErrRematchStalePreview) {
		t.Fatalf("err = %v, want a stale preview refused", err)
	}
}

// Across volumes: a verified copy, the original removed only after commit.
// Stop before commit puts already-moved files back and leaves the source
// untouched; a cancelled copy leaves no temp file.
func TestRematchCrossVolumeCopyAndStop(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	old := sameStorage
	sameStorage = func(string, string) bool { return false }
	t.Cleanup(func() { sameStorage = old })
	src := f.movie(t, "Dune", 1984, 841, "")
	oldPath := filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv")
	f.file(t, src.ID, oldPath, strings.Repeat("d", 1<<20), "")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil || p.Moves[0].Mode != "copy" {
		t.Fatalf("preview = %+v %v, want a copy", p.Moves, err)
	}
	st := f.run(t, req, p)
	if st.State != "complete" || st.BytesDone != 1<<20 || st.BytesTotal != 1<<20 {
		t.Fatalf("status = %+v, want complete with every byte counted", st.RematchOperation)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("the original was kept after a committed copy")
	}

	// A cancelled copy: nothing left behind, source intact.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	srcFile := writeFile(t, filepath.Join(t.TempDir(), "a.mkv"), "abc")
	temp := filepath.Join(t.TempDir(), ".a.partial")
	if _, err := copyVerified(cctx, srcFile, temp, temp+".final", nil); err == nil {
		t.Fatal("a cancelled copy succeeded")
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("a cancelled copy left its temp file")
	}

	// Stop after one file moved, before commit: it goes back.
	sameStorage = old
	movedBack := writeFile(t, filepath.Join(dst.Path, "Dune (2021) [Bluray-2160p].mkv"), "")
	_ = os.Remove(movedBack)
	src2 := f.movie(t, "Arrival", 2016, 329865, "")
	a := filepath.Join(src2.Path, "Arrival (2016) [Bluray-2160p].mkv")
	f.file(t, src2.ID, a, "arrival", "")
	dst2 := f.movie(t, "Arrival", 2017, 999, "")
	req = RematchRequest{SourceItemID: src2.ID, TargetItemID: dst2.ID}
	p, _ = f.svc.PreviewRematch(ctx, req)
	req.RequestID, req.SourceRevision, req.TargetRevision, req.PlanFingerprint = "stop", p.SourceRevision, p.TargetRevision, p.Fingerprint
	op, err := f.svc.startRematchLocked(ctx, req, p.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	moves, _ := f.db.ListRematchMoves(ctx, op.ID)
	if err := f.svc.moveRematchFile(ctx, &moves[0], &rematchProgress{s: f.svc, id: op.ID}); err != nil {
		t.Fatal(err)
	}
	moves[0].State = "rematch_moved"
	_ = f.db.PutRematchMove(ctx, moves[0])
	if err := f.svc.CancelRematch(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.runRematch(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.GetRematch(ctx, op.ID)
	if got.State != "cancelled" {
		t.Fatalf("state = %s, want cancelled", got.State)
	}
	if b, err := os.ReadFile(a); err != nil || string(b) != "arrival" {
		t.Fatal("Stop did not put the moved file back")
	}
	item, _ := f.db.GetMediaItemFull(ctx, src2.ID)
	if len(item.Files) != 1 || item.Files[0].Path != a {
		t.Fatalf("source files = %+v, want the row untouched", item.Files)
	}
}

// A restart mid-operation resumes from the journal by what it observes: a
// rename that happened before the crash is committed, not redone; a source
// that changed since the preview is failed for review, never moved.
func TestRematchResumesAfterCrash(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	src := f.movie(t, "Dune", 1984, 841, "")
	a := f.file(t, src.ID, filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv"), "a", "")
	b := f.file(t, src.ID, filepath.Join(src.Path, "Dune (1984) [WEBDL-1080p].mkv"), "b", "")
	if err := f.db.SetFileQuality(ctx, b, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}); err != nil {
		t.Fatal(err)
	}
	dst := f.movie(t, "Dune", 2021, 438631, "")
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil || len(p.Moves) != 2 {
		t.Fatalf("preview = %+v %v", p.Moves, err)
	}
	req.RequestID, req.SourceRevision, req.TargetRevision, req.PlanFingerprint = "crash", p.SourceRevision, p.TargetRevision, p.Fingerprint
	op, err := f.svc.startRematchLocked(ctx, req, p.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	moves, _ := f.db.ListRematchMoves(ctx, op.ID)
	var moveA, moveB sqlite.RematchMove
	for _, m := range moves {
		if m.FileID == a {
			moveA = m
		} else {
			moveB = m
		}
	}
	// The crash: A was renamed but the journal still says planned; B was
	// rewritten by someone else.
	_ = os.MkdirAll(filepath.Dir(moveA.Target), 0o755)
	if err := os.Rename(moveA.Source, moveA.Target); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	writeFile(t, moveB.Source, "b changed")

	f.svc.driveRematches(ctx)
	st := f.wait(t, op.ID)
	if st.State != "complete" {
		t.Fatalf("state = %s (%s), want complete", st.State, st.LastError)
	}
	var result rematchResult
	_ = json.Unmarshal(st.Result, &result)
	if len(result.Moved) != 1 || len(result.Failed) != 1 || result.Failed[0].From != moveB.Source {
		t.Fatalf("result = %+v, want A moved and B failed for review", result)
	}
	if body, _ := os.ReadFile(moveB.Source); string(body) != "b changed" {
		t.Fatal("a changed source was moved")
	}
	target, _ := f.db.GetMediaItemFull(ctx, dst.ID)
	source, _ := f.db.GetMediaItemFull(ctx, src.ID)
	if len(target.Files) != 1 || target.Files[0].Path != moveA.Target || len(source.Files) != 1 {
		t.Fatalf("target %+v source %+v, want A in the target and B left with the source", target.Files, source.Files)
	}
}

// Two titles' leases are taken sorted and all-or-none, so two holders cannot
// deadlock; a held title makes the other wait, then busy.
func TestLockImportTargetsSortedAllOrNone(t *testing.T) {
	svc, _, _ := setup(t, nil, nil)
	ctx := context.Background()
	held, release := svc.lockImportTarget(ctx, 7)
	defer release()
	if _, _, err := svc.lockImportTargets(ctx, 50*time.Millisecond, 9, 7); !errors.Is(err, ErrImportTargetBusy) {
		t.Fatalf("err = %v, want busy while 7 is held", err)
	}
	// 9 was released when 7 could not be taken.
	if _, r, ok := svc.tryLockImportTarget(ctx, 9); !ok {
		t.Fatal("title 9 stayed held after a refused pair")
	} else {
		r()
	}
	_ = held
	release()
	both, releaseBoth, err := svc.lockImportTargets(ctx, time.Second, 9, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBoth()
	if h, ok := heldTarget(both); !ok || !h.covers(7) || !h.covers(9) || h.covers(8) {
		t.Fatalf("held = %+v, want both titles covered", h)
	}
}

// A move is not complete until every step after the commit is done: an
// original that cannot be removed keeps the operation committed, with the
// reason, and the next pass finishes it (the crash between commit and
// finish is the same state). A replaced file goes the same way.
func TestRematchFinishRetriesUntilEveryMoveIsDone(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	old := sameStorage
	sameStorage = func(string, string) bool { return false }
	t.Cleanup(func() { sameStorage = old })
	src := f.movie(t, "Dune", 1984, 841, "")
	orig := filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv")
	f.file(t, src.ID, orig, "4k bytes", "")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	worse := filepath.Join(dst.Path, "Dune (2021) [WEBDL-1080p].mkv")
	w := f.file(t, dst.ID, worse, "1080", "")
	_ = f.db.SetFileQuality(ctx, w, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080})
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil || p.Moves[0].Action != "replace" || p.Moves[0].Mode != "copy" {
		t.Fatalf("preview = %+v %v", p.Moves, err)
	}
	req.RequestID, req.SourceRevision, req.TargetRevision, req.PlanFingerprint = "finish", p.SourceRevision, p.TargetRevision, p.Fingerprint
	op, err := f.svc.startRematchLocked(ctx, req, p.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	rematchRemove = func(string) error { return errors.New("read-only file system") }
	if err := f.svc.runRematch(ctx, op.ID); err == nil {
		t.Fatal("a finish that could not remove the original reported done")
	}
	rematchRemove = os.Remove
	st, _ := f.svc.GetRematch(ctx, op.ID)
	if st.State != "committed" || !strings.Contains(st.LastError, orig) || !strings.Contains(st.LastError, "retrying") {
		t.Fatalf("status = %s %q, want committed with the original named", st.State, st.LastError)
	}
	if _, err := os.Stat(orig); err != nil {
		t.Fatal("the original went although its removal failed")
	}
	f.svc.driveRematches(ctx)
	st = f.wait(t, op.ID)
	if st.State != "complete" {
		t.Fatalf("state = %s (%s), want complete on the next pass", st.State, st.LastError)
	}
	for _, gone := range []string{orig, worse} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s is still on disk", gone)
		}
	}
	if moves, _ := f.db.ListRematchMoves(ctx, op.ID); len(moves) != 0 {
		t.Fatalf("journal = %+v, want it pruned once complete", moves)
	}
}

// An operation whose titles are busy waits, says so, and runs on a later
// pass: resume is not one-shot.
func TestRematchDriverWaitsForABusyTitle(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	src := f.movie(t, "Dune", 1984, 841, "")
	f.file(t, src.ID, filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv"), "x", "")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, _ := f.svc.PreviewRematch(ctx, req)
	req.RequestID, req.SourceRevision, req.TargetRevision, req.PlanFingerprint = "busy", p.SourceRevision, p.TargetRevision, p.Fingerprint
	op, err := f.svc.startRematchLocked(ctx, req, p.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	_, release := f.svc.lockImportTarget(ctx, dst.ID)
	// A move younger than the lease wait is still inside StartRematch's own
	// wait: no "waiting" noise on it yet.
	f.svc.driveRematches(ctx)
	if st, _ := f.svc.GetRematch(ctx, op.ID); st.LastError != "" {
		t.Fatalf("a fresh planned move says %q, want nothing", st.LastError)
	}
	if _, err := f.db.W.ExecContext(ctx, `UPDATE rematch_operations SET created_at=created_at-?`, int64(time.Minute/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	f.svc.driveRematches(ctx)
	st, _ := f.svc.GetRematch(ctx, op.ID)
	if st.State != "planned" || !strings.Contains(st.LastError, "waiting for its titles") {
		t.Fatalf("status = %s %q, want it waiting, and saying so", st.State, st.LastError)
	}
	release()
	f.svc.driveRematches(ctx)
	if st = f.wait(t, op.ID); st.State != "complete" {
		t.Fatalf("state = %s (%s)", st.State, st.LastError)
	}
}

// Stop rolls back; a copy that cannot be removed keeps the operation
// stopping, says what is where, and the next pass finishes it. A copy
// published before its digest reached the journal is still recognised as
// ours (by the source's bytes) and removed.
func TestRematchRollbackRetriesAndRecognisesAnUnjournaledCopy(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	old := sameStorage
	sameStorage = func(string, string) bool { return false }
	t.Cleanup(func() { sameStorage = old; rematchRemove = os.Remove })
	src := f.movie(t, "Arrival", 2016, 329865, "")
	a := filepath.Join(src.Path, "Arrival (2016) [Bluray-2160p].mkv")
	f.file(t, src.ID, a, "arrival", "")
	dst := f.movie(t, "Arrival", 2017, 999, "")
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, _ := f.svc.PreviewRematch(ctx, req)
	req.RequestID, req.SourceRevision, req.TargetRevision, req.PlanFingerprint = "rb", p.SourceRevision, p.TargetRevision, p.Fingerprint
	op, err := f.svc.startRematchLocked(ctx, req, p.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	moves, _ := f.db.ListRematchMoves(ctx, op.ID)
	if err := f.svc.moveRematchFile(ctx, &moves[0], &rematchProgress{s: f.svc, id: op.ID}); err != nil {
		t.Fatal(err)
	}
	moves[0].State, moves[0].SHA256 = "rematch_moved", "" // the crash: digest never journaled
	_ = f.db.PutRematchMove(ctx, moves[0])
	if err := f.svc.CancelRematch(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	rematchRemove = func(string) error { return errors.New("device busy") }
	if err := f.svc.runRematch(ctx, op.ID); err == nil {
		t.Fatal("a rollback that left a copy behind reported done")
	}
	st, _ := f.svc.GetRematch(ctx, op.ID)
	if st.State != "cancel_pending" || !strings.Contains(st.LastError, moves[0].Target) {
		t.Fatalf("status = %s %q, want still stopping, naming the copy", st.State, st.LastError)
	}
	rematchRemove = os.Remove
	f.svc.driveRematches(ctx)
	if st = f.wait(t, op.ID); st.State != "cancelled" {
		t.Fatalf("state = %s (%s)", st.State, st.LastError)
	}
	if _, err := os.Stat(moves[0].Target); !os.IsNotExist(err) {
		t.Fatal("the unjournaled copy was left in the library folder")
	}
	if b, _ := os.ReadFile(a); string(b) != "arrival" {
		t.Fatal("the source was touched")
	}
}

// Files sharing a video's stem are sidecars only when they are not media
// themselves (or library files).
func TestRematchSidecarsExcludeMediaSharingTheStem(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	src := f.movie(t, "Dune", 1984, 841, "")
	main := filepath.Join(src.Path, "Dune (1984).mkv")
	f.file(t, src.ID, main, "main", "")
	writeFile(t, filepath.Join(src.Path, "Dune (1984).1080p.mkv"), "another cut")
	writeFile(t, filepath.Join(src.Path, "Dune (1984).en.srt"), "subs")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	p, err := f.svc.PreviewRematch(ctx, RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID, FileIDs: []int64{f.fileID(t, main)}})
	if err != nil {
		t.Fatal(err)
	}
	sc := p.Moves[0].Sidecars
	if len(sc) != 1 || !strings.HasSuffix(sc[0].From, ".en.srt") {
		t.Fatalf("sidecars = %+v, want the subtitle only", sc)
	}
}

func (f rematchFixture) fileID(t *testing.T, path string) int64 {
	t.Helper()
	var id int64
	if err := f.db.R.QueryRowContext(context.Background(), `SELECT id FROM media_files WHERE path=?`, path).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// More files than Cinema takes in one request: every expected file is in
// some request (requests with removed_paths are probe-only), and so is
// every removed path.
func TestRematchOutboxProbesEveryFileOfALargeMove(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	if _, err := f.db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "cinema", Settings: map[string]string{"url": "http://c", "apiKey": "k"}, OnImport: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var placed []rematchPlaced
	for i := 0; i < 150; i++ {
		placed = append(placed, rematchPlaced{
			File:    ports.ExpectedFile{Path: filepath.Join(f.root.Path, "New Show", "Season 01", "New Show - S01E"+strconv.Itoa(i)+" - A Long Episode Title For Size [WEBDL-1080p].mkv"), Bytes: 1, MtimeNs: 1},
			Removed: []string{filepath.Join(f.root.Path, "Old Show", "Season 01", "Old Show - S01E"+strconv.Itoa(i)+" [WEBDL-1080p].mkv")}})
	}
	rows, err := f.svc.rematchOutbox(ctx, "rm-big", domain.MediaItem{ID: 1, Title: "Old Show"}, domain.MediaItem{ID: 2, Kind: domain.KindSeries, Title: "New Show"}, placed, nil)
	if err != nil {
		t.Fatal(err)
	}
	seenExp, seenRem := map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		var n ports.Notification
		_ = json.Unmarshal([]byte(r.Payload), &n)
		exp := n.Import.ExpectedFiles[r.Directory]
		if len(exp) > ports.MaxExpectedFiles || len(n.Import.RemovedPaths) > ports.MaxExpectedFiles || len(exp) != len(r.ExpectedFiles) {
			t.Fatalf("a request carries %d expected, %d removed", len(exp), len(n.Import.RemovedPaths))
		}
		raw, _ := json.Marshal(exp)
		if len(raw) > ports.MaxExpectedFileBytes {
			t.Fatalf("a request's expected files take %d bytes", len(raw))
		}
		for _, e := range exp {
			seenExp[e.Path] = true
		}
		for _, p := range n.Import.RemovedPaths {
			seenRem[p] = true
		}
	}
	pairOf := map[string]string{}
	for _, p := range placed {
		pairOf[p.Removed[0]] = p.File.Path
	}
	for _, r := range rows {
		var n ports.Notification
		_ = json.Unmarshal([]byte(r.Payload), &n)
		inReq := map[string]bool{}
		for _, e := range n.Import.ExpectedFiles[r.Directory] {
			inReq[e.Path] = true
		}
		for _, p := range n.Import.RemovedPaths {
			if !inReq[pairOf[p]] {
				t.Fatalf("removed %s travels without its expected file %s", p, pairOf[p])
			}
		}
		if r.MediaItemID != 2 {
			t.Fatalf("outbox row media item = %d, want the target (2)", r.MediaItemID)
		}
	}
	if len(seenExp) != 150 || len(seenRem) != 150 {
		t.Fatalf("probed %d expected and %d removed of 150 each over %d requests", len(seenExp), len(seenRem), len(rows))
	}
}

// The item page's status panel shows the open Re-match on both titles.
func TestItemStatusShowsOpenRematchOnBothSides(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	src := f.movie(t, "Dune", 1984, 841, "")
	f.file(t, src.ID, filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv"), "dune", "")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.RequestID, req.SourceRevision, req.TargetRevision, req.PlanFingerprint = "panel", p.SourceRevision, p.TargetRevision, p.Fingerprint
	op, err := f.svc.startRematchLocked(ctx, req, p.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	on, err := f.svc.ItemStatus(ctx, dst.ID, -1)
	if err != nil {
		t.Fatal(err)
	}
	if on.Rematch == nil || on.Rematch.OperationID != op.ID || on.Rematch.Role != "target" || on.Rematch.Headline != "Files moving here from Dune (1984)" ||
		on.Rematch.FilesTotal != 1 || !on.Active || !on.Rematch.Stoppable {
		t.Fatalf("target rematch = %+v active=%v", on.Rematch, on.Active)
	}
	off, err := f.svc.ItemStatus(ctx, src.ID, -1)
	if err != nil || off.Rematch == nil || off.Rematch.Role != "source" || off.Rematch.Headline != "Files moving from here to Dune (2021)" {
		t.Fatalf("source rematch = %+v err=%v", off.Rematch, err)
	}
}

// The moved sub-delivery carries the target's media_item_id, so the target's
// status panel shows it in the in-cinema stage with cinema's lasting warnings.
func TestItemStatusShowsMovedDeliveryWithCinemaWarnings(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	if _, err := f.db.AddNotifier(ctx, ports.NotifierConfig{Type: "plurx", Name: "cinema", Settings: map[string]string{"url": "http://c", "apiKey": "k"}, OnImport: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	src := f.movie(t, "Dune", 1984, 841, "")
	f.file(t, src.ID, filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv"), "dune", "")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	st := f.run(t, req, p)
	if st.State != "complete" {
		t.Fatalf("state = %s", st.State)
	}
	dels, err := f.db.OperationDeliveries(ctx, st.ID)
	if err != nil || len(dels) != 1 || dels[0].MediaItemID != dst.ID {
		t.Fatalf("deliveries = %+v %v, want one carrying the target's id %d", dels, err, dst.ID)
	}
	d := dels[0]
	d.Status, d.VisibleAt, d.Result = "ok", time.Now().UnixMilli(), "visible"
	d.FileResults = []sqlite.DeliveryFileResult{
		{Path: d.ExpectedFiles[0].Path, State: "placed", ItemID: 77, Bytes: d.ExpectedFiles[0].Bytes, MtimeNs: d.ExpectedFiles[0].MtimeNs, Matched: true},
		{Path: filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv"), State: "retained", Code: "record_kept", Warning: "cinema kept the record of the old file: pinned"},
	}
	if err := f.db.SettleDelivery(ctx, d, sqlite.DeliveryTiming{}); err != nil {
		t.Fatal(err)
	}
	status, err := f.svc.ItemStatus(ctx, dst.ID, -1)
	if err != nil {
		t.Fatal(err)
	}
	in := stageOf(t, status, StageInCinema)
	if in.State != StatusDone || !strings.Contains(in.Detail, "warning: cinema kept the record of the old file: pinned") {
		t.Fatalf("in cinema = %+v, want done with cinema's lasting warning", in)
	}
	if status.Cinema == nil || len(status.Cinema.Warnings) != 1 {
		t.Fatalf("cinema = %+v", status.Cinema)
	}
}

// A Re-match that ended badly stays on the panel until dismissed.
func TestItemStatusKeepsAnEndedRematchUntilDismissed(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	src := f.movie(t, "Dune", 1984, 841, "")
	f.file(t, src.ID, filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv"), "dune", "")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, _ := f.svc.PreviewRematch(ctx, req)
	req.RequestID, req.SourceRevision, req.TargetRevision, req.PlanFingerprint = "ended", p.SourceRevision, p.TargetRevision, p.Fingerprint
	op, err := f.svc.startRematchLocked(ctx, req, p.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetRematchState(ctx, op.ID, "failed", "the target disk is full", json.RawMessage(`{"failed":[{"from":"/a.mkv","to":"no space left"}]}`)); err != nil {
		t.Fatal(err)
	}
	st, err := f.svc.ItemStatus(ctx, dst.ID, -1)
	if err != nil {
		t.Fatal(err)
	}
	r := st.Rematch
	if r == nil || !r.Ended || r.State != "failed" || r.Error != "the target disk is full" || len(r.FailedMoves) != 1 || r.FailedMoves[0].Reason != "no space left" || r.Stoppable || st.Active {
		t.Fatalf("rematch = %+v active=%v, want the failed operation with its reason and failed list", r, st.Active)
	}
	if err := f.svc.DismissRematch(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ = f.svc.ItemStatus(ctx, dst.ID, -1); st.Rematch != nil {
		t.Fatalf("dismissed rematch still shown: %+v", st.Rematch)
	}
}

// A copy whose source changed after the copy is never removed, and the result
// says it was left behind.
func TestRematchCopyWithChangedSourceIsLeftBehind(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	old := sameStorage
	sameStorage = func(string, string) bool { return false }
	t.Cleanup(func() { sameStorage = old; rematchRemove = os.Remove })
	src := f.movie(t, "Dune", 1984, 841, "")
	orig := filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv")
	f.file(t, src.ID, orig, "4k bytes", "")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.RequestID, req.SourceRevision, req.TargetRevision, req.PlanFingerprint = "changed", p.SourceRevision, p.TargetRevision, p.Fingerprint
	op, err := f.svc.startRematchLocked(ctx, req, p.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	rematchRemove = func(string) error { return errors.New("read-only file system") }
	if err := f.svc.runRematch(ctx, op.ID); err == nil {
		t.Fatal("expected the finish to be retried")
	}
	rematchRemove = os.Remove
	// Someone puts a different file at the source after the copy.
	if err := os.Remove(orig); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orig, []byte("something else entirely"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.svc.driveRematches(ctx)
	st := f.wait(t, op.ID)
	var res rematchResult
	_ = json.Unmarshal(st.Result, &res)
	if st.State != "complete" || !slices.Contains(res.LeftBehind, orig) {
		t.Fatalf("state %s, result %+v, want complete with %s left behind", st.State, res, orig)
	}
	if b, err := os.ReadFile(orig); err != nil || string(b) != "something else entirely" {
		t.Fatal("the changed source was removed")
	}
}

// A scan that pruned the file's row while it moved makes the commit stale:
// every file goes back, and the operation says why instead of a bare failed.
func TestRematchLosingToAScanSaysSo(t *testing.T) {
	f := newRematchFixture(t)
	ctx := context.Background()
	src := f.movie(t, "Dune", 1984, 841, "")
	orig := filepath.Join(src.Path, "Dune (1984) [Bluray-2160p].mkv")
	id := f.file(t, src.ID, orig, "dune", "")
	dst := f.movie(t, "Dune", 2021, 438631, "")
	req := RematchRequest{SourceItemID: src.ID, TargetItemID: dst.ID}
	p, err := f.svc.PreviewRematch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.RequestID, req.SourceRevision, req.TargetRevision, req.PlanFingerprint = "scan-race", p.SourceRevision, p.TargetRevision, p.Fingerprint
	op, err := f.svc.startRematchLocked(ctx, req, p.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.DeleteFile(ctx, id); err != nil { // the scan's prune
		t.Fatal(err)
	}
	_ = f.svc.runRematch(ctx, op.ID)
	st, _ := f.svc.GetRematch(ctx, op.ID)
	if st.State != "failed" || !strings.Contains(st.LastError, "library scan or import changed this title") || !strings.Contains(st.LastError, "run it again") {
		t.Fatalf("status = %s %q, want failed saying a scan changed the title", st.State, st.LastError)
	}
	if _, err := os.Stat(orig); err != nil {
		t.Fatal("the file did not go back")
	}
}
