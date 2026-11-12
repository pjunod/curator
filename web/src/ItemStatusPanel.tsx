import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import {
  autoSearchItem,
  confirmItemInCinema,
  getItemStatus,
  importQueueItem,
  removeQueueItem,
  retryDelivery,
  type ItemStatus,
  type StatusAction,
  type StatusAttempt,
  type StatusDownload,
  type StatusEpisode,
  type StatusSeason,
  type StatusStage,
} from './api'
import { ActionDialog, ActionNotice, focusActionTrigger } from './ActionDialog'
import { describeAutoSearch } from './autosearch'
import {
  ACTION_LABELS,
  STAGE_NAMES,
  STATE_ICON,
  doneAbout,
  fmtBytes,
  fmtDuration,
  fmtRate,
  fmtWhen,
  stageActions,
  stateClass,
  statusPollMs,
  updatedAgo,
} from './itemStatus'

// "Where it stands": the item's path from search to cinema, at the top of the
// item page. The server projects every state (GET /library/{id}/status); this
// draws it, polls it (2 s while a download moves, else 30 s, never moving
// scroll or focus) and runs the one action that fixes what is wrong.

type Target = { season?: number; episode?: number }

export function ItemStatusPanel({ itemId, kind, onInteractiveSearch }: {
  itemId: number
  kind: string
  onInteractiveSearch: (target: Target) => void
}) {
  const status = useQuery({
    queryKey: ['item-status', itemId],
    queryFn: () => getItemStatus(itemId),
    refetchInterval: (q) => statusPollMs(q.state.data),
  })
  const [expanded, setExpanded] = useState(false)
  const now = useNow()
  const s = status.data

  return (
    <section className="panel item-status" aria-labelledby="item-status-title" data-testid="item-status">
      <div className="item-status-h">
        <h2 id="item-status-title">Where it stands</h2>
        {s && <span className="muted small">{updatedAgo(status.dataUpdatedAt, now)}</span>}
      </div>
      {status.isError && !s && <p className="muted">Status unavailable: {(status.error as Error).message}</p>}
      {!s && !status.isError && <p className="muted">Loading…</p>}
      {s && s.done && !expanded ? (
        <button type="button" className="item-status-oneline" aria-expanded="false" onClick={() => setExpanded(true)}>
          <span className="dot s-done" aria-hidden="true" /> {s.headline.title}
        </button>
      ) : s ? (
        <>
          <Headline s={s} />
          {kind === 'series' ? (
            <SeriesSeasons itemId={itemId} seasons={s.seasons ?? []} onInteractiveSearch={onInteractiveSearch} />
          ) : (
            <MovieTrack itemId={itemId} s={s} onInteractiveSearch={onInteractiveSearch} />
          )}
          {s.done && (
            <button type="button" className="link-button small" aria-expanded="true" onClick={() => setExpanded(false)}>
              Collapse
            </button>
          )}
        </>
      ) : null}
    </section>
  )
}

/** A clock for "updated N s ago" that ticks without refetching. */
function useNow(): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = window.setInterval(() => setNow(Date.now()), 5_000)
    return () => window.clearInterval(t)
  }, [])
  return now
}

function Headline({ s }: { s: ItemStatus }) {
  return (
    <div className="item-status-headline">
      <span className={`dot ${stateClass(s.headline.state)}`} aria-hidden="true" />
      <div>
        <div className="item-status-title">{s.headline.title}</div>
        {s.headline.detail && <div className="muted small">{s.headline.detail}</div>}
      </div>
    </div>
  )
}

export function Track({ stages, label }: { stages: StatusStage[]; label: string }) {
  return (
    <ol className="item-track" aria-label={label}>
      {stages.map((st) => (
        <li key={st.key} className={stateClass(st.state)} data-stage={st.key} data-state={st.state}>
          <span className="st">
            <i aria-hidden="true">{STATE_ICON[st.state]}</i>
            {STAGE_NAMES[st.key]}
          </span>
          <span className="when">{st.at ? fmtWhen(st.at) : st.state === 'active' ? 'now' : ''}</span>
          {st.detail && <span className="det">{st.detail}</span>}
        </li>
      ))}
    </ol>
  )
}

