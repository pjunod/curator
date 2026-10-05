package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/ports"
)

type editorialStub struct{ err error }

func (f editorialStub) Feed(context.Context) (ports.EditorialFeed, error) {
	return ports.EditorialFeed{URL: "https://dekkoo.blog/feed/", Categories: []string{"Gay Series"},
		Items:     []ports.EditorialArticle{{Title: "A series article", URL: "https://dekkoo.blog/series/", Categories: []string{"Gay Series"}}},
		FetchedAt: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), Stale: true}, f.err
}
func TestDekkooFeedContractAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source ports.EditorialSource
		status int
	}{
		{"articles", editorialStub{}, 200},
		{"upstream failure", editorialStub{err: errors.New("private upstream detail")}, 502},
		{"not installed", nil, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Deps{Dekkoo: tc.source})
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/discover/dekkoo", nil))
			if rr.Code != tc.status {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
			}
			if rr.Code == 200 {
				var body apigen.EditorialFeed
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if len(body.Items) != 1 || !body.Stale || body.Items[0].PublishedAt != nil {
					t.Fatalf("body=%+v", body)
				}
			}
		})
	}
}
