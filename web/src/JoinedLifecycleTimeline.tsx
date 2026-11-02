import type { DeliveryLifecycleReceipt } from './api'
import { lifecycleTimeline } from './lifecycleTimeline'

export const formatTimelineDuration = (milliseconds: number) => milliseconds < 1000 ? `${milliseconds} ms` : `${(milliseconds / 1000).toFixed(1)} s`

export function LifecycleTimeline({ receipt }: { receipt: DeliveryLifecycleReceipt }) {
  const lanes = lifecycleTimeline(receipt)
  const omitted = Object.entries(receipt.lifecycleTiming?.omittedIntervals ?? {}).filter(([,count]) => count > 0)
  return <section className="lifecycle-timeline" aria-label="Joined download lifecycle">
    <h4>Download, handoff, import and cleanup</h4>
    <p className="muted">Times from different machines or restarts stay separate. Overlapping completed steps count once. Processing details expand the recorded major steps.</p>
    {!receipt.lifecycleTiming && <p className="muted">Complete download timing was not recorded for this publication. Missing durations remain unknown.</p>}
    {omitted.length > 0 && <p className="muted">Recorded subset: at least {omitted.map(([stage,count]) => `${count} ${stage.replaceAll('_', ' ')} intervals`).join(', ')} omitted. Totals include only recorded intervals.</p>}
    {lanes.map((lane) => <section key={lane.id} className="lifecycle-clock-lane">
      <strong>{lane.label}</strong><details><summary>Timing reference</summary><p className="mono">{lane.clockDomain}</p></details>
      {lane.knownWorkMs !== undefined && <p>{omitted.length && !lane.rows.some((row) => row.detailOnly) ? 'Recorded subset · ' : ''}Measured time (overlapping steps counted once): {formatTimelineDuration(lane.knownWorkMs)}{lane.overlapMs ? ` · ${formatTimelineDuration(lane.overlapMs)} overlapping` : ''}{lane.unknownIntervals ? ` · ${lane.unknownIntervals} steps have no complete timing` : ''}</p>}
      {lane.knownWorkMs === undefined && lane.unknownIntervals > 0 && <p>Measured total unavailable: {lane.unknownIntervals} steps have no complete timing.</p>}
      <ol>{lane.rows.map((row) => <li key={row.id}>
        <strong>{row.stage}</strong> · {row.state}
        <div>{row.startedAtMs === undefined ? 'Start time not recorded' : new Date(row.startedAtMs).toLocaleString()} · {row.workMs === undefined ? 'Duration unknown' : `${formatTimelineDuration(row.workMs)}${lane.id === 'curator' ? ' observed elapsed time' : ' measured time'}`}</div>
        {row.error && <p className="error-text">{row.error}</p>}
      </li>)}</ol>
    </section>)}
    {receipt.sourceCopies?.length ? <section aria-label="Processing source copies"><strong>Processing source copies</strong><ul>{receipt.sourceCopies.map((copy) => <li key={copy.id}><strong>{copy.ownerNode || 'Owner not recorded'}</strong> · {copy.status.replaceAll('_', ' ')}<div className="muted">{copy.attempts ?? 0} cleanup attempts{copy.nextRetryAt ? ` · next ${new Date(copy.nextRetryAt * 1000).toLocaleString()}` : ''}</div>{copy.error && <p className="error-text">{copy.error}</p>}<details><summary>Copy reference</summary><code>{copy.id}</code></details></li>)}</ul></section> : null}
  </section>
}
