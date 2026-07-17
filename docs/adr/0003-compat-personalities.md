# ADR 0003 — Sonarr/Radarr v3 compat as URL-base personalities

- **Status:** Accepted
- **Date:** 2026-07-17

## Context

Jellyseerr/Overseerr, Prowlarr, and Bazarr each speak to Sonarr and Radarr as *separate
servers* over their v3 APIs. Monarr is useful long before feature parity only if those tools
work against it from day one. Options considered: separate ports per personality, a single
merged API the tools don't understand, or two URL-base-scoped personalities on one port.

## Decision

Expose **two compat personalities on one port, distinguished by URL base**:

```
http://host:7676/sonarr/api/v3/...   ← configured in tools as Sonarr, URL base /sonarr
http://host:7676/radarr/api/v3/...   ← configured in tools as Radarr, URL base /radarr
```

Every mainstream ecosystem tool supports a URL base for *arr servers. Each personality reports
a plausible upstream `version` in `/system/status`, honors `X-Api-Key`, and matches routes
case-insensitively. Scope is limited to **endpoints the consumers actually call** (blueprint
§6), verified by a conformance harness running real Jellyseerr/Prowlarr/Bazarr.

The shim is a **translation layer only**: compat handlers map v3 DTOs onto native application
services, own no logic, and live quarantined in `internal/compat/{sonarr,radarr}`. The native
`/api/v1` stays clean and is the only API the UI uses.

## Consequences

- Ecosystem leverage arrives in Phase 4 without contaminating the native API design.
- Unknown v3 requests are logged (never guessed at) to guide shim expansion.
- The shim is treated as forever-beta; drift is caught by the conformance harness, not by
  chasing the full v3 surface.
