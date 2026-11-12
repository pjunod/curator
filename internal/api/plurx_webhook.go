package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// PlurxWebhook implements POST /api/v1/webhooks/plurx (master plan §11.1).
//
// The only inbound push Monarr accepts. Three properties are deliberate:
//
//   - **Ids only.** The item is resolved by TMDB or IMDb id and never by
//     title. An application guessing which item you meant is the failure this
//     whole integration was built to remove, and accepting a title here would
//     reintroduce it from the other direction.
//   - **Unknown is not an error.** A notification for something Monarr does
//     not have returns 200 with `matched: false`, not a 404 — plurx's outbox
//     retries failures, and there is nothing to retry about a film Monarr was
//     never asked to manage.
//   - **One acting event.** `watched` is recorded, shown, and read by the
//     upgrade ordering, and deletes nothing. The single event that makes
//     Monarr act is `reacquire` (ADR 0028): an admin in plurx asked for an
//     offloaded title back, so Monarr un-offloads the scope and searches.
//     It only ever brings files back; nothing here deletes anything, and
//     Offload itself is started from Monarr's own UI, never from here.
//     `reacquire_status` is its read-only companion.
func (s *Server) PlurxWebhook(w http.ResponseWriter, r *http.Request) {
	var body apigen.PlurxWebhookJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch body.Event {
	case "reacquire", "reacquire_status":
		s.plurxReacquire(w, r, body)
		return
	}
	if body.Event != "" && body.Event != "watched" {
		// Unknown events are accepted and dropped rather than rejected: a
		// newer plurx sending something this build does not understand is
		// not a failure worth making it retry forever.
		writeJSON(w, http.StatusOK, map[string]any{"matched": false})
		return
	}

	kind := domain.KindMovie
	switch body.Kind {
	case apigen.PlurxWatchedEventKindEpisode:
		kind = domain.KindSeries
	case apigen.PlurxWatchedEventKindMovie:
	default:
		// show and season are scopes of reacquire, not things anyone watches.
		writeJSON(w, http.StatusOK, map[string]any{"matched": false})
		return
	}
	tmdb := int64(0)
	if body.Tmdb != nil {
		tmdb = *body.Tmdb
	}
	imdb := ""
	if body.Imdb != nil {
		imdb = *body.Imdb
	}
	if tmdb == 0 && imdb == "" {
		writeError(w, http.StatusBadRequest, "a tmdb or imdb id is required — Monarr matches on ids, never on titles")
		return
	}

	itemID, err := s.deps.Store.FindItemByIDs(r.Context(), kind, tmdb, imdb)
	if err != nil {
		// Not in the library. Say so plainly and do not make plurx retry it.
		s.deps.Log.Debug("plurx webhook: no such item",
			"kind", kind, "tmdb", tmdb, "imdb", imdb)
		writeJSON(w, http.StatusOK, map[string]any{"matched": false})
		return
	}

	watchedAt := time.Now()
	if body.WatchedAt != nil {
		watchedAt = time.Unix(*body.WatchedAt, 0)
	}
	rec := sqlite.PlurxWatch{
		MediaItemID: itemID,
		WatchedAt:   watchedAt,
	}
	if body.User != nil {
		rec.Username = *body.User
	}
	if body.Season != nil {
		rec.Season = int64(*body.Season)
	}
	if body.Episode != nil {
		rec.Episode = int64(*body.Episode)
	}
	if err := s.deps.Store.RecordPlurxWatched(r.Context(), rec); err != nil {
		s.acqErr(w, err)
		return
	}
	s.deps.Log.Info("plurx: watched",
		"item", itemID, "kind", body.Kind, "user", rec.Username,
		"season", rec.Season, "episode", rec.Episode)
	writeJSON(w, http.StatusOK, map[string]any{"matched": true, "mediaItemId": itemID})
}

