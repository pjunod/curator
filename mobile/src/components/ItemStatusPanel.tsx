import { useCallback, useEffect, useRef, useState } from 'react'
import { Alert, Pressable, StyleSheet, Text, View } from 'react-native'
import type { MonarrClient } from '../api'
import { describeAutoSearch } from '../autosearch'
import { ACTION_LABELS, REMATCH_STATE, STAGE_NAMES, STATE_ICON, downloadLine, fmtWhen, rematchCounts, stageActions, statusPollMs } from '../itemStatus'
import { useTheme, type Theme } from '../theme'
import type { ItemStatus, ReleaseSearchScope, StatusAction, StatusCinema, StatusEpisode, StatusRematch, StatusStage, StatusState } from '../types'
import { Button, Panel, SectionTitle } from './UI'

// "Where it stands" on the phone: the same GET /library/{id}/status the web
// panel reads, polled every 2 s while a download moves and every 30 s
// otherwise. Each action's result is written directly under its buttons.

function stateColor(theme: Theme, state: StatusState): string {
  return { done: theme.ok, active: theme.accent, waiting: theme.warning, failed: theme.error, todo: theme.border }[state]
}

/** Polls one status URL; the next poll is scheduled from the last answer. */
function useStatus(load: () => Promise<ItemStatus>, key: string) {
  const [data, setData] = useState<ItemStatus | null>(null)
  const [error, setError] = useState('')
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const alive = useRef(true)
  // Each refresh takes a generation; only the newest may answer or schedule
  // the next poll, so an action's refresh racing a timed one cannot fork the
  // chain into two pollers.
  const generation = useRef(0)
  const refresh = useCallback(async () => {
    if (timer.current) clearTimeout(timer.current)
    const mine = ++generation.current
    let next: ItemStatus | null = null
    try {
      next = await load()
      if (alive.current && mine === generation.current) {
        setData(next)
        setError('')
      }
    } catch (cause) {
      if (alive.current && mine === generation.current) setError(cause instanceof Error ? cause.message : 'Status unavailable.')
    }
    if (alive.current && mine === generation.current) timer.current = setTimeout(() => void refresh(), statusPollMs(next))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key])
  useEffect(() => {
    alive.current = true
    void refresh()
    return () => {
      alive.current = false
      generation.current++
      if (timer.current) clearTimeout(timer.current)
    }
  }, [refresh])
  return { data, error, refresh }
}

export function ItemStatusPanel({ client, id, kind, onInteractiveSearch, onOpenActivity }: {
  client: MonarrClient
  id: number
  kind: string
  onInteractiveSearch: (scope: ReleaseSearchScope) => void
  onOpenActivity?: () => void
}) {
  const theme = useTheme()
  const status = useStatus(() => client.getItemStatus(id), `item-${id}`)
  // Collapsed is decided on the first answer and then only by the viewer: a
  // background poll never folds the panel away.
  const [collapsed, setCollapsed] = useState<boolean | null>(null)
  const s = status.data
  useEffect(() => {
    if (s && collapsed === null) setCollapsed(s.done)
  }, [s, collapsed])
  return (
    <>
      <SectionTitle>Where it stands</SectionTitle>
      <Panel style={styles.panel}>
        <MovingFiles client={client} r={s?.rematch} onChanged={status.refresh} />
        {!s ? (
          <Text style={{ color: theme.muted }}>{status.error || 'Loading…'}</Text>
        ) : s.done && collapsed ? (
          <Pressable accessibilityRole="button" accessibilityState={{ expanded: false }} onPress={() => setCollapsed(false)}>
            <Text style={[styles.title, { color: theme.ok }]}>✓ {s.headline.title}{s.headline.at ? ` ${fmtWhen(s.headline.at)}` : ''}</Text>
          </Pressable>
        ) : (
          <>
            <Text style={[styles.title, { color: theme.text }]}>
              <Text style={{ color: stateColor(theme, s.headline.state) }}>{STATE_ICON[s.headline.state]} </Text>
              {s.headline.title}
              {s.headline.at ? <Text style={{ color: theme.muted }}> · {fmtWhen(s.headline.at)}</Text> : null}
            </Text>
            {s.headline.detail ? <Text style={[styles.small, { color: theme.muted }]}>{s.headline.detail}</Text> : null}
            {kind === 'series' ? (
              <Seasons client={client} id={id} s={s} onInteractiveSearch={onInteractiveSearch} onOpenActivity={onOpenActivity} />
            ) : (
              <>
                <Track stages={s.stages ?? []} />
                {s.download && s.active ? <Text style={[styles.small, { color: theme.text }]}>{s.download.release} · {downloadLine(s.download)}</Text> : null}
                {s.cinema?.waitingOn ? <Text style={[styles.small, { color: theme.warning }]}>Cinema is waiting on {s.cinema.waitingOn.replaceAll('_', ' ')}.</Text> : null}
                <Actions client={client} id={id} download={s.download} cinema={s.cinema} actions={stageActions(s.stages)} onChanged={status.refresh}
                  onInteractiveSearch={() => onInteractiveSearch({})} onOpenActivity={onOpenActivity} />
                {(s.attempts ?? []).map((a) => (
                  <Text key={a.downloadId} style={[styles.small, { color: theme.muted }]}>
                    Earlier: {fmtWhen(a.at)} {a.release} · {a.outcome}{a.reason ? ` (${a.reason})` : ''}
                  </Text>
                ))}
              </>
            )}
            {s.done ? <Button compact secondary label="Collapse" onPress={() => setCollapsed(true)} /> : null}
          </>
        )}
      </Panel>
    </>
  )
}

