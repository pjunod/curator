import { useQuery } from '@tanstack/react-query'
import { getNotifiers, type Notifier } from '../api'

/** One Cinema notifier's support for a feature, as its Test last read it. */
export function cinemaFeatureState(n: Notifier, feature = 'removed_paths'): { supported: boolean | null; read: string } {
  if (!n.featuresReadAt) return { supported: null, read: 'never read — Test the notifier, or it is read before the next move' }
  return { supported: (n.features ?? []).includes(feature), read: `read ${new Date(n.featuresReadAt).toLocaleString()}` }
}

function FeaturePill({ supported }: { supported: boolean | null }) {
  return <span className={'pill ' + (supported === true ? 'pill-ok' : supported === false ? 'pill-error' : 'pill-neutral')}>{supported === true ? 'advertised' : supported === false ? 'not advertised' : 'unknown'}</span>
}

/**
 * Dev tab, advisory only: what each Cinema notifier advertises. Re-match works
 * either way; without removed_paths a move scans only the new folder and the
 * old entry stays in cinema until its next reconcile scan (ADR 0027). Offload
 * works either way too; without offload Cinema drops the title and its watch
 * history, and the Offload dialog says so beforehand (ADR 0028).
 */
export function CinemaFeatures() {
  const notifiers = useQuery({ queryKey: ['notifiers'], queryFn: getNotifiers })
  const cinemas = (notifiers.data ?? []).filter((n) => n.type === 'plurx')
  return <section className="panel">
    <h2>Cinema features</h2>
    <p className="muted">Advisory. Nothing is turned off by what is shown here; the Re-match and Offload previews say which applies before anything happens.</p>
    {notifiers.isError && <p className="error-text">{(notifiers.error as Error).message}</p>}
    {notifiers.isSuccess && cinemas.length === 0 && <p className="muted">No Cinema notifier is configured.</p>}
    {cinemas.length > 0 && <ul className="cinema-features">
      {cinemas.map((n) => {
        const rp = cinemaFeatureState(n)
        const off = cinemaFeatureState(n, 'offload')
        return <li key={n.id}>
          <strong>{n.name}</strong> <span className="muted">— {rp.read}</span>
          <ul>
            <li>removed paths <FeaturePill supported={rp.supported} />
              <span className="muted">{rp.supported === false ? ' — a move leaves the old entry until cinema’s next reconcile scan' : ''}</span></li>
            <li>offload <FeaturePill supported={off.supported} />
              <span className="muted">{off.supported === false ? ' — an Offload makes cinema drop the title and its watch history' : off.supported === true ? ' — an Offload keeps the title greyed out with Get it back' : ''}</span></li>
          </ul>
        </li>
      })}
    </ul>}
  </section>
}
