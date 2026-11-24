package api

import (
	"context"
	"fmt"
	"github.com/pjunod/monarr/internal/app/availability"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/household"
	"github.com/pjunod/monarr/internal/domain"
)

func (s *Server) householdService() *household.Service {
	if s.deps.Household != nil {
		return s.deps.Household
	}
	return household.New(s.deps.Store, s.deps.Library, s.deps.Acquisition)
}
func (s *Server) householdActor(w http.ResponseWriter, r *http.Request) (principal, bool) {
	p := currentPrincipal(r)
	if expected := r.Header.Get("X-Curator-Principal"); expected != "" && expected != fmt.Sprintf("%d:%s:%s", p.Account.ID, p.Method, p.accessRole()) {
		writeError(w, 409, "Account changed. Refresh before continuing.")
		return p, false
	}
	if !p.Authenticated || p.Machine || p.Account.ID <= 0 {
		writeError(w, 403, "sign in to a personal Curator account")
		return p, false
	}
	return p, true
}
func (s *Server) SearchHouseholdCatalogue(w http.ResponseWriter, r *http.Request, p apigen.SearchHouseholdCatalogueParams) {
	if _, ok := s.householdActor(w, r); !ok {
		return
	}
	values, err := s.householdService().Search(r.Context(), domain.MediaKind(p.Kind), p.Q)
	if err != nil {
		writeError(w, 503, "catalogue search is unavailable; try another title or retry shortly")
		return
	}
	writeJSON(w, 200, values)
}
func (s *Server) GetHouseholdTitle(w http.ResponseWriter, r *http.Request, id int64, p apigen.GetHouseholdTitleParams) {
	if _, ok := s.householdActor(w, r); !ok {
		return
	}
	value, err := s.householdService().Title(r.Context(), domain.MediaKind(p.Kind), id)
	if err != nil {
		writeError(w, 503, "title details could not be verified; retry shortly")
		return
	}
	writeJSON(w, 200, value)
}
func (s *Server) ListHouseholdRequests(w http.ResponseWriter, r *http.Request, p apigen.ListHouseholdRequestsParams) {
	actor, ok := s.householdActor(w, r)
	if !ok {
		return
	}
	before := int64(0)
	if p.Before != nil {
		before = *p.Before
	}
	if before < 0 {
		writeError(w, 400, "invalid request page")
		return
	}
	values, err := s.deps.Store.HouseholdRequests(r.Context(), actor.Account.ID, actor.accessRole() == "owner", before)
	if err != nil {
		writeError(w, 503, "requests are unavailable")
		return
	}
	writeJSON(w, 200, values)
}
func (s *Server) GetHouseholdRequest(w http.ResponseWriter, r *http.Request, id int64) {
	actor, ok := s.householdActor(w, r)
	if !ok {
		return
	}
	value, err := s.deps.Store.HouseholdRequest(r.Context(), id, actor.Account.ID, actor.accessRole() == "owner")
	if err != nil {
		writeError(w, 404, "request unavailable")
		return
	}
	writeJSON(w, 200, value)
}
func (s *Server) SubmitHouseholdRequest(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.householdActor(w, r)
	if !ok {
		return
	}
	var body struct {
		Kind    domain.MediaKind `json:"kind"`
		TMDBID  int64            `json:"tmdbId"`
		Seasons []int            `json:"seasons"`
	}
	if !decodeHouseholdBody(w, r, &body) {
		return
	}
	if (body.Kind != domain.KindMovie && body.Kind != domain.KindSeries) || body.TMDBID <= 0 {
		writeError(w, 400, "choose a verified movie or show identity")
		return
	}
	value, err := s.householdService().Submit(r.Context(), actor.Account, digestSession(requestSessionToken(r)), body.Kind, body.TMDBID, body.Seasons)
	if err != nil {
		writeError(w, 409, "request scope could not be verified; refresh title details and try again")
		return
	}
	writeJSON(w, 201, value)
}
func (s *Server) JoinHouseholdRequest(w http.ResponseWriter, r *http.Request, id int64) {
	s.changeHouseholdSubscription(w, r, id, false)
}
func (s *Server) WithdrawHouseholdRequest(w http.ResponseWriter, r *http.Request, id int64) {
	s.changeHouseholdSubscription(w, r, id, true)
}
func (s *Server) changeHouseholdSubscription(w http.ResponseWriter, r *http.Request, id int64, withdraw bool) {
	actor, ok := s.householdActor(w, r)
	if !ok {
		return
	}
	var err error
	if withdraw {
		err = s.deps.Store.WithdrawHouseholdRequest(r.Context(), actor.Account, digestSession(requestSessionToken(r)), id)
	} else {
		err = s.deps.Store.JoinHouseholdRequest(r.Context(), actor.Account, digestSession(requestSessionToken(r)), id)
	}
	if err != nil {
		writeError(w, 409, "request changed; refresh it before changing your subscription")
		return
	}
	value, err := s.deps.Store.HouseholdRequest(r.Context(), id, actor.Account.ID, actor.accessRole() == "owner")
	if err != nil {
		writeError(w, 503, "subscription saved; refresh requests for its current state")
		return
	}
	writeJSON(w, 200, value)
}
func (s *Server) DeclineHouseholdRequest(w http.ResponseWriter, r *http.Request, id int64) {
	actor, ok := s.requireLocalAccount(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Revision    int64  `json:"revision"`
		PublicNote  string `json:"publicNote"`
		PrivateNote string `json:"privateNote"`
	}
	if !decodeHouseholdBody(w, r, &body) {
		return
	}
	body.PublicNote = strings.TrimSpace(body.PublicNote)
	if len(body.PublicNote) > 500 || len(body.PrivateNote) > 2000 || !utf8.ValidString(body.PublicNote) || !utf8.ValidString(body.PrivateNote) {
		writeError(w, 400, "request notes are too long")
		return
	}
	if err := s.deps.Store.DeclineHouseholdRequest(r.Context(), actor, digestSession(requestSessionToken(r)), id, body.Revision, body.PublicNote, body.PrivateNote); err != nil {
		writeError(w, 409, "request changed; review it again")
		return
	}
	w.WriteHeader(204)
}
func (s *Server) SaveHouseholdRequestNote(w http.ResponseWriter, r *http.Request, id int64) {
	actor, ok := s.requireLocalAccount(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if !decodeHouseholdBody(w, r, &body) {
		return
	}
	if len(body.Note) > 2000 || !utf8.ValidString(body.Note) {
		writeError(w, 400, "use a private note of at most 2000 characters")
		return
	}
	if err := s.deps.Store.SaveHouseholdRequestNote(r.Context(), actor, digestSession(requestSessionToken(r)), id, body.Note); err != nil {
		writeError(w, 409, "private note could not be saved")
		return
	}
	w.WriteHeader(204)
}

func (s *Server) ReviewHouseholdRequest(w http.ResponseWriter, r *http.Request, id int64, p apigen.ReviewHouseholdRequestParams) {
	actor, ok := s.requireLocalAccount(w, r, true)
	if !ok {
		return
	}
	root, profile := int64(0), int64(0)
	if p.RootId != nil {
		root = *p.RootId
	}
	if p.ProfileId != nil {
		profile = *p.ProfileId
	}
	if root < 0 || profile < 0 {
		writeError(w, 400, "choose valid destination and profile IDs")
		return
	}
	review, err := s.householdService().Review(r.Context(), id, actor.ID, root, profile)
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, review)
}
func (s *Server) ApproveHouseholdRequest(w http.ResponseWriter, r *http.Request, id int64) {
	actor, ok := s.requireLocalAccount(w, r, true)
	if !ok {
		return
	}
	var body struct {
		RootID      int64  `json:"rootId"`
		ProfileID   int64  `json:"profileId"`
		Fingerprint string `json:"fingerprint"`
		PrivateNote string `json:"privateNote"`
		Reacquire   bool   `json:"reacquire"`
	}
	if !decodeHouseholdBody(w, r, &body) {
		return
	}
	if len(body.Fingerprint) != 64 || len(body.PrivateNote) > 2000 || !utf8.ValidString(body.PrivateNote) {
		writeError(w, 400, "review the current request and use a private note of at most 2000 characters")
		return
	}
	if err := s.householdService().Approve(r.Context(), actor, digestSession(requestSessionToken(r)), id, body.RootID, body.ProfileID, body.Fingerprint, body.PrivateNote, body.Reacquire); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	s.householdService().Wake()
	w.WriteHeader(204)
}

