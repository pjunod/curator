// Hand-written client for the Phase 0 API surface. The OpenAPI spec
// (internal/api/openapi.yaml) is the source of truth; client codegen from the
// spec is planned once the surface grows (blueprint §7).

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

export type HealthStatus = 'ok' | 'warning' | 'error'

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

export interface TaskState {
  name: string
  intervalSeconds: number
  running: boolean
  lastRunAt?: string
  lastDurationMs?: number
  lastError?: string
  nextRunAt?: string
}

export interface BusEvent {
  type: string
  ts: string
  payload: unknown
}

// ---- Phase 1: library ----

export type MediaKind = 'movie' | 'series' | 'book'

export interface MediaItemSummary {
  id: number
  kind: MediaKind
  title: string
  year: number
  author: string // books only; '' otherwise
  posterPath: string
  monitored: boolean
  path: string
  rating: number // provider scale: TMDB /10, Open Library /5
  ratingVotes: number // 0 = no rating known
  ratings: Rating[] // all known ratings, labeled by source
  /** Which provider this came from, or 'manual' when none did (ADR 0012). */
  source?: string
  /** Weakest quality among the item's files, for display; '' if none. */
  quality?: string
  /** The profile's target — the point at which hunting stops (ADR 0014). */
  qualityTarget?: string
  /** Whether the SOURCE half of `quality` is measured or claimed, not guessed. */
  qualityVerified?: boolean
  /** missing · seeking · met · capped; '' when undetermined. */
  upgrade?: '' | 'missing' | 'seeking' | 'met' | 'capped'
  episodeCount: number // monitored episodes aired to date (series)
  episodeFileCount: number // of those, how many have a file
  fileCount: number // files on disk (movies/books completeness)
  addedAt: string // ISO timestamp — drives the "recently added" sort
}

export interface Rating {
  source: string // tmdb | openlibrary | imdb | rt | metacritic
  value: number // in the source's native scale
  votes?: number
  scale: number // 10, 5, or 100
}

export const RATING_SOURCE_LABELS: Record<string, string> = {
  tmdb: 'TMDB',
  tvmaze: 'TVmaze',
  openlibrary: 'Open Library',
  imdb: 'IMDb',
  rt: 'Rotten Tomatoes',
  metacritic: 'Metacritic',
}

// fmtRatingValue renders a rating in its native scale: 94/100 → "94%",
// 4.3/5 → "4.3/5", 8.4/10 → "8.4".
export function fmtRatingValue(r: Rating): string {
  if (r.scale === 100) return `${Math.round(r.value)}%`
  if (r.scale === 5) return `${r.value.toFixed(1)}/5`
  return r.value.toFixed(1)
}

export interface ExternalIds {
  tmdb: number
  imdb?: string
  tvdb?: number
  isbn13?: string
  olid?: string
  asin?: string
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
  copyId?: number // which quality copy owns this file; absent/0 = primary
  path: string
  size: number
  episodeIds: number[]
  /** Recorded quality, e.g. "WEB-DL 1080p"; '' when nothing could be determined. */
  quality?: string
  /** Where `quality` came from (ADR 0013). */
  provenance?: '' | 'probe' | 'filename' | 'release' | 'manual' | 'failed' | 'implausible'
  /** Badge text for `provenance`, rendered server-side. */
  provenanceLabel?: string
  /** Whether the SOURCE half of `quality` is trustworthy enough to replace on. */
  verified?: boolean
  /** Measured facts as one line: "1080p · HEVC · HDR10 · TrueHD · 23 Mbps". */
  facts?: string
  /**
   * Why this file's own measurements cannot be true, in one sentence.
   * Empty for every file that adds up. When set, the file is not counted
   * toward the item being satisfied, so the search for a real copy goes on.
   */
  implausible?: string
  /** The release that put this file here; empty for an adopted file. */
  sourceRelease?: string
}

export interface MediaCopy {
  id: number
  name: string // '' = unnamed; show the profile instead
  qualityProfileId: number
  rootFolderId: number
  path: string // '' = shares the item's folder
  monitored: boolean
}

