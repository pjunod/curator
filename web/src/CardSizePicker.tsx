import { useId, useState } from 'react'
import type { CardSize } from './cardsize'
import { CARD_SIZES, CARD_SIZE_LABEL, applyCardSize, readCardSize } from './cardsize'

/** CardSizePicker sits in the page head of every page that draws a poster
 *  grid or strip.
 *
 *  The label is visible, not just a tooltip: "S M L" alone is three letters
 *  with no subject, and nobody reads a tooltip to find out what a control
 *  they had not noticed does. It doubles as the group's accessible name via
 *  aria-labelledby, so the text on screen and the text a screen reader
 *  announces cannot drift apart.
 *
 *  The local state exists only to highlight the active button — the resize
 *  happens in CSS off the `data-card-size` attribute, so nothing below has to
 *  re-render, and the setting is already correct when the other page mounts
 *  its own copy of this control. */
export function CardSizePicker() {
  const [size, setSize] = useState<CardSize>(readCardSize)
  const labelId = useId()

  return (
    <div className="size-picker-wrap">
      <span className="muted size-picker-label" id={labelId}>
        {CARD_SIZE_LABEL}
      </span>
      <div className="size-picker theme-picker" role="group" aria-labelledby={labelId}>
        {CARD_SIZES.map((s) => (
          <button
            key={s.key}
            className={s.key === size ? 'active' : ''}
            aria-pressed={s.key === size}
            aria-label={s.name}
            title={s.hint}
            onClick={() => {
              applyCardSize(s.key)
              setSize(s.key)
            }}
          >
            {s.label}
          </button>
        ))}
      </div>
    </div>
  )
}
