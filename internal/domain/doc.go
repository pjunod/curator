// Package domain is Monarr's functional core: entities and pure logic with
// no I/O, no clock, no globals, and no knowledge of HTTP, SQL, or the
// filesystem.
//
// The rules (enforced mechanically by internal/arch_test.go):
//
//   - domain imports nothing but the standard library and other domain
//     packages. Not app, not infra, not adapters, not ports.
//   - Everything here must be a pure function of its inputs. Anything that
//     needs a clock takes time.Time as a parameter; anything that needs
//     randomness takes a seed.
//
// What will live here, phase by phase (blueprint §4):
//
//   - The Wantable contract — "a thing the system wants on disk at a given
//     quality". A movie is one Wantable; an episode is one Wantable; a season
//     is also a Wantable (the season-pack search target) that satisfies many
//     episode Wantables at import time. The entire acquisition pipeline is
//     built against this interface and never against movies or episodes
//     directly. Media-kind knowledge is quarantined behind exactly two
//     interfaces: SearchPlanner and ReleaseMatcher.
//   - parser/   — ParseReleaseTitle(string) → ParsedRelease. Table-driven,
//     golden-tested against the upstream corpus in testdata/releases/.
//   - decision/ — Decide(release, profile, currentFiles, blocklist, queue) →
//     Accept | Reject{reasons} | UpgradeFor{files}. Every rejection carries a
//     machine-readable reason code surfaced in the UI.
//   - naming/   — Render(template, mediaItem, fileAttrs) → relative path,
//     token-compatible with Sonarr/Radarr naming schemes.
//
// These arrive with Phases 1–2. Phase 0 keeps this package intentionally
// empty so no speculative types get frozen before their first real consumer.
package domain