export interface MediaCopyInput {
  qualityProfileId: number
  rootFolderId?: number // 0/absent = share the item's folder
  name?: string
  monitored?: boolean
}

export interface MediaItemDetail extends Omit<MediaItemSummary, 'episodeCount' | 'episodeFileCount' | 'fileCount'> {
  // quality / qualityTarget / upgrade are inherited from MediaItemSummary —
  // the detail response carries the same three fields.
  backdropPath: string
  overview: string
  genres: string[]
  status: string
  releaseDate: string
  runtime: number
  qualityProfileId: number
  rootFolderId: number
  ended: boolean
  ids: ExternalIds
  seasons: SeasonInfo[]
  files: MediaFileInfo[]
  copies: MediaCopy[]
  addedAt: string
}

export interface UpdateMediaItemRequest {
  monitored?: boolean
  qualityProfileId?: number
  rootFolderId?: number // recomputes the folder; 0 clears the assignment
  path?: string // explicit absolute folder; wins over rootFolderId
}

export interface SearchResult {
  kind: MediaKind
  tmdbId: number
  /** Set when the series came from the provider chain rather than TMDB. */
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
  monitor?: 'all' | 'latest' | 'none' // series: which seasons start monitored
  searchNow?: boolean
}

/** What a root folder holds. 'mixed' means "ask" — ADR 0009. */
export type RootKind = MediaKind | 'mixed'

export interface RootFolder {
  id: number
  path: string
  kind: RootKind
  /** Adoption applies matches here without asking. Off until confirmed. */
  autoAdopt?: boolean
  freeBytes: number
  accessible: boolean
}

export interface DirEntry {
  name: string
  path: string
  registered: boolean
}

export interface BrowseResult {
  path: string
  parent: string
  dirs: DirEntry[]
}

export interface AdoptionCandidate {
  kind: MediaKind
  tmdbId: number
  /** Set when the series came from the provider chain rather than TMDB (ADR 0011). */
  tvdbId?: number
  /** Which provider produced it ("tmdb", "tvmaze") — display only. */
  source?: string
  olid?: string
  author?: string
  title: string
  /**
   * Other names this work is released under that the folder's name actually
   * matches — usually absent. It is why "Cunk on Life" can be the answer for
   * a folder called "Cunk's Quest for Meaning".
   */
  altTitles?: string[]
  year: number
  overview?: string
  posterPath?: string
}

export interface Proposal {
  rootFolderId: number
  path: string
  name: string
  parsedTitle: string
  parsedYear: number
  kind?: MediaKind
  confidence: 'exact' | 'ambiguous' | 'none'
  candidates: AdoptionCandidate[]
  /**
   * The folder that already holds the leading candidate, when that candidate
   * only answers to this folder through an alternate title. The umbrella case:
   * a provider filing a franchise under one title lists every part of it among
   * that title's alternate names, so several folders match one entry.
   */
  heldBy?: string
  /** Other folders in this batch leading with the same entry, same route. */
  sharedWith?: string[]
}

export interface ReviewCounts {
  total: number
  ambiguous: number
  none: number
  movie: number
  series: number
  book: number
  /** Entries from mixed roots, where no kind was resolved. */
  unknown: number
}

export interface ReviewPage {
  items: Proposal[]
  total: number
  offset: number
  limit: number
  counts: ReviewCounts
}

export interface AdoptResult {
  adopted: Proposal[]
  review: Proposal[]
  failures: string[]
}

export interface IgnoredPath {
  path: string
  reason: string
  ignoredAt: string
}

export interface UnmatchedDir {
  rootFolderId: number
  path: string
  name: string
}

export interface MissingItem {
  id: number
  kind: MediaKind
  title: string
  path: string
}

export interface ScanReport {
  scannedAt: string
  rootsScanned: number
  itemsScanned: number
  filesLinked: number
  filesRemoved: number
  skippedDirs?: number
  ignoredDirs?: number
  /** True count; unmatchedDirs may be a capped prefix of it. */
  unmatchedTotal?: number
  unmatchedDirs: UnmatchedDir[]
  missingPaths: string[]
  missingItems?: MissingItem[]
}

