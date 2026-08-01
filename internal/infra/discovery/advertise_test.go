package discovery

import (
	"io"
	"log/slog"
	"slices"
	"testing"
)

func TestStartAdvertisesMonarrWithoutSecrets(t *testing.T) {
	var gotService, gotDomain string
	var gotPort int
	var gotText []string
	stopped := false
	stop, err := publish("Rack Monarr", 8787, "1.2.3", slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(_ string, service, domain string, port int, text []string) (func(), error) {
			gotService, gotDomain, gotPort, gotText = service, domain, port, text
			return func() { stopped = true }, nil
		})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if gotService != "_monarr._tcp" || gotDomain != "local." || gotPort != 8787 {
		t.Fatalf("registration = %q %q %d", gotService, gotDomain, gotPort)
	}
	for _, want := range []string{"api=1", "path=/", "version=1.2.3"} {
		if !slices.Contains(gotText, want) {
			t.Errorf("TXT record %q missing from %v", want, gotText)
		}
	}
	if !slices.ContainsFunc(gotText, func(value string) bool {
		return len(value) > len("host=.local") && value[:len("host=")] == "host=" && value[len(value)-len(".local"):] == ".local"
	}) {
		t.Errorf("local host TXT record missing from %v", gotText)
	}
	stop()
	if !stopped {
		t.Fatal("shutdown did not stop the advertisement")
	}
}

func TestPublishUsesRequestedInstanceName(t *testing.T) {
	var gotInstance string
	stop, err := publish("Basement Monarr", 7676, "1.2.3", slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(instance, _ string, _ string, _ int, _ []string) (func(), error) {
			gotInstance = instance
			return func() {}, nil
		})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer stop()
	if gotInstance != "Basement Monarr" {
		t.Fatalf("instance = %q", gotInstance)
	}
}

func TestLocalHostNameDoesNotDuplicateDomain(t *testing.T) {
	if got := localHostName("media.local."); got != "media.local" {
		t.Fatalf("localHostName() = %q", got)
	}
}
