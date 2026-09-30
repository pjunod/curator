import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getRecommendationStatus, getSettings, installRecommendationModel, removeRecommendationModel, updateSettings } from './api'

export function DiscoverySettings() {
  const client = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings })
  const status = useQuery({ queryKey: ['recommendation-status'], queryFn: getRecommendationStatus, refetchInterval: (query) => query.state.data?.modelState === 'downloading' ? 2000 : false })
  const refresh = () => { void client.invalidateQueries({ queryKey: ['settings'] }); void client.invalidateQueries({ queryKey: ['recommendation-status'] }) }
  const toggle = useMutation({ mutationFn: (enabled: boolean) => updateSettings({ semanticRankingEnabled: enabled }), onSuccess: refresh })
  const install = useMutation({ mutationFn: installRecommendationModel, onSuccess: refresh })
  const remove = useMutation({ mutationFn: removeRecommendationModel, onSuccess: refresh })
  return <section className="panel" id="discovery-settings">
    <h2>Discovery</h2>
    <p className="muted">Theme searches use TMDB catalog metadata. Local semantic ranking compares descriptions on this machine and works independently of Cinema. It does not generate facts or add media automatically.</p>
    <label className="inline"><input type="checkbox" checked={settings.data?.semanticRankingEnabled ?? false} disabled={toggle.isPending} onChange={(event) => toggle.mutate(event.target.checked)} />Use local semantic ranking</label>
    <p role="status">Model: {status.data?.modelState ?? 'Checking…'}{status.data?.message && ` · ${status.data.message}`}</p>
    <div className="form-row"><button disabled={install.isPending || status.data?.modelState === 'downloading'} onClick={() => install.mutate()}>Download verified model (about 92 MB)</button><button disabled={remove.isPending || settings.data?.semanticRankingEnabled || status.data?.modelState === 'downloading'} onClick={() => remove.mutate()}>Remove local model</button></div>
    <p className="muted">Download is explicit. Disabling stops inference and releases model memory; verified files remain for reuse. Without a ready model, results use evidence and provider ordering.</p>
    {[toggle.error, install.error, remove.error, status.error].filter(Boolean).map((error, index) => <p key={index} className="banner warning" role="alert">{(error as Error).message}</p>)}
  </section>
}
