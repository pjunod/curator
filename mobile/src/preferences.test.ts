import { describe, expect, it } from 'vitest'
import { DEFAULT_PREFERENCES, LAYOUT_OPTIONS, PALETTE_OPTIONS, libraryGrid, resolveTheme, sanitizePreferences } from './preferences'

describe('app preferences', () => {
  it('keeps the existing item layout as the medium default', () => {
    expect(DEFAULT_PREFERENCES).toEqual({
      theme: 'auto',
      itemSize: 'medium',
      layout: 'classic',
      palette: 'classic',
    })
    expect(sanitizePreferences(null)).toEqual(DEFAULT_PREFERENCES)
    expect(sanitizePreferences({ theme: 'sepia', itemSize: 'huge' })).toEqual(DEFAULT_PREFERENCES)
  })

  it('accepts each saved appearance choice independently', () => {
    expect(sanitizePreferences({ theme: 'light', itemSize: 'small' })).toEqual({
      theme: 'light',
      itemSize: 'small',
      layout: 'classic',
      palette: 'classic',
    })
    expect(sanitizePreferences({ theme: 'dark', itemSize: 'large', layout: 'theater', palette: 'tide' })).toEqual({
      theme: 'dark',
      itemSize: 'large',
      layout: 'theater',
      palette: 'tide',
    })
  })

  it('ships the shared display catalogue with Classic as the safe fallback', () => {
    expect(LAYOUT_OPTIONS.map((option) => option.id)).toEqual(['classic', 'plex', 'theater'])
    expect(PALETTE_OPTIONS.map((option) => option.id)).toEqual([
      'classic', 'terminal', 'noirr', 'amber', 'giallo', 'silver', 'void', 'vhs', 'paper', 'tide', 'panoptic', 'redline', 'panovic',
    ])
    expect(sanitizePreferences({ layout: 'future', palette: 'missing' })).toEqual(DEFAULT_PREFERENCES)
  })

  it('follows the system in auto mode and falls back to dark', () => {
    expect(resolveTheme('auto', 'light')).toBe('light')
    expect(resolveTheme('auto', 'dark')).toBe('dark')
    expect(resolveTheme('auto', 'unspecified')).toBe('dark')
    expect(resolveTheme('auto', null)).toBe('dark')
    expect(resolveTheme('auto', undefined)).toBe('dark')
  })

  it('honors explicit theme overrides', () => {
    expect(resolveTheme('light', 'dark')).toBe('light')
    expect(resolveTheme('dark', 'light')).toBe('dark')
  })

  it('keeps midnight-only palettes dark', () => {
    expect(resolveTheme('light', 'light', 'void')).toBe('dark')
    expect(resolveTheme('auto', 'light', 'vhs')).toBe('dark')
    expect(resolveTheme('light', 'light', 'panoptic')).toBe('dark')
    expect(resolveTheme('auto', 'light', 'redline')).toBe('dark')
    expect(resolveTheme('light', 'light', 'panovic')).toBe('dark')
  })

  it('keeps medium at the previous column count and scales around it', () => {
    expect(libraryGrid(390, 'small').columns).toBe(3)
    expect(libraryGrid(390, 'medium').columns).toBe(2)
    expect(libraryGrid(390, 'large').columns).toBe(1)
    expect(libraryGrid(390, 'small').cardWidth).toBeLessThan(libraryGrid(390, 'medium').cardWidth)
    expect(libraryGrid(390, 'medium').cardWidth).toBeLessThan(libraryGrid(390, 'large').cardWidth)
    expect(libraryGrid(1366, 'medium')).toMatchObject({ columns: 4, cardWidth: 324 })
  })
})
