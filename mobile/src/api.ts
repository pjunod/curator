import type {
  AddMediaCopyRequest,
  AutoSearchResult,
  AddMediaRequest,
  BrowseResult,
  CalendarEntry,
  Connection,
  DiscoverList,
  GrabRequest,
  HealthReport,
  HistoryEvent,
  ImportPlanFile,
  ImportPlanRequest,
  ManualImportRequest,
  MediaItemDetail,
  MediaItemSummary,
  MediaKind,
  MetadataPreview,
  PlaceRequest,
  QualityProfile,
  QueueItem,
  QueueSummary,
  RematchOperation,
  RematchPreview,
  RematchRequest,
  ReleaseCandidate,
  ReleaseSearchResponse,
  ReleaseSearchScope,
  RootFolder,
  ScannedFile,
  SearchResult,
  SystemStatus,
  UpdateMediaCopyRequest,
  UpdateMediaItemRequest,
  WantedItem,
  ItemStatus,
} from './types'

export const RELEASE_SEARCH_TIMEOUT_MS = 120_000
// The server gives an auto search five minutes. Giving up sooner would show a
// timeout for a search that then grabs anyway.
export const AUTO_SEARCH_TIMEOUT_MS = 310_000

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
  // Synchronous on the server: the answer says what the search actually did.
  autoSearchItem = async (id: number): Promise<AutoSearchResult> => {
    const { body } = await this.requestWithHeaders<AutoSearchResult | undefined>(
      `/library/${id}/autosearch`, { method: 'POST' }, AUTO_SEARCH_TIMEOUT_MS)
    return body ?? { grabbed: 0, targets: [] }
  }
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
  getItemStatus = (id: number, season?: number): Promise<ItemStatus> =>
    this.get(`/library/${id}/status${season === undefined ? '' : `?season=${season}`}`)
  confirmItemInCinema = (id: number, episodeId?: number): Promise<{ queued: number }> =>
    this.send('POST', `/library/${id}/status/confirm${episodeId ? `?episode=${episodeId}` : ''}`)
  searchWantable = (wantableId: string): Promise<{ scopeLabel: string }> =>
    this.send('POST', '/wanted/searches', { scope: 'target', wantableId })
  cancelDownload = (id: number): Promise<void> => this.send('DELETE', `/queue/${id}?fromClient=true&blocklist=true`)
  retryImport = (id: number): Promise<void> => this.send('POST', `/queue/${id}/import`)
  retryDelivery = (notifierId: number, deliveryId: number): Promise<void> =>
    this.send('POST', `/notifiers/${notifierId}/deliveries/${deliveryId}/retry`)
  resolveQueueReservation = (id: number): Promise<void> => this.send('POST', `/queue/${id}/resolve-reservation`, {acknowledgeDuplicateRisk:true})
  clearFailedQueue = (): Promise<{ cleared: number }> => this.send('DELETE', '/queue/failed')
  getHistory = (limit = 40): Promise<HistoryEvent[]> => this.get(`/history?limit=${limit}&offset=0`)
  // Re-match… and Move to… (ADR 0027).
  previewRematch = (itemId: number, req: RematchRequest): Promise<RematchPreview> => this.send('POST', `/library/${itemId}/rematch/preview`, req)
  startRematch = (itemId: number, req: RematchRequest): Promise<RematchOperation> => this.send('POST', `/library/${itemId}/rematch`, req)
  getLibraryRematch = async (itemId: number): Promise<RematchOperation | null> => (await this.get<RematchOperation | undefined>(`/library/${itemId}/rematch`)) ?? null
  getRematch = (operationId: string): Promise<RematchOperation> => this.get(`/rematch/${encodeURIComponent(operationId)}`)
  cancelRematch = (operationId: string): Promise<void> => this.send('POST', `/rematch/${encodeURIComponent(operationId)}/cancel`)
  // Import file… on the item page: browse, list one folder, plan, start, stop.
  browseFilesystem = (path: string): Promise<BrowseResult> => this.get(`/filesystem?path=${encodeURIComponent(path)}`)
  scanImportFolder = (path: string): Promise<ScannedFile[]> =>
    this.get(`/import/scan?path=${encodeURIComponent(path)}&depth=1`)
  getManualImportDefaultPath = (): Promise<{ path: string }> => this.get('/import/default-path')
  planImport = (req: ImportPlanRequest): Promise<ImportPlanFile[]> => this.send('POST', '/import/plan', req)
  manualImport = (req: ManualImportRequest): Promise<{ downloadId: number }> => this.send('POST', '/import/manual', req)
  placePublicationFiles = (id: number, publicationId: number, req: PlaceRequest): Promise<void> =>
    this.send('POST', `/queue/${id}/publications/${publicationId}/files/place`, req)
  getQueueItem = (id: number): Promise<QueueItem> => this.get(`/queue/${id}`)
  cancelQueueImport = (id: number, identity: { generationKey: string; epoch: string }): Promise<void> =>
    this.send('POST', `/queue/${id}/cancel-import`, { generationKey: identity.generationKey, epoch: identity.epoch })
  getCalendar = (start: string, end: string): Promise<CalendarEntry[]> =>
    this.get(`/calendar?start=${start}&end=${end}`)
}
