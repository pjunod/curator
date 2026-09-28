package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// probedFile puts a measured file on the movie: WEB-DL 1080p at high
// confidence, with the given audio tracks.
func probedFile(t *testing.T, db *sqlite.DB, movieID int64, name string, audio ...mediainfo.AudioInfo) int64 {
	t.Helper()
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, name)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, movieID, 0, path, 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	info := mediainfo.Info{
		Container:  mediainfo.ContainerMKV,
		Video:      &mediainfo.VideoInfo{Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8},
		Audio:      audio,
		DurationMS: 96 * 60 * 1000, BitrateKbps: 8000,
	}
	if err := db.SetFileMediaInfo(ctx, fileID, info, mediainfo.ProvenanceProbe, mediainfo.ConfidenceHigh, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fileID,
		quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		mediainfo.ProvenanceProbe, mediainfo.ConfidenceHigh); err != nil {
		t.Fatal(err)
	}
	return fileID
}

func requireEnglish(t *testing.T, db *sqlite.DB, profileID int64, langs ...string) {
	t.Helper()
	ctx := context.Background()
	p, err := db.GetProfile(ctx, profileID)
	if err != nil {
		t.Fatal(err)
	}
	p.Languages = langs
	if err := db.UpdateProfile(ctx, p); err != nil {
		t.Fatal(err)
	}
}

