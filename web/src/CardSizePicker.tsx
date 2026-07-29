import { useState } from 'react'
import type { CardSize } from './cardsize'
import { CARD_SIZES, applyCardSize, readCardSize } from './cardsize'

/** CardSizePicker sits in the page head of every page that draws a poster
 *  grid or strip. The local state exists only to highlight the active button
 *  — the resize itself happens in CSS off the `data-card-size` attribute, so
 *  nothing below has to re-render and the setting is already correct when the
 *  other page mounts its own copy of this control. */
export function CardSizePicker() {
  const [size, setSize] = useState<CardSize>(readCardSize)

  return (
    <div className="size-picker theme-picker" role="group" aria-label="Card size">
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
  )
}
