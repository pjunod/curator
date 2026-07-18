package ports

import "strings"

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
