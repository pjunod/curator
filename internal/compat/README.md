# internal/compat

The Sonarr/Radarr **v3 API compatibility shim** (Phase 4) — two personalities on one port,
distinguished by URL base:

```
/sonarr/api/v3/...   ← configure in ecosystem tools as Sonarr, URL base /sonarr
/radarr/api/v3/...   ← configure in ecosystem tools as Radarr, URL base /radarr
```

Translation layer **only**: handlers here map v3 DTOs onto native application services and own
no logic. Scope is limited to the endpoints Jellyseerr/Overseerr, Prowlarr, and Bazarr
actually call (see [ADR 0003](../../docs/adr/0003-compat-personalities.md) and blueprint §6),
verified by the conformance harness in `test/conformance/`. Unknown v3 requests are logged to
guide expansion. Dependency rule: compat imports app + domain only — never infra or adapters.
