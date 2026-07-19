// Package httpx builds the HTTP clients every outbound adapter uses, so
// they all identify Monarr the same way. Without this, Go's net/http
// stamps "Go-http-client/1.1" as the User-Agent, which is what indexers
// and download clients end up logging for Monarr's traffic.
//
// It lives under adapters (not infra) so adapter packages may import it
// without crossing the dependency arrows enforced by internal/arch_test.go.
package httpx

import (
	"net/http"
	"time"

	"github.com/monarr-media/monarr/internal/buildinfo"
)

// UserAgent is Monarr's outbound identity, e.g. "Monarr/0.3.0". Indexers
// and download clients see and log this instead of Go's default.
func UserAgent() string {
	return "Monarr/" + buildinfo.Version
}

// uaTransport sets Monarr's User-Agent on every request that doesn't
// already carry one, then delegates to base.
type uaTransport struct {
	base http.RoundTripper
	ua   string
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		// A RoundTripper must not mutate the caller's request, so clone
		// before adding the header.
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", t.ua)
	}
	return t.base.RoundTrip(req)
}

// Transport wraps base so every request carries Monarr's User-Agent. A
// nil base means http.DefaultTransport.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &uaTransport{base: base, ua: UserAgent()}
}

// NewClient returns an http.Client with the given timeout that identifies
// Monarr on every request.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport(nil)}
}

// NewClientJar is NewClient with a cookie jar, for clients (qBittorrent,
// Deluge) that authenticate with a session cookie.
func NewClientJar(timeout time.Duration, jar http.CookieJar) *http.Client {
	return &http.Client{Timeout: timeout, Jar: jar, Transport: Transport(nil)}
}