function DownloadNumbers({ dl }: { dl: StatusDownload }) {
  const pct = Math.round(dl.progress * 100)
  const fetching = dl.state === 'downloading' && (!dl.stage || dl.stage === 'downloading')
  return (
    <div className="item-status-download">
      {fetching && dl.progress > 0 && (
        <div className="item-bar" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} aria-label="Download progress">
          <span style={{ width: `${pct}%` }} />
        </div>
      )}
      <div className="item-numbers">
        <span className="mono item-release">{dl.release}{dl.indexer ? ` · ${dl.indexer}` : ''}</span>
        {dl.total > 0 && (
          <span>
            <b>{fmtBytes(dl.bytes)}</b> of {fmtBytes(dl.total)}
          </span>
        )}
        {fetching && dl.rateBps ? (
          <span>
            <b>{fmtRate(dl.rateBps)}</b> (runner's average)
          </span>
        ) : null}
        {fetching && dl.etaSeconds ? (
          <span>
            {fmtDuration(dl.etaSeconds)} left, done about <b>{doneAbout(dl)}</b>
          </span>
        ) : null}
        {dl.stage && dl.stage !== 'downloading' && (
          <span>
            Runner stage <b>{dl.stage.replaceAll('_', ' ')}</b>
            {dl.stageSince ? ` since ${fmtWhen(dl.stageSince)}` : ''}
          </span>
        )}
        {dl.episodes && dl.episodes > 1 ? <span>{dl.episodes} episodes in one season pack</span> : null}
      </div>
    </div>
  )
}

function Attempts({ attempts }: { attempts: StatusAttempt[] }) {
  if (attempts.length === 0) return null
  return (
    <details className="item-attempts">
      <summary>Earlier attempts ({attempts.length})</summary>
      <div className="item-att">
        {attempts.map((a) => (
          <div key={a.downloadId} className="item-att-row">
            <span className="muted">{fmtWhen(a.at)}</span>
            <span>
              {a.release}
              {a.indexer ? ` · ${a.indexer}` : ''} · <b>{a.outcome}</b>
              {a.reason ? ` (${a.reason})` : ''}
            </span>
          </div>
        ))}
      </div>
    </details>
  )
}

function MovieTrack({ itemId, s, onInteractiveSearch }: { itemId: number; s: ItemStatus; onInteractiveSearch: (t: Target) => void }) {
  const stages = s.stages ?? []
  const cinemaWait = stages.find((st) => st.key === 'in_cinema' && st.state === 'waiting')
  return (
    <>
      <Track stages={stages} label="Stages" />
      {s.download && s.active && <DownloadNumbers dl={s.download} />}
      {cinemaWait && s.cinema?.waitingOn && (
        <div className="item-callout s-wait">
          Cinema says it is waiting on: <b>{s.cinema.waitingOn.replaceAll('_', ' ')}</b>. Curator asks again on its retry schedule.
        </div>
      )}
      <StatusActions itemId={itemId} actions={stageActions(stages)} download={s.download} cinema={s.cinema}
        onInteractiveSearch={() => onInteractiveSearch({})} />
      <Attempts attempts={s.attempts ?? []} />
    </>
  )
}

function SeriesSeasons({ itemId, seasons, onInteractiveSearch }: {
  itemId: number
  seasons: StatusSeason[]
  onInteractiveSearch: (t: Target) => void
}) {
  const [open, setOpen] = useState<Record<number, boolean>>({})
  if (seasons.length === 0) return <p className="muted">No seasons known yet.</p>
  return (
    <div className="item-seasons-status">
      {seasons.map((sn) => (
        <div key={sn.season} className="item-season-status">
          <button type="button" className="item-season-h" aria-expanded={!!open[sn.season]}
            onClick={() => setOpen((o) => ({ ...o, [sn.season]: !o[sn.season] }))}>
            <span className={`dot ${sn.active ? 's-active' : sn.episodes > 0 && sn.inCinema === sn.episodes ? 's-done' : 's-wait'}`} aria-hidden="true" />
            <b>{sn.season === 0 ? 'Specials' : `Season ${sn.season}`}</b>
            <span className="muted small">{sn.summary}</span>
          </button>
          {sn.download && (
            <div className="item-pack">
              <span className="small">
                Season pack in runner: <b>{sn.download.stage ? sn.download.stage.replaceAll('_', ' ') : sn.download.state}</b>.
              </span>
              <DownloadNumbers dl={sn.download} />
              <Link to="/activity" className="small">Open in Activity</Link>
            </div>
          )}
          {open[sn.season] && <SeasonEpisodes itemId={itemId} season={sn.season} onInteractiveSearch={onInteractiveSearch} />}
        </div>
      ))}
    </div>
  )
}

function SeasonEpisodes({ itemId, season, onInteractiveSearch }: { itemId: number; season: number; onInteractiveSearch: (t: Target) => void }) {
  const q = useQuery({
    queryKey: ['item-status', itemId, season],
    queryFn: () => getItemStatus(itemId, season),
    refetchInterval: (query) => statusPollMs(query.state.data),
  })
  const [openEp, setOpenEp] = useState<number | null>(null)
  if (!q.data) return <p className="muted small">{q.isError ? (q.error as Error).message : 'Loading episodes…'}</p>
  return (
    <div className="item-eps">
      {(q.data.episodes ?? []).map((e) => (
        <EpisodeRow key={e.episodeId} itemId={itemId} e={e} open={openEp === e.episodeId}
          onToggle={() => setOpenEp(openEp === e.episodeId ? null : e.episodeId)}
          onInteractiveSearch={onInteractiveSearch} />
      ))}
    </div>
  )
}

