import { describe, expect, it } from 'vitest'
// Vitest runs this source-contract check in Node; the browser bundle does not
// otherwise need a Node type dependency.
// @ts-expect-error node:fs is available to Vitest but intentionally untyped here.
import { readFileSync } from 'node:fs'
import {
  coerceAppearance,
  coerceLayout,
  coercePalette,
  PALETTES,
  resolveDisplayMode,
} from './display'

const styles = readFileSync(new URL('./styles.css', import.meta.url), 'utf8')
const index = readFileSync(new URL('../index.html', import.meta.url), 'utf8')

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
    expect(resolveDisplayMode({ appearance: 'light', palette: 'panoptic' }, true)).toBe('dark')
    expect(resolveDisplayMode({ appearance: 'auto', palette: 'redline' }, true)).toBe('dark')
    expect(resolveDisplayMode({ appearance: 'light', palette: 'panovic' }, true)).toBe('dark')
  })

  it('ships every cockpit palette in the display catalogue', () => {
    expect(PALETTES.map((palette) => palette.id)).toContain('panoptic')
    expect(PALETTES.map((palette) => palette.id)).toContain('redline')
    expect(PALETTES.map((palette) => palette.id)).toContain('panovic')
  })

  it('keeps Panovic in the cockpit family with its shared contract palette', () => {
    expect(styles).toMatch(/:root\[data-palette='panovic'\]\s*\{[^}]*--bg: #000000;[^}]*--accent: #f0723b; --accent2: #8e4a1e;[^}]*--on-accent: #140a00;/s)
    expect(styles).not.toMatch(/:root:is\(\[data-palette='panoptic'\],\[data-palette='redline'\]\)/)
    expect(index).toMatch(/var palettes = \[[^\]]*'panovic'/)
    expect(index).toContain("palette === 'panovic'")
  })
})