/** The open Re-match, with Stop. Its answer stays under the button, also after the block is gone. */
function MovingFiles({ client, r, onChanged }: { client: MonarrClient; r?: StatusRematch; onChanged: () => Promise<void> }) {
  const theme = useTheme()
  const [busy, setBusy] = useState(false)
  const [message, setMessage] = useState({ text: '', error: false })
  const stop = async () => {
    setBusy(true)
    setMessage({ text: '', error: false })
    try {
      await client.cancelRematch(r!.operationId)
      setMessage({ text: 'Stopping the move; every moved file is put back where it was.', error: false })
      await onChanged()
    } catch (cause) {
      setMessage({ text: cause instanceof Error ? cause.message : 'That did not work.', error: true })
    } finally {
      setBusy(false)
    }
  }
  if (!r && !message.text) return null
  return (
    <View style={[styles.moving, { borderColor: theme.accent }]} accessibilityLabel="Moving files">
      {r ? (
        <>
          <Text style={[styles.stageName, { color: theme.muted }]}>MOVING FILES</Text>
          <Text style={[styles.title, { color: theme.text }]}>{r.headline}</Text>
          <Text style={[styles.small, { color: theme.muted }]}>{REMATCH_STATE[r.state]} · {rematchCounts(r)}</Text>
          {(r.moves ?? []).map((m) => (
            <Text key={m.from} style={[styles.small, { color: theme.text }]}>{m.from} → {m.to}</Text>
          ))}
          {r.cinemaLine ? <Text style={[styles.small, { color: theme.muted }]}>{r.cinemaLine}</Text> : null}
          {(r.warnings ?? []).map((w) => <Text key={w} style={[styles.small, { color: theme.warning }]}>{w}</Text>)}
          {r.error ? <Text style={[styles.small, { color: theme.warning }]}>{r.error}</Text> : null}
          {r.stoppable ? <Button compact secondary disabled={busy} label={busy ? 'Stopping…' : 'Stop'} onPress={() => void stop()} /> : null}
        </>
      ) : null}
      {message.text ? (
        <Text accessibilityLiveRegion="polite" style={[styles.small, { color: message.error ? theme.error : theme.ok }]}>{message.text}</Text>
      ) : null}
    </View>
  )
}

function Track({ stages }: { stages: StatusStage[] }) {
  const theme = useTheme()
  return (
    <View style={styles.track} accessibilityLabel="Stages">
      {stages.map((st) => (
        <View key={st.key} style={[styles.stage, { borderLeftColor: stateColor(theme, st.state) }]}>
          <View style={styles.stageHead}>
            <Text style={[styles.stageName, { color: st.state === 'todo' ? theme.muted : theme.text }]}>
              <Text style={{ color: stateColor(theme, st.state) }}>{STATE_ICON[st.state]} </Text>
              {STAGE_NAMES[st.key]}
            </Text>
            <Text style={[styles.small, { color: theme.muted }]}>{st.at ? fmtWhen(st.at) : st.state === 'active' ? 'now' : ''}</Text>
          </View>
          {st.detail ? <Text style={[styles.small, { color: theme.muted }]}>{st.detail}</Text> : null}
        </View>
      ))}
    </View>
  )
}

