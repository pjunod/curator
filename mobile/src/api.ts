import type {
  AddMediaRequest,
  CalendarEntry,
  Connection,
  DiscoverList,
  HealthReport,
  HistoryEvent,
  MediaItemDetail,
  MediaItemSummary,
  MediaKind,
  QualityProfile,
  QueueItem,
  QueueSummary,
  RootFolder,
  SearchResult,
  SystemStatus,
  WantedItem,
} from './types'

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

export class MonarrClient {
  constructor(readonly connection: Connection) {}

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const controller = new AbortController()
    const timeout = setTimeout(() => controller.abort(), 30_000)
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
        throw new ApiError('The Monarr server did not respond in 30 seconds.', 0)
      }
      throw new ApiError('Could not reach this Monarr server. Check the address and your network.', 0)
    } finally {
      clearTimeout(timeout)
    }

    const text = await response.text()
    if (!response.ok) {
      let message = `${response.status} ${response.statusText}`.trim()
      try {
        const body = JSON.parse(text) as { message?: string }
        if (body.message) message = body.message
      } catch {
        if (text.trim()) message = text.trim()
      }
      if (response.status === 401) {
        message = 'Authentication failed. Check the API key in Settings → Security.'
      }
      throw new ApiError(message, response.status)
    }
    return (text ? JSON.parse(text) : undefined) as T
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
  updateLibraryItem = (id: number, patch: { monitored?: boolean }): Promise<MediaItemDetail> =>
    this.send('PATCH', `/library/${id}`, patch)
  searchMetadata = (kind: MediaKind, query: string): Promise<SearchResult[]> =>
    this.get(`/metadata/search?kind=${kind}&query=${encodeURIComponent(query)}`)
  getDiscoverLists = (): Promise<DiscoverList[]> => this.get('/discover/lists')
  getDiscoverItems = (list: string, page = 1): Promise<SearchResult[]> =>
    this.get(`/discover/items?list=${encodeURIComponent(list)}&page=${page}`)
  getRootFolders = (): Promise<RootFolder[]> => this.get('/rootfolders')
  getProfiles = (): Promise<QualityProfile[]> => this.get('/profiles')
  addLibraryItem = (input: AddMediaRequest): Promise<MediaItemDetail> =>
    this.send('POST', '/library', input)
  getWanted = (): Promise<WantedItem[]> => this.get('/wanted')
  autoSearchItem = (id: number): Promise<void> => this.send('POST', `/library/${id}/autosearch`)
  runTask = (name: string): Promise<void> =>
    this.send('POST', `/system/tasks/${encodeURIComponent(name)}/run`)
  getQueue = (filter: 'active' | 'imported' | 'failed', limit = 30): Promise<QueueItem[]> =>
    this.get(`/queue?filter=${filter}&limit=${limit}&offset=0`)
  getQueueSummary = (): Promise<QueueSummary> => this.get('/queue/summary')
  getHistory = (limit = 40): Promise<HistoryEvent[]> => this.get(`/history?limit=${limit}&offset=0`)
  getCalendar = (start: string, end: string): Promise<CalendarEntry[]> =>
    this.get(`/calendar?start=${start}&end=${end}`)
}
