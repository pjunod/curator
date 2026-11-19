import { ApiError, getActivity, type ImportActivityItem, type ImportActivityPage, type ScannedFile } from './api'

export type ActivityView = 'progress' | 'attention' | 'history'
export type ActivityScope = 'all' | 'imports'
export type ActivityActionKind = 'retry' | 'delete' | 'approve' | 'archive'
export type ActivityIdentity = { id: number; generationKey: string; epoch: string }
export type ActivityAction = {
  kind: ActivityActionKind | 'choose_files'
  label: string
  enabled: boolean
  reason?: string
}
export type ActivityItem = ImportActivityItem & {
  identity?: ActivityIdentity
  reason?: string
  prerequisite?: string
  actions?: ActivityAction[]
  operation?: { id: string; action: ActivityActionKind; state: string; message?: string }
}
export type ActivityFileScope = { files: ScannedFile[]; copyId?: number }

export function activityFileScope(item: ActivityItem): ActivityFileScope {
  const d = item.download
  return {
    copyId: d.copyId,
    files: (d.publications ?? []).flatMap(publication =>
      publication.state === 'removed' ? [] : publication.files
        .filter(file => file.outcome === 'unresolved' && !file.removed &&
          file.reason === 'not selected for import; place or discard it in Resolve')
        .map(file => ({
          name: file.path,
          path: `${publication.path.replace(/\/$/, '')}/${file.path}`,
          size: file.bytes ?? 0, kind: item.kind === 'book' ? 'book' : 'video',
          quality: '', season: -1, episodes: [],
          publication: { downloadId: d.id, publicationId: publication.id, path: file.path, outcome: file.outcome },
        }))),
  }
}

export type ActivitySelection = {
  id: string
  total: number
  retryable: number
  targets: ActivityIdentity[]
}
export type ActivityTargets =
  | { targets: ActivityIdentity[] }
  | { selection: { id: string; excludeIds: number[] } }
export type ActivityRequest = ActivityTargets & { requestId: string; action: ActivityActionKind }
export type ActivityOperationTarget = ActivityIdentity & {
  title: string
  state: 'pending' | 'running' | 'succeeded' | 'blocked'
  message?: string
}
export type ActivityOperation = {
  id: string
  action: ActivityActionKind
  state: 'running' | 'completed'
  total: number
  completed: number
  succeeded: number
  blocked: number
  targets: ActivityOperationTarget[]
}
export type ActivityJobLog = {
  title: string
  summary: string
  events: { at: string; step: string; message: string }[]
}

async function request<T>(path: string, body?: unknown): Promise<T> {
  const response = await fetch(`/api/v1${path}`, body === undefined
    ? { cache: 'no-store' }
    : { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
  if (!response.ok) {
    const failure = await response.json().catch(() => null) as { message?: string; code?: string } | null
    throw new ApiError(failure?.message ?? `Request failed (${response.status}).`, response.status, failure?.code)
  }
  return response.json() as Promise<T>
}

export const getJobActivity = (view: ActivityView, scope: ActivityScope, search: string, limit: number, offset: number) =>
  getActivity(view, scope, search, limit, offset) as Promise<Omit<ImportActivityPage, 'items'> & { items: ActivityItem[] }>

export const createActivitySelection = (scope: ActivityScope, q: string) =>
  request<ActivitySelection>('/activity/selections', { view: 'attention', scope, q })
export const postActivityAction = (body: ActivityRequest) => request<ActivityOperation>('/activity/actions', body)
export const getActivityOperation = (id: string) => request<ActivityOperation>(`/activity/operations/${encodeURIComponent(id)}`)
export const getActivityJobLog = (id: number) => request<ActivityJobLog>(`/activity/jobs/${id}/log`)

export function activityIdentityKey(identity: ActivityIdentity) {
  return JSON.stringify([identity.id, identity.generationKey, identity.epoch])
}

export function newActivityRequestId() {
  // getRandomValues also works when Curator is served over plain HTTP on a LAN.
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('')
}

export type StorageObservation = {
  owner: 'Curator' | 'Runner'
  role: string
  name?: string
  path: string
  status: 'available' | 'unavailable' | 'unknown'
  observedAt?: string
  freeBytes?: number
  totalBytes?: number
  error?: string
  storageIdentity?: string
  observedStorageIdentity?: string
}
export type StorageObservations = {
  roots: StorageObservation[]
  cleanup: { logicalBytesRemoved?: number; filesRemoved?: number; lastRemovalAt?: string }
}

export const getStorageObservations = () => request<StorageObservations>('/system/storage-observations')

const operationKeyPrefix = 'curator.activity.operation.'
export const activityOperationChange = 'curator-activity-operation-change'
let localOperationIds: string[] = []

export function readActivityOperationIds() {
  try {
    const storage = window.localStorage
    return Object.keys(storage).filter(key => key.startsWith(operationKeyPrefix)).map(key => key.slice(operationKeyPrefix.length))
  } catch {
    // Keep operations visible for this tab if browser storage is unavailable.
    return localOperationIds
  }
}

export function updateActivityOperationIds(id: string, keep: boolean) {
  localOperationIds = readActivityOperationIds().filter(item => item !== id)
  if (keep) localOperationIds.unshift(id)
  try {
    // Independent keys keep one tab from overwriting another tab's operation.
    const key = operationKeyPrefix + id
    if (keep) window.localStorage.setItem(key, id)
    else window.localStorage.removeItem(key)
  } catch {
    // Query results and operation IDs remain available until the page reloads.
  }
  window.dispatchEvent(new Event(activityOperationChange))
}
