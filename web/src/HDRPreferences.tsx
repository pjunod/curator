// The HDR & Dolby Vision part of a target profile (ADR 0031): which HDR
// presentations the profile prefers, in order; which it never takes; and how
// releases are measured before they are grabbed.
//
// Reordering uses buttons rather than drag-and-drop: a keyboard, a screen
// reader and a phone can all press "move up", and the result is the same list.

import { useLayoutEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import type { HDRDevice, HDRTier, HDRTierOption, PreflightPolicy } from './api'
import { listHDRDevices } from './api'

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
  /** Tiers that rank as one; set from a device, cleared by any hand edit of the order. */
  hdrTies: HDRTier[][]
  preflight: PreflightPolicy
}

/** Whether tier t ranks as one with the tier before it in the order. */
export function tiedWithPrevious(d: Pick<HDRDraft, 'hdrOrder' | 'hdrTies'>, i: number): boolean {
  if (i === 0) return false
  const a = d.hdrOrder[i - 1]
  const b = d.hdrOrder[i]
  return d.hdrTies.some((g) => g.includes(a) && g.includes(b))
}

export function labelOf(options: HDRTierOption[] | undefined, tier: HDRTier): string {
  return options?.find((o) => o.tier === tier)?.label ?? tier
}

const PROBE_TOPS = [
  { value: 3, label: '3' },
  { value: 0, label: '5 (default)' },
  { value: 10, label: '10 (most)' },
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
  // Reordering by hand states a preference the device ties no longer
  // describe, so a hand edit clears them rather than leaving them stale.
  const move = (i: number, by: number) => {
    const next = [...order]
    const [t] = next.splice(i, 1)
    next.splice(i + by, 0, t)
    set({ hdrOrder: next, hdrTies: [] })
  }
  const unused = options.filter((o) => !order.includes(o.tier) && !reject.includes(o.tier))
  const enabled = order.length > 0 || reject.length > 0

  // A button that removes itself hands focus to what takes its place, never
  // to the page body (CLAUDE.md §5): the next row, the chip that replaced it,
  // or the button that turns the preference back on.
  const root = useRef<HTMLFieldSetElement>(null)
  const [focusKey, setFocusKey] = useState<string | null>(null)
  useLayoutEffect(() => {
    if (!focusKey) return
    const el = root.current?.querySelector<HTMLElement>(`[data-focus="${focusKey}"]`) ?? root.current
    el?.focus()
    setFocusKey(null)
  }, [focusKey, value])
  const then = (key: string | null, patch: Partial<HDRDraft>) => {
    set(patch)
    setFocusKey(key ?? 'fieldset')
  }
  const removeTier = (i: number) => {
    const next = order[i + 1] ?? order[i - 1]
    const rest = order.filter((_, k) => k !== i)
    then(next ? `remove-${next}` : rest.length || reject.length ? null : 'enable', { hdrOrder: rest, hdrTies: [] })
  }
  const unreject = (t: HDRTier) => {
    const k = reject.indexOf(t)
    const next = reject[k + 1] ?? reject[k - 1]
    const rest = reject.filter((x) => x !== t)
    then(next ? `unreject-${next}` : order.length || rest.length ? null : 'enable', { hdrReject: rest })
  }

  // One tree for both states, so the device panel keeps its place and its
  // trigger stays mounted when applying a device turns the preference on.
  return (
    <fieldset ref={root} tabIndex={-1} className="hdr-prefs" aria-label="HDR and Dolby Vision">
      <legend>{enabled ? 'HDR & Dolby Vision order' : 'HDR & Dolby Vision'}</legend>
      {!enabled ? (
        <>
          <p className="muted">
            No HDR preference: releases rank on resolution, source and custom formats alone.
          </p>
          <button type="button" data-focus="enable"
            onClick={() => then(`remove-${SUGGESTED_ORDER[0]}`, { hdrOrder: SUGGESTED_ORDER, hdrReject: SUGGESTED_REJECT, hdrTies: [] })}>
            Rank by HDR and Dolby Vision
          </button>
        </>
      ) : (
        <>
          <p className="muted">Higher wins when resolution and source tie. It never trades resolution or source away.</p>
          <ol className="hdr-order" data-testid="hdr-order">
            {order.map((t, i) => (
              <li key={t} className={tiedWithPrevious(value, i) ? 'tied' : ''}>
                <span className="hdr-rank" title={tiedWithPrevious(value, i) ? 'Same picture on the device: ranks with the tier above' : undefined}>
                  {tiedWithPrevious(value, i) ? '=' : i + 1}
                </span>
                <span className="hdr-tier">{labelOf(options, t)}</span>
                <span className="hdr-note">{TIER_NOTES[t]}</span>
                <span className="hdr-actions">
                  <button type="button" className="small" aria-label={`Move ${labelOf(options, t)} up`} disabled={i === 0} onClick={() => move(i, -1)}>
                    ↑
                  </button>
                  <button type="button" className="small" aria-label={`Move ${labelOf(options, t)} down`} disabled={i === order.length - 1} onClick={() => move(i, 1)}>
                    ↓
                  </button>
                  <button type="button" className="small" data-focus={`remove-${t}`} aria-label={`Remove ${labelOf(options, t)}`} onClick={() => removeTier(i)}>
                    ×
                  </button>
                </span>
              </li>
            ))}
          </ol>

          <div className="hdr-chips">
            <span className="muted">Reject</span>
            {reject.map((t) => (
              <button type="button" key={t} className="hdr-chip reject" data-focus={`unreject-${t}`} aria-label={`Stop rejecting ${labelOf(options, t)}`}
                onClick={() => unreject(t)}>
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
                  <button type="button" className="small" aria-label={`Prefer ${o.label}`} onClick={() => then(`remove-${o.tier}`, { hdrOrder: [...order, o.tier] })}>
                    + prefer
                  </button>
                  <button type="button" className="small" aria-label={`Reject ${o.label}`} onClick={() => then(`unreject-${o.tier}`, { hdrReject: [...reject, o.tier] })}>
                    reject
                  </button>
                </span>
              ))}
            </div>
          )}
        </>
      )}

      <DeviceDerivation options={options} onApply={(d) => set(d)} />

      {enabled && (
        <>
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
                <option value="skip">Skip an encrypted, compressed or incomplete release</option>
              </select>
            </label>
          </div>
          <button type="button" className="small" onClick={() => then('enable', { hdrOrder: [], hdrReject: [], hdrTies: [], preflight: {} })}>
            Remove HDR preference
          </button>
        </>
      )}
    </fieldset>
  )
}

