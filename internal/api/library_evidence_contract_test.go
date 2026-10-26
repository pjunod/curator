package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/format"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

func TestLibraryDetailRetainsProviderEvidenceAndFileExplanations(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	id, err := e.db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Evidence Show", IDs: domain.ExternalIDs{TMDB: 100, TVDB: 200}, Runtime: 120})
	if err != nil {
		t.Fatal(err)
	}
	fetched := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	attempted, retry := fetched.Add(time.Minute), fetched.Add(2*time.Hour)
	aliases := []domain.TitleAlias{{Title: "Alternative", SourceID: "100", Language: "fr", MarketCountry: "CA", Scope: "work", Role: "alternate", Searchable: true}}
	countries := []domain.CountryEvidence{{Code: "CA", Source: "tmdb", Basis: "origin"}}
	if err := e.db.ReplaceIdentitySnapshot(ctx, id, "tmdb", aliases, countries, fetched); err != nil {
		t.Fatal(err)
	}
	if err := e.db.RecordIdentityFailure(ctx, id, "tmdb", attempted, retry, "rate limited"); err != nil {
		t.Fatal(err)
	}
	copyID, err := e.db.AddMediaCopy(ctx, domain.MediaCopy{MediaItemID: id, QualityProfileID: 1, Name: "Alternate"})
	if err != nil {
		t.Fatal(err)
	}
	for i, duration := range []int64{1000, 120 * 60000} {
		fileID, err := e.db.UpsertFile(ctx, id, copyID, filepath.Join(e.root, fmt.Sprintf("evidence-%d.mkv", i)), 9_000_000_000)
		if err != nil {
			t.Fatal(err)
		}
		info := mediainfo.Info{Container: "matroska", Video: &mediainfo.VideoInfo{Codec: "h264", Width: 1920, Height: 1080}, Audio: []mediainfo.AudioInfo{{Codec: "aac", Channels: 2, Language: "eng"}}, DurationMS: duration, BitrateKbps: 10000, SizeBytes: 9_000_000_000}
		if err := e.db.SetFileMediaInfo(ctx, fileID, info, mediainfo.ProvenanceImplausible, mediainfo.ConfidenceNone, fetched); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.W.ExecContext(ctx, "UPDATE media_files SET source_indexer_id=7 WHERE id=?", fileID); err != nil {
			t.Fatal(err)
		}
	}
	rr := do(t, e.h, http.MethodGet, fmt.Sprintf("/api/v1/library/%d", id), "")
	var got struct {
		IDs struct {
			TVDB int64 `json:"tvdb"`
		} `json:"ids"`
		Aliases   []domain.TitleAlias           `json:"aliases"`
		Countries []domain.CountryEvidence      `json:"countries"`
		Sources   []domain.IdentitySourceStatus `json:"identitySources"`
		Files     []struct {
			CopyID      int64  `json:"copyId"`
			IndexerID   int64  `json:"sourceIndexerId"`
			Implausible string `json:"implausible"`
			Facts       string `json:"facts"`
		} `json:"files"`
	}
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &got) != nil {
		t.Fatalf("detail: %d %s", rr.Code, rr.Body.String())
	}
	if got.IDs.TVDB != 200 || len(got.Aliases) != 1 || got.Aliases[0].Language != "fr" || got.Aliases[0].MarketCountry != "CA" || got.Aliases[0].Scope != "work" || !got.Aliases[0].Searchable {
		t.Fatalf("identity evidence lost: %+v", got)
	}
	if !reflect.DeepEqual(got.Countries, countries) || len(got.Sources) != 1 || !reflect.DeepEqual(got.Sources[0].Countries, countries) || !got.Sources[0].FetchedAt.Equal(fetched) || !got.Sources[0].AttemptedAt.Equal(attempted) || !got.Sources[0].RetryAfter.Equal(retry) || got.Sources[0].LastError != "rate limited" {
		t.Fatalf("provider freshness/error evidence lost: %+v", got)
	}
	if len(got.Files) != 2 {
		t.Fatalf("files: %+v", got.Files)
	}
	fallback, current := false, false
	for _, file := range got.Files {
		if file.CopyID != copyID || file.IndexerID != 7 || file.Facts == "" || file.Implausible == "" {
			t.Fatalf("file explanation/source lost: %+v", file)
		}
		if strings.Contains(file.Implausible, "last checked") {
			fallback = true
		} else {
			current = true
		}
	}
	if !fallback || !current {
		t.Fatalf("current and historical explanations must differ: %+v", got.Files)
	}
}