// plurxReacquire answers reacquire and reacquire_status (contract §4.6, §4.7).
// Ids only: a movie by its tmdb/imdb id; a show, season or episode by the
// SHOW's tmdb/imdb id plus season and episode. It is idempotent: asking for
// what is already back answers where it stands, and never errors.
//
//	{matched, mediaItemId, state}   state: searching | downloading | importing |
//	                                on_disk | not_offloaded | offloading | offloaded | not_found
func (s *Server) plurxReacquire(w http.ResponseWriter, r *http.Request, body apigen.PlurxWatchedEvent) {
	kind, scope := domain.KindSeries, "item"
	switch body.Kind {
	case apigen.PlurxWatchedEventKindMovie:
		kind = domain.KindMovie
	case "show":
	case "season":
		scope = "season"
	case apigen.PlurxWatchedEventKindEpisode:
		scope = "episode"
	default:
		writeError(w, http.StatusBadRequest, "kind must be movie, show, season or episode")
		return
	}
	tmdb, imdb := int64(0), ""
	if body.Tmdb != nil {
		tmdb = *body.Tmdb
	}
	if body.Imdb != nil {
		imdb = *body.Imdb
	}
	if tmdb == 0 && imdb == "" {
		writeError(w, http.StatusBadRequest, "a tmdb or imdb id is required — Monarr matches on ids, never on titles")
		return
	}
	if (scope == "season" || scope == "episode") && body.Season == nil || scope == "episode" && body.Episode == nil {
		writeError(w, http.StatusBadRequest, "season needs a season number and episode needs a season and an episode number")
		return
	}
	notFound := func(id int64) {
		writeJSON(w, http.StatusOK, map[string]any{"matched": false, "mediaItemId": id, "state": "not_found"})
	}
	itemID, err := s.deps.Store.FindItemByIDs(r.Context(), kind, tmdb, imdb)
	if err != nil {
		s.deps.Log.Debug("plurx webhook: no such item", "event", body.Event, "kind", kind, "tmdb", tmdb, "imdb", imdb)
		notFound(0)
		return
	}
	req := acquisition.ReacquireRequest{ItemID: itemID, Scope: scope}
	if body.Season != nil {
		req.Season = *body.Season
	}
	if scope == "episode" {
		item, err := s.deps.Store.GetMediaItemFull(r.Context(), itemID)
		if err != nil {
			s.acqErr(w, err)
			return
		}
		for _, sn := range item.Seasons {
			for _, e := range sn.Episodes {
				if sn.Number == *body.Season && e.EpisodeNumber == *body.Episode {
					req.EpisodeID = e.ID
				}
			}
		}
		if req.EpisodeID == 0 {
			notFound(itemID)
			return
		}
	}
	var res acquisition.ReacquireResult
	if body.Event == "reacquire_status" {
		res, err = s.deps.Acquisition.ReacquireStatus(r.Context(), req)
	} else {
		res, err = s.deps.Acquisition.Reacquire(r.Context(), req)
	}
	switch {
	case errors.Is(err, acquisition.ErrOffloadBusy):
		// An Offload of the title is running: nothing came back yet, and the
		// sender should read it as `offloading`, not as a failure.
		state := res.State
		if state == "" {
			state = acquisition.ReacquireOffloading
		}
		writeJSON(w, http.StatusOK, map[string]any{"matched": true, "mediaItemId": itemID, "state": state, "message": err.Error()})
		return
	case errors.Is(err, acquisition.ErrNotFound):
		notFound(itemID)
		return
	case errors.Is(err, acquisition.ErrInvalidOffload):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		s.acqErr(w, err)
		return
	}
	user := ""
	if body.User != nil {
		user = *body.User
	}
	s.deps.Log.Info("plurx: "+body.Event, "item", itemID, "scope", scope, "user", user, "state", res.State)
	out := map[string]any{"matched": true, "mediaItemId": itemID, "state": res.State}
	if res.Message != "" {
		out["message"] = res.Message
	}
	writeJSON(w, http.StatusOK, out)
}
