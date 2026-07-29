import { describe, expect, it } from 'vitest'
import {
  CARD_SIZES,
  CARD_SIZE_KEY,
  DEFAULT_CARD_SIZE,
  coerceCardSize,
  readCardSize,
} from './cardsize'

describe('coerceCardSize', () => {
  it('passes the three real sizes through', () => {
    expect(coerceCardSize('s')).toBe('s')
    expect(coerceCardSize('m')).toBe('m')
    expect(coerceCardSize('l')).toBe('l')
  })

  // The input is a localStorage string, which means a user, another tab, or
  // an older version of this app could have written anything into it. A bad
  // value costs the preference, never the page.
  it('defaults on anything else rather than throwing', () => {
    for (const bad of [null, undefined, '', 'M', 'large', 'xl', 0, 1, {}, []]) {
      expect(coerceCardSize(bad)).toBe(DEFAULT_CARD_SIZE)
    }
  })
})

describe('the card size contract', () => {
  it('offers exactly the three sizes, each labelled and described', () => {
    expect(CARD_SIZES.map((s) => s.key)).toEqual(['s', 'm', 'l'])
    for (const s of CARD_SIZES) {
      expect(s.label.length).toBeGreaterThan(0)
      // name is the aria-label, so an empty one leaves a button that
      // announces as "S".
      expect(s.name.length).toBeGreaterThan(0)
      expect(s.hint.length).toBeGreaterThan(0)
    }
  })

  // Accessible names are matched by substring, across the whole app, by both
  // assistive tech and every by-role query in the e2e suite. "Small cards —
  // more titles on screen" made the phone shell's More tab ambiguous, and the
  // failure surfaced two files away as a strict-mode violation.
  it('keeps accessible names clear of names other controls already use', () => {
    for (const s of CARD_SIZES) {
      expect(s.name.toLowerCase()).not.toContain('more')
      expect(s.name.split(' ').length).toBeLessThanOrEqual(3)
    }
  })

  it('defaults to medium, which is the size everything was before', () => {
    expect(DEFAULT_CARD_SIZE).toBe('m')
  })

  // index.html applies this before first paint from a hardcoded copy of the
  // key. If the two drift, the grid silently reflows a frame after it draws.
  it('names the storage key index.html also uses', () => {
    expect(CARD_SIZE_KEY).toBe('monarr-card-size')
  })
})

describe('readCardSize', () => {
  // vitest runs with no DOM and no localStorage — the same shape as a
  // prerender or a locked-down browser. It must answer, not throw.
  it('falls back to the default with no document and no storage', () => {
    expect(readCardSize()).toBe(DEFAULT_CARD_SIZE)
  })
})
