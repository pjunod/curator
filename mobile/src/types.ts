export type MediaKind = 'movie' | 'series' | 'book'
export type BookType = 'ebook' | 'audiobook'

export interface BookEditionSummary {
  bookType: BookType
  monitored: boolean
  fileCount: number
}
export type HealthStatus = 'ok' | 'warning' | 'error'

export interface Connection {
  baseUrl: string
  apiKey: string
}

export interface SystemStatus {
  appName: string
  version: string
  commit: string
  goVersion: string
  os: string
  arch: string
  startedAt: string
  uptimeSeconds: number
  dataDir: string
  dbSchemaVersion: number
}

export interface HealthCheck {
  name: string
  status: HealthStatus
  message?: string
  checkedAt: string
}

export interface HealthReport {
  overall: HealthStatus
  checks: HealthCheck[]
}

export interface Rating {
  source: string
  value: number
  votes?: number
  scale: number
}

export interface MediaItemSummary {
  id: number
  kind: MediaKind
  title: string
  year: number
  author: string
  bookType?: BookType
  bookTypes?: BookType[]
  bookEditions?: BookEditionSummary[]
  posterPath: string
  monitored: boolean
  path: string
  rating: number
  ratingVotes: number
  ratings: Rating[]
  source?: string
  quality?: string
  qualityTarget?: string
  qualityVerified?: boolean
  upgrade?: '' | 'missing' | 'seeking' | 'met' | 'capped'
  episodeCount: number
  episodeFileCount: number
  fileCount: number
  addedAt: string
}

export interface EpisodeInfo {
  id: number
  seasonNumber: number
  episodeNumber: number
  title: string
  airDate: string
  monitored: boolean
  hasFile: boolean
}

export interface SeasonInfo {
  number: number
  monitored: boolean
  episodes: EpisodeInfo[]
}

export interface MediaFileInfo {
  id: number
  copyId?: number
  path: string
  size: number
  episodeIds: number[]
  quality?: string
  provenanceLabel?: string
  verified?: boolean
  facts?: string
  implausible?: string
  sourceRelease?: string
}

export interface MediaCopy {
  id: number
  bookType?: BookType
  name: string
  qualityProfileId: number
  rootFolderId: number
  path: string
  monitored: boolean
}

export interface MediaItemDetail extends Omit<MediaItemSummary, 'episodeCount' | 'episodeFileCount' | 'fileCount'> {
  backdropPath: string
  overview: string
  genres: string[]
  status: string
  releaseDate: string
  runtime: number
  qualityProfileId: number
  downloadPriority: number
  downloadPriorityOverride: number | null
  rootFolderId: number
  ended: boolean
  seasons: SeasonInfo[]
  files: MediaFileInfo[]
  copies: MediaCopy[]
  ids: { tmdb: number; tvdb?: number; imdb?: string; isbn13?: string; olid?: string; asin?: string }
  aliases: TitleAlias[]
  countries: CountryEvidence[]
  identitySources: IdentitySourceStatus[]
}

export interface TitleAlias {
  id: number
  title: string
  source: string
  sourceId: string
  language: string
  marketCountry: string
  scope: 'work' | 'season' | 'unsupported_numbering'
  role: 'original' | 'alternate' | 'historical' | 'manual'
  searchable: boolean
}

export interface CountryEvidence { code: string; source: string; basis: string }
export interface IdentitySourceStatus {
  source: string
  countries: CountryEvidence[]
  fetchedAt?: string
  attemptedAt?: string
  retryAfter?: string
  lastError: string
}

export interface UpdateMediaItemRequest {
  monitored?: boolean
  qualityProfileId?: number
  downloadPriority?: number
  inheritDownloadPriority?: boolean
  rootFolderId?: number
  path?: string
}

export interface AddMediaCopyRequest {
  bookType?: BookType
  qualityProfileId: number
  rootFolderId?: number
  name?: string
  monitored?: boolean
}

export interface UpdateMediaCopyRequest {
  name?: string
  qualityProfileId?: number
  monitored?: boolean
}

export interface SearchResult {
  kind: MediaKind
  tmdbId: number
  tvdbId?: number
  imdbId?: string
  source?: string
  hydrationSource?: string
  olid?: string
  author?: string
  title: string
  year: number
  overview: string
  posterPath: string
  inLibrary: boolean
  bookTypes?: BookType[]
}


