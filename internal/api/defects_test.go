package api

import (
	"net/http"
	"testing"
)

// Regression tests for the five defects the coverage work turned up. Each one
// failed before its fix, so each one is written to fail again if the fix is
// undone rather than merely to exercise the path.

// The lockout. Enabling auth checked for a username and not for a password,
// but Login refuses on `wantUser == "" || wantHash == ""` — so a username
// with an empty password turned auth ON and then answered every login with
// "no credentials configured". On an install with no API key set, the only
// way back in was editing app_meta by hand.
func TestDefectAuthCannotBeEnabledWithoutAPassword(t *testing.T) {
	e := newAPIEnv(t)

	// A username alone must not be enough to lock the door.
	e.put(t, "/api/v1/settings", `{"authUsername":"paul","authRequired":true}`).
		expect(t, http.StatusBadRequest)

	// ...and it must genuinely still be off, not merely reported as refused.
	var settings struct {
		AuthRequired bool `json:"authRequired"`
	}
	e.get(t, "/api/v1/settings").expect(t, http.StatusOK).into(t, &settings)
	if settings.AuthRequired {
		t.Fatal("auth is on with no password stored — this is the lockout")
	}

	// The whole credential pair in one request is accepted, and then login
	// works, which is the property the guard exists to protect.
	e.put(t, "/api/v1/settings",
		`{"authUsername":"paul","authPassword":"correct horse","authRequired":true}`).
		expect(t, http.StatusNoContent)
	// 204: the session is the cookie, so there is nothing to return.
	e.post(t, "/api/v1/auth/login", `{"username":"paul","password":"correct horse"}`).
		expect(t, http.StatusNoContent)
}

// A password on its own is the mirror image and must fail the same way: the
// guard reads both settings, so neither half alone may flip it.
func TestDefectAuthCannotBeEnabledWithoutAUsername(t *testing.T) {
	e := newAPIEnv(t)
	e.put(t, "/api/v1/settings", `{"authPassword":"secret","authRequired":true}`).
		expect(t, http.StatusBadRequest)
}

// The mass editor counted ids it had not touched: `UPDATE ... WHERE id = ?`
// succeeds against nothing, so the handler's err == nil branch incremented for
// rows that never existed and the UI reported changing them.
func TestDefectBulkEditCountsOnlyRowsItChanged(t *testing.T) {
	e := newAPIEnv(t)
	id := e.addMovie(t)

	var res struct {
		Updated int `json:"updated"`
	}
	e.post(t, "/api/v1/library/bulk", `{"ids":[999999],"monitored":false}`).
		expect(t, http.StatusOK).into(t, &res)
	if res.Updated != 0 {
		t.Errorf("updated = %d for an id that does not exist, want 0", res.Updated)
	}

	// A real id still counts, and a mixed batch counts only the real one —
	// otherwise the fix would just be "always report zero".
	e.post(t, "/api/v1/library/bulk", `{"ids":[999999],"monitored":false}`)
	body := `{"ids":[` + itoa(id) + `,999999],"monitored":false}`
	e.post(t, "/api/v1/library/bulk", body).expect(t, http.StatusOK).into(t, &res)
	if res.Updated != 1 {
		t.Errorf("updated = %d for one real id and one bogus, want 1", res.Updated)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// Three refusals answered 500 because the service error carried no sentinel
// for libraryErr to match, so a typo was reported as a server fault. They are
// all one kind of thing — the request is wrong — and now share one sentinel.
func TestDefectUserErrorsAreNotServerErrors(t *testing.T) {
	e := newAPIEnv(t)

	t.Run("relative manual-entry path", func(t *testing.T) {
		e.post(t, "/api/v1/library/manual",
			`{"kind":"movie","title":"Home Video","path":"relative/path"}`).
			expect(t, http.StatusBadRequest)
	})

	t.Run("blank manual title", func(t *testing.T) {
		// Previously wrapped in ErrNotFound, so a missing title answered 404 —
		// which says the endpoint does not exist, not that the body is short.
		e.post(t, "/api/v1/library/manual",
			`{"kind":"movie","title":"  ","path":"`+e.root+`/Thing"}`).
			expect(t, http.StatusBadRequest)
	})

	t.Run("unknown root folder kind", func(t *testing.T) {
		var root struct {
			ID int64 `json:"id"`
		}
		e.get(t, "/api/v1/rootfolders").expect(t, http.StatusOK)
		var roots []struct {
			ID int64 `json:"id"`
		}
		e.get(t, "/api/v1/rootfolders").into(t, &roots)
		if len(roots) == 0 {
			t.Fatal("harness should have registered a root folder")
		}
		root.ID = roots[0].ID
		e.patch(t, "/api/v1/rootfolders/"+itoa(root.ID), `{"kind":"film"}`).
			expect(t, http.StatusBadRequest)
	})
}

// Deleting a profile some kind defaults to is the same class of refusal as
// deleting one still in use — a live reference the caller must clear first.
// Only ErrProfileInUse was mapped, so this answered 500 while printing a
// perfectly actionable message.
func TestDefectDeletingADefaultProfileIsAConflict(t *testing.T) {
	e := newAPIEnv(t)

	var created struct {
		ID int64 `json:"id"`
	}
	e.post(t, "/api/v1/profiles",
		`{"name":"Throwaway","target":{"source":"webdl","resolution":1080},"upgradesAllowed":true}`).
		expect(t, http.StatusCreated).into(t, &created)

	e.put(t, "/api/v1/settings",
		`{"defaultProfiles":{"movie":`+itoa(created.ID)+`}}`).
		expect(t, http.StatusNoContent)

	e.del(t, "/api/v1/profiles/"+itoa(created.ID)).expect(t, http.StatusConflict)
}

// A list of a type no syncer knows about used to store happily, appear in the
// UI, and never sync — importlist.syncOne answers "unknown list type" into a
// log nobody reads. Every sibling create already called the generated
// Valid(); this one did not.
func TestDefectImportListTypeIsValidated(t *testing.T) {
	e := newAPIEnv(t)

	e.post(t, "/api/v1/importlists", `{"name":"Nonsense","type":"letterboxd"}`).
		expect(t, http.StatusBadRequest)

	// A known type still works, so the guard is not simply refusing everything.
	e.post(t, "/api/v1/importlists", `{"name":"Popular","type":"tmdb-popular"}`).
		expect(t, http.StatusCreated)
}

// DELETE of an id that never existed answered 204 on five endpoints and 404 on
// seven, and the spec documented a 404 for the seven. The five were the odd
// ones out: they told the caller they had removed something that was never
// there. All thirteen now agree.
func TestDefectDeletingSomethingAbsentIsNotFound(t *testing.T) {
	e := newAPIEnv(t)
	for _, path := range []string{
		"/api/v1/indexers/999999",
		"/api/v1/downloadclients/999999",
		"/api/v1/customformats/999999",
		"/api/v1/importlists/999999",
		"/api/v1/blocklist/999999",
		// Already correct before this change — here so the group is asserted
		// as one policy rather than as five special cases.
		"/api/v1/notifiers/999999",
		"/api/v1/profiles/999999",
	} {
		t.Run(path, func(t *testing.T) {
			e.del(t, path).expect(t, http.StatusNotFound)
		})
	}
}
