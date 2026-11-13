// Package household owns member-safe catalogue and durable household requests.
package household

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/availability"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type Service struct {
	DB          *sqlite.DB
	Library     *library.Service
	Acquisition *acquisition.Service
	admission   ports.WorkAdmission
	wake        chan struct{}
}

func New(db *sqlite.DB, lib *library.Service, acq *acquisition.Service) *Service {
	return &Service{DB: db, Library: lib, Acquisition: acq, wake: make(chan struct{}, 1)}
}

type SeasonChoice struct {
	Number       int `json:"number"`
	Aired        int `json:"aired"`
	UnknownDates int `json:"unknownDates"`
}
type Title struct {
	Kind         domain.MediaKind    `json:"kind"`
	TMDBID       int64               `json:"tmdbId"`
	Title        string              `json:"title"`
	Year         int                 `json:"year"`
	Overview     string              `json:"overview"`
	PosterPath   string              `json:"posterPath"`
	ReleaseDate  string              `json:"releaseDate,omitempty"`
	Seasons      []SeasonChoice      `json:"seasons"`
	Availability domain.Availability `json:"availability"`
}

func validKind(kind domain.MediaKind) bool {
	return kind == domain.KindMovie || kind == domain.KindSeries
}
func knownAired(date string, now time.Time) bool {
	day, err := time.Parse("2006-01-02", date)
	return err == nil && !day.After(now.UTC())
}
func (s *Service) providerTitle(ctx context.Context, kind domain.MediaKind, tmdb int64) (domain.MediaItem, error) {
	if s == nil || s.DB == nil || s.Library == nil || !validKind(kind) || tmdb <= 0 {
		return domain.MediaItem{}, fmt.Errorf("invalid or unavailable household title")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	preview, err := s.Library.PreviewMetadata(ctx, kind, domain.ExternalRef{Provider: "tmdb", Value: strconv.FormatInt(tmdb, 10)})
	if err != nil {
		return domain.MediaItem{}, err
	}
	if preview.Item.Kind != kind || preview.Item.IDs.TMDB != tmdb {
		return domain.MediaItem{}, fmt.Errorf("title identity changed")
	}
	return preview.Item, nil
}
func (s *Service) Title(ctx context.Context, kind domain.MediaKind, tmdb int64) (Title, error) {
	item, err := s.providerTitle(ctx, kind, tmdb)
	if err != nil {
		return Title{}, err
	}
	return s.safeTitle(ctx, item), nil
}
func (s *Service) safeTitle(ctx context.Context, item domain.MediaItem) Title {
	out := Title{Kind: item.Kind, TMDBID: item.IDs.TMDB, Title: item.Title, Year: item.Year, Overview: item.Overview, PosterPath: item.PosterPath, ReleaseDate: item.ReleaseDate, Seasons: []SeasonChoice{}}
	out.Availability = availability.New(s.DB).Lookup(ctx, []domain.AvailabilityTarget{{Kind: item.Kind, IDs: item.IDs}})[0]
	// Native library IDs are not links into an owner library endpoint for members.
	out.Availability.LibraryItemID = 0
	for _, season := range item.Seasons {
		if season.Number <= 0 {
			continue
		}
		choice := SeasonChoice{Number: season.Number}
		for _, ep := range season.Episodes {
			if ep.EpisodeNumber <= 0 {
				continue
			}
			if knownAired(ep.AirDate, time.Now()) {
				choice.Aired++
			} else if _, err := time.Parse("2006-01-02", ep.AirDate); err != nil {
				choice.UnknownDates++
			}
		}
		if choice.Aired > 0 || choice.UnknownDates > 0 {
			out.Seasons = append(out.Seasons, choice)
		}
	}
	sort.Slice(out.Seasons, func(i, j int) bool { return out.Seasons[i].Number < out.Seasons[j].Number })
	return out
}
func (s *Service) Search(ctx context.Context, kind domain.MediaKind, q string) ([]Title, error) {
	q = strings.TrimSpace(q)
	if !validKind(kind) || len(q) < 1 || len(q) > 200 {
		return nil, fmt.Errorf("enter a movie or show title of at most 200 characters")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	results, err := s.Library.Search(ctx, kind, q)
	if err != nil {
		return nil, err
	}
	out := []Title{}
	targets := []domain.AvailabilityTarget{}
	for _, v := range results {
		if v.Kind != kind || v.TMDBID <= 0 {
			continue
		}
		out = append(out, Title{Kind: v.Kind, TMDBID: v.TMDBID, Title: v.Title, Year: v.Year, Overview: v.Overview, PosterPath: v.PosterPath, Seasons: []SeasonChoice{}})
		targets = append(targets, domain.AvailabilityTarget{Kind: v.Kind, IDs: domain.ExternalIDs{TMDB: v.TMDBID, TVDB: v.TVDBID, IMDB: v.IMDBID}})
		if len(out) == 30 {
			break
		}
	}
	facts := availability.New(s.DB).Lookup(ctx, targets)
	for i, v := range facts {
		v.LibraryItemID = 0
		out[i].Availability = v
	}
	return out, nil
}
func (s *Service) Submit(ctx context.Context, a domain.Account, digest string, kind domain.MediaKind, tmdb int64, seasons []int) (domain.HouseholdRequest, error) {
	item, err := s.providerTitle(ctx, kind, tmdb)
	if err != nil {
		return domain.HouseholdRequest{}, err
	}
	request := domain.HouseholdRequest{Kind: kind, TMDBID: tmdb, IDs: item.IDs, Title: item.Title, Year: item.Year, PosterPath: item.PosterPath, Overview: item.Overview}
	if kind == domain.KindMovie {
		if len(seasons) != 0 {
			return request, fmt.Errorf("movies do not have a season scope")
		}
	} else {
		if len(seasons) == 0 || len(seasons) > 20 {
			return request, fmt.Errorf("choose at least one regular season")
		}
		selected := map[int]bool{}
		for _, n := range seasons {
			if n <= 0 || selected[n] {
				return request, fmt.Errorf("choose distinct regular seasons")
			}
			selected[n] = true
		}
		seenEpisodes := map[domain.RequestEpisode]bool{}
		for _, season := range item.Seasons {
			if !selected[season.Number] {
				continue
			}
			delete(selected, season.Number)
			for _, ep := range season.Episodes {
				if ep.EpisodeNumber > 0 && knownAired(ep.AirDate, time.Now()) {
					coordinate := domain.RequestEpisode{Season: season.Number, Episode: ep.EpisodeNumber}
					if !seenEpisodes[coordinate] {
						request.Scope.Episodes = append(request.Scope.Episodes, coordinate)
						seenEpisodes[coordinate] = true
					}
				}
			}
		}
		if len(selected) > 0 || len(request.Scope.Episodes) == 0 || len(request.Scope.Episodes) > 1000 {
			return request, fmt.Errorf("selected seasons need a bounded known-aired episode inventory")
		}
		sort.Slice(request.Scope.Episodes, func(i, j int) bool {
			a, b := request.Scope.Episodes[i], request.Scope.Episodes[j]
			if a.Season != b.Season {
				return a.Season < b.Season
			}
			return a.Episode < b.Episode
		})
	}
	data, err := json.Marshal(struct {
		Kind  domain.MediaKind
		TMDB  int64
		Scope domain.RequestScope
	}{kind, tmdb, request.Scope})
	if err != nil {
		return request, err
	}
	sum := sha256.Sum256(data)
	request.Fingerprint = hex.EncodeToString(sum[:])
	id, err := s.DB.SubmitHouseholdRequest(ctx, a, digest, request)
	if err != nil {
		return request, err
	}
	// Submission returns the shared receipt; private owner notes are available
	// only through the explicitly owner-authorized request read endpoints.
	return s.DB.HouseholdRequest(ctx, id, a.ID, false)
}