function Seasons({ client, id, s, onInteractiveSearch, onOpenActivity }: {
  client: MonarrClient
  id: number
  s: ItemStatus
  onInteractiveSearch: (scope: ReleaseSearchScope) => void
  onOpenActivity?: () => void
}) {
  const theme = useTheme()
  const [open, setOpen] = useState<number | null>(null)
  return (
    <View style={styles.seasons}>
      {(s.seasons ?? []).map((sn) => (
        <View key={sn.season} style={[styles.season, { borderTopColor: theme.border }]}>
          <Pressable accessibilityRole="button" accessibilityState={{ expanded: open === sn.season }}
            accessibilityLabel={`Where it stands for ${sn.season === 0 ? 'specials' : `season ${sn.season}`}: ${sn.summary}`} onPress={() => setOpen(open === sn.season ? null : sn.season)}>
            <Text style={[styles.stageName, { color: theme.text }]}>{sn.season === 0 ? 'Specials' : `Season ${sn.season}`}</Text>
            <Text style={[styles.small, { color: theme.muted }]}>{sn.summary}</Text>
          </Pressable>
          {sn.download ? (
            <Text style={[styles.small, { color: theme.text }]}>Season pack in runner: {sn.download.release} · {downloadLine(sn.download)}</Text>
          ) : null}
          {open === sn.season ? <Episodes client={client} id={id} season={sn.season} onInteractiveSearch={onInteractiveSearch} onOpenActivity={onOpenActivity} /> : null}
        </View>
      ))}
    </View>
  )
}

function Episodes({ client, id, season, onInteractiveSearch, onOpenActivity }: {
  client: MonarrClient
  id: number
  season: number
  onInteractiveSearch: (scope: ReleaseSearchScope) => void
  onOpenActivity?: () => void
}) {
  const theme = useTheme()
  const status = useStatus(() => client.getItemStatus(id, season), `item-${id}-${season}`)
  const [open, setOpen] = useState<number | null>(null)
  if (!status.data) return <Text style={[styles.small, { color: theme.muted }]}>{status.error || 'Loading episodes…'}</Text>
  return (
    <View>
      {(status.data.episodes ?? []).map((e: StatusEpisode) => (
        <View key={e.episodeId}>
          <Pressable accessibilityRole="button" accessibilityState={{ expanded: open === e.episodeId }} style={styles.ep}
            onPress={() => setOpen(open === e.episodeId ? null : e.episodeId)}>
            <Text style={[styles.small, { color: theme.muted, width: 44 }]}>{e.season}×{String(e.episode).padStart(2, '0')}</Text>
            <View style={styles.pips}>
              {e.stages.map((st) => <View key={st.key} style={[styles.pip, { backgroundColor: stateColor(theme, st.state) }]} />)}
            </View>
            <Text style={[styles.small, styles.say, { color: theme.muted }]}>{e.summary}{e.at ? ` · ${fmtWhen(e.at)}` : ''}</Text>
          </Pressable>
          {open === e.episodeId ? (
            <>
              <Track stages={e.stages} />
              <Actions client={client} id={id} episode={e}
                download={e.downloadId ? { id: e.downloadId } : undefined} cinema={e.cinema}
                actions={stageActions(e.stages).filter((a) => a !== 'cancel_download' || !e.inPack)}
                onChanged={status.refresh} onInteractiveSearch={() => onInteractiveSearch({ season: e.season, episode: e.episode })}
                onOpenActivity={onOpenActivity} />
            </>
          ) : null}
        </View>
      ))}
    </View>
  )
}

