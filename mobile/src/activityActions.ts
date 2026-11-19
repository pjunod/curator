import * as Crypto from 'expo-crypto'
import * as SecureStore from 'expo-secure-store'
import type { QueueItem } from './types'

export type ActivityView = 'progress' | 'attention' | 'history'
export type ActivityActionKind = 'retry' | 'delete' | 'approve'
export type ActivityIdentity = { id: number; generationKey: string; epoch: string }
export type ActivityItem = {
  download: QueueItem & { reservationActionable?: boolean }
  title: string
  updatedAt: string
  identity?: ActivityIdentity
  reason?: string
  prerequisite?: string
  actions?: { kind: ActivityActionKind; label: string; enabled: boolean; reason?: string }[]
  operation?: { id: string; action: ActivityActionKind; state: string; message?: string }
}
export type ActivityPage = {
  items: ActivityItem[]
  total: number
  hasMore: boolean
  counts: Record<ActivityView, number>
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
export type ActivityOperation = {
  id: string
  action: ActivityActionKind
  state: 'running' | 'completed'
  total: number
  completed: number
  succeeded: number
  blocked: number
  targets: (ActivityIdentity & { title: string; state: 'pending' | 'running' | 'succeeded' | 'blocked'; message?: string })[]
}
export type ActivityJobLog = {
  title: string
  summary: string
  events: { at: string; step: string; message: string }[]
}

export const identityKey = (item: ActivityIdentity) => JSON.stringify([item.id, item.generationKey, item.epoch])

export async function activityStorageKey(baseUrl: string) {
  const digest = await Crypto.digestStringAsync(Crypto.CryptoDigestAlgorithm.SHA256, baseUrl)
  return `monarr.activity.operations.${digest}`
}

const operationIds = new Map<string, string[]>()
const pendingWrites = new Map<string, Promise<void>>()

export async function loadActivityIds(key: string): Promise<string[]> {
  if (operationIds.has(key)) return operationIds.get(key) ?? []
  try {
    const stored = JSON.parse(await SecureStore.getItemAsync(key) ?? '[]') as unknown
    if (Array.isArray(stored)) return stored.filter((id): id is string => typeof id === 'string')
  } catch {
    // Secure storage can be temporarily unavailable while the device is locked.
  }
  return []
}

export function saveActivityIds(key: string, ids: string[]) {
  operationIds.set(key, ids)
  const write = (pendingWrites.get(key) ?? Promise.resolve()).then(async () => {
    try {
      await SecureStore.setItemAsync(key, JSON.stringify(ids))
    } catch {
      // IDs remain available during this session.
    }
  })
  pendingWrites.set(key, write)
  void write.finally(() => { if (pendingWrites.get(key) === write) pendingWrites.delete(key) })
  return write
}
