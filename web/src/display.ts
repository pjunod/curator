export type Appearance = 'auto' | 'light' | 'dark'
export type LayoutId = 'classic' | 'plex' | 'theater'
export type PaletteId =
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
  | 'panovic'

export interface DisplaySettings {
  appearance: Appearance
  layout: LayoutId
  palette: PaletteId
}

export const LAYOUTS: ReadonlyArray<{ id: LayoutId; name: string }> = [
  { id: 'classic', name: 'Classic' },
  { id: 'plex', name: 'Plex' },
  { id: 'theater', name: 'Theater' },
]

export const PALETTES: ReadonlyArray<{ id: PaletteId; name: string; darkOnly?: boolean }> = [
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
  { id: 'panovic', name: 'Panovic', darkOnly: true },
]

export const DEFAULT_DISPLAY_SETTINGS: DisplaySettings = {
  appearance: 'auto',
  layout: 'classic',
  palette: 'classic',
}

const LAYOUT_KEY = 'monarr-layout'
const PALETTE_KEY = 'monarr-palette'
// Keep the existing key so the old auto/light/dark preference migrates without
// a flash or a one-time reset.
const APPEARANCE_KEY = 'monarr-theme'

export function coerceAppearance(value: string | null): Appearance {
  return value === 'light' || value === 'dark' ? value : 'auto'
}

export function coerceLayout(value: string | null): LayoutId {
  return LAYOUTS.some((layout) => layout.id === value) ? (value as LayoutId) : 'classic'
}

export function coercePalette(value: string | null): PaletteId {
  return PALETTES.some((palette) => palette.id === value) ? (value as PaletteId) : 'classic'
}

export function loadDisplaySettings(): DisplaySettings {
  try {
    return {
      appearance: coerceAppearance(localStorage.getItem(APPEARANCE_KEY)),
      layout: coerceLayout(localStorage.getItem(LAYOUT_KEY)),
      palette: coercePalette(localStorage.getItem(PALETTE_KEY)),
    }
  } catch {
    return DEFAULT_DISPLAY_SETTINGS
  }
}

export function resolveDisplayMode(
  settings: Pick<DisplaySettings, 'appearance' | 'palette'>,
  systemPrefersLight: boolean,
): 'light' | 'dark' {
  if (PALETTES.find((palette) => palette.id === settings.palette)?.darkOnly) return 'dark'
  if (settings.appearance === 'light' || settings.appearance === 'dark') return settings.appearance
  return systemPrefersLight ? 'light' : 'dark'
}

export function applyDisplaySettings(settings: DisplaySettings): void {
  const root = document.documentElement
  const systemPrefersLight = window.matchMedia?.('(prefers-color-scheme: light)').matches ?? false
  const mode = resolveDisplayMode(settings, systemPrefersLight)
  root.dataset.layout = settings.layout
  root.dataset.palette = settings.palette
  root.dataset.mode = mode
  root.style.colorScheme = mode
}

export function saveDisplaySettings(settings: DisplaySettings): void {
  try {
    localStorage.setItem(APPEARANCE_KEY, settings.appearance)
    localStorage.setItem(LAYOUT_KEY, settings.layout)
    localStorage.setItem(PALETTE_KEY, settings.palette)
  } catch {
    // The live page can still honor the choice when storage is unavailable.
  }
  applyDisplaySettings(settings)
}