/** The profile a newly added item gets when the add form does not name one.
 *  Always fully populated on read — the server resolves built-in fallbacks, so
 *  the settings screen can never show a blank where a default is in force. */
export interface DefaultProfiles {
  movie: number
  series: number
  book: number
}

export interface Settings {
  tmdbApiKeyConfigured: boolean
  tmdbApiKeyHint: string
  omdbApiKeyConfigured?: boolean
  omdbApiKeyHint?: string
  apiKey?: string
  authRequired?: boolean
  scanSkipPatterns?: string
  defaultProfiles?: DefaultProfiles
}

/** An API failure that still carries its HTTP status, so callers can tell a
 *  resolvable conflict (409) from a flat rejection (400). */
export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function parseError(res: Response, fallback: string): Promise<never> {
  let msg = fallback
  try {
    const body = (await res.json()) as { message?: string }
    if (body.message) msg = body.message
  } catch {
    /* keep fallback */
  }
  throw new ApiError(msg, res.status)
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(`/api/v1${path}`)
  if (!res.ok) await parseError(res, `GET ${path}: ${res.status} ${res.statusText}`)
  return res.json() as Promise<T>
}

async function send<T = void>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) await parseError(res, `${method} ${path}: ${res.status} ${res.statusText}`)
  // Success bodies may be empty regardless of status (e.g. the /test
  // endpoints return a bare 200) — never let JSON parsing turn a success
  // into an error.
  const text = await res.text()
  if (!text) return undefined as T
  return JSON.parse(text) as T
}

export const getLibrary = (kind?: MediaKind) =>
  get<MediaItemSummary[]>(`/library${kind ? `?kind=${kind}` : ''}`)
export const getLibraryItem = (id: number) => get<MediaItemDetail>(`/library/${id}`)
export const addLibraryItem = (req: AddMediaRequest) =>
  send<MediaItemDetail>('POST', '/library', req)
export const deleteLibraryItem = (id: number) => send('DELETE', `/library/${id}`)
/** Re-measure an item's files, ignoring the already-probed cache. */
export const reprobeLibraryItem = (id: number) =>
  send<{ files: number }>('POST', `/library/${id}/probe`)
export const searchMetadata = (kind: MediaKind, query: string) =>
  get<SearchResult[]>(`/metadata/search?kind=${kind}&query=${encodeURIComponent(query)}`)
export const getRootFolders = () => get<RootFolder[]>('/rootfolders')
export const addRootFolder = (path: string, kind: RootKind) =>
  send<RootFolder>('POST', '/rootfolders', { path, kind })
export const updateRootFolderKind = (id: number, kind: RootKind) =>
  send<RootFolder>('PATCH', `/rootfolders/${id}`, { kind })
export const deleteRootFolder = (id: number) => send('DELETE', `/rootfolders/${id}`)
export const browseFilesystem = (path: string) =>
  get<BrowseResult>(`/filesystem?path=${encodeURIComponent(path)}`)
export const runAdoption = () => send<AdoptResult>('POST', '/library/adopt')
export const adoptOne = (path: string, pick: AdoptionCandidate, force = false) =>
  send('POST', '/library/adopt/one', {
    path,
    kind: pick.kind,
    tmdbId: pick.tmdbId || undefined,
    tvdbId: pick.tvdbId || undefined,
    olid: pick.olid || undefined,
    title: pick.title,
    year: pick.year,
    force: force || undefined,
  })
export const adoptExact = () => send<AdoptResult>('POST', '/library/adopt/exact')

/** ADR 0012: a record no provider backs, for media none of them has right. */
export interface ManualEntryRequest {
  kind: MediaKind
  title: string
  year?: number
  author?: string
  overview?: string
  path: string
}
export const addManualEntry = (req: ManualEntryRequest) =>
  send<MediaItemDetail>('POST', '/library/manual', req)
export const rescanManualEntry = (id: number) =>
  send<MediaItemDetail>('POST', `/library/${id}/rescan`)
export const setRootAutoAdopt = (rootFolderId: number, autoAdopt: boolean) =>
  send('POST', '/library/adopt/confirm', { rootFolderId, autoAdopt })
