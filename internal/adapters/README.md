# internal/adapters

Implementations of the interfaces in `internal/ports`, one package per external service:

```
torznab/        Newznab + Torznab in one adapter (the protocols are near-identical)   Phase 2
qbittorrent/    torrent download client (WebUI API)                                   Phase 2
sabnzbd/        usenet download client                                                Phase 2
tmdb/           metadata provider for both movies and TV                              Phase 1
webhook/        generic outbound notifier                                             Phase 3
discord/        Discord notifier                                                      Phase 3
mediaserver/    Plex/Jellyfin library-refresh notifiers                               Phase 3
```

Dependency rules (enforced by `internal/arch_test.go`): adapters import ports + domain +
stdlib + third-party client libraries only — never `app`, `infra`, `api`, or `compat`.
Contract tests run against real services in containers on a CI schedule (blueprint §8).
