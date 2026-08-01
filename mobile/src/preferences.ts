export type ThemePreference = 'auto' | 'light' | 'dark'
export type ItemSize = 'small' | 'medium' | 'large'

export interface AppPreferences {
  theme: ThemePreference
  itemSize: ItemSize
}

export const DEFAULT_PREFERENCES: AppPreferences = {
  theme: 'auto',
  itemSize: 'medium',
}

export function sanitizePreferences(value: unknown): AppPreferences {
  if (!value || typeof value !== 'object') return DEFAULT_PREFERENCES
  const candidate = value as Partial<AppPreferences>
  return {
    theme: candidate.theme === 'light' || candidate.theme === 'dark' ? candidate.theme : 'auto',
    itemSize: candidate.itemSize === 'small' || candidate.itemSize === 'large'
      ? candidate.itemSize
      : 'medium',
  }
}

export function resolveTheme(
  preference: ThemePreference,
  system: 'light' | 'dark' | 'unspecified' | null | undefined,
): 'light' | 'dark' {
  if (preference === 'light' || preference === 'dark') return preference
  return system === 'light' ? 'light' : 'dark'
}

export function libraryGrid(width: number, itemSize: ItemSize): {
  columns: number
  slotWidth: number
  cardWidth: number
} {
  const safeWidth = Math.max(320, width)
  const mediumColumns = safeWidth >= 900 ? 4 : safeWidth >= 600 ? 3 : 2
  const columns = itemSize === 'small'
    ? mediumColumns + 1
    : itemSize === 'large'
      ? Math.max(1, mediumColumns - 1)
      : mediumColumns
  const slotWidth = Math.max(96, Math.floor((safeWidth - 32 - 12 * (columns - 1)) / columns))
  // Medium intentionally remains the original, full-width layout. Small and
  // large use caps so their relative sizing stays useful on wide tablets.
  const maxCardWidth = itemSize === 'small' ? 150 : itemSize === 'large' ? 300 : slotWidth
  return { columns, slotWidth, cardWidth: Math.min(slotWidth, maxCardWidth) }
}