func TestPreviewHTTPReportsBothOwnedBookEditionsWithoutCaching(t *testing.T) {
	e := newAPIEnv(t)
	e.lib.WithBooks(fakeBookProvider{})
	ctx := context.Background()
	work, err := e.lib.Add(ctx, library.AddRequest{Kind: domain.KindBook, OLID: "OL17091839W", BookType: quality.BookTypeEbook})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.lib.Add(ctx, library.AddRequest{Kind: domain.KindBook, OLID: "OL17091839W", BookType: quality.BookTypeAudiobook}); err != nil {
		t.Fatal(err)
	}
	rr := do(t, e.h, http.MethodGet, "/api/v1/metadata/preview?kind=book&olid=OL17091839W", "")
	var got struct {
		Ownership string   `json:"ownership"`
		ItemID    int64    `json:"libraryItemId"`
		BookTypes []string `json:"bookTypes"`
	}
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got.Ownership != "present" || got.ItemID != work.ID || !reflect.DeepEqual(got.BookTypes, []string{"ebook", "audiobook"}) || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("owned preview: %d %s", rr.Code, rr.Body.String())
	}
}

func TestAddHTTPRejectsCrossNamespaceIdentityConflictsWithoutWrites(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	for _, ids := range []domain.ExternalIDs{{TMDB: 100, TVDB: 111}, {TVDB: 200}} {
		if _, err := e.db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Identity fixture", IDs: ids}); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{`{"kind":"series","tmdbId":100,"tvdbId":200}`, `{"kind":"series","tmdbId":999,"tvdbId":111}`} {
		rr := do(t, e.h, http.MethodPost, "/api/v1/library", body)
		var got struct {
			Code string `json:"code"`
		}
		if rr.Code != 409 || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got.Code != "identity_conflict" {
			t.Fatalf("identity fence: %d %s", rr.Code, rr.Body.String())
		}
	}
	rows, err := e.db.ListMediaItems(ctx, domain.KindSeries)
	if err != nil || len(rows) != 2 {
		t.Fatalf("conflict created a work: %+v %v", rows, err)
	}
}

type recordingSemanticModel struct {
	recommendationAPIModel
	changes []bool
}

func (m *recordingSemanticModel) Enable(value bool) { m.changes = append(m.changes, value) }

type rejectingSettings struct {
	SettingsStore
	key string
}

func (s rejectingSettings) SetMeta(ctx context.Context, key, value string) error {
	if key == s.key {
		return errors.New("settings unavailable")
	}
	return s.SettingsStore.SetMeta(ctx, key, value)
}

func TestSettingsPersistencePrecedesLiveSemanticModelChanges(t *testing.T) {
	e := newAPIEnv(t)
	model := &recordingSemanticModel{}
	e.srv.deps.RecommendationModel = model
	for _, enabled := range []bool{true, false} {
		rr := do(t, e.h, http.MethodPut, "/api/v1/settings", fmt.Sprintf(`{"semanticRankingEnabled":%t}`, enabled))
		stored, err := e.db.GetMeta(context.Background(), "discovery_semantic_enabled")
		if rr.Code != 204 || err != nil || stored != fmt.Sprint(enabled) {
			t.Fatalf("setting/live model diverged: %d %s %v", rr.Code, stored, err)
		}
	}
	if !reflect.DeepEqual(model.changes, []bool{true, false}) {
		t.Fatalf("live transitions: %+v", model.changes)
	}
	e.srv.deps.Settings = rejectingSettings{SettingsStore: e.db, key: "discovery_semantic_enabled"}
	rr := do(t, e.h, http.MethodPut, "/api/v1/settings", `{"semanticRankingEnabled":true}`)
	if rr.Code != 500 || !reflect.DeepEqual(model.changes, []bool{true, false}) {
		t.Fatalf("rejected write toggled model: %d %+v", rr.Code, model.changes)
	}
	for key, field := range map[string]string{TMDBKeySetting: "tmdbApiKey", OMDBKeySetting: "omdbApiKey", TraktClientIDSetting: "traktClientId", library.SkipPatternsSetting: "scanSkipPatterns"} {
		e.srv.deps.Settings = rejectingSettings{SettingsStore: e.db, key: key}
		if rr := do(t, e.h, http.MethodPut, "/api/v1/settings", fmt.Sprintf(`{"%s":"fixture"}`, field)); rr.Code != 500 {
			t.Fatalf("rejected %s write returned %d", key, rr.Code)
		}
	}
}

