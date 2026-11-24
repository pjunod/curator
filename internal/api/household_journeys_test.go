package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pjunod/monarr/internal/adapters/tmdb"
	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/discover"
	"github.com/pjunod/monarr/internal/app/household"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func householdResult[T any](t *testing.T, response *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if response.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", response.Code, status, response.Body.String())
	}
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func householdExpect(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", response.Code, status, response.Body.String())
	}
}
func householdEnroll(t *testing.T, h http.Handler, owner, name string) string {
	t.Helper()
	invite := householdResult[struct{ Token string }](t, householdHTTP(t, h, "POST", "/api/v1/household/invitations", owner, map[string]string{"password": householdTestPassword}), 201)
	householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/invitations/accept", "", map[string]string{"token": invite.Token, "username": name, "password": householdTestPassword}), 201)
	return householdHTTPLogin(t, h, name, householdTestPassword)
}
func householdSubmitMovie(t *testing.T, h http.Handler, token string) domain.HouseholdRequest {
	t.Helper()
	return householdResult[domain.HouseholdRequest](t, householdHTTP(t, h, "POST", "/api/v1/household/requests", token, map[string]any{"kind": "movie", "tmdbId": 550, "seasons": []int{}}), 201)
}
func householdGetRequest(t *testing.T, h http.Handler, token string, id int64) domain.HouseholdRequest {
	t.Helper()
	return householdResult[domain.HouseholdRequest](t, householdHTTP(t, h, "GET", fmt.Sprintf("/api/v1/household/requests/%d", id), token, nil), 200)
}
func householdAddRoot(t *testing.T, h http.Handler, token, path string) domain.RootFolder {
	t.Helper()
	return householdResult[domain.RootFolder](t, householdHTTP(t, h, "POST", "/api/v1/rootfolders", token, map[string]string{"path": path, "kind": "mixed"}), 201)
}

