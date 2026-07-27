package ports

import (
	"net/url"
	"strings"
)

// NormalizeURL makes user-entered service URLs usable: trims whitespace and
// trailing slashes, and assumes http:// when no scheme was given ("people
// paste bare IPs" is a fact of life, not an error). Empty stays empty.
func NormalizeURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return strings.TrimRight(s, "/")
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
