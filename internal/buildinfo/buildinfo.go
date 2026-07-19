// Package buildinfo carries the version and commit stamped in at build
// time via -ldflags (see scripts/version.sh, the Makefile, and
// deploy/Dockerfile). It is a dependency-free leaf so every layer that
// needs the version — main, api, adapters — reads the same values.
package buildinfo

var (
	// Version is the human-readable version, e.g. "0.3.0" or
	// "0.3.0-2-gabc1234". "dev" when built without ldflags.
	Version = "dev"
	// Commit is the short hash the binary was built from.
	Commit = "none"
)