func TestHouseholdOwnerReviewAndMemberSubscriptionJourney(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	member := householdEnroll(t, h, owner, "member")
	title := householdResult[household.Title](t, householdHTTP(t, h, "GET", "/api/v1/household/titles/550?kind=movie", member, nil), 200)
	if title.TMDBID != 550 || title.Availability.LibraryItemID != 0 {
		t.Fatal("unsafe title", title)
	}
	first := householdSubmitMovie(t, h, owner)
	path := fmt.Sprintf("/api/v1/household/requests/%d", first.ID)
	joined := householdResult[domain.HouseholdRequest](t, householdHTTP(t, h, "POST", path+"/join", member, nil), 200)
	if joined.SubscriberCount != 2 || !joined.Subscribed {
		t.Fatal("join did not share work", joined)
	}
	withdrawn := householdResult[domain.HouseholdRequest](t, householdHTTP(t, h, "POST", path+"/withdraw", member, nil), 200)
	if withdrawn.SubscriberCount != 1 || withdrawn.State != "pending" || withdrawn.Subscribed {
		t.Fatal("withdraw removed another member", withdrawn)
	}
	current := householdGetRequest(t, h, owner, first.ID)
	householdExpect(t, householdHTTP(t, h, "POST", path+"/decline", owner, map[string]any{"revision": current.Revision - 1, "publicNote": "no", "privateNote": "private"}), 409)
	householdExpect(t, householdHTTP(t, h, "POST", path+"/decline", owner, map[string]any{"revision": current.Revision, "publicNote": "Not this week", "privateNote": "Owner-only context"}), 204)
	receipt := householdGetRequest(t, h, member, first.ID)
	if receipt.State != "declined" || receipt.PublicNote != "Not this week" || receipt.PrivateNote != "" {
		t.Fatal("decline receipt/notes", receipt)
	}
	householdExpect(t, householdHTTP(t, h, "POST", path+"/join", member, nil), 409)
	request := householdSubmitMovie(t, h, member)
	if request.ID == first.ID {
		t.Fatal("terminal request was silently reopened")
	}
	root := householdAddRoot(t, h, owner, t.TempDir())
	reviewURL := fmt.Sprintf("/api/v1/household/requests/%d/review?rootId=%d&profileId=1", request.ID, root.ID)
	householdExpect(t, householdHTTP(t, h, "GET", reviewURL, member, nil), 403)
	review := householdResult[household.Review](t, householdHTTP(t, h, "GET", reviewURL, owner, nil), 200)
	if !review.NewTitle || review.Missing != 1 || review.Present != 0 || review.RootID != root.ID {
		t.Fatal("new title review", review)
	}
	approveURL := fmt.Sprintf("/api/v1/household/requests/%d/approve", request.ID)
	body := map[string]any{"rootId": root.ID, "profileId": 1, "fingerprint": strings.Repeat("0", 64), "privateNote": "Approve exactly this movie", "reacquire": false}
	householdExpect(t, householdHTTP(t, h, "POST", approveURL, owner, body), 409)
	if _, err := db.GetMediaItemByKindTmdb(context.Background(), domain.KindMovie, 550); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatal("stale review partially added title", err)
	}
	body["fingerprint"] = review.Fingerprint
	householdExpect(t, householdHTTP(t, h, "POST", approveURL, owner, body), 204)
	id, err := db.GetMediaItemByKindTmdb(context.Background(), domain.KindMovie, 550)
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.GetMediaItemFull(context.Background(), id)
	if err != nil || item.Monitored || item.RootFolderID != root.ID || item.QualityProfileID != 1 {
		t.Fatal("approval changed persistent authority", item, err)
	}
	ownerReceipt := householdGetRequest(t, h, owner, request.ID)
	safe := householdGetRequest(t, h, member, request.ID)
	if ownerReceipt.State != "approved" || ownerReceipt.PrivateNote != "Approve exactly this movie" || safe.PrivateNote != "" {
		t.Fatal("approval receipt/notes", ownerReceipt, safe)
	}
	older := householdResult[[]domain.HouseholdRequest](t, householdHTTP(t, h, "GET", fmt.Sprintf("/api/v1/household/requests?before=%d", request.ID), owner, nil), 200)
	if len(older) != 1 || older[0].ID != first.ID {
		t.Fatal("request cursor lost terminal history", older)
	}
	householdExpect(t, householdHTTP(t, h, "GET", reviewURL, owner, nil), 409)
	var transfers int
	if err = db.R.QueryRowContext(context.Background(), `SELECT count(*) FROM downloads`).Scan(&transfers); err != nil || transfers != 0 {
		t.Fatal("review admitted a transfer", transfers, err)
	}
}
func TestHouseholdNewTitleReviewPreservesUnindexedFilesAndVolumePolicy(t *testing.T) {
	for _, change := range []string{"media arrived", "symlink arrived", "volume changed", "wrong profile"} {
		t.Run(change, func(t *testing.T) {
			h, db, owner := householdHTTPAccount(t)
			request := householdSubmitMovie(t, h, owner)
			root := householdAddRoot(t, h, owner, t.TempDir())
			path := fmt.Sprintf("/api/v1/household/requests/%d/review?rootId=%d&profileId=1", request.ID, root.ID)
			review := householdResult[household.Review](t, householdHTTP(t, h, "GET", path, owner, nil), 200)
			switch change {
			case "media arrived":
				if err := os.MkdirAll(review.Destination, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(review.Destination, "arrived.mkv"), []byte("preserve this"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink arrived":
				if err := os.MkdirAll(review.Destination, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(review.Destination, "linked")); err != nil {
					t.Fatal(err)
				}
			case "volume changed":
				if _, err := db.W.ExecContext(context.Background(), `UPDATE library_presence_roots SET identity='different-volume',generation=generation+1 WHERE root_id=?`, root.ID); err != nil {
					t.Fatal(err)
				}
			case "wrong profile":
				if _, err := db.W.ExecContext(context.Background(), `DELETE FROM quality_profiles WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			}
			householdExpect(t, householdHTTP(t, h, "GET", path, owner, nil), 409)
			householdExpect(t, householdHTTP(t, h, "POST", fmt.Sprintf("/api/v1/household/requests/%d/approve", request.ID), owner, map[string]any{"rootId": root.ID, "profileId": 1, "fingerprint": review.Fingerprint}), 409)
			if _, err := db.GetMediaItemByKindTmdb(context.Background(), domain.KindMovie, 550); !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatal("refused review added a title", err)
			}
			if change == "media arrived" {
				raw, err := os.ReadFile(filepath.Join(review.Destination, "arrived.mkv"))
				if err != nil || string(raw) != "preserve this" {
					t.Fatal("existing file changed", err)
				}
			}
		})
	}
}

func TestHouseholdAccountLifecycleRetainsIdentityAndRevokesOldCredentials(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	member := householdEnroll(t, h, owner, "member")
	ctx := context.Background()
	account, err := db.AccountByUsername(ctx, "member")
	if err != nil {
		t.Fatal(err)
	}
	accounts := householdResult[[]domain.Account](t, householdHTTP(t, h, "GET", "/api/v1/household/accounts", owner, nil), 200)
	if len(accounts) != 2 {
		t.Fatal("owner account inventory", accounts)
	}
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/household/accounts", member, nil), 403)
	for _, change := range []map[string]any{{"revision": account.Revision, "role": "owner"}, {"revision": account.Revision, "status": "suspended"}, {"revision": 0, "displayName": "New name"}, {"revision": account.Revision, "displayName": " "}, {"revision": account.Revision, "newPassword": "short"}} {
		change["password"] = householdTestPassword
		response := householdHTTP(t, h, "PUT", "/api/v1/auth/account", member, change)
		if response.Code != 400 && response.Code != 409 {
			t.Fatal("unsafe personal account change accepted", response.Code, response.Body.String())
		}
	}
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/auth/account", member, map[string]any{"revision": account.Revision, "password": householdTestPassword, "displayName": "Daily member"}), 204)
	updated, err := db.AccountByUsername(ctx, "member")
	if err != nil || updated.ID != account.ID || updated.Role != "member" || updated.DisplayName != "Daily member" {
		t.Fatal("display update changed identity", updated, err)
	}
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/auth/me", member, nil), 200)
	next := "a-different-local-password"
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/auth/account", member, map[string]any{"revision": updated.Revision, "password": householdTestPassword, "newPassword": next}), 204)
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/auth/me", member, nil), 401)
	householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/sessions", "", map[string]string{"username": "member", "password": householdTestPassword}), 401)
	member = householdHTTPLogin(t, h, "member", next)
	current, err := db.AccountByUsername(ctx, "member")
	if err != nil || current.ID != account.ID {
		t.Fatal("password reset changed account identity", err)
	}
	ownerAccount, err := db.AccountByUsername(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	householdExpect(t, householdHTTP(t, h, "PATCH", fmt.Sprintf("/api/v1/household/accounts/%d", ownerAccount.ID), owner, map[string]any{"revision": ownerAccount.Revision, "password": householdTestPassword, "role": "member", "status": "active"}), 409)
	householdExpect(t, householdHTTP(t, h, "PATCH", fmt.Sprintf("/api/v1/household/accounts/%d", current.ID), owner, map[string]any{"revision": current.Revision, "password": householdTestPassword, "role": "owner", "status": "active"}), 204)
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/auth/me", member, nil), 401)
	member = householdHTTPLogin(t, h, "member", next)
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/household/accounts", member, nil), 200)
	householdExpect(t, householdHTTP(t, h, "PATCH", fmt.Sprintf("/api/v1/household/accounts/%d", ownerAccount.ID), member, map[string]any{"revision": ownerAccount.Revision, "password": next, "role": "member", "status": "active"}), 204)
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/system/status", owner, nil), 401)
	if a, e := db.AccountByID(ctx, account.ID); e != nil || a.Role != "owner" {
		t.Fatal("usable local owner was lost", a, e)
	}
}
func TestHouseholdInviteAndRequestBoundariesRejectUnsafeInput(t *testing.T) {
	h, _, owner := householdHTTPAccount(t)
	invite := householdResult[struct{ Token string }](t, householdHTTP(t, h, "POST", "/api/v1/household/invitations", owner, map[string]string{"password": householdTestPassword}), 201)
	for _, name := range []string{"bad name", "../bad", strings.Repeat("a", 81)} {
		householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/invitations/accept", "", map[string]string{"token": invite.Token, "username": name, "password": householdTestPassword}), 400)
	}
	householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/invitations/accept", "", map[string]string{"token": invite.Token, "username": "member", "password": householdTestPassword}), 201)
	householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/invitations/accept", "", map[string]string{"token": invite.Token, "username": "another", "password": householdTestPassword}), 409)
	member := householdHTTPLogin(t, h, "member", householdTestPassword)
	request := householdSubmitMovie(t, h, member)
	path := fmt.Sprintf("/api/v1/household/requests/%d", request.ID)
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/household/requests?before=-1", member, nil), 400)
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/household/requests/999999", member, nil), 404)
	householdExpect(t, householdHTTP(t, h, "PUT", path+"/note", owner, map[string]string{"note": strings.Repeat("x", 2001)}), 400)
	householdExpect(t, householdHTTP(t, h, "POST", path+"/decline", owner, map[string]any{"revision": request.Revision, "publicNote": strings.Repeat("x", 501)}), 400)
	householdExpect(t, householdHTTP(t, h, "GET", path+"/review?rootId=-1", owner, nil), 400)
	householdExpect(t, householdHTTP(t, h, "POST", path+"/approve", owner, map[string]any{"fingerprint": "bad"}), 400)
	householdExpect(t, householdHTTP(t, h, "POST", path+"/withdraw", member, nil), 200)
	receipt := householdGetRequest(t, h, member, request.ID)
	if receipt.State != "withdrawn" || receipt.SubscriberCount != 0 {
		t.Fatal("last pending interest was retained", receipt)
	}
}

func TestCinemaDisconnectDrainsRemoteGrantsAndPreservesLocalRequests(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	f := newCinemaFixture(t)
	ctx := context.Background()
	initial := householdResult[map[string]any](t, householdHTTP(t, h, "GET", "/api/v1/auth/cinema/connection", owner, nil), 200)
	if initial["configured"] != false {
		t.Fatal(initial)
	}
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", owner, map[string]string{"password": householdTestPassword, "baseUrl": f.server.URL, "clientId": f.client, "clientSecret": f.secret, "redirect": "https://curator.example/api/v1/auth/cinema/callback"}), 204)
	config := householdResult[map[string]any](t, householdHTTP(t, h, "GET", "/api/v1/auth/cinema/connection", owner, nil), 200)
	if config["configured"] != true || config["clientSecret"] != nil || config["issuer"] != f.issuer {
		t.Fatal("unsafe connection view", config)
	}
	before := householdResult[map[string]any](t, householdHTTP(t, h, "GET", "/api/v1/auth/cinema/link", owner, nil), 200)
	if before["linked"] != false {
		t.Fatal(before)
	}
	state, cookie := cinemaStart(t, h, "/api/v1/auth/cinema/link", owner, map[string]string{"password": householdTestPassword})
	linked := cinemaCallbackTest(t, h, f, state, owner, cookie)
	if linked.Header().Get("Location") != "/account?cinema=linked" {
		t.Fatal(linked.Header())
	}
	link := householdResult[map[string]any](t, householdHTTP(t, h, "GET", "/api/v1/auth/cinema/link", owner, nil), 200)
	if link["linked"] != true || link["issuer"] != f.issuer || link["subject"] != nil {
		t.Fatal(link)
	}
	state, cookie = cinemaStart(t, h, "/api/v1/auth/cinema/start", "", nil)
	signed := cinemaCallbackTest(t, h, f, state, "", cookie)
	var derived string
	for _, c := range signed.Result().Cookies() {
		if c.Name == "monarr_session" {
			derived = c.Value
		}
	}
	if derived == "" {
		t.Fatal("no delegated session")
	}
	request := householdSubmitMovie(t, h, derived)
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", owner, map[string]any{"password": householdTestPassword, "disconnect": true}), 204)
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/household/requests", derived, nil), 401)
	saved := householdGetRequest(t, h, owner, request.ID)
	if !saved.Subscribed {
		t.Fatal("disconnect lost local household history", saved)
	}
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/system/status", owner, nil), 200)
	pending, err := db.CinemaRevocations(ctx)
	if err != nil || len(pending) < 2 {
		t.Fatal("grant revocations were not durably queued", pending, err)
	}
	worker, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	server := New(Deps{Store: db})
	go func() { defer close(done); server.RunCinemaRevocations(worker) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(3 * time.Second)
	for {
		var remaining int
		if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM cinema_revocations`).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		active := len(f.active)
		f.mu.Unlock()
		if remaining == 0 && active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("revocations failed to drain", remaining, active)
		}
		time.Sleep(10 * time.Millisecond)
	}
	local := householdHTTPLogin(t, h, "owner", householdTestPassword)
	if got := householdGetRequest(t, h, local, request.ID); got.ID != request.ID {
		t.Fatal(got)
	}
	householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/cinema/start", "", nil), 503)
}

func TestHouseholdSeriesApprovalFreezesAiredScopeWithoutMonitoring(t *testing.T) {
	h, db := newLibraryServer(t, householdSeriesProvider{stubProvider{configured: true}})
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/settings", "", map[string]any{"authUsername": "owner", "authPassword": householdTestPassword, "authRequired": true}), 204)
	owner := householdHTTPLogin(t, h, "owner", householdTestPassword)
	member := householdEnroll(t, h, owner, "member")
	request := householdResult[domain.HouseholdRequest](t, householdHTTP(t, h, "POST", "/api/v1/household/requests", member, map[string]any{"kind": "series", "tmdbId": 100, "seasons": []int{1}}), 201)
	root := householdAddRoot(t, h, owner, t.TempDir())
	path := fmt.Sprintf("/api/v1/household/requests/%d", request.ID)
	review := householdResult[household.Review](t, householdHTTP(t, h, "GET", fmt.Sprintf("%s/review?rootId=%d&profileId=1", path, root.ID), owner, nil), 200)
	if !review.NewTitle || review.Missing != 1 || review.Scope != "1 frozen aired regular episodes · primary copy" {
		t.Fatal(review)
	}
	householdExpect(t, householdHTTP(t, h, "POST", path+"/approve", owner, map[string]any{"rootId": root.ID, "profileId": 1, "fingerprint": review.Fingerprint}), 204)
	targets, err := db.HouseholdPendingTargets(context.Background())
	if err != nil || len(targets) != 1 || targets[0].Season != 1 || targets[0].Episode != 1 {
		t.Fatal("scope expanded on approval", targets, err)
	}
	item, err := db.GetMediaItemFull(context.Background(), targets[0].MediaItemID)
	if err != nil || item.Monitored {
		t.Fatal("approval enabled broad monitoring", item, err)
	}
	for _, season := range item.Seasons {
		if season.Monitored {
			t.Fatal("season monitoring enabled")
		}
		for _, ep := range season.Episodes {
			if ep.Monitored {
				t.Fatal("episode monitoring enabled")
			}
		}
	}
	if targets[0].DownloadID != 0 {
		t.Fatal("approval itself submitted a transfer")
	}
	receipt := householdGetRequest(t, h, member, request.ID)
	if receipt.State != "approved" || len(receipt.Scope.Episodes) != 1 {
		t.Fatal(receipt)
	}
}

func TestHouseholdSeriesUsesRealTMDBInventoryAndFailsClosedOnSeasonErrors(t *testing.T) {
	for _, scenario := range []string{"normal", "season unavailable", "wrong identity", "duplicate episode", "wrong parent"} {
		t.Run(scenario, func(t *testing.T) {
			var seasonHits atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/tv/100":
					id := 100
					if scenario == "wrong identity" {
						id = 200
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": "Real series", "first_air_date": "2020-01-01", "seasons": []map[string]int{{"season_number": 1}}})
				case "/tv/100/season/1":
					seasonHits.Add(1)
					if scenario == "season unavailable" {
						http.Error(w, "upstream failed", http.StatusServiceUnavailable)
						return
					}
					parent := 1
					if scenario == "wrong parent" {
						parent = 2
					}
					episodes := []map[string]any{{"season_number": parent, "episode_number": 1, "air_date": "2020-01-01"}, {"season_number": 1, "episode_number": 2, "air_date": "2099-01-01"}, {"season_number": 1, "episode_number": 3, "air_date": ""}}
					if scenario == "duplicate episode" {
						episodes = append(episodes, episodes[0])
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"season_number": 1, "episodes": episodes})
				default:
					http.NotFound(w, r)
				}
			}))
			defer remote.Close()
			provider := tmdb.New(remote.URL, func(context.Context) (string, error) { return "test-key", nil })
			// Prove this adapter's ordinary preview really is lightweight.
			preview, err := provider.PreviewExternal(context.Background(), domain.KindSeries, domain.ExternalRef{Provider: "tmdb", Value: "100"})
			if scenario != "wrong identity" && (err != nil || len(preview.Seasons) != 0 || seasonHits.Load() != 0) {
				t.Fatal("fixture bypassed production preview semantics", preview, err)
			}
			h, db := newLibraryServer(t, provider)
			householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/settings", "", map[string]any{"authUsername": "owner", "authPassword": householdTestPassword, "authRequired": true}), 204)
			owner := householdHTTPLogin(t, h, "owner", householdTestPassword)
			if scenario != "normal" {
				householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/household/titles/100?kind=series", owner, nil), 503)
				householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/household/requests", owner, map[string]any{"kind": "series", "tmdbId": 100, "seasons": []int{1}}), 409)
				rows, e := db.HouseholdRequests(context.Background(), 1, true, 0)
				if e != nil || len(rows) != 0 {
					t.Fatal("failed inventory persisted a request", rows, e)
				}
				return
			}
			title := householdResult[household.Title](t, householdHTTP(t, h, "GET", "/api/v1/household/titles/100?kind=series", owner, nil), 200)
			if len(title.Seasons) != 1 || title.Seasons[0].Aired != 1 || title.Seasons[0].UnknownDates != 1 || seasonHits.Load() == 0 {
				t.Fatal("production season choices absent", title)
			}
			req := householdResult[domain.HouseholdRequest](t, householdHTTP(t, h, "POST", "/api/v1/household/requests", owner, map[string]any{"kind": "series", "tmdbId": 100, "seasons": []int{1}}), 201)
			if len(req.Scope.Episodes) != 1 || req.Scope.Episodes[0].Episode != 1 {
				t.Fatal(req.Scope)
			}
			root := householdAddRoot(t, h, owner, t.TempDir())
			path := fmt.Sprintf("/api/v1/household/requests/%d", req.ID)
			review := householdResult[household.Review](t, householdHTTP(t, h, "GET", fmt.Sprintf("%s/review?rootId=%d&profileId=1", path, root.ID), owner, nil), 200)
			householdExpect(t, householdHTTP(t, h, "POST", path+"/approve", owner, map[string]any{"rootId": root.ID, "profileId": 1, "fingerprint": review.Fingerprint}), 204)
			targets, e := db.HouseholdPendingTargets(context.Background())
			if e != nil || len(targets) != 1 || targets[0].Episode != 1 {
				t.Fatal(targets, e)
			}
		})
	}
}

