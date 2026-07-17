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

async function get<T>(path: string): Promise<T> {
  const res = await fetch(`/api/v1${path}`)
  if (!res.ok) throw new Error(`GET ${path}: ${res.status} ${res.statusText}`)
  return res.json() as Promise<T>
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

export function fmtRelative(iso: string | undefined, now: Date = new Date()): string {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  const diff = (t - now.getTime()) / 1000
  const abs = Math.abs(diff)
  const fmt = fmtDuration(abs)
  if (abs < 1) return 'now'
  return diff < 0 ? `${fmt} ago` : `in ${fmt}`
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
