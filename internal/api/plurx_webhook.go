package api

import (
	"encoding/json"
	"net/http"
	"time"

	apigen "github.com/monarr-media/monarr/internal/api/gen"
	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
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
//   - **Nothing here deletes anything.** Recorded, shown, and read by the
//     upgrade ordering. That is the whole of it, by the §11.1 decision.
func (s *Server) PlurxWebhook(w http.ResponseWriter, r *http.Request) {
	var body apigen.PlurxWebhookJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
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
	if body.Kind == apigen.PlurxWatchedEventKindEpisode {
		kind = domain.KindSeries
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

	rec := sqlite.PlurxWatch{
		MediaItemID: itemID,
		WatchedAt:   time.Unix(body.WatchedAt, 0),
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
