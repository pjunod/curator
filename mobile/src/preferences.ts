export type ThemePreference = 'auto' | 'light' | 'dark'
export type ItemSize = 'small' | 'medium' | 'large'
export type LayoutPreference = 'classic' | 'plex' | 'theater'
export type PalettePreference =
  | 'classic'
  | 'terminal'
  | 'noirr'
  | 'amber'
  | 'giallo'
  | 'silver'
  | 'void'
  | 'vhs'
  | 'paper'
  | 'tide'
  | 'panoptic'
  | 'redline'

export const LAYOUT_OPTIONS: ReadonlyArray<{ id: LayoutPreference; name: string }> = [
  { id: 'classic', name: 'Classic' },
  { id: 'plex', name: 'Plex' },
  { id: 'theater', name: 'Theater' },
]

export const PALETTE_OPTIONS: ReadonlyArray<{ id: PalettePreference; name: string; darkOnly?: boolean }> = [
  { id: 'classic', name: 'Classic' },
  { id: 'terminal', name: 'Terminal' },
  { id: 'noirr', name: 'noirr' },
  { id: 'amber', name: 'Amber' },
  { id: 'giallo', name: 'Giallo' },
  { id: 'silver', name: 'Silver' },
  { id: 'void', name: 'Void', darkOnly: true },
  { id: 'vhs', name: 'VHS', darkOnly: true },
  { id: 'paper', name: 'Paper' },
  { id: 'tide', name: 'Tide' },
  { id: 'panoptic', name: 'Panoptic', darkOnly: true },
  { id: 'redline', name: 'Redline', darkOnly: true },
]

export interface AppPreferences {
  theme: ThemePreference
  itemSize: ItemSize
  layout: LayoutPreference
  palette: PalettePreference
}

export const DEFAULT_PREFERENCES: AppPreferences = {
  theme: 'auto',
  itemSize: 'medium',
  layout: 'classic',
  palette: 'classic',
}

export function sanitizePreferences(value: unknown): AppPreferences {
  if (!value || typeof value !== 'object') return DEFAULT_PREFERENCES
  const candidate = value as Partial<AppPreferences>
  return {
    theme: candidate.theme === 'light' || candidate.theme === 'dark' ? candidate.theme : 'auto',
    itemSize: candidate.itemSize === 'small' || candidate.itemSize === 'large'
      ? candidate.itemSize
      : 'medium',
    layout: LAYOUT_OPTIONS.some((option) => option.id === candidate.layout)
      ? candidate.layout as LayoutPreference
      : 'classic',
    palette: PALETTE_OPTIONS.some((option) => option.id === candidate.palette)
      ? candidate.palette as PalettePreference
      : 'classic',
  }
}

export function isDarkOnlyPalette(palette: PalettePreference): boolean {
  return PALETTE_OPTIONS.find((option) => option.id === palette)?.darkOnly === true
}

export function resolveTheme(
  preference: ThemePreference,
  system: 'light' | 'dark' | 'unspecified' | null | undefined,
  palette: PalettePreference = 'classic',
): 'light' | 'dark' {
  if (isDarkOnlyPalette(palette)) return 'dark'
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
