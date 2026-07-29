package ports_test

import (
	"net/url"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

// What a person types is a host. The scheme and the port are constants the
// software already knows, and asking someone to supply them is asking them to
// repeat a fact back to us — which is also one more place to get it wrong.
func TestABareHostBecomesAUsableURL(t *testing.T) {
	cases := []struct {
		in   string
		port int
		want string
	}{
		// The whole point: a container name and nothing else.
		{"plurxd", 32400, "http://plurxd:32400"},
		{"nzbd", 6789, "http://nzbd:6789"},
		{"192.168.1.10", 8080, "http://192.168.1.10:8080"},
		{"  monarr/ ", 7676, "http://monarr:7676"},

		// A port given without a scheme is a port they chose.
		{"nzbd:9999", 6789, "http://nzbd:9999"},

		// A scheme given is a decision made, and it is respected in full.
		// Appending 32400 to an https URL behind a reverse proxy would break
		// a setup that was already correct.
		{"https://media.example.com", 32400, "https://media.example.com"},
		{"http://10.0.0.4:7676/", 7676, "http://10.0.0.4:7676"},

		// No convention for this service: complete the scheme, nothing else.
		{"hooks.example.com/abc", 0, "http://hooks.example.com/abc"},

		// Empty is a state, not a bad URL.
		{"", 6789, ""},
		{"   ", 6789, ""},
	}
	for _, c := range cases {
		if got := ports.NormalizeServiceURL(c.in, c.port); got != c.want {
			t.Errorf("NormalizeServiceURL(%q, %d) = %q, want %q", c.in, c.port, got, c.want)
		}
	}
}

// A typo must come back as a typo, never as nonsense.
//
// The sibling of this function in plurx matched on the literal "://", so
// `http:/monarr:7676` — one mistyped slash — did not look like it had a
// scheme, was treated as a bare hostname, and came back as
// `http://http:/monarr:7676:7676`. The person can then no longer see what
// they typed, and the error message is about our guess rather than their
// input.
func TestAMistypedURLIsRepairedOrLeftAlone(t *testing.T) {
	// One slash is unambiguous: nothing else could be meant.
	if got := ports.NormalizeServiceURL("http:/plurxd:32400", 32400); got != "http://plurxd:32400" {
		t.Errorf("single-slash repair = %q", got)
	}
	if got := ports.NormalizeServiceURL("https:/media.example.com", 32400); got != "https://media.example.com" {
		t.Errorf("single-slash https repair = %q", got)
	}

	// Anything unrepairable comes back exactly as typed.
	for _, bad := range []string{"http://", "http:", "://plurxd", ":32400", "http://:32400"} {
		if got := ports.NormalizeServiceURL(bad, 32400); got != bad {
			t.Errorf("NormalizeServiceURL(%q) = %q, want it returned untouched", bad, got)
		}
	}

	// And the property the original bug violated: nothing this produces from
	// a plausible input may fail to parse, or carry two ports.
	for _, in := range []string{
		"plurxd", "plurxd:32400", "http:/plurxd:32400", "http://plurxd:32400",
		"https://media.example.com/", "10.0.0.4", "[::1]:6789",
	} {
		out := ports.NormalizeServiceURL(in, 32400)
		u, err := url.Parse(out)
		if err != nil {
			t.Errorf("NormalizeServiceURL(%q) = %q, which does not parse: %v", in, out, err)
			continue
		}
		if u.Hostname() == "" {
			t.Errorf("NormalizeServiceURL(%q) = %q, which has no host", in, out)
		}
	}
}

// The port table is a set of facts about other applications, in one place
// rather than sprinkled through the adapters.
func TestDefaultPortsMatchTheApplications(t *testing.T) {
	want := map[string]int{
		"nzbd": 6789, "nzbget": 6789,
		"qbittorrent": 8080, "sabnzbd": 8080,
		"transmission": 9091, "deluge": 8112,
		"plurx": 32400, "plex": 32400, "jellyfin": 8096,
		// A webhook is a full URL by nature; there is nothing to default.
		"webhook": 0, "discord": 0,
	}
	for kind, port := range want {
		if got := ports.DefaultPortFor(kind); got != port {
			t.Errorf("DefaultPortFor(%q) = %d, want %d", kind, got, port)
		}
	}
}
