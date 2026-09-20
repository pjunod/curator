package acquisition

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type packThenEpisodeIndexer struct {
	mu    sync.Mutex
	calls int
}

func (i *packThenEpisodeIndexer) Search(context.Context, domain.SearchQuery) ([]ports.Release, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	if i.calls == 1 {
		return []ports.Release{{Title: "Test.Show.S01.1080p.WEB-DL-PACK", DownloadURL: "pack", Indexer: "idx", Protocol: "torrent"}}, nil
	}
	return []ports.Release{{Title: "Test.Show.S01E01.1080p.WEB-DL-GRP", DownloadURL: "episode", Indexer: "idx", Protocol: "torrent"}}, nil
}

func (i *packThenEpisodeIndexer) FetchRSS(context.Context) ([]ports.Release, error) { return nil, nil }
func (i *packThenEpisodeIndexer) Test(context.Context) error                        { return nil }

func wantedQuality() *quality.Quality {
	q := quality.Quality{Source: quality.SourceHDTV, Resolution: 720}
	return &q
}

func TestAcquisitionReservationsSerializeAnItemCopyAndHonorCancellation(t *testing.T) {
	var reservations acquisitionReservations
	primary := domain.EpisodeWantable{Item: 5, Season: 2, Episode: 7}
	season := domain.SeasonWantable{Item: 5, Season: 2}
	secondary := domain.EpisodeWantable{Item: 5, Season: 2, Episode: 7, Copy: 3}
	release, err := reservations.acquire(context.Background(), primary)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := reservations.acquire(ctx, season); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-copy season wait = %v, want deadline", err)
	}
	otherRelease, err := reservations.acquire(context.Background(), secondary)
	if err != nil {
		t.Fatalf("different copy was blocked: %v", err)
	}
	otherRelease()
}

func TestWantableOnRowSeasonCoverageRequiresTheSameCopy(t *testing.T) {
	tests := []struct {
		pack, episode string
		want          bool
	}{
		{"season:5:2", "episode:5:2:7", true},
		{"season:5:2", "episode:5:2:7:c3", false},
		{"season:5:2:c3", "episode:5:2:7:c3", true},
		{"season:5:2:c3", "episode:5:2:7", false},
		{"season:50:2", "episode:5:2:7", false},
	}
	for _, tc := range tests {
		got := wantableOnRow(sqlite.Download{WantableIDs: []string{tc.pack}}, tc.episode)
		if got != tc.want {
			t.Errorf("wantableOnRow(%q, %q) = %v, want %v", tc.pack, tc.episode, got, tc.want)
		}
	}
}

func TestWantedSearchScopeSelectorPreservesExactMembershipAndCopies(t *testing.T) {
	wanted := []domain.Wantable{
		domain.EpisodeWantable{Item: 10, Season: 1, Episode: 2, Mon: true},
		domain.EpisodeWantable{Item: 10, Season: 1, Episode: 10, Mon: true, Have: wantedQuality(), Files: true, Copy: 4},
		domain.MovieWantable{Item: 20, Mon: true},
		domain.BookWantable{Item: 30, Mon: true, Copy: 8},
	}
	tests := []struct {
		name string
		req  WantedSearchRequest
		want []string
	}{
		{"all", WantedSearchRequest{Scope: "all"}, []string{"episode:10:1:2", "episode:10:1:10:c4", "movie:20", "book:30:c8"}},
		{"missing", WantedSearchRequest{Scope: "reason", Reason: WantedMissing}, []string{"episode:10:1:2", "movie:20", "book:30:c8"}},
		{"upgrade", WantedSearchRequest{Scope: "reason", Reason: WantedUpgrade}, []string{"episode:10:1:10:c4"}},
		{"group", WantedSearchRequest{Scope: "group", MediaItemID: 10}, []string{"episode:10:1:2", "episode:10:1:10:c4"}},
		{"group reason", WantedSearchRequest{Scope: "group", MediaItemID: 10, Reason: WantedUpgrade}, []string{"episode:10:1:10:c4"}},
		{"target copy", WantedSearchRequest{Scope: "target", WantableID: "episode:10:1:10:c4"}, []string{"episode:10:1:10:c4"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wantableIDs(selectWantedTargets(wanted, tc.req))
			if len(got) != len(tc.want) {
				t.Fatalf("selected %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("selected %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestWantedSearchRequestRejectsConflictingOrMalformedSelectors(t *testing.T) {
	bad := []WantedSearchRequest{
		{Scope: ""},
		{Scope: "all", Reason: WantedMissing},
		{Scope: "reason", Reason: "new"},
		{Scope: "group", MediaItemID: 0},
		{Scope: "group", MediaItemID: 1, WantableID: "movie:1"},
		{Scope: "target", WantableID: "season:1:2"},
		{Scope: "target", WantableID: "episode:1:2"},
		{Scope: "target", WantableID: "movie:0"},
	}
	for _, req := range bad {
		if err := validateWantedSearch(req); !errors.Is(err, ErrInvalidWantedSearch) {
			t.Errorf("validateWantedSearch(%+v) = %v, want invalid", req, err)
		}
	}
}

func TestWantedExactCandidateRejectsPacksAndMultiEpisodeReleases(t *testing.T) {
	for _, title := range []string{
		"Example.Show.S02.1080p.WEB-DL-GRP",
		"Example.Show.S02E01-E03.1080p.WEB-DL-GRP",
	} {
		if exactWantedCandidate(parser.Parse(title)) {
			t.Errorf("accepted multi-target release %q", title)
		}
	}
	if !exactWantedCandidate(parser.Parse("Example.Show.S02E01.1080p.WEB-DL-GRP")) {
		t.Error("rejected an exact single-episode release")
	}
}

func TestWantedExactSearchDoesNotLetAPackStopALaterEpisodeQuery(t *testing.T) {
	client := &fakeClient{}
	svc, _, _ := seriesSetup(t, client)
	indexer := &packThenEpisodeIndexer{}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return indexer }
	ctx := context.Background()
	wanted, err := svc.Wanted(ctx)
	if err != nil || len(wanted) != 1 {
		t.Fatalf("wanted = %v, %v", wanted, err)
	}
	enabled, err := svc.enabledIndexers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tally, err := svc.searchAndGrabBestWhere(ctx, wanted[0], enabled, exactWantedCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if tally.Grabbed == "" || len(client.added) != 1 || client.added[0] != "episode" {
		t.Fatalf("tally = %+v, grabs = %v; exact episode should win", tally, client.added)
	}
	indexer.mu.Lock()
	calls := indexer.calls
	indexer.mu.Unlock()
	if calls < 2 {
		t.Fatalf("queries = %d; rejected pack incorrectly stopped the planner", calls)
	}
}
