package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
)

// Migration 34 derives audio_languages from the media_info every probe has
// been storing since Phase 6, so the ADR 0022 rule applies to an existing
// library without a re-probe. The backfill must read every real shape and
// leave the odd ones alone rather than failing the migration.
func TestAudioLanguagesMigrationBackfills(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 33); err != nil {
		t.Fatal(err)
	}

	rows := []struct{ path, info, want string }{
		{"/a/german.mkv", `{"container":"mkv","audio":[{"codec":"ac3","channels":6,"language":"ger"}]}`, "ger"},
		{"/a/both.mkv", `{"container":"mkv","audio":[{"codec":"truehd","language":"eng"},{"codec":"ac3","language":"de-DE"}]}`, "eng+de-de"},
		{"/a/untagged.mp4", `{"container":"mp4","audio":[{"codec":"aac","channels":2}]}`, "und"},
		{"/a/mixed.mkv", `{"container":"mkv","audio":[{"codec":"ac3","language":"ger"},{"codec":"aac"}]}`, "ger+und"},
		{"/a/noaudio.mkv", `{"container":"mkv","video":{"codec":"h264"}}`, ""},
		{"/a/unprobed.mkv", ``, ""},
		{"/a/garbage.mkv", `{not json`, ""},
		{"/a/odd.mkv", `{"container":"mkv","audio":"nope"}`, ""},
	}
	for _, r := range rows {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files (path,size,added_at,media_info) VALUES (?,1,0,?)`, r.path, r.info); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	assertColumn(t, ctx, db, "media_files", "audio_languages")
	for _, r := range rows {
		var got string
		if err := db.R.QueryRowContext(ctx, `SELECT audio_languages FROM media_files WHERE path = ?`, r.path).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != r.want {
			t.Errorf("%s: audio_languages = %q, want %q", r.path, got, r.want)
		}
	}
}

// The column and the JSON are written together, from the probe path and
// from the placement commit, so they never disagree.
func TestAudioLanguagesColumnFollowsTheProbe(t *testing.T) {
	cases := map[string]string{
		"":          audioFromColumnProbe(nil),
		"ger":       audioFromColumnProbe([]string{"ger"}),
		"deu+und":   audioFromColumnProbe([]string{"deu", ""}),
		"en-us+jpn": audioFromColumnProbe([]string{"en-US", "jpn"}),
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("audioLanguagesColumn = %q, want %q", got, want)
		}
	}
	a := audioFromColumn("ger+und")
	if a.Known || len(a.Languages) != 1 || a.Languages[0] != "de" {
		t.Errorf("audioFromColumn(ger+und) = %+v", a)
	}
	a = audioFromColumn("eng+de-de")
	if !a.Known || len(a.Languages) != 2 || a.Languages[0] != "de" || a.Languages[1] != "en" {
		t.Errorf("audioFromColumn(eng+de-de) = %+v", a)
	}
	if a = audioFromColumn(""); a.Known || a.Languages != nil {
		t.Errorf("audioFromColumn('') = %+v", a)
	}
}

func audioFromColumnProbe(langs []string) string {
	var info mediainfo.Info
	for _, l := range langs {
		info.Audio = append(info.Audio, mediainfo.AudioInfo{Codec: "aac", Language: l})
	}
	return audioLanguagesColumn(info)
}

// weakestRecorded reads the languages field with the same weakest-link
// rule the single-item path uses: known only when every file is known,
// carrying only what every file carries.
func TestWeakestRecordedLanguages(t *testing.T) {
	cases := []struct {
		name   string
		joined string
		known  bool
		langs  []string
	}{
		{"one German file", "webdl-1080~probe~high~ger", true, []string{"de"}},
		{"German and English on every file", "webdl-1080~probe~high~ger+eng,hdtv-720~probe~high~eng+deu", true, []string{"de", "en"}},
		{"German file and English file: known, nothing in common", "webdl-1080~probe~high~ger,hdtv-720~probe~high~eng", true, nil},
		{"known plus untagged: unknown", "webdl-1080~probe~high~ger,hdtv-720~probe~high~und", false, nil},
		{"known plus unprobed: unknown", "webdl-1080~probe~high~ger,hdtv-720~filename~none~", false, nil},
		{"old three-field rows: unknown", "webdl-1080~probe~high", false, nil},
	}
	for _, tc := range cases {
		q, _, audio := weakestRecorded(tc.joined)
		if q.Resolution == 0 {
			t.Errorf("%s: quality not read", tc.name)
		}
		if audio.Known != tc.known || len(audio.Languages) != len(tc.langs) {
			t.Errorf("%s: audio = %+v, want known=%v langs=%v", tc.name, audio, tc.known, tc.langs)
			continue
		}
		for i := range tc.langs {
			if audio.Languages[i] != tc.langs[i] {
				t.Errorf("%s: audio = %+v, want %v", tc.name, audio, tc.langs)
			}
		}
	}
}
