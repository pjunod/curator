export type MediaKind = 'movie' | 'series' | 'book'
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

export interface MediaItemDetail extends Omit<MediaItemSummary, 'episodeCount' | 'episodeFileCount' | 'fileCount'> {
  backdropPath: string
  overview: string
  genres: string[]
  status: string
  releaseDate: string
  runtime: number
  qualityProfileId: number
  rootFolderId: number
  ended: boolean
  seasons: SeasonInfo[]
  files: MediaFileInfo[]
}

export interface SearchResult {
  kind: MediaKind
  tmdbId: number
  tvdbId?: number
  source?: string
  olid?: string
  author?: string
  title: string
  year: number
  overview: string
  posterPath: string
  inLibrary: boolean
}

export interface AddMediaRequest {
  kind: MediaKind
  tmdbId?: number
  tvdbId?: number
  olid?: string
  rootFolderId?: number
  qualityProfileId?: number
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
}
