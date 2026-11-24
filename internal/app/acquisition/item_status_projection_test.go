package acquisition

import (
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Projection cases, built from facts directly: every state the panel can be
// asked to describe, without a database.

var projNow = time.Date(2026, 11, 11, 18, 30, 0, 0, time.UTC)

const moviePath = "/lib/Movie (2024)/Movie (2024).mkv"

func movieFile() domain.MediaFile {
	return domain.MediaFile{ID: 1, Path: moviePath, Size: 10, AddedAt: projNow.Add(-48 * time.Hour)}
}

func projMovie(kind domain.MediaKind, files []domain.MediaFile, dls ...sqlite.Download) statusFacts {
	return statusFacts{item: domain.MediaItem{ID: 1, Kind: kind, Files: files}, now: projNow, downloads: dls}
}

func visible(path string, at time.Time) sqlite.Delivery {
	return sqlite.Delivery{ID: 9, NotifierID: 3, Status: "ok", RemoteRequestID: "r", VisibleAt: at.UnixMilli(), ItemIDs: []int64{77},
		ExpectedFiles: []ports.ExpectedFile{{Path: path}}, FileResults: []sqlite.DeliveryFileResult{{Path: path, State: "placed", ItemID: 77, Matched: true}}}
}

func dl(id int64, state string) sqlite.Download {
	return sqlite.Download{ID: id, ReleaseTitle: "Movie.2024.1080p", Indexer: "idx", State: state, Size: 1000,
		AddedAt: projNow.Add(-time.Hour), UpdatedAt: projNow.Add(-10 * time.Second), StateEnteredAt: projNow.Add(-30 * time.Minute)}
}

func states(st ItemStatus) string {
	var b []string
	for _, s := range st.Stages {
		b = append(b, s.State)
	}
	return strings.Join(b, ",")
}

func expect(t *testing.T, st ItemStatus, want string) {
	t.Helper()
	if got := states(st); got != want {
		t.Fatalf("stages = %s, want %s\n%+v", got, want, st.Stages)
	}
}

func TestProjectionFailedDownloads(t *testing.T) {
	t.Run("a failed grab with nothing on disk fails the stage it failed in", func(t *testing.T) {
		d := dl(5, "failed")
		d.RunnerOutcome, d.RunnerOutcomeReason, d.RunnerOutcomeAt = ports.OutcomeFailed, ports.ReasonHealth, projNow
		st := projMovie(domain.KindMovie, nil, d).project(-1)
		expect(t, st, "done,failed,todo,todo,todo,todo")
		if st.Headline.Title != "Downloading failed" || !hasAction(st.Stages[1], ActionSearch) || st.Download.RateBps != 0 || st.Active {
			t.Fatalf("headline = %+v download = %+v", st.Headline, st.Download)
		}
	})
	t.Run("a par failure fails Processed only, even with no stage relayed", func(t *testing.T) {
		d := dl(5, "failed")
		d.RunnerOutcome, d.RunnerOutcomeReason, d.RunnerOutcomeAt = ports.OutcomeFailed, ports.ReasonPar, projNow
		expect(t, projMovie(domain.KindMovie, nil, d).project(-1), "done,done,failed,todo,todo,todo")
	})
	t.Run("a cancelled grab", func(t *testing.T) {
		d := dl(5, "failed")
		d.RunnerOutcome, d.Error = ports.OutcomeCancelled, "cancelled in Curator by the operator"
		st := projMovie(domain.KindMovie, nil, d).project(-1)
		expect(t, st, "done,failed,todo,todo,todo,todo")
		if !strings.Contains(st.Stages[1].Detail, "cancelled") {
			t.Fatalf("detail = %q", st.Stages[1].Detail)
		}
	})
	t.Run("an item already in cinema is not undone by a failed upgrade grab", func(t *testing.T) {
		d := dl(5, "failed")
		d.RunnerOutcome, d.RunnerOutcomeReason = ports.OutcomeFailed, ports.ReasonPar
		f := projMovie(domain.KindMovie, []domain.MediaFile{movieFile()}, d)
		f.deliveries = []sqlite.Delivery{visible(moviePath, projNow.Add(-24*time.Hour))}
		st := f.project(-1)
		expect(t, st, "done,done,done,done,done,done")
		if !st.Done || len(st.Attempts) != 1 || st.Attempts[0].DownloadID != 5 || st.Attempts[0].Outcome != ports.OutcomeFailed {
			t.Fatalf("done=%v attempts=%+v, want the failed grab listed as an earlier attempt", st.Done, st.Attempts)
		}
		if st.Headline.Title != "In cinema, confirmed" || st.Headline.At == 0 {
			t.Fatalf("headline = %+v, want the title without a time and the time in At", st.Headline)
		}
	})
	t.Run("an import failure is Moved, not Processed, though Runner's last stage lingers", func(t *testing.T) {
		d := dl(5, "failed")
		d.RunnerOutcome, d.RunnerStage, d.Error = ports.OutcomeCompleted, "unpack", "no space left on device"
		st := projMovie(domain.KindMovie, nil, d).project(-1)
		expect(t, st, "done,done,done,failed,todo,todo")
		if !hasAction(st.Stages[3], ActionRetryImport) {
			t.Fatalf("moved = %+v, want Retry import", st.Stages[3])
		}
	})
}

func TestProjectionHoldsAndProgress(t *testing.T) {
	t.Run("a hold during post-processing shows on Processed", func(t *testing.T) {
		d := dl(5, "held")
		d.RunnerStage, d.Error = "par_repair", "disk full"
		st := projMovie(domain.KindMovie, nil, d).project(-1)
		expect(t, st, "done,done,waiting,todo,todo,todo")
		if st.Headline.Title != "Held by Runner" || !strings.Contains(st.Stages[2].Detail, "disk full") {
			t.Fatalf("headline = %+v processed = %+v", st.Headline, st.Stages[2])
		}
	})
	t.Run("a hold while fetching shows on Downloading", func(t *testing.T) {
		expect(t, projMovie(domain.KindMovie, nil, dl(5, "held")).project(-1), "done,waiting,todo,todo,todo,todo")
	})
	t.Run("planned says it is being sent", func(t *testing.T) {
		st := projMovie(domain.KindMovie, nil, dl(5, "planned")).project(-1)
		if st.Headline.Title != "Grabbed, sending to runner" || st.Stages[1].Detail != "grabbed, sending to runner" {
			t.Fatalf("headline = %+v", st.Headline)
		}
	})
	t.Run("a stale rate is not shown", func(t *testing.T) {
		d := dl(5, "downloading")
		d.Progress, d.RateBps, d.RemainingBytes, d.UpdatedAt = 0.5, 10, 500, projNow.Add(-10*time.Minute)
		st := projMovie(domain.KindMovie, nil, d).project(-1)
		if st.Download.RateBps != 0 || st.Download.ETASeconds != 0 || strings.Contains(st.Stages[1].Detail, "left") || strings.Contains(st.Headline.Title, "left") {
			t.Fatalf("download = %+v stage = %+v headline = %+v, want no rate or ETA from stale numbers", st.Download, st.Stages[1], st.Headline)
		}
		d.UpdatedAt = projNow.Add(-5 * time.Second)
		if st := projMovie(domain.KindMovie, nil, d).project(-1); st.Download.ETASeconds != 50 {
			t.Fatalf("fresh eta = %d, want 50", st.Download.ETASeconds)
		}
	})
	t.Run("a replacement over an existing file follows the new download", func(t *testing.T) {
		d := dl(5, "downloading")
		d.Progress = 0.4
		f := projMovie(domain.KindMovie, []domain.MediaFile{movieFile()}, d)
		f.deliveries = []sqlite.Delivery{visible(moviePath, projNow.Add(-24*time.Hour))}
		st := f.project(-1)
		expect(t, st, "done,active,todo,todo,todo,todo")
		if !st.Active || st.Done {
			t.Fatalf("active=%v done=%v", st.Active, st.Done)
		}
	})
	t.Run("notified is imported", func(t *testing.T) {
		d := dl(5, "notified")
		d.RunnerOutcome = ports.OutcomeCompleted
		st := projMovie(domain.KindMovie, []domain.MediaFile{movieFile()}, d).project(-1)
		if st.Stages[3].State != StatusDone {
			t.Fatalf("moved = %+v", st.Stages[3])
		}
	})
	t.Run("an additional copy's download does not drive the primary track", func(t *testing.T) {
		d := dl(5, "downloading")
		d.CopyID, d.Progress = 2, 0.5
		f := projMovie(domain.KindMovie, []domain.MediaFile{movieFile()}, d)
		f.deliveries = []sqlite.Delivery{visible(moviePath, projNow)}
		st := f.project(-1)
		if !st.Done || st.Download != nil || st.Active {
			t.Fatalf("primary track = %s download = %+v active = %v", states(st), st.Download, st.Active)
		}
	})
}

func TestProjectionSearchesAndCinema(t *testing.T) {
	t.Run("an empty upgrade search does not undo an item on disk", func(t *testing.T) {
		f := projMovie(domain.KindMovie, []domain.MediaFile{movieFile()})
		f.deliveries = []sqlite.Delivery{visible(moviePath, projNow.Add(-24*time.Hour))}
		f.searches = []sqlite.SearchRecord{{At: projNow, Trigger: "automatic", Seen: 11}}
		st := f.project(-1)
		if !st.Done || !strings.Contains(st.Stages[0].Detail, "latest search automatic · 11 seen, 0 accepted") {
			t.Fatalf("searched = %+v done = %v", st.Stages[0], st.Done)
		}
		f.searches = []sqlite.SearchRecord{{At: projNow, Trigger: "automatic", Skipped: SkipInFlight}}
		if st := f.project(-1); !st.Done || st.Headline.Title == "Search skipped" {
			t.Fatalf("skipped search on disk = %+v", st.Headline)
		}
	})
	t.Run("another file's read-back does not confirm this one", func(t *testing.T) {
		f := projMovie(domain.KindMovie, []domain.MediaFile{movieFile()})
		other := visible("/lib/Movie (2024)/other.mkv", projNow)
		other.ExpectedFiles = append(other.ExpectedFiles, ports.ExpectedFile{Path: moviePath})
		f.deliveries = []sqlite.Delivery{other}
		if st := f.project(-1); st.Stages[5].State == StatusDone {
			t.Fatalf("in cinema = %+v, want not confirmed", st.Stages[5])
		}
	})
	t.Run("a cinema that reports no per-file results confirms by visibility", func(t *testing.T) {
		f := projMovie(domain.KindMovie, []domain.MediaFile{movieFile()})
		d := visible(moviePath, projNow)
		d.FileResults = nil
		f.deliveries = []sqlite.Delivery{d}
		if st := f.project(-1); !st.Done {
			t.Fatalf("stages = %s", states(st))
		}
	})
	t.Run("a book with a file and no record offers Ask cinema to confirm", func(t *testing.T) {
		st := projMovie(domain.KindBook, []domain.MediaFile{movieFile()}).project(-1)
		expect(t, st, "done,done,done,done,todo,todo")
		if !hasAction(st.Stages[5], ActionConfirmCinema) {
			t.Fatalf("in cinema = %+v", st.Stages[5])
		}
	})
}

func projSeries() statusFacts {
	eps := []domain.Episode{
		{ID: 11, SeasonNumber: 1, EpisodeNumber: 1, Monitored: true, AirDate: "2026-01-01"},
		{ID: 12, SeasonNumber: 1, EpisodeNumber: 2, Monitored: true, AirDate: "2026-01-08"},
		{ID: 13, SeasonNumber: 1, EpisodeNumber: 3, Monitored: true, AirDate: "2027-01-01"}, // not aired
		{ID: 14, SeasonNumber: 1, EpisodeNumber: 4, Monitored: false, AirDate: "2026-01-15"},
	}
	return statusFacts{now: projNow, item: domain.MediaItem{ID: 1, Kind: domain.KindSeries, Seasons: []domain.Season{
		{Number: 1, Monitored: true, Episodes: eps},
		{Number: 2, Monitored: true, Episodes: []domain.Episode{{ID: 21, SeasonNumber: 2, EpisodeNumber: 1, Monitored: true, AirDate: "2026-06-01"}}},
	}}}
}

func TestProjectionSeries(t *testing.T) {
	t.Run("a file covering two episodes puts both in cinema; the summary counts monitored aired episodes", func(t *testing.T) {
		f := projSeries()
		path := "/lib/Show/Season 01/Show - S01E01-E02.mkv"
		f.item.Files = []domain.MediaFile{{ID: 1, Path: path, EpisodeIDs: []int64{11, 12}, AddedAt: projNow.Add(-time.Hour)}}
		f.deliveries = []sqlite.Delivery{visible(path, projNow)}
		st := f.project(1)
		if len(st.Episodes) != 4 || !allDone(st.Episodes[0].Stages) || !allDone(st.Episodes[1].Stages) {
			t.Fatalf("episodes = %+v", st.Episodes)
		}
		if st.Episodes[0].Cinema == nil || st.Episodes[0].Cinema.DeliveryID != 9 || st.Episodes[0].WantableID != "episode:1:1:1" {
			t.Fatalf("episode actions target = %+v", st.Episodes[0])
		}
		if s := st.Seasons; len(s) != 1 || s[0].Episodes != 2 || s[0].InCinema != 2 || st.Headline.State != StatusDone {
			t.Fatalf("seasons = %+v headline = %+v, want only season 1, 2 of 2 counted and done", s, st.Headline)
		}
	})
	t.Run("an episode's failed import names its download for Retry import", func(t *testing.T) {
		f := projSeries()
		d := dl(5, "failed")
		d.RunnerOutcome, d.WantableIDs = ports.OutcomeCompleted, []string{"episode:1:1:2"}
		f.downloads = []sqlite.Download{d}
		st := f.project(1)
		ep := st.Episodes[1]
		if ep.DownloadID != 5 || ep.Stages[3].State != StatusFailed || !hasAction(ep.Stages[3], ActionRetryImport) {
			t.Fatalf("episode 2 = %+v", ep)
		}
	})
	t.Run("the summary needs no searches and leaves episodes out", func(t *testing.T) {
		st := projSeries().project(-1)
		if len(st.Seasons) != 2 || len(st.Episodes) != 0 || st.Seasons[0].Season != 2 {
			t.Fatalf("summary = %+v", st)
		}
	})
}

func TestProjectionOpenRematchBlock(t *testing.T) {
	cin := []RematchCinema{{Directory: "/lib/Dune (1984)", Status: "ok", VisibleAt: 0,
		Warnings: []string{"cinema still has /old/Dune.mkv", "cinema kept the record of /x.mkv: pinned"}}}
	facts := func(itemID int64, state string) statusFacts {
		f := projMovie(domain.KindMovie, []domain.MediaFile{movieFile()})
		f.item.ID = itemID
		f.rematch = &rematchFacts{itemID: itemID, sourceTitle: "Dune (1984)", targetTitle: "Dune (2021)",
			op:    RematchStatus{RematchOperation: sqlite.RematchOperation{ID: "op1", State: state, SourceItemID: 1, TargetItemID: 2, BytesDone: 5, BytesTotal: 10}, Cinema: cin},
			moves: []sqlite.RematchMove{{Source: "/a", Target: "/b", Size: 5, State: "rematch_moved"}, {Source: "/c", Target: "/d", Size: 5, State: "rematch_planned"}}}
		return f
	}
	t.Run("the source side moves out, polls fast and can stop", func(t *testing.T) {
		st := facts(1, "moving").project(-1)
		r := st.Rematch
		if r == nil || r.Role != "source" || r.Headline != "Files moving from here to Dune (2021)" || !st.Active || !r.Stoppable ||
			r.FilesDone != 1 || r.FilesTotal != 2 || r.BytesDone != 5 || len(r.Warnings) != 2 || !strings.HasPrefix(r.CinemaLine, "Cinema is being told.") {
			t.Fatalf("rematch = %+v active=%v", r, st.Active)
		}
	})
	t.Run("the target side says where the files come from", func(t *testing.T) {
		r := facts(2, "moving").project(-1).Rematch
		if r.Role != "target" || r.Headline != "Files moving here from Dune (1984)" {
			t.Fatalf("rematch = %+v", r)
		}
	})
	t.Run("committed cannot be stopped; no open operation shows nothing", func(t *testing.T) {
		if r := facts(1, "committed").project(-1).Rematch; r.Stoppable {
			t.Fatalf("committed rematch stoppable: %+v", r)
		}
		if r := projMovie(domain.KindMovie, nil).project(-1).Rematch; r != nil {
			t.Fatalf("rematch = %+v, want none", r)
		}
	})
}
