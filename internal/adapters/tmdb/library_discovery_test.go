package tmdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

func TestLibraryRecommendationsUseKindAndBoundResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movie/100/recommendations" && r.URL.Path != "/tv/100/recommendations" {
			t.Errorf("path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("page") != "1" {
			t.Error("unbounded pagination")
		}
		_, _ = fmt.Fprint(w, `{"results":[{"id":100,"title":"seed","name":"seed"},{"id":1,"title":"Movie","name":"Show"},{"id":1,"title":"duplicate"}`)
		for i := 2; i < 40; i++ {
			_, _ = fmt.Fprintf(w, `,{"id":%d,"title":"Movie","name":"Show"}`, i)
		}
		_, _ = fmt.Fprint(w, `]}`)
	}))
	defer srv.Close()
	c := New(srv.URL, staticKey("key"))
	for _, kind := range []domain.MediaKind{domain.KindMovie, domain.KindSeries} {
		rows, err := c.Related(context.Background(), kind, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 30 || rows[0].Kind != kind || rows[0].TMDBID != 1 {
			t.Fatalf("%s %+v", kind, rows)
		}
	}
	if _, err := c.Related(context.Background(), domain.KindBook, 100); err == nil {
		t.Fatal("book accepted as movie")
	}
}
func TestLibraryCollectionRejectsFalseMembership(t *testing.T) {
	for _, tc := range []struct {
		name, collection string
		bad              bool
	}{{"good", `{"id":9,"name":"Pair","parts":[{"id":100,"title":"Seed"},{"id":101,"title":"Other"}]}`, false}, {"wrong collection", `{"id":10,"parts":[{"id":100,"title":"Seed"}]}`, true}, {"missing seed", `{"id":9,"parts":[{"id":101,"title":"Other"}]}`, true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/movie/100":
					_, _ = fmt.Fprint(w, `{"id":100,"belongs_to_collection":{"id":9}}`)
				case "/collection/9":
					_, _ = fmt.Fprint(w, tc.collection)
				default:
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			got, err := New(srv.URL, staticKey("key")).MovieCollection(context.Background(), 100)
			if (err != nil) != tc.bad {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestLibraryMetadataCacheDoesNotRepollForLocalRefreshes(t *testing.T) {
	calls := 0
	key := "first-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = fmt.Fprint(w, `{"results":[{"id":101,"title":"Related"}]}`)
	}))
	defer srv.Close()
	c := New(srv.URL, func(context.Context) (string, error) { return key, nil })
	for i := 0; i < 3; i++ {
		if _, err := c.Related(context.Background(), domain.KindMovie, 100); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("local refresh repeated %d provider requests", calls)
	}
	key = "second-key"
	if _, err := c.Related(context.Background(), domain.KindMovie, 100); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("credential change reused old metadata: %d", calls)
	}
}
