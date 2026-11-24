// HDR and Dolby Vision badges and the pre-flight evidence panel (ADR 0026).
//
// Two badge styles carry the one distinction that matters: a dashed badge
// with a "?" is what a release NAME claims; a solid badge is what the bytes
// MEASURED. The two never look alike, because ranking does not treat them
// alike.

import { useLayoutEffect, useRef, type RefObject } from 'react'
import type { DolbyVision, ProbeEvidence, ReleaseCandidate } from './api'
import { fmtBytes } from './api'

const PROBE_REASONS: Record<string, string> = {
  encrypted: 'encrypted',
  compressed: 'compressed archive',
  missing_articles: 'articles missing',
  no_media: 'no media file',
  unreadable: 'could not read',
}

/** "6 candidates · top 5 probed · 22.6 MB read", or '' when nothing was probed. */
export function probeSummary(all: ReleaseCandidate[]): string {
  const probed = all.filter((c) => c.probe)
  if (probed.length === 0) return ''
  const bytes = probed.reduce((n, c) => n + (c.probe?.bytes ?? 0), 0)
  return `${all.length} candidates · top ${probed.length} probed · ${fmtBytes(bytes)} read in total`
}

/** The HDR cell of a release row: the claim, and the measurement or why there is none. */
export function HDRCell({ c, open, onToggle, triggerRef }: {
  c: ReleaseCandidate
  open: boolean
  onToggle: () => void
  /** Where focus returns when the evidence closes (CLAUDE.md §5). */
  triggerRef: RefObject<HTMLButtonElement | null>
}) {
  const claim = c.hdr && c.hdr.tier ? c.hdr : undefined
  const probe = c.probe
  return (
    <div className="hdr-cell">
      {claim && (
        <span className="hdr-badge claim" title={`The name claims this${claim.confidence ? ` (${claim.confidence} confidence)` : ''}`}>
          {claim.label}
          {claim.measured ? '' : '?'}
        </span>
      )}
      {c.measuredHdr && probe && (
        <button
          ref={triggerRef}
          type="button"
          className="hdr-badge measured"
          aria-expanded={open}
          title="Measured by a pre-flight probe; show the evidence"
          onClick={onToggle}
        >
          {c.measuredHdr.label}
        </button>
      )}
      {probe && probe.status !== 'measured' && (
        <button ref={triggerRef} type="button" className="link-button warn-text" aria-expanded={open} onClick={onToggle}
          title={probe.detail || 'The probe could not read this release; it ranks by its name, below measured releases'}>
          Can't peek · {PROBE_REASONS[probe.reason ?? ''] ?? probe.reason ?? probe.status}
        </button>
      )}
    </div>
  )
}

function Fact({ k, v }: { k: string; v: string | number | undefined }) {
  if (v === undefined || v === '') return null
  return (
    <div className="probe-fact">
      <span className="muted">{k}</span>
      <span className="mono">{v}</span>
    </div>
  )
}

const COMPAT: Record<number, string> = { 0: 'none (P5)', 1: 'HDR10', 2: 'SDR', 4: 'HLG', 6: 'HDR10 (UHD Blu-ray)' }

/** The Dolby Vision record, as the evidence panel and the library file view show it. */
export function DolbyVisionFacts({ dv }: { dv: DolbyVision }) {
  const yn = (b: boolean) => (b ? 'yes' : 'no')
  const el = dv.elType === 'fel' ? 'FEL · residual present' : dv.elType === 'mel' ? 'MEL · no residual' : dv.elType === 'none' ? 'none (single layer)' : 'not read'
  return (
    <>
      <div className="probe-heading">Config record</div>
      <Fact k="Profile / level" v={`${dv.profile} / ${dv.level ?? '—'}`} />
      <Fact k="RPU · EL · BL" v={`${yn(dv.rpu)} · ${yn(dv.el)} · ${yn(dv.bl)}`} />
      <Fact k="Base layer" v={`${COMPAT[dv.blCompatId] ?? 'unknown'} (${dv.blCompatId})`} />
      {dv.source === 'rpu' && (
        <>
          <div className="probe-heading">First RPU</div>
          <Fact k="Enhancement layer" v={el} />
          <Fact k="CM version" v={dv.cmVersion ? `v${dv.cmVersion}` : undefined} />
          <Fact k="L6 MaxCLL / mastering" v={dv.masteringMaxNits ? `${dv.maxCll ?? 0} / ${dv.masteringMaxNits} nits` : undefined} />
        </>
      )}
    </>
  )
}

/** The evidence a pre-flight probe gathered, shown under the release it explains. */
export function ProbeEvidencePanel({ probe, onClose }: { probe: ProbeEvidence; onClose: () => void }) {
  const ref = useRef<HTMLDivElement>(null)
  // The panel opens under the row that was clicked: bring it into view and
  // move focus into it, so the click visibly did something (CLAUDE.md §5).
  useLayoutEffect(() => {
    ref.current?.scrollIntoView({ block: 'nearest' })
    ref.current?.focus({ preventScroll: true })
  }, [])
  const path = (probe.path ?? []).map((p) => (p === 'rar-store' ? 'RAR (store)' : p === 'yenc' ? 'yEnc' : p)).join(' › ')
  return (
    <div
      ref={ref}
      tabIndex={-1}
      className="probe-evidence"
      data-testid="probe-evidence"
      role="region"
      aria-label="Probe evidence"
      onKeyDown={(e) => {
        // Escape closes the evidence, not the search dialog around it: the
        // key's default action on a modal <dialog> is to cancel the dialog.
        if (e.key === 'Escape') {
          e.preventDefault()
          e.stopPropagation()
          onClose()
        }
      }}
    >
      <div className="probe-evidence-head">
        <strong>{probe.dolbyVision?.label ?? probe.facts ?? (probe.status === 'measured' ? 'Measured' : "Couldn't read the release")}</strong>
        <button type="button" className="small" aria-label="Close evidence" onClick={onClose}>
          ×
        </button>
      </div>
      {probe.status !== 'measured' && <p className="warn-text">{probe.detail || probe.reason}</p>}
      <Fact k="Read via Runner" v={probe.bytes ? `${fmtBytes(probe.bytes)} · ${probe.articles ?? 0} articles${probe.millis ? ` · ${(probe.millis / 1000).toFixed(1)} s` : ''}` : undefined} />
      <Fact k="Inner file" v={probe.file} />
      <Fact k="Path" v={path} />
      <Fact k="Measured" v={probe.facts} />
      {probe.dolbyVision && <DolbyVisionFacts dv={probe.dolbyVision} />}
      {probe.status === 'measured' && !probe.dolbyVision && <Fact k="Dolby Vision" v="not present" />}
    </div>
  )
}
