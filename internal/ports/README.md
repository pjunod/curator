# internal/ports

Driven-port interfaces — the seams between the application core and the outside world. One
interface per file; adapters live in sibling packages under `internal/adapters/` and are the
only code allowed to implement them against real services.

Planned ports (blueprint §5):

| Port | Shape | First adapter | Phase |
|---|---|---|---|
| `MetadataProvider` | search + hydrate movie/series/episodes | TMDB | 1 |
| `Indexer` | `Search(ctx, SearchQuery)`, `FetchRSS(ctx)` | Newznab/Torznab (one adapter) | 2 |
| `DownloadClient` | `Add`, `Statuses`, `Remove` | qBittorrent, SABnzbd | 2 |
| `Notifier` | `OnEvent(ctx, Event) error` off the bus | webhook, Discord | 3 |
| `ImportListProvider` | list wanted media from external services | Trakt/TMDB lists | 5 |
| `DiscoverProvider` | publish a catalogue of curated rows, and serve one | TMDB, Trakt | 9 |
| `AiringProvider` | a series' broadcast slot: wall-clock time, timezone, network | TVmaze | 3 (rebuilt) |

Interfaces are added here together with their first consumer, not speculatively — signatures
frozen before a real adapter exists tend to be wrong. Dependency rules (enforced by
`internal/arch_test.go`): ports may import domain and stdlib only.