export const getReviewQueue = (
  kind: MediaKind | undefined,
  q: string,
  limit: number,
  offset: number,
) =>
  get<ReviewPage>(
    `/library/review?${kind ? `kind=${kind}&` : ''}q=${encodeURIComponent(q)}` +
      `&limit=${limit}&offset=${offset}`,
  )
export const getIgnoredDirs = () => get<IgnoredPath[]>('/library/scan/ignored')
export const ignoreDir = (path: string, reason = '') =>
  send('POST', '/library/scan/ignored', { path, reason })
export const unignoreDir = (path: string) =>
  send('DELETE', `/library/scan/ignored?path=${encodeURIComponent(path)}`)
export const getSettings = () => get<Settings>('/settings')
export const updateSettings = (patch: {
  tmdbApiKey?: string
  omdbApiKey?: string
  authRequired?: boolean
  authUsername?: string
  authPassword?: string
  scanSkipPatterns?: string
  defaultProfiles?: Partial<DefaultProfiles>
}) => send('PUT', '/settings', patch)
export const triggerScan = () => send('POST', '/library/scan')
export const getScanReport = async (): Promise<ScanReport | null> => {
  const res = await fetch('/api/v1/library/scan/report')
  if (res.status === 404) return null
  if (!res.ok) await parseError(res, `scan report: ${res.status}`)
  return res.json() as Promise<ScanReport>
}

// ---- Phase 2: acquisition ----

export interface Quality {
  source: string
  resolution: number
  display: string
}

/**
 * A profile is a target, an optional floor, and an upgrades switch (ADR 0014).
 * `sentence` is rendered by the server so every surface describes a profile
 * with exactly the same words — the old model needed each client to explain
 * why "Any" stopped at 1080p, and no two of them explained it the same way.
 */
export interface QualityProfile {
  id: number
  name: string
  target: Quality
  floor?: Quality
  upgradesAllowed: boolean
  sentence: string
  /** How many items/copies/lists use it; non-zero means delete is refused. */
  inUse?: number
}

export interface ProfileInput {
  name: string
  target: { source: string; resolution?: number }
  floor?: { source: string; resolution?: number }
  upgradesAllowed?: boolean
}

export interface IndexerInput {
  name: string
  url: string
  apiKey?: string
  protocol: 'torrent' | 'usenet'
  categories?: number[]
  enabled?: boolean
}

export interface Indexer extends IndexerInput {
  id: number
}

export interface PathMapping {
  remote: string // path prefix as the download client reports it
  local: string // the same location as Monarr sees it
}

export interface DownloadClientInput {
  type: 'qbittorrent' | 'sabnzbd' | 'transmission' | 'deluge' | 'nzbget' | 'nzbd'
  name: string
  url: string
  username?: string
  password?: string
  category?: string
  enabled?: boolean
  manualApproval?: boolean
  /**
   * Delete the payload from this client once Monarr has imported it.
   * Defaults on for usenet, off for torrents (still seeding).
   */
  removeCompleted?: boolean
  /** How Monarr learns this client's state: poll every 30s, or hold its event stream open. */
  mode?: 'poll' | 'push'
  pathMappings?: PathMapping[]
}

export interface DownloadClientConfig extends DownloadClientInput {
  id: number
}

export interface Rejection {
  code: string
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
  /** A caution that does not decline the release — today, an implausible size. */
  warning?: string
}

export interface GrabRequest {
  mediaItemId: number
  season?: number
  episode?: number
  title: string
  downloadUrl: string
  indexer?: string
  protocol: string
  size?: number
}

export interface HandoffEntry {
  step: string
  at: number // unix millis
  detail?: string
}

