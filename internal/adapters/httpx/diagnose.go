package httpx

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
)

// Diagnose turns a connection failure into something a user can act on.
//
// Reachability tests fail for a handful of reasons and Go's wrapped errors
// state them accurately but unhelpfully: "dial tcp: lookup
// host.docker.internal: no such host" is precise and tells a homelab operator
// nothing about what to change. Each case below is a real configuration
// mistake with a specific remedy, and naming the remedy is the whole point of
// a Test button.
//
// The original error is always wrapped, never replaced — a hint that turns
// out to be wrong must not hide the evidence.
func Diagnose(err error, rawURL string) error {
	if err == nil {
		return nil
	}
	host := hostOf(rawURL)

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case isDockerInternal(host):
			return hintedError{err, "the name " + host + " only resolves from inside a " +
				"Docker container. If Monarr is running directly on the host, use the " +
				"machine's own address (localhost, or its LAN IP) instead"}
		case strings.Contains(host, "_"):
			return hintedError{err, "the hostname " + host + " contains an underscore, " +
				"which many resolvers reject — try the IP address"}
		default:
			return hintedError{err, "the hostname " + host + " could not be resolved from " +
				"where Monarr is running. Check spelling, or use an IP address"}
		}
	}

	if errors.Is(err, os.ErrDeadlineExceeded) || isTimeout(err) {
		return hintedError{err, "the connection to " + host + " timed out. Usually a firewall " +
			"or a wrong port — a refused connection fails instantly, a filtered one hangs"}
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return hintedError{err, "nothing accepted a connection at " + host +
			". The port is probably wrong, or the service is not running there"}
	}

	if strings.Contains(err.Error(), "connection refused") {
		return hintedError{err, "the connection to " + host + " was refused, so the address " +
			"is reachable but nothing is listening on that port"}
	}

	if strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), "tls") {
		return hintedError{err, "the TLS certificate at " + host + " was rejected. A " +
			"self-signed certificate needs http:// instead, or a trusted certificate"}
	}

	return err
}

// hintedError keeps the original error reachable through errors.Is/As while
// leading with the part a human can act on.
type hintedError struct {
	err  error
	hint string
}

func (e hintedError) Error() string { return e.hint + " (" + e.err.Error() + ")" }
func (e hintedError) Unwrap() error { return e.err }

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}

// isDockerInternal covers the host-gateway aliases Docker Desktop provides.
// They resolve only inside a container, which is exactly why they end up
// copied into a config that later runs somewhere else.
func isDockerInternal(host string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	switch strings.ToLower(name) {
	case "host.docker.internal", "gateway.docker.internal", "docker.for.mac.host.internal",
		"docker.for.win.host.internal":
		return true
	}
	return false
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
