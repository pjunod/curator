package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/decision"
	"github.com/pjunod/monarr/internal/domain/hdr"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// probingClient is a usenet client that can peek, answering each release URL
// with the bytes of a real Dolby Vision file from the mediainfo corpus, or a
// refusal.
type probingClient struct {
	fakeClient
	heads    map[string]string // download URL → corpus fixture
	refusals map[string]string // download URL → refusal reason
	probed   []string
}

func (p *probingClient) SupportsReleaseProbe(context.Context) (bool, error) { return true, nil }

func (p *probingClient) ProbeRelease(_ context.Context, url string, maxBytes int64) (ports.ReleaseHead, error) {
	p.mu.Lock()
	p.probed = append(p.probed, url)
	p.mu.Unlock()
	if reason, ok := p.refusals[url]; ok {
		return ports.ReleaseHead{}, &ports.ProbeRefusal{Reason: reason}
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "domain", "mediainfo", "testdata", p.heads[url]))
	if err != nil {
		return ports.ReleaseHead{}, err
	}
	if int64(len(b)) > maxBytes {
		b = b[:maxBytes]
	}
	return ports.ReleaseHead{File: p.heads[url], FileSize: int64(len(b)), Path: []string{"yenc"}, Articles: 1, Bytes: b}, nil
}

