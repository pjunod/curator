// Package discovery advertises a running Monarr server over DNS-SD/mDNS.
// Clients discover the service instead of guessing IPv4 subnet boundaries;
// the DNS-SD response can carry both IPv4 and IPv6 addresses.
package discovery

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/betamos/zeroconf"
)

const (
	ServiceType = "_monarr._tcp"
	Domain      = "local."
)

type registerFunc func(instance, service, domain string, port int, text []string) (func(), error)

// Start publishes the HTTP endpoint until the returned function is called.
// Advertising is best-effort: a locked-down multicast interface must not stop
// the media server itself from starting.
func Start(port int, version string, log *slog.Logger) func() {
	return start(port, version, log, func(instance, service, domain string, port int, text []string) (func(), error) {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid service port %d", port)
		}
		record := zeroconf.NewService(zeroconf.NewType(service+"."+trimDot(domain)), instance, uint16(port))
		record.Text = text
		client, err := zeroconf.New().Logger(log).Publish(record).Open()
		if err != nil {
			return nil, err
		}
		return func() {
			if err := client.Close(); err != nil {
				log.Warn("local discovery shutdown failed", "service", ServiceType, "err", err)
			}
		}, nil
	})
}

func start(port int, version string, log *slog.Logger, register registerFunc) func() {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "server"
	}
	instance := fmt.Sprintf("Monarr on %s", hostname)
	// The Android DNSSD resolver exposes every A/AAAA answer but currently
	// reports the service instance as its host. Carry the real SRV target in
	// TXT as a portable fallback, especially for scoped link-local IPv6 where
	// clients should use the .local name instead of constructing a zone id.
	serviceHost := localHostName(hostname)
	shutdown, err := register(instance, ServiceType, Domain, port, []string{
		"api=1",
		"host=" + serviceHost,
		"path=/",
		"version=" + version,
	})
	if err != nil {
		log.Warn("local discovery unavailable", "service", ServiceType, "err", err)
		return func() {}
	}
	log.Info("local discovery advertised", "service", ServiceType, "port", port)
	return shutdown
}

func localHostName(hostname string) string {
	host := trimDot(hostname)
	domain := trimDot(Domain)
	if !strings.HasSuffix(strings.ToLower(host), "."+strings.ToLower(domain)) {
		host += "." + domain
	}
	return host
}

func trimDot(value string) string {
	for len(value) > 0 && value[len(value)-1] == '.' {
		value = value[:len(value)-1]
	}
	return value
}
