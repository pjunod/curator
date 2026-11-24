import type { Notifier } from '../api'

/** One Cinema notifier's support for a feature, as its Test last read it. */
export function cinemaFeatureState(n: Notifier, feature = 'removed_paths'): { supported: boolean | null; read: string } {
  if (!n.featuresReadAt) return { supported: null, read: 'never read — Test the notifier, or it is read before the next move' }
  return { supported: (n.features ?? []).includes(feature), read: `read ${new Date(n.featuresReadAt).toLocaleString()}` }
}

function FeaturePill({ supported }: { supported: boolean | null }) {
  return <span className={'pill ' + (supported === true ? 'pill-ok' : supported === false ? 'pill-error' : 'pill-neutral')}>{supported === true ? 'advertised' : supported === false ? 'not advertised' : 'unknown'}</span>
}

/** Advisory capabilities belong beside the saved connection's Test. */
export function CinemaConnectionFeatures({ notifier }: { notifier: Notifier }) {
  const rp = cinemaFeatureState(notifier)
  const off = cinemaFeatureState(notifier, 'offload')
  return <div className="cinema-features small">
    <p className="muted">Capabilities {rp.read}</p>
    <p>Removed paths <FeaturePill supported={rp.supported} />{rp.supported === false && ' — old entries remain until the next Cinema scan.'}</p>
    <p>Offload <FeaturePill supported={off.supported} />{off.supported === false && ' — Cinema drops the title and watch history.'}</p>
  </div>
}