func preflightSetup(t *testing.T, releases []ports.Release, client *probingClient, profile quality.Profile) (*Service, *sqlite.DB, int64) {
	t.Helper()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	t.Cleanup(b.Close)
	svc := New(db, b, nil,
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{releases: releases} },
		func(ports.ClientConfig) ports.DownloadClient { return client },
	)
	profile.ID = 3
	if err := db.UpdateProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Title", SortTitle: "title", Year: 2021,
		IDs: domain.ExternalIDs{TMDB: 4242}, Monitored: true, QualityProfileID: 3,
		Path: filepath.Join(t.TempDir(), "Title (2021)"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "idx", URL: "http://x", Protocol: "usenet", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddDownloadClient(ctx, ports.ClientConfig{Type: "nzbd", Name: "runner", URL: "http://runner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return svc, db, itemID
}

func remux(title, url string) ports.Release {
	return ports.Release{Title: title, DownloadURL: url, Indexer: "idx", Protocol: "usenet", Size: 70 << 30}
}

func fel() quality.Profile {
	return quality.Profile{
		Name: "UHD Reference", Target: quality.Quality{Source: quality.SourceRemux, Resolution: 2160}, UpgradesAllowed: true,
		HDROrder:  []hdr.Tier{hdr.TierDVP7FEL, hdr.TierDVP7MEL, hdr.TierDVP8, hdr.TierHDR10Plus, hdr.TierHDR10},
		HDRReject: []hdr.Tier{hdr.TierDVP5},
	}
}

// Interactive search measures the best releases before showing them: the
// measured FEL remux ranks first, an encrypted one keeps its claim and ranks
// below it, a "DV" remux that measures profile 5 is refused, and the title
// is now known to be FEL on disc.
func TestPreflightRanksAndRefusesOnTheMeasurement(t *testing.T) {
	client := &probingClient{
		heads: map[string]string{
			"nzb://a": "mkv-64p-hevc-dv-p7-fel.mkv",
			"nzb://c": "mp4-2160p-hevc-dv-p5.mp4",
		},
		refusals: map[string]string{"nzb://b": ports.ProbeEncrypted},
	}
	releases := []ports.Release{
		remux("Title.2021.2160p.BluRay.REMUX.DoVi.HDR10.HEVC-B", "nzb://b"),
		remux("Title.2021.2160p.BluRay.REMUX.DV.HEVC-C", "nzb://c"),
		remux("Title.2021.2160p.BluRay.REMUX.DV.HDR.HEVC-A", "nzb://a"),
	}
	svc, db, itemID := preflightSetup(t, releases, client, fel())

	cands, err := svc.Search(context.Background(), itemID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 3 {
		t.Fatalf("got %d candidates", len(cands))
	}
	if len(client.probed) != 3 {
		t.Errorf("probed %v, want all three accepted releases", client.probed)
	}
	a, b, c := cands[0], cands[1], cands[2]
	if a.Release.DownloadURL != "nzb://a" || !a.Accepted || a.MeasuredHDR == nil || a.MeasuredHDR.Tier != hdr.TierDVP7FEL {
		t.Fatalf("first = %s accepted=%v measured=%+v, want the measured FEL remux", a.Release.Title, a.Accepted, a.MeasuredHDR)
	}
	if a.Probe == nil || a.Probe.Status != ProbeMeasured || a.Probe.DoVi == nil || a.Probe.DoVi.CMVersion != "4.0" {
		t.Errorf("FEL evidence = %+v", a.Probe)
	}
	if b.Release.DownloadURL != "nzb://b" || !b.Accepted || b.MeasuredHDR != nil || b.Probe == nil || b.Probe.Reason != ports.ProbeEncrypted {
		t.Errorf("second = %s accepted=%v probe=%+v, want the encrypted remux, still accepted on its claim", b.Release.Title, b.Accepted, b.Probe)
	}
	if c.Release.DownloadURL != "nzb://c" || c.Accepted || len(c.Rejections) == 0 || c.Rejections[0].Code != decision.CodeHDRRejected {
		t.Errorf("third = %s accepted=%v rejections=%+v, want the measured P5 refused", c.Release.Title, c.Accepted, c.Rejections)
	}
	item, err := db.GetMediaItemFull(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if item.DiscEL != hdr.ELFEL {
		t.Errorf("disc master = %q, want fel after measuring a FEL remux", item.DiscEL)
	}
}

// A profile that asks to skip unreadable releases refuses the encrypted one.
func TestPreflightCanSkipUnreadableReleases(t *testing.T) {
	client := &probingClient{refusals: map[string]string{"nzb://b": ports.ProbeEncrypted}}
	p := fel()
	p.Preflight.SkipUnreadable = true
	svc, _, itemID := preflightSetup(t, []ports.Release{remux("Title.2021.2160p.BluRay.REMUX.DoVi.HDR10.HEVC-B", "nzb://b")}, client, p)
	cands, err := svc.Search(context.Background(), itemID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Accepted || cands[0].Rejections[len(cands[0].Rejections)-1].Code != CodeProbeUnreadable {
		t.Fatalf("candidates = %+v, want the encrypted release refused as probe_unreadable", cands)
	}
}

// A profile with no HDR preference never probes: a measurement could not
// change anything it decides.
func TestPreflightOnlyRunsForHDRProfiles(t *testing.T) {
	client := &probingClient{heads: map[string]string{"nzb://a": "mkv-64p-hevc-dv-p7-fel.mkv"}}
	plain := quality.Profile{Name: "4K", Target: quality.Quality{Source: quality.SourceRemux, Resolution: 2160}, UpgradesAllowed: true}
	svc, _, itemID := preflightSetup(t, []ports.Release{remux("Title.2021.2160p.BluRay.REMUX.DV.HDR.HEVC-A", "nzb://a")}, client, plain)
	if _, err := svc.Search(context.Background(), itemID, 0, 0); err != nil {
		t.Fatal(err)
	}
	if len(client.probed) != 0 {
		t.Errorf("probed %v under a profile with no HDR order", client.probed)
	}
}

func TestEvidenceOfMeasuresWhatRunnerRead(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "domain", "mediainfo", "testdata", "mp4-64p-hevc-dv-p81.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	ev := evidenceOf(ports.ReleaseHead{File: "x.mp4", FileSize: int64(len(b)), Bytes: b}, nil)
	if !ev.measured() || ev.Tier != hdr.TierDVP8 || ev.DoVi == nil || ev.DoVi.Label() != "DV 8.1" {
		t.Errorf("evidence = %+v", ev)
	}
	if ev := evidenceOf(ports.ReleaseHead{Bytes: []byte("not a container")}, nil); ev.Status != ProbeUnavailable {
		t.Errorf("junk head evidence = %+v", ev)
	}
	if ev := evidenceOf(ports.ReleaseHead{}, os.ErrDeadlineExceeded); ev.Status != ProbeFailed {
		t.Errorf("transport error evidence = %+v", ev)
	}
	if note := discMasterNote(hdr.ELFEL, &ProbeEvidence{Status: ProbeMeasured, Tier: hdr.TierDVP7MEL}); note == "" {
		t.Error("a MEL remux of an FEL disc carries no different-master note")
	}
}
