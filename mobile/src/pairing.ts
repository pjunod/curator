import type { Connection, SystemStatus } from './types'
import { sanitizeConnection } from './connection'

export const PAIRING_PROTOCOL = 'monarr:'
export const PAIRING_HOST = 'pair'

export interface DiscoveredServer {
  name: string
  baseUrl: string
  status?: SystemStatus
  requiresApiKey: boolean
}

export interface LocalService {
  name: string
  host?: string
  port: number
  addresses?: string[]
  txt?: Record<string, string>
}

export function parsePairingCode(value: string): Connection {
  let code: URL
  try {
    code = new URL(value.trim())
  } catch {
    throw new Error('This is not a Curator pairing QR code.')
  }
  if (code.protocol !== PAIRING_PROTOCOL || code.hostname !== PAIRING_HOST) {
    throw new Error('This is not a Curator pairing QR code.')
  }
  if (code.searchParams.get('v') !== '1') {
    throw new Error('This Curator pairing code uses an unsupported version.')
  }
  const server = code.searchParams.get('server')
  if (!server) throw new Error('This Curator pairing code has no server address.')
  return sanitizeConnection({ baseUrl: server, apiKey: code.searchParams.get('key') ?? '' })
}

export function serviceBaseUrls(service: LocalService): string[] {
  if (!Number.isInteger(service.port) || service.port < 1 || service.port > 65535) return []
  const path = service.txt?.path?.trim() ?? ''
  const suffix = !path || path === '/' ? '' : `/${path.replace(/^\/+|\/+$/g, '')}`
  const hosts = [
    service.txt?.host?.replace(/\.$/, ''),
    service.host?.replace(/\.$/, ''),
    ...(service.addresses ?? []),
  ]
  const urls: string[] = []
  for (const candidate of hosts) {
    if (!candidate) continue
    const authority = addressAuthority(candidate)
    if (!authority) continue
    const value = `http://${authority}:${service.port}${suffix}`
    if (!urls.includes(value)) urls.push(value)
  }
  return urls
}

function addressAuthority(value: string): string {
  let host = value.trim()
  if (!host) return ''
  if (host.startsWith('[') && host.endsWith(']')) host = host.slice(1, -1)
  if (!host.includes(':')) return /^[a-z0-9_.-]+$/i.test(host) ? host : ''
  // A scoped link-local address needs an interface id. WHATWG URL parsing
  // cannot represent that id portably, so the DNS-SD hostname (added first
  // above) is the usable route for those services.
  if (host.includes('%') || /^fe[89ab][0-9a-f]:/i.test(host)) return ''
  return `[${host}]`
}

export async function probeMonarrServer(
  name: string,
  baseUrl: string,
  apiKey: string,
  timeoutMs = 1800,
): Promise<DiscoveredServer | null> {
  const controller = new AbortController()
  const timeout = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const headers = new Headers({ Accept: 'application/json' })
    if (apiKey.trim()) headers.set('X-Api-Key', apiKey.trim())
    const response = await fetch(`${baseUrl}/api/v1/system/status`, {
      headers,
      signal: controller.signal,
    })
    const text = await response.text()
    if (response.ok) {
      const status = JSON.parse(text) as SystemStatus
      if (status.appName.toLowerCase() !== 'monarr') return null
      return { name, baseUrl, status, requiresApiKey: false }
    }
    if (response.status === 401) {
      const body = JSON.parse(text) as { message?: string }
      if (body.message === 'authentication required') {
        return { name, baseUrl, requiresApiKey: true }
      }
    }
    return null
  } catch {
    return null
  } finally {
    clearTimeout(timeout)
  }
}
