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

/** The visible label beside the buttons, and the group's accessible name.
 *  "S M L" on its own is three letters that could mean anything — the control
 *  has to say what it sizes before it says what the sizes are. */
export const CARD_SIZE_LABEL = 'Poster size'

/** The three sizes. `label` is what the button shows, `name` what it
 *  announces and what every by-role query matches on, `hint` the tooltip.
 *
 *  Keep `name` to one plain word. It was "Small cards — more titles on
 *  screen" for a while, and because accessible names match by substring, the
 *  word "more" in it made the phone shell's **More** tab ambiguous — a
 *  failure that surfaced two files away. */
export const CARD_SIZES: { key: CardSize; label: string; name: string; hint: string }[] = [
  { key: 's', label: 'S', name: 'Small', hint: 'Small posters — fit the most on screen' },
  { key: 'm', label: 'M', name: 'Medium', hint: 'Medium posters — the default' },
  { key: 'l', label: 'L', name: 'Large', hint: 'Large posters — bigger artwork' },
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