func safeHouseholdSearchResults(ctx context.Context, db *sqlite.DB, items []ports.SearchResult, limit int) []household.Title {
	out := []household.Title{}
	targets := []domain.AvailabilityTarget{}
	for _, v := range items {
		if (v.Kind != domain.KindMovie && v.Kind != domain.KindSeries) || v.TMDBID <= 0 {
			continue
		}
		out = append(out, household.Title{Kind: v.Kind, TMDBID: v.TMDBID, Title: v.Title, Year: v.Year, PosterPath: v.PosterPath, Overview: v.Overview, Seasons: []household.SeasonChoice{}})
		targets = append(targets, domain.AvailabilityTarget{Kind: v.Kind, IDs: domain.ExternalIDs{TMDB: v.TMDBID, TVDB: v.TVDBID, IMDB: v.IMDBID}})
		if len(out) == limit {
			break
		}
	}
	facts := availability.New(db).Lookup(ctx, targets)
	for i, v := range facts {
		v.LibraryItemID = 0
		out[i].Availability = v
	}
	return out
}
func (s *Server) HouseholdDiscoverySeeds(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.householdActor(w, r); !ok {
		return
	}
	if s.deps.LibraryDiscovery == nil {
		writeJSON(w, 200, map[string]any{"items": []household.Title{}, "hasMore": false})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if r.URL.Query().Get("offset") == "" {
		offset = 0
		err = nil
	}
	if err != nil || offset < 0 || offset > 100000 || len(q) > 200 {
		writeError(w, 400, "invalid library seed page")
		return
	}
	seeds, more, err := s.deps.LibraryDiscovery.Seeds(r.Context(), q, offset)
	if err != nil {
		writeError(w, 503, "library seeds are unavailable")
		return
	}
	items := []ports.SearchResult{}
	for _, seed := range seeds {
		items = append(items, seed.Item)
	}
	writeJSON(w, 200, map[string]any{"items": safeHouseholdSearchResults(r.Context(), s.deps.Store, items, 50), "hasMore": more})
}
func (s *Server) HouseholdDiscoveryShelf(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.householdActor(w, r); !ok {
		return
	}
	if s.deps.LibraryDiscovery == nil {
		writeError(w, 503, "library discovery unavailable")
		return
	}
	q := r.URL.Query()
	kind := domain.MediaKind(q.Get("kind"))
	tmdb, err := strconv.ParseInt(q.Get("tmdbId"), 10, 64)
	shelf := q.Get("shelf")
	if err != nil || tmdb <= 0 || (kind != domain.KindMovie && kind != domain.KindSeries) || (shelf != "related" && shelf != "collection") {
		writeError(w, 400, "choose a current library movie or show")
		return
	}
	facts, err := s.deps.Store.LookupAvailability(r.Context(), []domain.AvailabilityTarget{{Kind: kind, IDs: domain.ExternalIDs{TMDB: tmdb}}})
	if err != nil || len(facts) != 1 || facts[0].Catalog != "present" {
		writeError(w, 409, "library identity changed; choose the seed again")
		return
	}
	seed, err := s.deps.LibraryDiscovery.Seed(r.Context(), facts[0].LibraryItemID, kind, tmdb)
	if err != nil {
		writeError(w, 409, "library seed changed")
		return
	}
	if !s.validLibrarySeed(r, seed) {
		writeError(w, 409, "library seed identity is conflicting")
		return
	}
	title, items, err := s.deps.LibraryDiscovery.Items(r.Context(), seed, shelf)
	if err != nil {
		writeError(w, 503, "recommendations could not be refreshed; try another library title")
		return
	}
	current, e := s.deps.LibraryDiscovery.Seed(r.Context(), seed.LibraryItemID, kind, tmdb)
	if e != nil || domain.ExternalIDsConflict(domain.ExternalIDs{TMDB: seed.Item.TMDBID, TVDB: seed.Item.TVDBID, IMDB: seed.Item.IMDBID}, domain.ExternalIDs{TMDB: current.Item.TMDBID, TVDB: current.Item.TVDBID, IMDB: current.Item.IMDBID}) || !s.validLibrarySeed(r, current) {
		writeError(w, 409, "library seed changed; choose it again")
		return
	}
	writeJSON(w, 200, map[string]any{"title": title, "items": safeHouseholdSearchResults(r.Context(), s.deps.Store, items, 30)})
}
