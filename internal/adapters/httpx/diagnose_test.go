package httpx

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"
)

// Each case is a real configuration mistake. The test asserts the remedy is
// named, because "no such host" is accurate and useless.
func TestDiagnoseNamesTheRemedy(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		url  string
		want []string // substrings that must appear
	}{
		{
			name: "docker host alias from outside a container",
			err:  &net.DNSError{Err: "no such host", Name: "host.docker.internal", IsNotFound: true},
			url:  "http://host.docker.internal:6789",
			want: []string{"only resolves from inside a", "localhost"},
		},
		{
			name: "windows docker alias too",
			err:  &net.DNSError{Err: "no such host", Name: "docker.for.win.host.internal"},
			url:  "http://docker.for.win.host.internal:6789",
			want: []string{"only resolves from inside a"},
		},
		{
			name: "ordinary typo",
			err:  &net.DNSError{Err: "no such host", Name: "nzbgett"},
			url:  "http://nzbgett:6789",
			want: []string{"could not be resolved", "IP address"},
		},
		{
			name: "underscore in hostname",
			err:  &net.DNSError{Err: "no such host", Name: "my_nas"},
			url:  "http://my_nas:6789",
			want: []string{"underscore"},
		},
		{
			name: "refused",
			err:  errors.New("dial tcp 192.168.1.10:6789: connect: connection refused"),
			url:  "http://192.168.1.10:6789",
			want: []string{"refused", "nothing is listening"},
		},
		{
			name: "timeout points at a firewall",
			err:  os.ErrDeadlineExceeded,
			url:  "http://192.168.1.10:6789",
			want: []string{"timed out", "firewall"},
		},
		{
			name: "tls",
			err:  errors.New("x509: certificate signed by unknown authority"),
			url:  "https://nas:6789",
			want: []string{"certificate", "http://"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Diagnose(tc.err, tc.url)
			if got == nil {
				t.Fatal("want an error back")
			}
			for _, want := range tc.want {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("message %q does not mention %q", got.Error(), want)
				}
			}
			// The original must stay reachable: a hint that turns out to be
			// wrong must not hide the evidence.
			if !errors.Is(got, tc.err) && !strings.Contains(got.Error(), tc.err.Error()) {
				t.Errorf("original error lost: %v", got)
			}
		})
	}
}

func TestDiagnosePassesThroughWhatItCannotExplain(t *testing.T) {
	err := errors.New("nzbget: authentication rejected (401)")
	if got := Diagnose(err, "http://nas:6789"); got.Error() != err.Error() {
		t.Errorf("an already-clear error should be left alone, got %q", got.Error())
	}
	if Diagnose(nil, "http://nas:6789") != nil {
		t.Error("nil in, nil out")
	}
}