// TestJustFriends is ADR 0022 at the level it was reported: a WEB-DL 1080p
// copy of an English-language film with no English audio track sat "at the
// target" under the 1080p profile, and none of the other releases were "any
// better" by quality, so nothing was ever tried. With English required, the
// item is wanted again and the English release is the one grabbed — not the
// higher-ranked German remux.
func TestJustFriends(t *testing.T) {
	german := mediainfo.AudioInfo{Codec: "ac3", Channels: 6, Language: "ger"}
	ctx := context.Background()

	t.Run("a German-only file at the target is not done once English is required", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, movieID := autoSetup(t, nil, client)
		probedFile(t, db, movieID, "Just Friends (2005).mkv", german)

		wanted, err := svc.Wanted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if huntedFor(wanted, movieID) {
			t.Fatal("with no language requirement, a file at the target is done -- unchanged behaviour")
		}

		requireEnglish(t, db, 1, "en")
		svc.InvalidateWanted()
		wanted, err = svc.Wanted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !huntedFor(wanted, movieID) {
			t.Fatal("a German-only file under a profile requiring English must be wanted")
		}
		rows, err := svc.WantedList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var row WantedSummary
		for _, r := range rows {
			if r.MediaItemID == movieID {
				row = r
			}
		}
		if row.Reason != "upgrade" || row.Current != "WEB-DL 1080p (German)" {
			t.Errorf("wanted row = %+v, want an upgrade from \"WEB-DL 1080p (German)\"", row)
		}
	})

	t.Run("the English release is grabbed, not the better-ranked German one", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, movieID := autoSetup(t, []ports.Release{
			rel("Test.Movie.2024.GERMAN.1080p.BluRay.REMUX-BIG", 100),
			rel("Test.Movie.2024.1080p.WEB-DL.DD5.1-FINE", 50),
			rel("Test.Movie.2024.720p.WEB-DL-SMALL", 10),
		}, client)
		probedFile(t, db, movieID, "Test Movie.mkv", german)
		requireEnglish(t, db, 1, "en")

		if _, err := svc.AutoSearchItem(ctx, movieID); err != nil {
			t.Fatal(err)
		}
		if len(client.added) != 1 {
			t.Fatalf("adds = %v, want exactly one grab", client.added)
		}
		if got := client.added[0]; got != "http://dl/Test.Movie.2024.1080p.WEB-DL.DD5.1-FINE" {
			t.Errorf("grabbed %q, want the untagged (English) 1080p release", got)
		}
	})

	t.Run("a lower quality in the right language still beats the wrong language", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, movieID := autoSetup(t, []ports.Release{
			rel("Test.Movie.2024.German.DL.1080p.WEB-DL-DUB", 100),
			rel("Test.Movie.2024.720p.WEB-DL-SMALL", 10),
		}, client)
		probedFile(t, db, movieID, "Test Movie.mkv", german)
		requireEnglish(t, db, 1, "en")

		if _, err := svc.AutoSearchItem(ctx, movieID); err != nil {
			t.Fatal(err)
		}
		// German.DL = German + original (Multi), which satisfies English and
		// outranks the 720p; it is the right grab.
		if len(client.added) != 1 || client.added[0] != "http://dl/Test.Movie.2024.German.DL.1080p.WEB-DL-DUB" {
			t.Fatalf("adds = %v, want the MULTi 1080p", client.added)
		}
	})

	t.Run("only the wrong language on offer: nothing grabbed", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, movieID := autoSetup(t, []ports.Release{
			rel("Test.Movie.2024.GERMAN.1080p.BluRay.REMUX-BIG", 100),
			rel("Test.Movie.2024.FRENCH.720p.WEB-DL-SMALL", 10),
		}, client)
		probedFile(t, db, movieID, "Test Movie.mkv", german)
		requireEnglish(t, db, 1, "en")

		if _, err := svc.AutoSearchItem(ctx, movieID); err != nil {
			t.Fatal(err)
		}
		if len(client.added) != 0 {
			t.Fatalf("adds = %v, want nothing: no release offers English", client.added)
		}
	})

	t.Run("an untagged track is not proof of absence", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, movieID := autoSetup(t, nil, client)
		probedFile(t, db, movieID, "Test Movie.mkv", german, mediainfo.AudioInfo{Codec: "aac", Channels: 2})
		requireEnglish(t, db, 1, "en")

		wanted, err := svc.Wanted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if huntedFor(wanted, movieID) {
			t.Fatal("a file with an untagged audio track might already be English; it must not be hunted")
		}
	})

	t.Run("an English track among others is enough", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, movieID := autoSetup(t, nil, client)
		probedFile(t, db, movieID, "Test Movie.mkv", german, mediainfo.AudioInfo{Codec: "aac", Channels: 2, Language: "en-US"})
		requireEnglish(t, db, 1, "en")

		wanted, err := svc.Wanted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if huntedFor(wanted, movieID) {
			t.Fatal("a file that carries English is satisfied")
		}
	})

	t.Run("the item page says why", func(t *testing.T) {
		client := &fakeClient{}
		_, db, movieID := autoSetup(t, nil, client)
		probedFile(t, db, movieID, "Test Movie.mkv", german)
		requireEnglish(t, db, 1, "en")
		state, err := db.DiskStateForItem(ctx, movieID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !state.Audio.Known || len(state.Audio.Languages) != 1 || state.Audio.Languages[0] != "de" {
			t.Errorf("disk state audio = %+v, want German, known", state.Audio)
		}
	})
}

