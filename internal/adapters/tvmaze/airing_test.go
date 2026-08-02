package tvmaze

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

// airingServer serves one hand-written show per TVDB id, one per shape the
// real API produces. Hand-written rather than recorded because the point of
// each is a field combination, and a recorded show carries four hundred
// lines of everything else.
func airingServer(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	shows := map[string]string{
		// A linear broadcaster: name, country, and therefore a zone.
		"1": `{"id":1,"name":"Broadcast Show",
		       "schedule":{"time":"21:00","days":["Sunday"]},
		       "network":{"name":"HBO","country":{"timezone":"America/New_York"}},
		       "webChannel":null}`,
		// A global streamer: a channel worth naming, no country, no slot.
		"2": `{"id":2,"name":"Stream Show","schedule":{"time":"","days":[]},
		       "network":null,
		       "webChannel":{"name":"Netflix","country":null}}`,
		// A broadcaster that publishes no time — network still worth having.
		"3": `{"id":3,"name":"Slotless Show","schedule":{"time":"","days":[]},
		       "network":{"name":"PBS","country":{"timezone":"America/New_York"}},
		       "webChannel":null}`,
		// A regional streamer: country present, so the stated slot is real.
		"4": `{"id":4,"name":"Regional Stream",
		       "schedule":{"time":"22:00","days":["Tuesday"]},
		       "network":null,
		       "webChannel":{"name":"BBC iPlayer","country":{"timezone":"Europe/London"}}}`,
		// A time with nothing to read it in. Must NOT become a time.
		"5": `{"id":5,"name":"Zoneless Show",
		       "schedule":{"time":"20:00","days":["Friday"]},
		       "network":{"name":"Mystery","country":null},
		       "webChannel":null}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		key := r.URL.Query().Get("thetvdb")
		if key == "" {
			key = r.URL.Query().Get("imdb")
		}
		body, ok := shows[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

func TestAiringShapes(t *testing.T) {
	base, _ := airingServer(t)
	c := New(base)

	cases := []struct {
		name                       string
		tvdb                       int64
		wantTime, wantZone, wantCh string
	}{
		{"broadcast network keeps time and zone", 1, "21:00", "America/New_York", "HBO"},
		{"global streamer names the channel and nothing else", 2, "", "", "Netflix"},
		{"no schedule still yields the network", 3, "", "America/New_York", "PBS"},
		{"regional streamer publishes a real slot", 4, "22:00", "Europe/London", "BBC iPlayer"},
		// A clock with no zone is a number, not a time: composing it would
		// read 20:00 in whatever zone the reader happens to sit in.
		{"a time with no zone is dropped", 5, "", "", "Mystery"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Airing(context.Background(), tc.tvdb, "")
			if err != nil {
				t.Fatal(err)
			}
			if got.Time != tc.wantTime || got.Timezone != tc.wantZone || got.Network != tc.wantCh {
				t.Errorf("Airing = %+v, want {%q %q %q}",
					got, tc.wantTime, tc.wantZone, tc.wantCh)
			}
		})
	}
}

func TestAiringFallsBackToIMDbAndAsksNothingWithoutIDs(t *testing.T) {
	base, hits := airingServer(t)
	c := New(base)

	// No TVDB id: the IMDb lookup is the second key, not a failure.
	got, err := c.Airing(context.Background(), 0, "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != "HBO" {
		t.Errorf("imdb lookup returned %+v, want the HBO show", got)
	}

	before := hits.Load()
	got, err = c.Airing(context.Background(), 0, "  ")
	if err != nil {
		t.Fatalf("no ids is not an error, got %v", err)
	}
	if got != (airingZero) {
		t.Errorf("no ids must yield the zero value, got %+v", got)
	}
	if hits.Load() != before {
		t.Error("a lookup with no key was sent; it can only 404")
	}
}

func TestAiringUnknownShowIsNotAnError(t *testing.T) {
	base, _ := airingServer(t)
	// 404 for an unknown id — the adapter surfaces it as an error, which the
	// library treats as "no schedule known" and carries on. What must not
	// happen is a panic or a half-filled struct.
	got, err := New(base).Airing(context.Background(), 999, "")
	if err == nil && got != airingZero {
		t.Errorf("unknown show yielded %+v", got)
	}
}

// airingZero names the "we know nothing" answer, so a test asserting it
// reads as intent rather than as an empty struct literal.
var airingZero = ports.Airing{}