const yes = (b: boolean, ok: string, no: string) => (b ? ok : no)

/**
 * "Derive order from device" (ADR 0031 §5): the playback devices Cinema
 * reports, what each decodes, and the order, ties and refusals that follow.
 * Applying one fills the profile; the profile, not the device, stays the
 * policy, and it can be edited afterwards.
 */
function DeviceDerivation(props: { options: HDRTierOption[]; onApply: (d: Pick<HDRDraft, 'hdrOrder' | 'hdrTies' | 'hdrReject'>) => void }) {
  const [open, setOpen] = useState(false)
  const devices = useQuery({ queryKey: ['hdr-devices'], queryFn: listHDRDevices, enabled: open })
  const panel = useRef<HTMLDivElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)
  // The panel opens under the button: bring it into view and focus it (CLAUDE.md §5).
  useLayoutEffect(() => {
    if (open) {
      panel.current?.scrollIntoView({ block: 'nearest' })
      panel.current?.focus({ preventScroll: true })
    }
  }, [open])
  const close = () => {
    setOpen(false)
    trigger.current?.focus()
  }
  const list = devices.data?.devices ?? []
  // The trigger stays mounted while the panel is open, so closing it (×,
  // Escape, or applying a device) has somewhere to return focus.
  return (
    <>
    <button ref={trigger} type="button" className="small" aria-expanded={open} aria-controls="hdr-devices"
      onClick={() => (open ? close() : setOpen(true))}>
      Derive order from device
    </button>
    {open && (
    <div ref={panel} id="hdr-devices" tabIndex={-1} className="hdr-devices" data-testid="hdr-devices" role="region" aria-label="Playback devices"
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          e.preventDefault()
          e.stopPropagation()
          close()
        }
      }}>
      <div className="hdr-devices-head">
        <strong>Playback target · from Cinema</strong>
        <button type="button" className="small" aria-label="Close devices" onClick={close}>×</button>
      </div>
      {devices.isPending && <p className="muted" role="status">Asking Cinema for its devices…</p>}
      {devices.isError && <p className="warn-text">{(devices.error as Error).message}</p>}
      {devices.data?.errors.map((e) => (
        <p key={e.source} className="warn-text">{e.source}: {e.message}</p>
      ))}
      {devices.data && list.length === 0 && devices.data.errors.length === 0 && (
        <p className="muted">
          No device has reported yet. Each device reports what it decodes the first time it plays something; a
          plurx connection needs the playback:read scope on its key.
        </p>
      )}
      {list.map((d: HDRDevice) => (
        <div key={`${d.source}-${d.id}`} className="hdr-device">
          <div className="hdr-device-name">
            {d.device || d.client || 'Device'} <span className="muted">· {d.user} · {d.client}</span>
          </div>
          <div className="hdr-device-facts">
            <span className="muted">P7 FEL</span><span>{yes(d.fel, 'EL decoded', d.dolbyVision ? 'EL dropped' : 'base layer')}</span>
            <span className="muted">P7 MEL / 8.1</span><span>{yes(d.dolbyVision, 'Dolby Vision', 'HDR10 base')}</span>
            <span className="muted">P5</span><span>{yes(d.profile5, 'Yes', 'Wrong colours: rejected')}</span>
            <span className="muted">HDR10+</span><span>{yes(d.hdr10plus, 'Dynamic', 'Static fallback')}</span>
          </div>
          <p className="muted">
            {d.hdrTies.length
              ? `Ties: ${d.hdrTies.map((g) => g.map((t) => labelOf(props.options, t)).join(' = ')).join('; ')} — Curator will not pay for an upgrade this device cannot show.`
              : 'This device shows every tier differently.'}
          </p>
          <button type="button" onClick={() => { props.onApply({ hdrOrder: d.hdrOrder, hdrTies: d.hdrTies, hdrReject: d.hdrReject }); close() }}>
            Use this device's order
          </button>
        </div>
      ))}
    </div>
    )}
    </>
  )
}

/**
 * The order as the profile sentence prints it: tied neighbours merged into
 * one "A = B" entry, so joining with " › " matches the server's sentence.
 */
export function orderLabels(d: Pick<HDRDraft, 'hdrOrder' | 'hdrTies'>, options?: HDRTierOption[]): string[] {
  const out: string[] = []
  d.hdrOrder.forEach((t, i) => {
    const label = labelOf(options, t)
    if (tiedWithPrevious(d, i)) out[out.length - 1] += ` = ${label}`
    else out.push(label)
  })
  return out
}