func TestHouseholdMemberLibraryDiscoveryProjectsSafeFactsAndAcceptsRelatedRequest(t *testing.T) {
	original, db, owner := householdHTTPAccount(t)
	member := householdEnroll(t, original, owner, "member")
	ctx := context.Background()
	for i := range 52 {
		_, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: fmt.Sprintf("Seed %02d", i), SortTitle: fmt.Sprintf("seed %02d", i), IDs: domain.ExternalIDs{TMDB: int64(1000 + i)}, Path: fmt.Sprintf("/private/owner/library/%d", i), QualityProfileID: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	source := changingLibraryProvider{during: func() { calls++ }}
	h := New(Deps{Store: db, LibraryDiscovery: discover.NewLibrary(db, source)}).Handler()
	type seedPage struct {
		Items   []household.Title
		HasMore bool
	}
	first := householdResult[seedPage](t, householdHTTP(t, h, "GET", "/api/v1/household/discovery/seeds", member, nil), 200)
	if len(first.Items) != 50 || !first.HasMore {
		t.Fatal(first)
	}
	for _, item := range first.Items {
		if item.Availability.Catalog != "present" || item.Availability.LibraryItemID != 0 || item.Availability.Presence != "unknown" {
			t.Fatal("unsafe library projection", item)
		}
	}
	last := householdResult[seedPage](t, householdHTTP(t, h, "GET", "/api/v1/household/discovery/seeds?offset=50", member, nil), 200)
	if len(last.Items) != 2 || last.HasMore {
		t.Fatal(last)
	}
	if last.Items[0].TMDBID != 1050 || last.Items[1].TMDBID != 1051 {
		t.Fatal("seed pages overlap", last)
	}
	filtered := householdResult[seedPage](t, householdHTTP(t, h, "GET", "/api/v1/household/discovery/seeds?q=Seed%2003", member, nil), 200)
	if len(filtered.Items) != 1 || filtered.Items[0].Title != "Seed 03" {
		t.Fatal(filtered)
	}
	for _, query := range []string{"offset=-1", "offset=100001", "offset=invalid", "q=" + strings.Repeat("x", 201)} {
		householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/household/discovery/seeds?"+query, member, nil), 400)
	}
	shelfPath := "/api/v1/household/discovery/shelf?kind=movie&tmdbId=1000&shelf=related"
	response := householdHTTP(t, h, "GET", shelfPath, member, nil)
	shelf := householdResult[struct {
		Title string
		Items []household.Title
	}](t, response, 200)
	if calls != 1 || len(shelf.Items) != 1 || shelf.Items[0].TMDBID != 99 || shelf.Items[0].Availability.Catalog != "absent" || shelf.Items[0].Availability.LibraryItemID != 0 {
		t.Fatal(shelf, calls)
	}
	for _, private := range []string{"/private/owner", "qualityProfile", "rootFolder", "libraryItemId\":1"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatal("member shelf leaked owner data", response.Body.String())
		}
	}
	// Recommendations carry provider identity directly into the ordinary request flow.
	request := householdResult[domain.HouseholdRequest](t, householdHTTP(t, original, "POST", "/api/v1/household/requests", member, map[string]any{"kind": "movie", "tmdbId": shelf.Items[0].TMDBID, "seasons": []int{}}), 201)
	if request.TMDBID != 99 || request.State != "pending" {
		t.Fatal(request)
	}
	for _, query := range []string{"kind=book&tmdbId=1000&shelf=related", "kind=movie&tmdbId=1000&shelf=unknown", "kind=movie&tmdbId=no&shelf=related"} {
		householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/household/discovery/shelf?"+query, member, nil), 400)
	}
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/household/discovery/shelf?kind=movie&tmdbId=999999&shelf=related", member, nil), 409)
	unavailable := New(Deps{Store: db, LibraryDiscovery: discover.NewLibrary(db, nil)}).Handler()
	householdExpect(t, householdHTTP(t, unavailable, "GET", shelfPath, member, nil), 503)
	collection := householdResult[struct{ Items []household.Title }](t, householdHTTP(t, h, "GET", "/api/v1/household/discovery/shelf?kind=movie&tmdbId=1000&shelf=collection", member, nil), 200)
	if len(collection.Items) != 0 {
		t.Fatal(collection)
	}
	// Unsupported provider result kinds and missing canonical IDs never become request cards.
	if values := safeHouseholdSearchResults(ctx, db, []ports.SearchResult{{Kind: domain.KindBook, TMDBID: 1}, {Kind: domain.KindMovie}, {Kind: domain.KindMovie, TMDBID: 99}}, 30); len(values) != 1 || values[0].TMDBID != 99 {
		t.Fatal(values)
	}
}