/** Read-only display metadata. Never merge this into a SearchResult or Add request. */
export interface MetadataPreview {
  kind: MediaKind
  title: string
  overview: string
  year?: number
  author?: string
  posterPath?: string
  previewSource?: string
  genres?: string[]
  status?: string
  runtimeMinutes?: number
  ids: { tmdb?: number; tvdb?: number; imdb?: string; olid?: string }
  ownership: 'absent' | 'present' | 'ambiguous' | 'unknown'
  libraryItemId?: number
  bookTypes?: BookType[]
  addability: 'supported' | 'unsupported' | 'conflict'
  addBlockReason?: string
}

export interface AddMediaRequest {
  kind: MediaKind
  tmdbId?: number
  tvdbId?: number
  imdbId?: string
  hydrationSource?: string
  olid?: string
  bookType?: BookType
  rootFolderId?: number
  qualityProfileId?: number
  downloadPriority?: number
  monitored?: boolean
  monitor?: 'all' | 'latest' | 'none'
  searchNow?: boolean
}

export interface DiscoverList {
  id: string
  title: string
  blurb: string
  kind: MediaKind
  source: string
}

export interface RootFolder {
  id: number
  path: string
  kind: MediaKind | 'mixed'
  freeBytes: number
  accessible: boolean
}

export interface QualityProfile {
  id: number
  name: string
  sentence: string
  upgradesAllowed: boolean
  downloadPriority: number
  target: { source: string; resolution: number; display: string }
}

export interface WantedItem {
  wantableId: string
  mediaItemId: number
  title: string
  detail: string
  missing: boolean
  current: string
  copy: string
}

export interface MatchEvidence {
  version: number
  matched: boolean
  method?: string
  code?: string
  reason: string
  originalTitle?: string
  parsedTitle?: string
  targetTitle?: string
  matchedTitle?: string
  matchedId?: { provider: string; value: string }
  country?: string
  warnings?: string[]
}

export interface QueueItem {
  id: number
  mediaItemId: number
  title: string
  state: string
  progress: number
  protocol: string
  quality: string
  error?: string
  addedAt: string
  stage?: string
  stageDetail?: string
  bytes?: number
  total?: number
  bytesPerSecond?: number
  match?: MatchEvidence
}

export interface QueueSummary {
  total: number
  active: number
  counts: Record<string, number>
  retentionDays?: number
}

export interface HistoryEvent {
  ts: string
  type: string
  mediaItemId: number
  releaseTitle: string
  data?: Record<string, unknown>
}

export interface CalendarEntry {
  date: string
  kind: string
  mediaItemId: number
  title: string
  detail: string
  hasFile: boolean
  // Optional on the wire (ADR 0016) — and optional here for a second reason:
  // the app ships separately from the server, so a phone on a new build
  // routinely talks to an old one. Every field below must render absent.
  posterPath?: string
  /** Exact UTC instant; episodes with a known broadcast slot only. Absent
   *  means the schedule is unknown, NOT midnight. */
  airDateUtc?: string
  network?: string
  seasonNumber?: number
  episodeNumber?: number
  episodeTitle?: string
  runtime?: number
  monitored?: boolean
}

export interface Rejection {
  code: string
  reason: string
}

export interface MatchEvidence {
  version: number
  matched: boolean
  method?: string
  code?: string
  reason: string
}

export interface ReleaseCandidate {
  title: string
  downloadUrl: string
  infoUrl?: string
  indexer: string
  protocol: string
  size: number
  seeders: number
  age: string
  quality: string
  score: number
  formats?: string[]
  accepted: boolean
  isUpgrade: boolean
  rejections: Rejection[]
  match: MatchEvidence
  candidateToken: string
  /** A caution that does not decline the release — today, an implausible size. */
  warning?: string
}

export interface ReleaseSearchScope {
  season?: number
  episode?: number
  copyId?: number
}

export interface ReleaseSearchResponse {
  candidates: ReleaseCandidate[]
  /** True when at least one indexer failed or timed out, so the list is not the whole picture. */
  partial: boolean
  reason?: string
}

export interface GrabRequest {
  mediaItemId: number
  copyId?: number
  season?: number
  episode?: number
  title: string
  downloadUrl: string
  indexer?: string
  protocol: string
  size?: number
  candidateToken?: string
}