function EpisodeRow({ itemId, e, open, onToggle, onInteractiveSearch }: {
  itemId: number
  e: StatusEpisode
  open: boolean
  onToggle: () => void
  onInteractiveSearch: (t: Target) => void
}) {
  const label = `${e.season}×${String(e.episode).padStart(2, '0')}`
  return (
    <>
      <button type="button" className={`item-ep${open ? ' open' : ''}`} aria-expanded={open} onClick={onToggle}>
        <span className="lbl">{label}</span>
        <span className="item-pips" aria-hidden="true">
          {e.stages.map((st) => <span key={st.key} className={stateClass(st.state)} />)}
        </span>
        <span className="say">{e.summary}</span>
      </button>
      {open && (
        <div className="item-ep-track">
          <Track stages={e.stages} label={`Stages for ${label}`} />
          <StatusActions itemId={itemId} actions={stageActions(e.stages).filter((a) => a !== 'cancel_download')}
            onInteractiveSearch={() => onInteractiveSearch({ season: e.season, episode: e.episode })} />
        </div>
      )}
    </>
  )
}

function StatusActions({ itemId, actions, download, cinema, onInteractiveSearch }: {
  itemId: number
  actions: StatusAction[]
  download?: StatusDownload
  cinema?: ItemStatus['cinema']
  onInteractiveSearch: () => void
}) {
  const qc = useQueryClient()
  const [notice, setNotice] = useState({ message: '', warning: false, revision: 0 })
  const [confirmCancel, setConfirmCancel] = useState(false)
  const say = (message: string, warning = false) => setNotice((n) => ({ message, warning, revision: n.revision + 1 }))
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['item-status', itemId] })
    void qc.invalidateQueries({ queryKey: ['library-item', String(itemId)] })
    void qc.invalidateQueries({ queryKey: ['queue'] })
  }
  const run = useMutation({
    mutationFn: async (action: StatusAction): Promise<string> => {
      switch (action) {
        case 'search':
          return describeAutoSearch(await autoSearchItem(itemId))
        case 'cancel_download':
          await removeQueueItem(download!.id, true)
          return 'Download cancelled in Runner.'
        case 'retry_import':
          await importQueueItem(download!.id)
          return 'Import started again.'
        case 'resend_cinema':
          await retryDelivery(cinema!.notifierId, cinema!.deliveryId)
          return 'Sent to cinema again; the panel shows its answer.'
        case 'confirm_cinema': {
          const r = await confirmItemInCinema(itemId)
          return `Asked cinema to confirm ${r.queued} folder${r.queued === 1 ? '' : 's'}; the panel shows its answer.`
        }
        default:
          return ''
      }
    },
    onSuccess: (message) => {
      say(message)
      refresh()
    },
    onError: (e) => say(`✕ ${(e as Error).message}`, true),
  })
  const usable = actions.filter((a) =>
    a === 'search' || a === 'interactive_search' || a === 'open_activity' || a === 'confirm_cinema' ||
    ((a === 'cancel_download' || a === 'retry_import') && download) || (a === 'resend_cinema' && cinema))
  if (usable.length === 0 && !notice.message) return null
  return (
    <>
      <div className="item-status-actions">
        {usable.map((a) =>
          a === 'open_activity' ? (
            <Link key={a} to="/activity" className="btn-link">{ACTION_LABELS[a]}</Link>
          ) : (
            <button key={a} type="button" className={a === 'search' ? 'btn-accent' : a === 'cancel_download' ? 'btn-danger' : undefined}
              disabled={run.isPending}
              onClickCapture={focusActionTrigger}
              onClick={() => {
                if (a === 'interactive_search') onInteractiveSearch()
                else if (a === 'cancel_download') setConfirmCancel(true)
                else run.mutate(a)
              }}>
              {run.isPending && run.variables === a ? 'Working…' : ACTION_LABELS[a]}
            </button>
          ),
        )}
      </div>
      {notice.message && <ActionNotice warning={notice.warning} revision={notice.revision}>{notice.message}</ActionNotice>}
      {confirmCancel && download && (
        <ActionDialog title="Cancel download" onClose={() => setConfirmCancel(false)}>
          <p>Cancel <span className="mono">{download.release}</span> in Runner? The item stays wanted.</p>
          <div className="head-actions">
            <button type="button" className="btn-danger" onClick={() => { setConfirmCancel(false); run.mutate('cancel_download') }}>
              Cancel download
            </button>
            <button type="button" onClick={() => setConfirmCancel(false)}>Keep it</button>
          </div>
        </ActionDialog>
      )}
    </>
  )
}
