import type {
  AddMediaCopyRequest,
  AddMediaRequest,
  CalendarEntry,
  Connection,
  DiscoverList,
  GrabRequest,
  HealthReport,
  HistoryEvent,
  MediaItemDetail,
  MediaItemSummary,
  MediaKind,
  MetadataPreview,
  QualityProfile,
  QueueItem,
  QueueSummary,
  ReleaseCandidate,
  ReleaseSearchResponse,
  ReleaseSearchScope,
  RootFolder,
  SearchResult,
  SystemStatus,
  UpdateMediaCopyRequest,
  UpdateMediaItemRequest,
  WantedItem,
} from './types'

export const RELEASE_SEARCH_TIMEOUT_MS = 120_000

export function releasesPath(id: number, scope: ReleaseSearchScope): string {
  const params = new URLSearchParams()
  if (scope.season !== undefined) params.set('season', String(scope.season))
  if (scope.episode !== undefined) params.set('episode', String(scope.episode))
  if (scope.copyId) params.set('copyId', String(scope.copyId))
  const query = params.toString()
  return `/library/${id}/releases${query ? `?${query}` : ''}`
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code?: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

export class MonarrClient {
  constructor(readonly connection: Connection) {}

  private async request<T>(path: string, init: RequestInit = {}, timeoutMs = 30_000): Promise<T> {
    const { body } = await this.requestWithHeaders<T>(path, init, timeoutMs)
    return body
  }

  // Most endpoints answer in the body alone. Release search also reports
  // whether the list is complete in response headers, so that path needs the
  // headers back as well as the parsed body.
  private async requestWithHeaders<T>(path: string, init: RequestInit = {}, timeoutMs = 30_000): Promise<{ body: T; headers: Headers }> {
    const controller = new AbortController()
    const timeout = setTimeout(() => controller.abort(), timeoutMs)
    const headers = new Headers(init.headers)
    headers.set('Accept', 'application/json')
    if (this.connection.apiKey) headers.set('X-Api-Key', this.connection.apiKey)
    if (init.body !== undefined) headers.set('Content-Type', 'application/json')

    let response: Response
    try {
      response = await fetch(`${this.connection.baseUrl}/api/v1${path}`, {
        ...init,
        headers,
        signal: controller.signal,
      })
    } catch (error) {
      if (error instanceof Error && error.name === 'AbortError') {
        throw new ApiError(`The Curator server did not respond in ${Math.round(timeoutMs / 1000)} seconds.`, 0)
      }
      throw new ApiError('Could not reach this Curator server. Check the address and your network.', 0)
    } finally {
      clearTimeout(timeout)
    }

    const text = await response.text()
    if (!response.ok) {
      let message = `${response.status} ${response.statusText}`.trim()
      let code: string | undefined
      try {
        const body = JSON.parse(text) as { message?: string; code?: string }
        if (body.message) message = body.message
        code = body.code
      } catch {
        if (text.trim()) message = text.trim()
      }
      if (response.status === 401) {
        message = 'Authentication failed. Check the API key in Access → API access.'
      }
      throw new ApiError(message, response.status, code)
    }
    return { body: (text ? JSON.parse(text) : undefined) as T, headers: response.headers }
  }

  private get<T>(path: string): Promise<T> {
    return this.request<T>(path)
  }

  private send<T = void>(method: string, path: string, body?: unknown): Promise<T> {
    return this.request<T>(path, {
      method,
      body: body === undefined ? undefined : JSON.stringify(body),
    })
  }

  getStatus = (): Promise<SystemStatus> => this.get('/system/status')
  getHealth = (): Promise<HealthReport> => this.get('/health')
  getLibrary = (kind?: MediaKind): Promise<MediaItemSummary[]> =>
    this.get(`/library${kind ? `?kind=${kind}` : ''}`)
  getLibraryItem = (id: number): Promise<MediaItemDetail> => this.get(`/library/${id}`)
  updateLibraryItem = (id: number, patch: UpdateMediaItemRequest): Promise<MediaItemDetail> =>
    this.send('PATCH', `/library/${id}`, patch)
  addMediaCopy = (id: number, input: AddMediaCopyRequest): Promise<MediaItemDetail> =>
    this.send('POST', `/library/${id}/copies`, input)
  updateMediaCopy = (id: number, copyId: number, patch: UpdateMediaCopyRequest): Promise<MediaItemDetail> =>
    this.send('PATCH', `/library/${id}/copies/${copyId}`, patch)
  deleteMediaCopy = (id: number, copyId: number): Promise<MediaItemDetail> =>
    this.send('DELETE', `/library/${id}/copies/${copyId}`)
  setSeasonMonitored = (id: number, season: number, monitored: boolean): Promise<MediaItemDetail> =>
    this.send('PATCH', `/library/${id}/seasons/${season}`, { monitored })
  searchMetadata = (kind: MediaKind, query: string): Promise<SearchResult[]> =>
    this.get(`/metadata/search?kind=${kind}&query=${encodeURIComponent(query)}`)
  getMetadataPreview = (query: string): Promise<MetadataPreview> => this.get(`/metadata/preview?${query}`)
  getDiscoverLists = (): Promise<DiscoverList[]> => this.get('/discover/lists')
  getDiscoverItems = (list: string, page = 1): Promise<SearchResult[]> =>
    this.get(`/discover/items?list=${encodeURIComponent(list)}&page=${page}`)
  getRootFolders = (): Promise<RootFolder[]> => this.get('/rootfolders')
  getProfiles = (): Promise<QualityProfile[]> => this.get('/profiles')
  addLibraryItem = (input: AddMediaRequest): Promise<MediaItemDetail> =>
    this.send('POST', '/library', input)
  getWanted = (): Promise<WantedItem[]> => this.get('/wanted')
  autoSearchItem = (id: number): Promise<void> => this.send('POST', `/library/${id}/autosearch`)
  // Indexers are queried live, so this waits well past the default timeout:
  // a slow tracker is a partial result, not a dead server.
  searchReleases = async (id: number, scope: ReleaseSearchScope = {}): Promise<ReleaseSearchResponse> => {
    const { body, headers } = await this.requestWithHeaders<ReleaseCandidate[] | null>(releasesPath(id, scope), {}, RELEASE_SEARCH_TIMEOUT_MS)
    return {
      candidates: body ?? [],
      partial: headers.get('X-Monarr-Search-Partial') === 'true',
      reason: headers.get('X-Monarr-Search-Reason') || undefined,
    }
  }
  grabRelease = (input: GrabRequest): Promise<{ id: number }> => this.send('POST', '/grab', input)
  runTask = (name: string): Promise<void> =>
    this.send('POST', `/system/tasks/${encodeURIComponent(name)}/run`)
  getQueue = (filter: 'active' | 'imported' | 'failed', limit = 30): Promise<QueueItem[]> =>
    this.get(`/queue?filter=${filter}&limit=${limit}&offset=0`)
  getQueueSummary = (): Promise<QueueSummary> => this.get('/queue/summary')
  clearFailedQueue = (): Promise<{ cleared: number }> => this.send('DELETE', '/queue/failed')
  getHistory = (limit = 40): Promise<HistoryEvent[]> => this.get(`/history?limit=${limit}&offset=0`)
  getCalendar = (start: string, end: string): Promise<CalendarEntry[]> =>
    this.get(`/calendar?start=${start}&end=${end}`)
}