export interface QueueItem {
  id: number
  mediaItemId: number
  copyId?: number
  clientId?: number
  title: string
  state: string
  progress: number
  protocol: string
  quality: string
  error?: string
  savePath?: string
  importPath?: string
  handoff?: HandoffEntry[]
  addedAt: string
  /**
   * What is happening to this job right now — finer-grained than `state`,
   * and present only while something is moving. Copying into the library is
   * not a different kind of thing from downloading it, so it is the same row
   * with a different stage rather than a separate surface.
   */
  stage?: Stage
  stageSince?: string
  stageDetail?: string
  /** Which application is doing the current stage. Empty while Monarr copies. */
  stagePeer?: string
  /** Absent when the stage cannot measure itself — which is not the same as 0%. */
  bytes?: number
  total?: number
  bytesPerSecond?: number
}

export interface ScannedFile {
  path: string
  name: string
  size: number
  kind: string // 'video' | 'book'
  quality: string
  season: number // -1 when not a series file
  episodes: number[]
}

export interface ManualImportRequest {
  path: string
  mediaItemId: number
  copyId?: number
  downloadId?: number
}

export const getProfiles = () => get<QualityProfile[]>('/profiles')
export const createProfile = (body: ProfileInput) =>
  send<QualityProfile>('POST', '/profiles', body)
export const updateProfile = (id: number, body: ProfileInput) =>
  send<QualityProfile>('PUT', `/profiles/${id}`, body)
export const deleteProfile = (id: number) => send('DELETE', `/profiles/${id}`)
export const getIndexers = () => get<Indexer[]>('/indexers')
export const addIndexer = (i: IndexerInput) => send<Indexer>('POST', '/indexers', i)
export const testIndexer = (i: IndexerInput) => send('POST', '/indexers/test', i)
export const testIndexerById = (id: number) => send('POST', `/indexers/${id}/test`)
export const testDownloadClientById = (id: number) => send('POST', `/downloadclients/${id}/test`)
export const deleteIndexer = (id: number) => send('DELETE', `/indexers/${id}`)
export const getDownloadClients = () => get<DownloadClientConfig[]>('/downloadclients')
export const addDownloadClient = (c: DownloadClientInput) =>
  send<DownloadClientConfig>('POST', '/downloadclients', c)
export const updateDownloadClient = (id: number, c: DownloadClientInput) =>
  send<DownloadClientConfig>('PUT', `/downloadclients/${id}`, c)
export const testDownloadClient = (c: DownloadClientInput) => send('POST', '/downloadclients/test', c)
export const deleteDownloadClient = (id: number) => send('DELETE', `/downloadclients/${id}`)
export const searchReleases = (itemId: number, season?: number, episode?: number) => {
  const p = new URLSearchParams()
  if (season !== undefined) p.set('season', String(season))
  if (episode !== undefined) p.set('episode', String(episode))
  const qs = p.toString()
  return get<ReleaseCandidate[]>(`/library/${itemId}/releases${qs ? `?${qs}` : ''}`)
}
export const grabRelease = (req: GrabRequest) => send<{ id: number }>('POST', '/grab', req)
export const getQueue = () => get<QueueItem[]>('/queue')
export const removeQueueItem = (id: number, fromClient: boolean) =>
  send('DELETE', `/queue/${id}?fromClient=${fromClient}`)
export const importQueueItem = (id: number) => send('POST', `/queue/${id}/import`)
export const blocklistQueueItem = (id: number) => send('POST', `/queue/${id}/blocklist`)

export interface RemoveFileResult {
  path: string
  deletedFromDisk: boolean
  blocklisted?: string
  searched: boolean
  note?: string
}

export interface RemoveFileOptions {
  fromDisk?: boolean
  blocklist?: boolean
  search?: boolean
}

/**
 * removeLibraryFile drops one file from an item.
 *
 * The three options are three separate statements, which is why they are not
 * one "delete hard" flag: deleting says "I do not want this copy",
 * blocklisting says "and never take this release again". A file removed to
 * free space should not poison a release that was fine.
 */
export const removeLibraryFile = (itemId: number, fileId: number, opts: RemoveFileOptions = {}) => {
  const qs = new URLSearchParams()
  if (opts.fromDisk) qs.set('fromDisk', 'true')
  if (opts.blocklist) qs.set('blocklist', 'true')
  if (opts.search) qs.set('search', 'true')
  const q = qs.toString()
  return send<RemoveFileResult>('DELETE', `/library/${itemId}/files/${fileId}${q ? `?${q}` : ''}`)
}
export const scanImportPath = (path: string) =>
  get<ScannedFile[]>(`/import/scan?path=${encodeURIComponent(path)}`)