function Actions({ client, id, episode, download, cinema, actions, onChanged, onInteractiveSearch, onOpenActivity }: {
  client: MonarrClient
  id: number
  /** Set for an episode's track: search, confirm and cancel act on it alone. */
  episode?: StatusEpisode
  download?: { id: number; release?: string }
  cinema?: StatusCinema
  actions: StatusAction[]
  onChanged: () => Promise<void>
  onInteractiveSearch: () => void
  onOpenActivity?: () => void
}) {
  const theme = useTheme()
  const [busy, setBusy] = useState<StatusAction | null>(null)
  const [message, setMessage] = useState({ text: '', error: false })
  const run = async (action: StatusAction) => {
    setBusy(action)
    setMessage({ text: '', error: false })
    try {
      let text = ''
      if (action === 'search') {
        text = episode
          ? `Searching ${(await client.searchWantable(episode.wantableId)).scopeLabel}; this panel shows what it finds.`
          : describeAutoSearch(await client.autoSearchItem(id))
      }
      if (action === 'cancel_download' && download) {
        await client.cancelDownload(download.id)
        text = 'Download cancelled in Runner; the release is blocklisted.'
      }
      if (action === 'retry_import' && download) {
        await client.retryImport(download.id)
        text = 'Import started again.'
      }
      if (action === 'resend_cinema' && cinema) {
        await client.retryDelivery(cinema.notifierId, cinema.deliveryId)
        text = 'Sent to cinema again; this panel shows its answer.'
      }
      if (action === 'confirm_cinema') {
        const r = await client.confirmItemInCinema(id, episode?.episodeId)
        text = `Asked cinema to confirm ${r.queued} folder${r.queued === 1 ? '' : 's'}; this panel shows its answer.`
      }
      setMessage({ text, error: false })
      await onChanged()
    } catch (cause) {
      setMessage({ text: cause instanceof Error ? cause.message : 'That did not work.', error: true })
    } finally {
      setBusy(null)
    }
  }
  const press = (action: StatusAction) => {
    if (action === 'interactive_search') return onInteractiveSearch()
    if (action === 'open_activity') return onOpenActivity?.()
    if (action === 'cancel_download') {
      return Alert.alert('Cancel download', `Cancel ${download?.release ?? 'this download'} in Runner? The item stays wanted, and this release is blocklisted so automation does not grab it again.`, [
        { text: 'Keep it', style: 'cancel' },
        { text: 'Cancel download', style: 'destructive', onPress: () => void run('cancel_download') },
      ])
    }
    void run(action)
  }
  const usable = actions.filter((a) => (a !== 'open_activity' || onOpenActivity) &&
    ((a !== 'cancel_download' && a !== 'retry_import') || download) && (a !== 'resend_cinema' || cinema))
  if (usable.length === 0 && !message.text) return null
  return (
    <View style={styles.actions}>
      {usable.map((a) => (
        <Button key={a} compact secondary={a !== 'search'} disabled={busy !== null}
          label={busy === a ? 'Working…' : ACTION_LABELS[a]} onPress={() => press(a)} />
      ))}
      {message.text ? (
        <Text accessibilityLiveRegion="polite" style={[styles.small, styles.message, { color: message.error ? theme.error : theme.ok }]}>{message.text}</Text>
      ) : null}
    </View>
  )
}

const styles = StyleSheet.create({
  panel: { gap: 8 },
  moving: { borderWidth: 1, borderRadius: 8, padding: 10, gap: 4 },
  title: { fontSize: 15, fontWeight: '700' },
  small: { fontSize: 12, lineHeight: 17 },
  track: { gap: 2 },
  stage: { borderLeftWidth: 3, paddingLeft: 10, paddingVertical: 3 },
  stageHead: { flexDirection: 'row', justifyContent: 'space-between', gap: 8 },
  stageName: { fontSize: 13, fontWeight: '600' },
  actions: { flexDirection: 'row', flexWrap: 'wrap', gap: 8, alignItems: 'center' },
  message: { flexBasis: '100%' },
  seasons: { gap: 8 },
  season: { borderTopWidth: StyleSheet.hairlineWidth, paddingTop: 8, gap: 4 },
  ep: { flexDirection: 'row', alignItems: 'center', gap: 8, paddingVertical: 4, flexWrap: 'wrap' },
  pips: { flexDirection: 'row', gap: 3, width: 120 },
  pip: { height: 6, flex: 1, borderRadius: 3 },
  say: { flexBasis: '100%', paddingLeft: 52 },
})
