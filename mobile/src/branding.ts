export const PUBLIC_APP_NAME = 'Noirr Curator'
export const SHORT_APP_NAME = 'Curator'

// The server keeps its original identity for compatibility with released
// clients. Translate that legacy value only where a person sees it.
export function displayAppName(value: string): string {
  return value.trim().toLowerCase() === 'monarr' ? PUBLIC_APP_NAME : value
}
