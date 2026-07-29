/** Poster card size — one preference, applied wherever artwork is drawn in a
 *  grid or a strip (the Library page and Discover).
 *
 *  It is carried as `data-card-size` on <html> and read by CSS through the
 *  `--card-w` custom property, which is why changing it needs no React state
 *  outside the picker itself and why it survives navigation between the two
 *  pages that use it. Same mechanism as the theme picker, for the same
 *  reason: a preference that two unrelated components must agree on is
 *  cheaper as one attribute than as shared state.
 */
export type CardSize = 's' | 'm' | 'l'

export const DEFAULT_CARD_SIZE: CardSize = 'm'

/** The localStorage key. Also read by the pre-paint script in index.html —
 *  change one and you must change the other, or the grid resizes visibly
 *  after first paint. */
export const CARD_SIZE_KEY = 'monarr-card-size'

/** The three sizes. `name` is the accessible name — kept to two plain words,
 *  because it is what a screen reader announces and what every by-role query
 *  matches on. The first draft said "Small cards — more titles on screen",
 *  and the word "more" in it made the phone shell's **More** tab ambiguous to
 *  a name lookup. `hint` carries the explanation, as a tooltip only. */
export const CARD_SIZES: { key: CardSize; label: string; name: string; hint: string }[] = [
  { key: 's', label: 'S', name: 'Small cards', hint: 'Small cards — fit the most on screen' },
  { key: 'm', label: 'M', name: 'Medium cards', hint: 'Medium cards — the default' },
  { key: 'l', label: 'L', name: 'Large cards', hint: 'Large cards — bigger artwork' },
]

/** coerceCardSize turns anything at all into a valid size, defaulting rather
 *  than throwing: the input is a localStorage string a user or an older
 *  version could have written, and a broken value should cost the preference,
 *  not the page. */
export function coerceCardSize(value: unknown): CardSize {
  return value === 's' || value === 'm' || value === 'l' ? value : DEFAULT_CARD_SIZE
}

/** readCardSize prefers what is already on <html> (the pre-paint script put
 *  it there) and falls back to storage, so the picker's highlight always
 *  matches what is actually rendered. */
export function readCardSize(): CardSize {
  if (typeof document !== 'undefined' && document.documentElement.dataset.cardSize) {
    return coerceCardSize(document.documentElement.dataset.cardSize)
  }
  try {
    return coerceCardSize(localStorage.getItem(CARD_SIZE_KEY))
  } catch {
    // Storage unavailable (private mode). The default still applies.
    return DEFAULT_CARD_SIZE
  }
}

/** applyCardSize sets the attribute CSS reads and remembers the choice.
 *  The attribute is written even when storage fails, so the change still
 *  takes effect for this page. */
export function applyCardSize(size: CardSize): void {
  if (typeof document !== 'undefined') {
    document.documentElement.dataset.cardSize = size
  }
  try {
    localStorage.setItem(CARD_SIZE_KEY, size)
  } catch {
    /* still applied for this session */
  }
}
