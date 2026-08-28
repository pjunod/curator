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
    expect(resolveDisplayMode({ appearance: 'light', palette: 'copper' }, true)).toBe('dark')
  })

  it('ships every cockpit palette in the display catalogue', () => {
    expect(PALETTES.map((palette) => palette.id)).toContain('panoptic')
    expect(PALETTES.map((palette) => palette.id)).toContain('redline')
    expect(PALETTES.map((palette) => palette.id)).toContain('panovic')
    expect(PALETTES.map((palette) => palette.id)).toContain('copper')
  })

  it('keeps Burnt Pumpkin and Copper in the cockpit family with their contract palettes', () => {
    expect(styles).toMatch(/:root\[data-palette='panovic'\]\s*\{[^}]*--bg: #000000;[^}]*--accent: #e8871e; --accent2: #81420b;[^}]*--on-accent: #150b00;/s)
    expect(styles).toMatch(/:root\[data-palette='copper'\]\s*\{[^}]*--bg: #000000;[^}]*--accent: #cf7643; --accent2: #70402b;[^}]*--on-accent: #160b06;/s)
    expect(styles).not.toMatch(/:root:is\(\[data-palette='panoptic'\],\[data-palette='redline'\],\[data-palette='panovic'\]\)/)
    expect(index).toMatch(/var palettes = \[[^\]]*'panovic'[^\]]*'copper'/)
    expect(index).toContain("palette === 'panovic'")
    expect(index).toContain("palette === 'copper'")
  })
})