func TestImportListExplicitPolicySurvivesRoundTrip(t *testing.T) {
	e := newAPIEnv(t)
	root, err := e.lib.AddRootFolder(context.Background(), t.TempDir(), domain.RootKindOf(domain.KindSeries))
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"name":"Opted out","type":"tmdb-popular","kind":"series","config":{"key":"value"},"rootFolderId":%d,"qualityProfileId":2,"monitored":false,"enabled":false}`, root.ID)
	rr := do(t, e.h, http.MethodPost, "/api/v1/importlists", body)
	if rr.Code != 201 {
		t.Fatalf("import list: %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	rows, err := e.db.ListImportLists(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ID != created.ID || rows[0].Kind != "series" || rows[0].RootFolderID != root.ID || rows[0].QualityProfileID != 2 || rows[0].Monitored || rows[0].Enabled || rows[0].Config["key"] != "value" {
		t.Fatalf("explicit list policy was replaced by defaults: %+v %v", rows, err)
	}
}

func TestReviewHTTPPreservesSharedFolderAndTVDBWarnings(t *testing.T) {
	e := newAPIEnv(t)
	proposal := library.Proposal{RootFolderID: 1, Path: "/media/review/Show", Name: "Show", Kind: domain.KindSeries, Confidence: library.Confidence("ambiguous"), HeldBy: "/media/library/Other", SharedWith: []string{"Other title"}, Candidates: []ports.SearchResult{{Kind: domain.KindSeries, TVDBID: 200, Title: "Show", Source: "tvmaze", AltTitles: []string{"Regional title"}}}}
	raw, err := json.Marshal([]library.Proposal{proposal})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SetMeta(context.Background(), "adoption_review_queue", string(raw)); err != nil {
		t.Fatal(err)
	}
	rr := do(t, e.h, http.MethodGet, "/api/v1/library/review", "")
	var got struct {
		Items []struct {
			HeldBy     string   `json:"heldBy"`
			SharedWith []string `json:"sharedWith"`
			Candidates []struct {
				TVDB      int64    `json:"tvdbId"`
				Source    string   `json:"source"`
				AltTitles []string `json:"altTitles"`
			} `json:"candidates"`
		} `json:"items"`
	}
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &got) != nil || len(got.Items) != 1 {
		t.Fatalf("review: %d %s", rr.Code, rr.Body.String())
	}
	p := got.Items[0]
	if p.HeldBy != proposal.HeldBy || !reflect.DeepEqual(p.SharedWith, proposal.SharedWith) || len(p.Candidates) != 1 || p.Candidates[0].TVDB != 200 || p.Candidates[0].Source != "tvmaze" || !reflect.DeepEqual(p.Candidates[0].AltTitles, proposal.Candidates[0].AltTitles) {
		t.Fatalf("review erased decision warnings: %+v", p)
	}
}

func TestLibraryHTTPDistinguishesDisjointAndSharedDeclaredAudio(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			e := newAPIEnv(t)
			ctx := context.Background()
			id, err := e.db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Measured audio", Runtime: 120})
			if err != nil {
				t.Fatal(err)
			}
			languages := []string{"eng"}
			want := []string{"en"}
			if !shared {
				languages = append(languages, "fra")
				want = []string{}
			}
			for i, lang := range languages {
				file, err := e.db.UpsertFile(ctx, id, 0, filepath.Join(e.root, fmt.Sprintf("measured-%d.mkv", i)), 9_000_000_000)
				if err != nil {
					t.Fatal(err)
				}
				info := mediainfo.Info{Container: "matroska", Video: &mediainfo.VideoInfo{Codec: "h264", Width: 1920, Height: 1080}, DurationMS: 120 * 60000, BitrateKbps: 10000, Audio: []mediainfo.AudioInfo{{Codec: "aac", Language: lang, Channels: 2}}}
				if err := e.db.SetFileQualityFrom(ctx, file, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, mediainfo.ProvenanceProbe, mediainfo.ConfidenceHigh); err != nil {
					t.Fatal(err)
				}
				if err := e.db.SetFileMediaInfo(ctx, file, info, mediainfo.ProvenanceProbe, mediainfo.ConfidenceHigh, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			rr := do(t, e.h, http.MethodGet, fmt.Sprintf("/api/v1/library/%d", id), "")
			var got struct {
				AudioLanguages *[]string `json:"audioLanguages"`
			}
			if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got.AudioLanguages == nil || !reflect.DeepEqual(*got.AudioLanguages, want) {
				t.Fatalf("known common audio rendered as unknown: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}

type contractSearchProvider struct{ stubProvider }

func (p contractSearchProvider) GetSeries(context.Context, int64) (domain.MediaItem, error) {
	return domain.MediaItem{Kind: domain.KindSeries, Title: "Test Show", Year: 2020, Runtime: 42, IDs: domain.ExternalIDs{TMDB: 100}, Seasons: []domain.Season{{Number: 1, Monitored: true, Episodes: []domain.Episode{{SeasonNumber: 1, EpisodeNumber: 2, Title: "Second", Monitored: true}}}}}, nil
}

type contractSearchIndexer struct {
	fakeIndexer
	queries chan domain.SearchQuery
}

func (f contractSearchIndexer) Search(_ context.Context, q domain.SearchQuery) ([]ports.Release, error) {
	f.queries <- q
	if strings.Contains(f.cfg.Name, "Broken") {
		return nil, errors.New("provider offline")
	}
	return []ports.Release{{Title: "Test.Show.S01E02.1080p.WEB-DL.FRENCH-GROUP", DownloadURL: "https://indexer.invalid/result", InfoURL: "https://indexer.invalid/details", Protocol: "usenet", Indexer: f.cfg.Name, IndexerID: f.cfg.ID, Size: 10_000_000}}, nil
}

func TestPartialReleaseSearchKeepsEvidenceAndSanitizesHeaders(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	svc := library.New(e.db, contractSearchProvider{}, e.bus, nil)
	item, err := svc.Add(ctx, library.AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	queries := make(chan domain.SearchQuery, 20)
	acq := acquisition.New(e.db, e.bus, nil, func(cfg ports.IndexerConfig) ports.Indexer {
		return contractSearchIndexer{fakeIndexer: fakeIndexer{cfg: cfg}, queries: queries}
	}, func(cfg ports.ClientConfig) ports.DownloadClient { return fakeClient{cfg: cfg} })
	e.srv.deps.Acquisition = acq
	for _, name := range []string{"Working", "Broken\r\nX-Injected: yes"} {
		if _, err := e.db.AddIndexer(ctx, ports.IndexerConfig{Name: name, Protocol: "usenet", URL: "https://indexer.invalid", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.db.AddCustomFormat(ctx, format.CustomFormat{Name: "Preferred group", Pattern: "GROUP", Score: 25}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.AddBlocklist(ctx, item.ID, "Test.Show.S01E02.1080p.WEB-DL.FRENCH-GROUP", "Working", "known bad source"); err != nil {
		t.Fatal(err)
	}
	rr := do(t, e.h, http.MethodGet, fmt.Sprintf("/api/v1/library/%d/releases?season=1&episode=2", item.ID), "")
	var got []struct {
		Title     string   `json:"title"`
		Warning   string   `json:"warning"`
		Formats   []string `json:"formats"`
		Languages []string `json:"languages"`
		Blacklist string   `json:"blacklistReason"`
		InfoURL   string   `json:"infoUrl"`
		Score     int      `json:"score"`
	}
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &got) != nil || len(got) != 1 {
		t.Fatalf("partial candidates lost: %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Monarr-Search-Partial") != "true" || strings.ContainsAny(rr.Header().Get("X-Monarr-Search-Reason"), "\r\n") || rr.Header().Get("X-Injected") != "" {
		t.Fatalf("unsafe/incomplete search headers: %+v", rr.Header())
	}
	c := got[0]
	if c.Warning == "" || c.Blacklist != "known bad source" || c.InfoURL == "" || c.Score != 25 || !reflect.DeepEqual(c.Formats, []string{"Preferred group"}) || !reflect.DeepEqual(c.Languages, []string{"fr"}) {
		t.Fatalf("candidate evidence lost: %+v", c)
	}
	if len(queries) != 2 {
		t.Fatalf("searches issued: %d", len(queries))
	}
	for len(queries) > 0 {
		q := <-queries
		if q.Season != 1 || q.Episode != 2 {
			t.Fatalf("episode scope lost: %+v", q)
		}
	}
}
