package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecommendationStrictBody(t *testing.T) {
	for _, body := range []string{
		`{"kind":"series","query":null}`, `{"kind":"series","limit":null}`,
		`{"kind":"series","filters":{"hideInLibrary":null}}`,
		`{"kind":"series","filters":{"secret":true}}`,
		`{"kind":"series","seed":{"provider":"tmdb","id":"1","extra":true}}`,
		`{"kind":"series","query":"` + strings.Repeat("a", 17000) + `"}`,
		`{"kind":"series"} {"kind":"series"}`,
	} {
		r := httptest.NewRequest("POST", "/api/v1/metadata/recommendations", strings.NewReader(body))
		if _, err := decodeRecommendation(httptest.NewRecorder(), r); err == nil {
			t.Fatalf("accepted %s", body[:min(len(body), 100)])
		}
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"kind":"series","query":"gay series","filters":{"theme":null,"genres":[],"excludeTeenFocus":false}}`))
	v, err := decodeRecommendation(httptest.NewRecorder(), r)
	if err != nil || !v.Explicit["theme"] || !v.Explicit["genres"] || !v.Explicit["excludeTeenFocus"] {
		t.Fatalf("explicit clears lost: %+v %v", v, err)
	}
}