/** What happened to one file in an import — including why it did not land. */
export interface ImportedFile {
  name: string
  imported: boolean
  upgrade?: boolean
  quality?: string
  /** Why it was declined. Empty when it was imported. */
  reason?: string
}

export interface ImportOutcome {
  imported: number
  upgrade: boolean
  files: ImportedFile[]
}

export const manualImport = (req: ManualImportRequest) =>
  send<ImportOutcome>('POST', '/import/manual', req)

export function posterUrl(path: string, size: 'w185' | 'w342' | 'w500' = 'w342'): string {
  if (!path) return ''
  // Book covers (Open Library) arrive as absolute URLs; TMDB paths are relative.
  if (path.startsWith('http')) return path
  return `https://image.tmdb.org/t/p/${size}${path}`
}

export function fmtBytes(n: number): string {
  if (n <= 0) return '—'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 || i === 0 ? 0 : 1)} ${units[i]}`
}

export const getStatus = () => get<SystemStatus>('/system/status')
export const getHealth = () => get<HealthReport>('/health')
export const getTasks = () => get<TaskState[]>('/system/tasks')

export async function runTask(name: string): Promise<void> {
  const res = await fetch(`/api/v1/system/tasks/${encodeURIComponent(name)}/run`, {
    method: 'POST',
  })
  if (!res.ok) throw new Error(`run ${name}: ${res.status} ${res.statusText}`)
}

// ---- formatting helpers ----

export function fmtDuration(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  if (d > 0) return `${d}d ${h}h ${m}m`
  if (h > 0) return `${h}h ${m}m ${sec}s`
  if (m > 0) return `${m}m ${sec}s`
  return `${sec}s`
}

/**
 * fmtRelative renders "how long ago" in at most TWO units, dropping the
 * smaller one entirely once it stops mattering.
 *
 * It used to hand fmtDuration's full breakdown straight through, which
 * produced "1h 30m 28s ago" — three units, sixteen characters, in a table
 * column narrow enough that it wrapped onto four lines and set the height of
 * every row beside it. The seconds in "an hour and a half ago" are not
 * information anybody wanted; they were just the format not knowing when to
 * stop.
 */
export function fmtRelative(iso: string | undefined, now: Date = new Date()): string {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  const diff = (t - now.getTime()) / 1000
  const abs = Math.abs(diff)
  if (abs < 1) return 'now'
  const fmt = fmtRelativeDuration(abs)
  return diff < 0 ? `${fmt} ago` : `in ${fmt}`
}

/** fmtRelativeDuration is fmtDuration with the tail lopped off: precision
 *  shrinks as the magnitude grows, because that is how people read elapsed
 *  time. Exported for the tests that pin the boundaries. */
export function fmtRelativeDuration(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`
  if (m > 0) return `${m}m`
  return `${sec}s`
}

export function fmtInterval(seconds: number): string {
  if (seconds % 3600 === 0) {
    const h = seconds / 3600
    return h === 1 ? 'every hour' : `every ${h}h`
  }
  if (seconds % 60 === 0) {
    const m = seconds / 60
    return m === 1 ? 'every minute' : `every ${m}m`
  }
  return `every ${seconds}s`
}

// ---- Phase 3: automation ----

export interface CalendarEntry {
  date: string
  kind: string
  mediaItemId: number
  title: string
  detail: string
  hasFile: boolean
}

export interface WantedItem {
  wantableId: string
  mediaItemId: number
  title: string
  detail: string
  missing: boolean
  current: string
  copy: string // media-copy label; '' = the primary
}

export interface BlocklistEntry {
  id: number
  mediaItemId: number
  releaseTitle: string
  indexer: string
  reason: string
  createdAt: string
}

export const getCalendar = (start: string, end: string) =>
  get<CalendarEntry[]>(`/calendar?start=${start}&end=${end}`)
