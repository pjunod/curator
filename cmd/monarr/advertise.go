package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/pjunod/monarr/internal/buildinfo"
	"github.com/pjunod/monarr/internal/infra/config"
)

type discoveryPublisher func(string, int, string, *slog.Logger) (func(), error)

type advertiseOptions struct {
	name string
	port int
}

// runAdvertise is the Docker bridge companion: it publishes the API already
// mapped onto the host, without opening the database or starting a second
// Monarr server. The process lifetime is the DNS-SD record lifetime.
func runAdvertise(
	ctx context.Context,
	args []string,
	output io.Writer,
	log *slog.Logger,
	publish discoveryPublisher,
) error {
	opts, err := parseAdvertiseOptions(args, output)
	if err != nil {
		return err
	}
	shutdown, err := publish(opts.name, opts.port, buildinfo.Version, log)
	if err != nil {
		return fmt.Errorf("advertise Monarr on port %d: %w", opts.port, err)
	}
	defer shutdown()
	log.Info("discovery companion ready", "name", opts.name, "port", opts.port)
	<-ctx.Done()
	return nil
}

func parseAdvertiseOptions(args []string, output io.Writer) (advertiseOptions, error) {
	flags := flag.NewFlagSet("monarr advertise", flag.ContinueOnError)
	flags.SetOutput(output)
	name := flags.String("name", "", "service name shown to nearby devices")
	port := flags.Int("port", config.DefaultPort, "host port where Monarr is reachable")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(output, "Usage: monarr advertise [--name NAME] [--port PORT]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return advertiseOptions{}, err
	}
	if flags.NArg() != 0 {
		return advertiseOptions{}, fmt.Errorf("advertise: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *port < 1 || *port > 65535 {
		return advertiseOptions{}, fmt.Errorf("advertise: port %d is outside 1-65535", *port)
	}
	return advertiseOptions{name: strings.TrimSpace(*name), port: *port}, nil
}
