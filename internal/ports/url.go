package ports

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// NormalizeURL makes user-entered service URLs usable: trims whitespace and
// trailing slashes, and assumes http:// when no scheme was given ("people
// paste bare IPs" is a fact of life, not an error). Empty stays empty.
//
// Use NormalizeServiceURL when the service has a well-known port; a bare
// hostname is far more common than a bare hostname that happens to be on 80.
func NormalizeURL(s string) string {
	return NormalizeServiceURL(s, 0)
}

// NormalizeServiceURL is NormalizeURL plus a default port.
//
// What a person types is a host — `plurxd`, `192.168.1.10`, `nzbd` — because
// that is the part they had to look up. The scheme and the port are things
// the software already knows, and making someone supply them is asking them
// to repeat a constant back to us. So: no scheme means http, and no port
// means defaultPort (when one is given; pass 0 for services with no
// convention).
//
// Two guards, both learned the hard way:
//
//   - A scheme the person supplied is respected in full, port included.
//     Appending a default port to `https://media.example.com` would break a
//     reverse-proxy setup that was already correct.
//   - A malformed URL comes back untouched rather than being "completed".
//     A sibling of this function once matched on the literal "://", so
//     `http:/host` — one mistyped slash — did not look like it had a scheme,
//     was treated as a hostname, and came out as `http://http:/host:7676`.
//     Turning a typo into nonsense is worse than leaving it alone: the person
//     can no longer see what they typed, and the error is about our guess.
func NormalizeServiceURL(s string, defaultPort int) string {
	typed := strings.TrimSpace(s)
	if typed == "" {
		return ""
	}

	// `http:/host` — a scheme and one slash. Unambiguous, and the commonest
	// way to mistype a URL.
	candidate := typed
	if scheme, rest, ok := strings.Cut(typed, ":"); ok &&
		isURLScheme(scheme) && strings.HasPrefix(rest, "/") && !strings.HasPrefix(rest, "//") {
		candidate = scheme + "://" + strings.TrimLeft(rest, "/")
	}

	hadScheme := strings.Contains(candidate, "://")
	if !hadScheme {
		// Something scheme-shaped with nothing usable after it (`http:`) is
		// not ours to guess at. A colon followed by digits is a port, not a
		// scheme, however scheme-shaped the word before it.
		if scheme, rest, ok := strings.Cut(typed, ":"); ok &&
			isURLScheme(scheme) && (rest == "" || strings.HasPrefix(rest, "/")) {
			return typed
		}
		candidate = "http://" + candidate
	}

	u, err := url.Parse(candidate)
	if err != nil || u.Hostname() == "" {
		return typed
	}
	if defaultPort > 0 && u.Port() == "" && !hadScheme {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(defaultPort))
	}
	return strings.TrimRight(u.String(), "/")
}

// isURLScheme reports the RFC 3986 scheme shape: alpha, then alphanumerics
// and +-.
func isURLScheme(s string) bool {
	if s == "" || !isAlpha(rune(s[0])) {
		return false
	}
	for _, c := range s {
		if !isAlpha(c) && !(c >= '0' && c <= '9') && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func isAlpha(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// DefaultPortFor is the port a service listens on when nobody says otherwise,
// so a settings field can accept a bare hostname.
//
// Kept in one place rather than sprinkled through the adapters, because these
// are facts about other applications and the place a reader looks for "what
// port does X use" should be a single table.
func DefaultPortFor(kind string) int {
	switch kind {
	case "qbittorrent", "sabnzbd":
		return 8080
	case "transmission":
		return 9091
	case "deluge":
		return 8112
	case "nzbget", "nzbd":
		return 6789
	case "plurx":
		return 32400
	case "plex":
		return 32400
	case "jellyfin":
		return 8096
	default:
		// Webhooks, Discord, and anything else that is a full URL by nature.
		return 0
	}
}

// RedactURL strips credentials from a URL so it can be shown on a status
// page.
//
// A connections panel exists to be looked at, screenshotted and pasted into
// an issue, and `http://admin:hunter2@nzbd:6789` in a client URL is exactly
// the kind of thing that rides along unnoticed. A debugging surface that
// leaks a credential is a regression, not a feature (plan §10.10).
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		// Unparseable is not a licence to print it. Anything with an "@"
		// before the host may carry a password, so it goes.
		if err != nil && strings.Contains(raw, "@") {
			return "(url hidden — it could not be parsed and may contain credentials)"
		}
		return raw
	}
	name := u.User.Username()
	if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.UserPassword(name, "***")
	} else {
		u.User = url.User(name)
	}
	return u.String()
}