export const getWanted = () => get<WantedItem[]>('/wanted')
export const getBlocklist = () => get<BlocklistEntry[]>('/blocklist')
export const removeBlocklistEntry = (id: number) => send('DELETE', `/blocklist/${id}`)

export type NotifierType = 'webhook' | 'discord' | 'plex' | 'jellyfin' | 'plurx'

export interface NotifierInput {
  type: NotifierType
  name: string
  settings?: Record<string, string>
  onGrab?: boolean
  onImport?: boolean
  onFailed?: boolean
  onHealth?: boolean
  enabled?: boolean
}

export interface Notifier extends NotifierInput {
  id: number
}

export interface BackupInfo {
  name: string
  sizeBytes: number
  createdAt: string
}

export const getNotifiers = () => get<Notifier[]>('/notifiers')
export const addNotifier = (n: NotifierInput) => send<Notifier>('POST', '/notifiers', n)
export const testNotifier = (n: NotifierInput) => send('POST', '/notifiers/test', n)
// The SAVED config, not the typed one — those diverge the moment anyone edits.
export const testNotifierByID = (id: number) => send('POST', `/notifiers/${id}/test`)
export const deleteNotifier = (id: number) => send('DELETE', `/notifiers/${id}`)
export const updateNotifier = (id: number, n: NotifierInput) =>
  send<Notifier>('PUT', `/notifiers/${id}`, n)

// One queued notification attempt. Media-server notifications are retried,
// so "did it arrive?" has an answer that outlives the log buffer.
export interface Delivery {
  id: number
  notifierId: number
  downloadId?: number
  event: string
  attempts: number
  lastError?: string
  /** What the far side answered, e.g. "scanned → plurx item 1201". */
  result?: string
  status: 'pending' | 'ok' | 'failed'
  nextAt?: number
  createdAt: number
  updatedAt: number
}

export const getDeliveries = (id: number) => get<Delivery[]>(`/notifiers/${id}/deliveries`)

// One remote application and whether it is talking back.
export interface Connection {
  name: string
  kind: 'downloadclient' | 'mediaserver' | 'inbound'
  type: string
  url?: string
  /** live = a push stream is open · polling = answering, on the 30s poll ·
   *  degraded = answering but not working · unreachable = not answering ·
   *  unprobed = configured, but has no side-effect-free test. */
  state: 'live' | 'polling' | 'degraded' | 'unreachable' | 'unprobed' | 'calling' | 'quiet'
  version?: string
  lastContact?: number
  lastEventSeq?: number
  detail?: string
}

export interface Connections {
  checkedAt?: number
  connections: Connection[]
}

/**
 * Every stage one job passes through, in pipeline order.
 *
 * The post-processing names are the download client's own spellings, carried
 * through rather than translated — nzbd does that work and owns those words.
 * They are turned into human labels at the point of render and nowhere else.
 */
export type Stage =
  | 'downloading'
  | 'par_rename'
  | 'par_verify'
  | 'par_repair'
  | 'rar_rename'
  | 'unpack'
  | 'cleanup'
  | 'move'
  | 'post_unpack_rename'
  | 'script'
  | 'importing'
  | 'notifying'

export interface Transfer {
  downloadId: number
  transfer?: string
  title: string
  stage: Stage
  peer?: string
  outbound: boolean
  startedAt: string
  /** Absent when the stage cannot measure itself — which is not the same as 0%. */
  bytes?: number
  total?: number
  bytesPerSecond?: number
  detail?: string
}

export const getTransfers = () => get<Transfer[]>('/system/transfers')

export const getConnections = () => get<Connections>('/system/connections')
export const getBackups = () => get<BackupInfo[]>('/system/backups')

// ---- Phase 5: depth ----

export interface CustomFormat {
  id: number
  name: string
  pattern: string
  score?: number
}

export interface ImportList {
  id: number
  name: string
  type: 'tmdb-popular' | 'tmdb-top' | 'trakt-list'
  config?: Record<string, string>
  kind?: MediaKind
  rootFolderId?: number
  qualityProfileId?: number
  monitored?: boolean
  enabled?: boolean
}

