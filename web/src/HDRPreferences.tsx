// The HDR & Dolby Vision part of a target profile (ADR 0026): which HDR
// presentations the profile prefers, in order; which it never takes; and how
// releases are measured before they are grabbed.
//
// Reordering uses buttons rather than drag-and-drop: a keyboard, a screen
// reader and a phone can all press "move up", and the result is the same list.

import type { HDRTier, HDRTierOption, PreflightPolicy } from './api'

/** What each tier means for the picture, in a phrase. */
export const TIER_NOTES: Partial<Record<HDRTier, string>> = {
  'dv-p7-fel': 'Full enhancement layer',
  'dv-p7-mel': 'Same picture as 8.1 on most players',
  'dv-p8': 'HDR10 fallback',
  hdr10plus: 'Dynamic metadata, no Dolby Vision',
  hdr10: 'Static metadata',
  hlg: 'Broadcast HDR',
  'dv-p5': 'No HDR10 fallback: wrong colours without Dolby Vision',
  sdr: 'No HDR',
}

/** The order a new HDR preference starts from: the rendered "UHD Reference". */
export const SUGGESTED_ORDER: HDRTier[] = ['dv-p7-fel', 'dv-p7-mel', 'dv-p8', 'hdr10plus', 'hdr10']
export const SUGGESTED_REJECT: HDRTier[] = ['dv-p5']

export interface HDRDraft {
  hdrOrder: HDRTier[]
  hdrReject: HDRTier[]
  preflight: PreflightPolicy
}

export function labelOf(options: HDRTierOption[] | undefined, tier: HDRTier): string {
  return options?.find((o) => o.tier === tier)?.label ?? tier
}

const PROBE_TOPS = [
  { value: 0, label: '5 (default)' },
  { value: 10, label: '10' },
  { value: 1000, label: 'All' },
]
const PROBE_BYTES = [
  { value: 0, label: '4 MB · header and first frame (default)' },
  { value: 1 << 20, label: '1 MB · header only' },
]

export function HDRPreferences(props: {
  value: HDRDraft
  options: HDRTierOption[]
  onChange: (next: HDRDraft) => void
}) {
  const { value, options, onChange } = props
  const order = value.hdrOrder
  const reject = value.hdrReject
  const set = (patch: Partial<HDRDraft>) => onChange({ ...value, ...patch })
  const move = (i: number, by: number) => {
    const next = [...order]
    const [t] = next.splice(i, 1)
    next.splice(i + by, 0, t)
    set({ hdrOrder: next })
  }
  const unused = options.filter((o) => !order.includes(o.tier) && !reject.includes(o.tier))
  const enabled = order.length > 0 || reject.length > 0

  if (!enabled) {
    return (
      <fieldset className="hdr-prefs" aria-label="HDR and Dolby Vision">
        <legend>HDR &amp; Dolby Vision</legend>
        <p className="muted">
          No HDR preference: releases rank on resolution, source and custom formats alone.
        </p>
        <button type="button" onClick={() => set({ hdrOrder: SUGGESTED_ORDER, hdrReject: SUGGESTED_REJECT })}>
          Rank by HDR and Dolby Vision
        </button>
      </fieldset>
    )
  }

  return (
    <fieldset className="hdr-prefs" aria-label="HDR and Dolby Vision">
      <legend>HDR &amp; Dolby Vision order</legend>
      <p className="muted">Higher wins when resolution and source tie. It never trades resolution or source away.</p>
      <ol className="hdr-order" data-testid="hdr-order">
        {order.map((t, i) => (
          <li key={t}>
            <span className="hdr-rank">{i + 1}</span>
            <span className="hdr-tier">{labelOf(options, t)}</span>
            <span className="hdr-note">{TIER_NOTES[t]}</span>
            <span className="hdr-actions">
              <button type="button" className="small" aria-label={`Move ${labelOf(options, t)} up`} disabled={i === 0} onClick={() => move(i, -1)}>
                ↑
              </button>
              <button type="button" className="small" aria-label={`Move ${labelOf(options, t)} down`} disabled={i === order.length - 1} onClick={() => move(i, 1)}>
                ↓
              </button>
              <button type="button" className="small" aria-label={`Remove ${labelOf(options, t)}`} onClick={() => set({ hdrOrder: order.filter((x) => x !== t) })}>
                ×
              </button>
            </span>
          </li>
        ))}
      </ol>

      <div className="hdr-chips">
        <span className="muted">Reject</span>
        {reject.map((t) => (
          <button type="button" key={t} className="hdr-chip reject" aria-label={`Stop rejecting ${labelOf(options, t)}`}
            onClick={() => set({ hdrReject: reject.filter((x) => x !== t) })}>
            {labelOf(options, t)} ×
          </button>
        ))}
        {reject.length === 0 && <span className="muted">nothing</span>}
      </div>

      {unused.length > 0 && (
        <div className="hdr-chips">
          <span className="muted">Not ranked</span>
          {unused.map((o) => (
            <span key={o.tier} className="hdr-chip">
              {o.label}
              <button type="button" className="small" aria-label={`Prefer ${o.label}`} onClick={() => set({ hdrOrder: [...order, o.tier] })}>
                + prefer
              </button>
              <button type="button" className="small" aria-label={`Reject ${o.label}`} onClick={() => set({ hdrReject: [...reject, o.tier] })}>
                reject
              </button>
            </span>
          ))}
        </div>
      )}

      <h4>Pre-flight probe</h4>
      <p className="muted">
        Before grabbing, Runner reads the first bytes of the best releases and Curator measures them, so a
        release is ranked on what it is rather than what its name says.
      </p>
      <div className="form-row">
        <label>
          Probe top candidates
          <select value={value.preflight.top ?? 0}
            onChange={(e) => set({ preflight: { ...value.preflight, top: Number(e.target.value) } })}>
            {PROBE_TOPS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        </label>
        <label>
          Bytes per probe
          <select value={value.preflight.bytes ?? 0}
            onChange={(e) => set({ preflight: { ...value.preflight, bytes: Number(e.target.value) } })}>
            {PROBE_BYTES.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        </label>
        <label>
          When the probe can't read
          <select value={value.preflight.skipUnreadable ? 'skip' : 'rank'}
            onChange={(e) => set({ preflight: { ...value.preflight, skipUnreadable: e.target.value === 'skip' } })}>
            <option value="rank">Rank by name, below measured</option>
            <option value="skip">Skip the release</option>
          </select>
        </label>
      </div>
      <button type="button" className="small" onClick={() => set({ hdrOrder: [], hdrReject: [], preflight: {} })}>
        Remove HDR preference
      </button>
    </fieldset>
  )
}
