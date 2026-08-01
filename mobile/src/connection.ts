import type { Connection } from './types'

export const DEFAULT_MONARR_PORT = '7676'

export function normalizeServerUrl(input: string): string {
  let raw = input.trim()
  if (!raw) throw new Error('Enter your Monarr server address.')
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(raw) && !/^https?:\/\//i.test(raw)) {
    throw new Error('Monarr server addresses must use http:// or https://.')
  }
  const hadExplicitScheme = /^https?:\/\//i.test(raw)
  if (!hadExplicitScheme) raw = `http://${raw}`

  // URL.port is empty for both a missing port and an explicitly entered
  // protocol-default port (for example :80), so inspect the authority before
  // parsing. A direct Monarr host with no port should use Monarr's own default
  // instead of silently falling back to HTTP 80. HTTPS and URL-path inputs can
  // be reverse proxies, so their standard port remains intact.
  const authority = raw.replace(/^https?:\/\//i, '').split(/[/?#]/, 1)[0] ?? ''
  const hasExplicitPort = authority.startsWith('[')
    ? /^\[[^\]]+\]:\d+$/.test(authority)
    : /:\d+$/.test(authority)

  let url: URL
  try {
    url = new URL(raw)
  } catch {
    throw new Error('Enter a valid server address, such as http://192.168.1.20:7676.')
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') {
    throw new Error('Monarr server addresses must use http:// or https://.')
  }
  if (url.username || url.password || url.search || url.hash) {
    throw new Error('Remove credentials, query parameters, and fragments from the server address.')
  }
  const isDirectHost = !hadExplicitScheme || (url.protocol === 'http:' && url.pathname === '/')
  if (!hasExplicitPort && isDirectHost) url.port = DEFAULT_MONARR_PORT

  let path = url.pathname.replace(/\/+$/, '')
  path = path.replace(/\/api\/v1$/i, '')
  return `${url.origin}${path}`
}

export function sanitizeConnection(value: Connection): Connection {
  return {
    baseUrl: normalizeServerUrl(value.baseUrl),
    apiKey: value.apiKey.trim(),
  }
}