export const getCustomFormats = () => get<CustomFormat[]>('/customformats')
export const addCustomFormat = (f: Omit<CustomFormat, 'id'>) =>
  send<CustomFormat>('POST', '/customformats', f)
export const deleteCustomFormat = (id: number) => send('DELETE', `/customformats/${id}`)
export const getImportLists = () => get<ImportList[]>('/importlists')
export const addImportList = (l: Omit<ImportList, 'id'>) =>
  send<ImportList>('POST', '/importlists', l)
export const deleteImportList = (id: number) => send('DELETE', `/importlists/${id}`)
export const bulkEditLibrary = (req: {
  ids: number[]
  monitored?: boolean
  qualityProfileId?: number
}) => send<{ updated: number }>('POST', '/library/bulk', req)
export const login = (username: string, password: string) =>
  send('POST', '/auth/login', { username, password })
export const logout = () => send('POST', '/auth/logout')

// composeHostPort joins a host (bare, or with scheme/path) with a port,
// unless the host already carries one. The download-client form uses it to
// offer a separate, default-prefilled port box.
export function composeHostPort(host: string, port: string): string {
  let h = host.trim()
  const p = port.trim()
  if (!h || !p) return h
  let scheme = ''
  const si = h.indexOf('://')
  if (si >= 0) {
    scheme = h.slice(0, si + 3)
    h = h.slice(si + 3)
  }
  const slash = h.indexOf('/')
  const hostPart = slash >= 0 ? h.slice(0, slash) : h
  const rest = slash >= 0 ? h.slice(slash) : ''
  if (hostPart.includes(':')) return scheme + h // port already present
  return `${scheme}${hostPart}:${p}${rest}`
}

export const autoSearchItem = (id: number) => send('POST', `/library/${id}/autosearch`)
export const refreshLibraryItem = (id: number) =>
  send<MediaItemDetail>('POST', `/library/${id}/refresh`)
export const addMediaCopy = (id: number, req: MediaCopyInput) =>
  send<MediaItemDetail>('POST', `/library/${id}/copies`, req)
export const updateMediaCopy = (
  id: number,
  copyId: number,
  req: { name?: string; qualityProfileId?: number; monitored?: boolean },
) => send<MediaItemDetail>('PATCH', `/library/${id}/copies/${copyId}`, req)
export const deleteMediaCopy = (id: number, copyId: number) =>
  send<MediaItemDetail>('DELETE', `/library/${id}/copies/${copyId}`)
export const setSeasonMonitored = (id: number, season: number, monitored: boolean) =>
  send<MediaItemDetail>('PATCH', `/library/${id}/seasons/${season}`, { monitored })
export const setEpisodeMonitored = (id: number, episodeId: number, monitored: boolean) =>
  send<MediaItemDetail>('PATCH', `/library/${id}/episodes/${episodeId}`, { monitored })
export const updateLibraryItem = (id: number, req: UpdateMediaItemRequest) =>
  send<MediaItemDetail>('PATCH', `/library/${id}`, req)

// States that mean a download for the item is in flight right now.
export const ACTIVE_DOWNLOAD_STATES = [
  'grabbed',
  'downloading',
  'downloaded',
  'awaiting_import',
  'importing',
]

// fmtRating renders a provider-scale rating for display: TMDB rates /10,
// Open Library rates books /5.
export function fmtRating(kind: MediaKind, rating: number): string {
  return kind === 'book' ? `${rating.toFixed(1)}/5` : rating.toFixed(1)
}

// completeness reduces an item's have/want file state to a pill: green =
// everything wanted is on disk, yellow = partial, red = nothing, gray =
// nothing wanted (unaired / no episodes).
export function completeness(
  kind: MediaKind,
  episodeFileCount: number,
  episodeCount: number,
  fileCount: number,
): { have: number; total: number; cls: string } {
  const total = kind === 'series' ? episodeCount : 1
  const have = kind === 'series' ? episodeFileCount : fileCount > 0 ? 1 : 0
  let cls = 'pill-neutral'
  if (total > 0) {
    cls = have >= total ? 'pill-ok' : have > 0 ? 'pill-warning' : 'pill-error'
  }
  return { have, total, cls }
}