type householdMixedSeriesProvider struct {
	householdSeriesProvider
	newlyAired bool
}

func (p *householdMixedSeriesProvider) GetSeries(ctx context.Context, id int64) (domain.MediaItem, error) {
	item, err := p.householdSeriesProvider.GetSeries(ctx, id)
	item.Seasons[1].Episodes[1].AirDate = "2020-01-02"
	if p.newlyAired {
		item.Seasons[1].Episodes = append(item.Seasons[1].Episodes, domain.Episode{SeasonNumber: 1, EpisodeNumber: 4, AirDate: "2020-01-04"})
	}
	return item, err
}
func (p *householdMixedSeriesProvider) PreviewExternal(ctx context.Context, kind domain.MediaKind, ref domain.ExternalRef) (domain.MediaItem, error) {
	item, err := householdPreviewRecord(ctx, p, kind, ref)
	item.Seasons = nil
	return item, err
}
func TestHouseholdExistingSeriesReviewRequiresExactOffloadConsent(t *testing.T) {
	provider := &householdMixedSeriesProvider{householdSeriesProvider: householdSeriesProvider{stubProvider{configured: true}}}
	h, db := newLibraryServer(t, provider)
	ctx := context.Background()
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/settings", "", map[string]any{"authUsername": "owner", "authPassword": householdTestPassword, "authRequired": true}), 204)
	owner := householdHTTPLogin(t, h, "owner", householdTestPassword)
	root := householdAddRoot(t, h, owner, t.TempDir())
	item, err := provider.GetSeries(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	item.RootFolderID = root.ID
	item.QualityProfileID = 1
	item.Path = filepath.Join(root.Path, "Frozen show")
	item.Monitored = false
	if err = os.Mkdir(item.Path, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateMediaItem(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	first, err := db.GetEpisodeID(ctx, id, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.GetEpisodeID(ctx, id, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(item.Path, "S01E01.mkv")
	if err = os.WriteFile(file, []byte("primary"), 0600); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, id, 0, file, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, fid, []int64{first}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE episodes SET offloaded_at=123 WHERE id=?`, second); err != nil {
		t.Fatal(err)
	}
	request := householdResult[domain.HouseholdRequest](t, householdHTTP(t, h, "POST", "/api/v1/household/requests", owner, map[string]any{"kind": "series", "tmdbId": 100, "seasons": []int{1}}), 201)
	if len(request.Scope.Episodes) != 2 {
		t.Fatal(request.Scope)
	}
	provider.newlyAired = true
	path := fmt.Sprintf("/api/v1/household/requests/%d", request.ID)
	review := householdResult[household.Review](t, householdHTTP(t, h, "GET", path+"/review", owner, nil), 200)
	if review.NewTitle || review.Present != 1 || review.Missing != 1 || review.Offloaded != 1 || review.RootID != root.ID {
		t.Fatal(review)
	}
	body := map[string]any{"fingerprint": review.Fingerprint}
	householdExpect(t, householdHTTP(t, h, "POST", path+"/approve", owner, body), 409)
	targets, err := db.HouseholdPendingTargets(ctx)
	if err != nil || len(targets) != 0 {
		t.Fatal("missing consent committed work", targets, err)
	}
	body["reacquire"] = true
	householdExpect(t, householdHTTP(t, h, "POST", path+"/approve", owner, body), 204)
	targets, err = db.HouseholdPendingTargets(ctx)
	if err != nil || len(targets) != 1 || targets[0].EpisodeID != second || targets[0].OffloadEpoch != 123 {
		t.Fatal("frozen missing scope expanded", targets, err)
	}
	current, err := db.GetMediaItemFull(ctx, id)
	if err != nil || current.Monitored {
		t.Fatal(current, err)
	}
	if current.Seasons[1].Episodes[1].OffloadedAt != 123 {
		t.Fatal("approval cleared offload before primary placement")
	}
	bytes, err := os.ReadFile(file)
	if err != nil || string(bytes) != "primary" {
		t.Fatal("present primary altered", err)
	}
	saved := householdGetRequest(t, h, owner, request.ID)
	if len(saved.Scope.Episodes) != 2 {
		t.Fatal("provider airing expanded scope", saved)
	}
}

func TestCinemaUnboundAndMalformedHandoffsNeverIssueSessions(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	f := newCinemaFixture(t)
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", owner, map[string]string{"password": householdTestPassword, "baseUrl": f.server.URL, "clientId": f.client, "clientSecret": f.secret, "redirect": "https://curator.example/api/v1/auth/cinema/callback"}), 204)
	for _, body := range []map[string]string{{"challenge": "invalid"}, {"challenge": ""}} {
		householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/cinema/native", "", body), 400)
	}
	for _, path := range []string{"/api/v1/auth/cinema/browser", "/api/v1/auth/cinema/browser?state=unknown"} {
		householdExpect(t, householdHTTP(t, h, "GET", path, "", nil), 409)
	}
	for _, body := range []map[string]string{{}, {"state": "unknown", "verifier": strings.Repeat("a", 64)}, {"state": "unknown", "verifier": strings.Repeat("a", 42)}, {"state": "unknown", "verifier": strings.Repeat("!", 64)}} {
		rr := householdHTTP(t, h, "POST", "/api/v1/auth/cinema/native/result", "", body)
		if rr.Code != 400 && rr.Code != 409 {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	for _, query := range []string{"", "?state=unknown", "?state=unknown&iss=wrong&code=one-use-code"} {
		rr := householdHTTP(t, h, "GET", "/api/v1/auth/cinema/callback"+query, "", nil)
		if rr.Header().Get("Location") != "/login?cinema=failed" {
			t.Fatal(rr.Header())
		}
		for _, c := range rr.Result().Cookies() {
			if c.Name == "monarr_session" && c.Value != "" {
				t.Fatal("malformedcallbackissuedidentity")
			}
		}
	}
	state, cookie := cinemaStart(t, h, "/api/v1/auth/cinema/start", "", nil)
	rr := householdHTTP(t, h, "GET", "/api/v1/auth/cinema/callback?state="+state+"&iss=wrong&code=one-use-code", "", nil, cookie)
	if rr.Header().Get("Location") != "/login?cinema=failed" {
		t.Fatal(rr.Header())
	}
	rows, err := db.ListAccounts(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatal("unbound response created localaccount", rows, err)
	}
}

func TestHouseholdLegacyCredentialMigrationCommitsAtomically(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("write failure=", fail), func(t *testing.T) {
			h, db := newLibraryServer(t, householdPreviewProvider{stubProvider{configured: true}})
			ctx := context.Background()
			legacy := legacyAuthSettings()
			for key, value := range legacy.values {
				if err := db.SetMeta(ctx, key, value); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.SetMeta(ctx, "auth_required", "true"); err != nil {
				t.Fatal(err)
			}
			if err := db.EnsureInitialHouseholdOwner(ctx); err != nil {
				t.Fatal(err)
			}
			before, err := db.AccountByUsername(ctx, "owner")
			if err != nil {
				t.Fatal(err)
			}
			if err = db.CreateAccountSession(ctx, "old-device", before, "before upgrade", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			id, err := db.SubmitHouseholdRequest(ctx, before, "old-device", domain.HouseholdRequest{Kind: domain.KindMovie, TMDBID: 550, IDs: domain.ExternalIDs{TMDB: 550}, Title: "Local history", Fingerprint: "legacy"})
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if _, err = db.W.ExecContext(ctx, `CREATE TRIGGER fail_legacy_sync BEFORE UPDATE ON app_meta WHEN old.key='auth_password_hash' BEGIN SELECT RAISE(ABORT,'fixture sync failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/sessions", "", map[string]string{"username": "owner", "password": "wrong"}), 401)
			response := householdHTTP(t, h, "POST", "/api/v1/auth/sessions", "", map[string]string{"username": "owner", "password": "old-password", "device": strings.Repeat("d", 200)})
			if fail {
				householdExpect(t, response, 503)
				unchanged, e := db.AccountByID(ctx, before.ID)
				if e != nil || unchanged.PasswordHash != before.PasswordHash || unchanged.LegacySalt != before.LegacySalt {
					t.Fatal("failed migration partially replaced credential", unchanged, e)
				}
				if _, err = db.W.ExecContext(ctx, `DROP TRIGGER fail_legacy_sync`); err != nil {
					t.Fatal(err)
				}
				response = householdHTTP(t, h, "POST", "/api/v1/auth/sessions", "", map[string]string{"username": "owner", "password": "old-password"})
			}
			result := householdResult[struct{ Token string }](t, response, 200)
			if result.Token == "" {
				t.Fatal("successful login missing session")
			}
			after, err := db.AccountByID(ctx, before.ID)
			if err != nil || !strings.HasPrefix(after.PasswordHash, passwordHashPrefix) || after.LegacySalt != "" || after.CredentialGeneration != before.CredentialGeneration {
				t.Fatal("migration lost durableidentity", after, err)
			}
			mirrored, err := db.GetMeta(ctx, authHashSetting)
			if err != nil || mirrored != after.PasswordHash {
				t.Fatal("initial owner credential mirror drift", mirrored, err)
			}
			if _, err = db.AccountSession(ctx, "old-device"); err != nil {
				t.Fatal("hash upgrade revoked independent session", err)
			}
			saved, err := db.HouseholdRequest(ctx, id, after.ID, true)
			if err != nil || !saved.Subscribed {
				t.Fatal("upgrade lost requesthistory", saved, err)
			}
			if err = db.UpgradeAccountPassword(ctx, before, "stale"); !errors.Is(err, sqlite.ErrAccountConflict) {
				t.Fatal("stale migration overwrote credential", err)
			}
			householdHTTPLogin(t, h, "owner", "old-password")
		})
	}
}

func TestHouseholdWriteOutagePreservesAccountsAndPendingRequests(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	ctx := context.Background()
	member := householdEnroll(t, h, owner, "member")
	account, err := db.AccountByUsername(ctx, "member")
	if err != nil {
		t.Fatal(err)
	}
	invite := householdResult[struct{ Token string }](t, householdHTTP(t, h, "POST", "/api/v1/household/invitations", owner, map[string]string{"password": householdTestPassword}), 201)
	request := householdSubmitMovie(t, h, member)
	root := householdAddRoot(t, h, owner, t.TempDir())
	path := fmt.Sprintf("/api/v1/household/requests/%d", request.ID)
	review := householdResult[household.Review](t, householdHTTP(t, h, "GET", fmt.Sprintf("%s/review?rootId=%d&profileId=1", path, root.ID), owner, nil), 200)
	if err = db.W.Close(); err != nil {
		t.Fatal(err)
	}
	for _, action := range []struct {
		method, path, token string
		body                any
		status              int
	}{
		{"POST", "/api/v1/household/invitations", owner, map[string]string{"password": householdTestPassword}, 409},
		{"POST", "/api/v1/auth/invitations/accept", "", map[string]string{"token": invite.Token, "username": "another", "password": householdTestPassword}, 409},
		{"PUT", "/api/v1/auth/account", member, map[string]any{"revision": account.Revision, "displayName": "Changed", "password": householdTestPassword}, 409},
		{"POST", path + "/join", owner, nil, 409},
		{"POST", path + "/withdraw", member, nil, 409},
		{"POST", path + "/decline", owner, map[string]any{"revision": request.Revision, "publicNote": "Declined"}, 409},
		{"PUT", path + "/note", owner, map[string]string{"note": "Private update"}, 409},
		{"POST", path + "/approve", owner, map[string]any{"rootId": root.ID, "profileId": 1, "fingerprint": review.Fingerprint}, 409},
		{"POST", "/api/v1/household/requests", member, map[string]any{"kind": "movie", "tmdbId": 551}, 409},
		{"PUT", "/api/v1/auth/cinema/connection", owner, map[string]any{"password": householdTestPassword, "disconnect": true}, 409},
		{"DELETE", "/api/v1/auth/cinema/link", member, map[string]string{"password": householdTestPassword}, 409},
		{"POST", "/api/v1/auth/sessions", "", map[string]string{"username": "owner", "password": householdTestPassword}, 503},
	} {
		t.Run(action.path+action.method, func(t *testing.T) {
			householdExpect(t, householdHTTP(t, h, action.method, action.path, action.token, action.body), action.status)
		})
	}
	saved := householdGetRequest(t, h, member, request.ID)
	if saved.State != request.State || saved.Revision != request.Revision || !saved.Subscribed || saved.PublicNote != "" {
		t.Fatal("failed writes changed request", saved, request)
	}
	unchanged, err := db.AccountByID(ctx, account.ID)
	if err != nil || unchanged.Revision != account.Revision || unchanged.DisplayName != account.DisplayName {
		t.Fatal("failed writes changed account", unchanged, err)
	}
	rows, err := db.ListAccounts(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatal("failed invite created account", rows, err)
	}
	if _, err = db.AccountSession(ctx, digestSession(member)); err != nil {
		t.Fatal("failed edit revoked existing localidentity", err)
	}
	var titles int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_items`).Scan(&titles); err != nil || titles != 0 {
		t.Fatal("failed approval partially added title", titles, err)
	}
}

func TestHouseholdReadOutageAfterAuthenticationCannotExposeOrMutatePrivateState(t *testing.T) {
	_, db, token := householdHTTPAccount(t)
	ctx := context.Background()
	session, err := db.AccountSession(ctx, digestSession(token))
	if err != nil {
		t.Fatal(err)
	}
	server := New(Deps{Store: db, Settings: db, LibraryDiscovery: discover.NewLibrary(db, nil)})
	// Model a pool failure between middleware authentication and handler execution.
	// The request already owns a valid actor snapshot; each authoritative read
	// must still fail closed, rather than returning cached private data or success.
	if err = db.R.Close(); err != nil {
		t.Fatal(err)
	}
	actor := principal{Account: session.Account, Method: "local", Authenticated: true}
	for _, test := range []struct {
		name   string
		status int
		run    func(http.ResponseWriter, *http.Request)
	}{
		{"accounts", 503, server.ListHouseholdAccounts},
		{"connection", 503, server.GetCinemaConnection},
		{"link", 503, server.GetCinemaLink},
		{"requests", 503, func(w http.ResponseWriter, r *http.Request) {
			server.ListHouseholdRequests(w, r, apigen.ListHouseholdRequestsParams{})
		}},
		{"request", 404, func(w http.ResponseWriter, r *http.Request) { server.GetHouseholdRequest(w, r, 1) }},
		{"seeds", 503, server.HouseholdDiscoverySeeds},
		{"shelf", 409, server.HouseholdDiscoveryShelf},
		{"identity", 200, server.GetCurrentAccount},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "https://curator.example/?kind=movie&tmdbId=550&shelf=related", nil)
			req = req.WithContext(context.WithValue(ctx, principalContextKey{}, actor))
			rr := httptest.NewRecorder()
			test.run(rr, req)
			householdExpect(t, rr, test.status)
			if strings.Contains(rr.Body.String(), "passwordHash") || strings.Contains(rr.Body.String(), token) {
				t.Fatal("read outage disclosed credentials")
			}
		})
	}
	householdExpect(t, householdHTTP(t, server.Handler(), "GET", "/api/v1/household/requests", token, nil), 503)
	var count int
	if err = db.W.QueryRowContext(ctx, `SELECT count(*) FROM household_accounts`).Scan(&count); err != nil || count != 1 {
		t.Fatal("read outage mutated authority", count, err)
	}
}

func TestHouseholdRejectsUnverifiedRequestScopesAndOwnerPlans(t *testing.T) {
	h, db := newLibraryServer(t, householdSeriesProvider{stubProvider{configured: true}})
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/settings", "", map[string]any{"authUsername": "owner", "authPassword": householdTestPassword, "authRequired": true}), 204)
	owner := householdHTTPLogin(t, h, "owner", householdTestPassword)
	for _, body := range []map[string]any{
		{"kind": "book", "tmdbId": 100}, {"kind": "movie", "tmdbId": 0}, {"kind": "movie", "tmdbId": 550, "seasons": []int{1}},
		{"kind": "series", "tmdbId": 100}, {"kind": "series", "tmdbId": 100, "seasons": []int{1, 1}},
		{"kind": "series", "tmdbId": 100, "seasons": []int{0}}, {"kind": "series", "tmdbId": 100, "seasons": []int{2}},
	} {
		rr := householdHTTP(t, h, "POST", "/api/v1/household/requests", owner, body)
		if rr.Code != 400 && rr.Code != 409 {
			t.Fatal("unsafe scope accepted", body, rr.Code, rr.Body.String())
		}
	}
	for _, query := range []string{"kind=movie&q=", "kind=movie&q=" + strings.Repeat("x", 201)} {
		householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/household/catalogue?"+query, owner, nil), 503)
	}
	request := householdSubmitMovie(t, h, owner)
	path := fmt.Sprintf("/api/v1/household/requests/%d", request.ID)
	householdExpect(t, householdHTTP(t, h, "GET", path+"/review", owner, nil), 409)
	root := householdAddRoot(t, h, owner, t.TempDir())
	householdExpect(t, householdHTTP(t, h, "GET", fmt.Sprintf("%s/review?rootId=%d&profileId=999999", path, root.ID), owner, nil), 409)
	row, err := db.AccountByUsername(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	householdExpect(t, householdHTTP(t, h, "PATCH", fmt.Sprintf("/api/v1/household/accounts/%d", row.ID), owner, map[string]any{"revision": row.Revision, "role": "administrator", "password": householdTestPassword}), 409)
	householdExpect(t, householdHTTP(t, h, "PATCH", "/api/v1/household/accounts/999999", owner, map[string]any{"revision": 1, "password": householdTestPassword}), 404)
	householdExpect(t, householdHTTP(t, h, "PUT", path+"/note", owner, map[string]string{"note": strings.Repeat("x", 2001)}), 400)
	// Unknown fields and trailing bodies cannot silently authorize broader writes.
	for _, body := range []string{`{"kind":"movie","tmdbId":550,"monitor":"all"}`, `{"kind":"movie","tmdbId":550} {}`} {
		req := httptest.NewRequest("POST", "https://curator.example/api/v1/household/requests", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+owner)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		householdExpect(t, rr, 400)
	}
}

func TestCinemaNativeRevocationAndConnectionChangeCannotCompleteOldConsent(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	f := newCinemaFixture(t)
	config := map[string]string{"password": householdTestPassword, "baseUrl": f.server.URL, "clientId": f.client, "clientSecret": f.secret, "redirect": "https://curator.example/api/v1/auth/cinema/callback"}
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", owner, config), 204)
	state, cookie := cinemaStart(t, h, "/api/v1/auth/cinema/link", owner, map[string]string{"password": householdTestPassword})
	if rr := cinemaCallbackTest(t, h, f, state, owner, cookie); rr.Header().Get("Location") != "/account?cinema=linked" {
		t.Fatal(rr.Header())
	}
	verifier := strings.Repeat("a", 64)
	state, _ = cinemaStart(t, h, "/api/v1/auth/cinema/native", "", map[string]string{"challenge": cinemaChallenge(verifier)})
	householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/cinema/native/result", "", map[string]string{"state": state, "verifier": verifier}), 202)
	opened := householdHTTP(t, h, "GET", "/api/v1/auth/cinema/browser?state="+state, "", nil)
	householdExpect(t, opened, 303)
	cookie = opened.Result().Cookies()[0]
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/auth/cinema/browser?state="+state, "", nil), 409)
	ready := cinemaCallbackTest(t, h, f, state, "", cookie)
	completionURL, err := url.Parse(ready.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	completion := completionURL.Query().Get("code")
	if completion == "" {
		t.Fatal(ready.Header())
	}
	// Revoking an upstream grant between consent and native completion fails closed.
	f.mu.Lock()
	clear(f.active)
	f.mu.Unlock()
	householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/cinema/native/result", "", map[string]string{"state": state, "verifier": verifier, "completion": completion}), 503)
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/system/status", owner, nil), 200)
	// Replacing the integration invalidates every outstanding handoff generation.
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", owner, config), 204)
	householdExpect(t, householdHTTP(t, h, "POST", "/api/v1/auth/cinema/native/result", "", map[string]string{"state": state, "verifier": verifier, "completion": completion}), 409)
	account, err := db.AccountByUsername(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if account.Role != "owner" {
		t.Fatal("upstream revocation changed local privileges")
	}
	householdHTTPLogin(t, h, "owner", householdTestPassword)
}

func TestOwnerLibraryDiscoveryAndExplicitVolumeReviewRemainProtected(t *testing.T) {
	original, db, owner := householdHTTPAccount(t)
	member := householdEnroll(t, original, owner, "member")
	ctx := context.Background()
	root := householdAddRoot(t, original, owner, t.TempDir())
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Library seed", SortTitle: "library seed", IDs: domain.ExternalIDs{TMDB: 88, TVDB: 888, IMDB: "tt88"}, RootFolderID: root.ID, Path: filepath.Join(root.Path, "Seed"), QualityProfileID: 1})
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	source := changingLibraryProvider{during: func() { called++ }}
	h := New(Deps{Store: db, LibraryDiscovery: discover.NewLibrary(db, source)}).Handler()
	seeds := householdResult[struct {
		Items   []apigen.SearchResult
		HasMore bool
	}](t, householdHTTP(t, h, "GET", "/api/v1/discover/library/seeds?q=Library&offset=0", owner, nil), 200)
	if len(seeds.Items) != 1 || seeds.HasMore || seeds.Items[0].Availability == nil || seeds.Items[0].Availability.LibraryItemId == nil || *seeds.Items[0].Availability.LibraryItemId != id || !seeds.Items[0].InLibrary {
		t.Fatal(seeds)
	}
	path := fmt.Sprintf("/api/v1/discover/library/%d/items?kind=movie&tmdbId=88&shelf=related", id)
	shelf := householdResult[struct {
		Title, Source string
		Seed          apigen.SearchResult
		Items         []apigen.SearchResult
	}](t, householdHTTP(t, h, "GET", path, owner, nil), 200)
	if called != 1 || shelf.Source != "tmdb" || len(shelf.Items) != 1 || shelf.Items[0].Availability == nil || shelf.Items[0].Availability.Catalog != "absent" || !shelf.Seed.InLibrary {
		t.Fatal(shelf, called)
	}
	for _, query := range []string{"offset=-1", "offset=100001", "q=" + strings.Repeat("x", 201)} {
		householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/discover/library/seeds?"+query, owner, nil), 400)
	}
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/discover/library/0/items?kind=movie&tmdbId=88&shelf=related", owner, nil), 400)
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/discover/library/999999/items?kind=movie&tmdbId=88&shelf=related", owner, nil), 409)
	unavailable := New(Deps{Store: db}).Handler()
	householdExpect(t, householdHTTP(t, unavailable, "GET", path, owner, nil), 503)
	collectionPath := fmt.Sprintf("/api/v1/discover/library/%d/items?kind=movie&tmdbId=88&shelf=collection", id)
	householdExpect(t, householdHTTP(t, h, "GET", collectionPath, owner, nil), 200)
	householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/discover/library/seeds", member, nil), 403)
	volumePath := fmt.Sprintf("/api/v1/rootfolders/%d/volume", root.ID)
	householdExpect(t, householdHTTP(t, original, "GET", volumePath, member, nil), 403)
	initialResponse := householdHTTP(t, original, "GET", volumePath, owner, nil)
	initial := householdResult[library.VolumeReview](t, initialResponse, 200)
	if initialResponse.Header().Get("Cache-Control") != "no-store" || initial.Fingerprint == "" || initial.Blocker != "" {
		t.Fatal(initial, initialResponse.Header())
	}
	householdExpect(t, householdHTTP(t, original, "POST", volumePath, owner, map[string]any{"fingerprint": "invalid", "acknowledged": true}), 400)
	householdExpect(t, householdHTTP(t, original, "POST", volumePath, owner, map[string]any{"fingerprint": initial.Fingerprint, "acknowledged": false}), 409)
	householdExpect(t, householdHTTP(t, original, "POST", volumePath, owner, map[string]any{"fingerprint": strings.Repeat("0", 64), "acknowledged": true}), 409)
	householdExpect(t, householdHTTP(t, original, "POST", volumePath, owner, map[string]any{"fingerprint": initial.Fingerprint, "acknowledged": true}), 204)
	after := householdResult[library.VolumeReview](t, householdHTTP(t, original, "GET", volumePath, owner, nil), 200)
	if after.State.Generation != initial.State.Generation+1 || after.State.ExpectedIdentity != after.ObservedIdentity {
		t.Fatal("volume not bound exactly once", after)
	}
	householdExpect(t, householdHTTP(t, original, "POST", volumePath, owner, map[string]any{"fingerprint": initial.Fingerprint, "acknowledged": true}), 409)
	// Confirmation never adopts a missing volume, nor removes any existing bytes.
	if err = os.Rename(root.Path, root.Path+"-offline"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := os.Rename(root.Path+"-offline", root.Path); e != nil {
			t.Error(e)
		}
	})
	offline := householdResult[library.VolumeReview](t, householdHTTP(t, original, "GET", volumePath, owner, nil), 200)
	if offline.Blocker == "" {
		t.Fatal("missing volume appeared safe")
	}
	householdExpect(t, householdHTTP(t, original, "POST", volumePath, owner, map[string]any{"fingerprint": offline.Fingerprint, "acknowledged": true}), 409)
	householdExpect(t, householdHTTP(t, original, "GET", "/api/v1/rootfolders/999999/volume", owner, nil), 404)
	if err = db.R.Close(); err != nil {
		t.Fatal(err)
	}
	// A prior authenticated actor still cannot turn a discovery read error into facts.
	server := New(Deps{Store: db, LibraryDiscovery: discover.NewLibrary(db, source)})

	req := httptest.NewRequest("GET", "https://curator.example/", nil)
	rr := httptest.NewRecorder()
	server.ListLibraryDiscoverySeeds(rr, req, apigen.ListLibraryDiscoverySeedsParams{})
	householdExpect(t, rr, 503)
}
