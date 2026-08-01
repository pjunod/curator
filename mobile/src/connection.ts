import type { Connection } from './types'

export function normalizeServerUrl(input: string): string {
  let raw = input.trim()
  if (!raw) throw new Error('Enter your Monarr server address.')
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(raw) && !/^https?:\/\//i.test(raw)) {
    throw new Error('Monarr server addresses must use http:// or https://.')
  }
  if (!/^https?:\/\//i.test(raw)) raw = `http://${raw}`

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
