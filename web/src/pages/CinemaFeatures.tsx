import { useQuery } from '@tanstack/react-query'
import { getNotifiers, type Notifier } from '../api'

/** One Cinema notifier's removed-paths support, as its Test last read it. */
export function cinemaFeatureState(n: Notifier): { supported: boolean | null; read: string } {
  if (!n.featuresReadAt) return { supported: null, read: 'never read — Test the notifier, or it is read before the next move' }
  return { supported: (n.features ?? []).includes('removed_paths'), read: `read ${new Date(n.featuresReadAt).toLocaleString()}` }
}

/**
 * Dev tab, advisory only: what each Cinema notifier supports. Re-match works
 * either way; without removed_paths a move scans only the new folder and the
 * old entry stays in cinema until its next reconcile scan (ADR 0027).
 */
export function CinemaFeatures() {
  const notifiers = useQuery({ queryKey: ['notifiers'], queryFn: getNotifiers })
  const cinemas = (notifiers.data ?? []).filter((n) => n.type === 'plurx')
  return <section className="panel">
    <h2>Cinema features</h2>
    <p className="muted">Advisory. Nothing is turned off by what is shown here; the Re-match preview says which applies before anything moves.</p>
    {notifiers.isError && <p className="error-text">{(notifiers.error as Error).message}</p>}
    {notifiers.isSuccess && cinemas.length === 0 && <p className="muted">No Cinema notifier is configured.</p>}
    {cinemas.length > 0 && <ul className="cinema-features">
      {cinemas.map((n) => {
        const { supported, read } = cinemaFeatureState(n)
        return <li key={n.id}>{n.name}: removed paths{' '}
          <span className={'pill ' + (supported === true ? 'pill-ok' : supported === false ? 'pill-error' : 'pill-neutral')}>{supported === true ? 'supported' : supported === false ? 'not supported' : 'unknown'}</span>
          <span className="muted"> — {read}{supported === false ? '; a move leaves the old entry until cinema’s next reconcile scan' : ''}</span>
        </li>
      })}
    </ul>}
  </section>
}
