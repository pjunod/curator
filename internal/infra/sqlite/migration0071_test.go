package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
)

// Migration 71 derives hdr_tier from the media_info already stored, and must
// agree with mediainfo.Info.HDRTier on every record a pre-0026 prober could
// have written — the backfill and the probe write are two implementations of
// one rule.
func TestHDRTierMigrationBackfillsLikeTheProbe(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 70); err != nil {
		t.Fatal(err)
	}

	rows := []struct{ path, info, prov string }{
		{"/a/dv.mkv", `{"container":"mkv","video":{"codec":"hevc","width":3840,"height":2160,"hdr":["dv","hdr10"]}}`, "probe"},
		{"/a/hdr10.mkv", `{"container":"mkv","video":{"codec":"hevc","width":3840,"height":2160,"hdr":["hdr10"]}}`, "probe"},
		{"/a/hlg.mkv", `{"container":"mkv","video":{"codec":"hevc","width":1920,"height":1080,"hdr":["hlg"]}}`, "probe"},
		{"/a/sdr.mp4", `{"container":"mp4","video":{"codec":"h264","width":1920,"height":1080}}`, "probe"},
		{"/a/novideo.mka", `{"container":"mkv","audio":[{"codec":"flac"}]}`, "probe"},
		{"/a/failed.mkv", `{"container":"mkv","video":{"codec":"hevc","width":3840,"height":2160}}`, "failed"},
		{"/a/unprobed.mkv", ``, ""},
		{"/a/garbage.mkv", `{not json`, "probe"},
	}
	for _, r := range rows {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files (path,size,added_at,media_info,quality_provenance) VALUES (?,1,0,?,?)`, r.path, r.info, r.prov); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	assertColumn(t, ctx, db, "media_files", "hdr_tier")
	for _, r := range rows {
		var got string
		if err := db.R.QueryRowContext(ctx, `SELECT hdr_tier FROM media_files WHERE path = ?`, r.path).Scan(&got); err != nil {
			t.Fatal(err)
		}
		var info mediainfo.Info
		_ = json.Unmarshal([]byte(r.info), &info)
		want := hdrTierColumn(info, mediainfo.Provenance(r.prov))
		if got != want {
			t.Errorf("%s: hdr_tier = %q, probe would write %q", r.path, got, want)
		}
	}
}