// TestLanguageMismatchIsBlocklisted is the loop ADR 0022 §4 has to bound. A
// release with no language tag reads as English, gets grabbed to replace a
// German-only file, and turns out to be German too. Without a record of
// that, the item is wanted again the moment the import lands, the same
// release is the best candidate again, and every scheduled search downloads
// it again forever. A measured mismatch blocklists the release and says so.
func TestLanguageMismatchIsBlocklisted(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	release := "Test.Movie.2024.1080p.WEB-DL.DD5.1-SILENT"
	svc, db, movieID := autoSetup(t, []ports.Release{rel(release, 50)}, client)
	probedFile(t, db, movieID, "Test Movie.mkv", mediainfo.AudioInfo{Codec: "ac3", Channels: 6, Language: "ger"})
	requireEnglish(t, db, 1, "en")

	if _, err := svc.AutoSearchItem(ctx, movieID); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Fatalf("adds = %v, want the silent (assumed English) release grabbed once", client.added)
	}

	// The download completes and the file is the German dub after all — a
	// real MKV whose one audio track declares "ger".
	payload := filepath.Join(t.TempDir(), "p")
	if err := os.MkdirAll(payload, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payload, release+".mkv"),
		corpusFile(t, "mkv-1080p-h264-ac3-ger-whole.mkv"), 0o644); err != nil {
		t.Fatal(err)
	}
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: release, State: ports.StateCompleted, Progress: 1, SavePath: payload,
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	dls, err := db.ListRecentDownloads(ctx)
	if err != nil || len(dls) != 1 || dls[0].State != "imported" {
		t.Fatalf("downloads = %+v err=%v, want the grab imported", dls, err)
	}

	// Measured, not inferred: every track declared a language and none is
	// English. That is a fact about the release, so it is blocklisted.
	blocked, err := db.IsBlocklisted(ctx, release, "idx")
	if err != nil || !blocked {
		t.Fatalf("blocklisted = %v err=%v, want the German release blocklisted", blocked, err)
	}
	events, err := db.ListHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == HistoryLanguageMismatch && e.ReleaseTitle == release {
			found = true
		}
	}
	if !found {
		t.Error("no language_mismatch history entry for a release measured to lack the language")
	}

	// The item is still wanted (the new file is German too) — and the same
	// release is NOT grabbed again.
	svc.InvalidateWanted()
	wanted, err := svc.Wanted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !huntedFor(wanted, movieID) {
		t.Fatal("a German file replacing a German file leaves the item wanted")
	}
	if _, err := svc.AutoSearchItem(ctx, movieID); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Fatalf("adds = %v, want no second grab of the blocklisted release", client.added)
	}
}

// TestManualImportNeverDeletesTheRightLanguageForTheWrongOne: a manual
// import proceeds whatever the profile says, but it only REPLACES a file
// when the new one is better — and a file in a language the profile refuses
// is worse whatever its rank (ADR 0022). The German 1080p lands beside the
// English 720p; it does not delete it.
func TestManualImportNeverDeletesTheRightLanguageForTheWrongOne(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	svc, db, movieID := autoSetup(t, nil, client)
	requireEnglish(t, db, 1, "en")

	// English 720p on disk, measured.
	item, err := db.GetMediaItemFull(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	english := filepath.Join(item.Path, "Test Movie (2024) [HDTV 720p].mkv")
	if err := os.WriteFile(english, corpusFile(t, wholeCorpus720), 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, movieID, 0, english, 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	info := mediainfo.Info{Container: mediainfo.ContainerMKV,
		Video:      &mediainfo.VideoInfo{Codec: "h264", Width: 1280, Height: 720, BitDepth: 8},
		Audio:      []mediainfo.AudioInfo{{Codec: "aac", Channels: 2, Language: "eng"}},
		DurationMS: 5_400_000, BitrateKbps: 4000}
	if err := db.SetFileMediaInfo(ctx, fileID, info, mediainfo.ProvenanceProbe, mediainfo.ConfidenceHigh, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fileID, quality.Quality{Source: quality.SourceHDTV, Resolution: 720},
		mediainfo.ProvenanceProbe, mediainfo.ConfidenceHigh); err != nil {
		t.Fatal(err)
	}

	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Movie.2024.GERMAN.1080p.WEB-DL-DUB.mkv"),
		corpusFile(t, "mkv-1080p-h264-ac3-ger-whole.mkv"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := svc.ManualImport(ctx, ManualImportRequest{Path: payload, MediaItemID: movieID})
	if err != nil {
		t.Fatalf("a manual import is never gated: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("imported = %d: %+v", res.Imported, res.Files)
	}
	if _, err := os.Stat(english); err != nil {
		t.Fatalf("the English file was deleted by a manual import of a German one: %v", err)
	}
	records, err := db.FileQualityRecords(ctx, movieID)
	if err != nil || len(records) != 2 {
		t.Fatalf("records = %d (%v), want both files kept", len(records), err)
	}
}
