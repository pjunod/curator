import { describe, expect, it } from 'vitest'
import {
  coerceAppearance,
  coerceLayout,
  coercePalette,
  resolveDisplayMode,
} from './display'

describe('display preferences', () => {
  it('keeps the shipped UI as the safe classic fallback', () => {
    expect(coerceLayout('future-layout')).toBe('classic')
    expect(coercePalette('future-palette')).toBe('classic')
    expect(coerceAppearance('future-appearance')).toBe('auto')
  })

  it('resolves appearance independently from palette and layout', () => {
    expect(resolveDisplayMode({ appearance: 'auto', palette: 'classic' }, true)).toBe('light')
    expect(resolveDisplayMode({ appearance: 'auto', palette: 'classic' }, false)).toBe('dark')
    expect(resolveDisplayMode({ appearance: 'dark', palette: 'paper' }, true)).toBe('dark')
  })

  it('keeps midnight-only palettes dark', () => {
    expect(resolveDisplayMode({ appearance: 'light', palette: 'void' }, true)).toBe('dark')
    expect(resolveDisplayMode({ appearance: 'light', palette: 'vhs' }, true)).toBe('dark')
  })
})
