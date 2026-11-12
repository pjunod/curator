import { useEffect, useMemo, useState } from 'react'
import { Modal, ScrollView, StyleSheet, Switch, Text, View } from 'react-native'
import { SafeAreaProvider, SafeAreaView } from 'react-native-safe-area-context'
import type { MonarrClient } from '../api'
import { AppScreen, Badge, Button, Chip, Field, Header, IconButton, InlineError, LoadingState, Panel, SectionTitle } from '../components/UI'
import { formatBytes } from '../format'
import { episodeToken, rematchCandidates, type RematchCandidate } from '../rematch'
import { useTheme } from '../theme'
import type { MediaItemDetail, MediaItemSummary, RematchChoice, RematchMove, RematchOperation, RematchPreview, RematchTarget, SearchResult } from '../types'

const RUNNING = new Set(['planned', 'moving', 'moved', 'committed', 'notified', 'cancel_pending'])
const MODE_TEXT = { rename: 'renamed (same volume)', copy: 'copied across volumes, verified, then the original removed', relink: 'already named this; only the library changes' }

/**
 * Re-match… (all files) or Move to… (one file) on the item screen: choose
 * the title or episode, read the preview, move, and follow it (bytes, Stop,
 * result). Same endpoints as web (ADR 0027).
 */
export function RematchSheet({ client, item, onClose }: { client: MonarrClient; item: MediaItemDetail; onClose: () => void }) {
  const theme = useTheme()
  const series = item.kind === 'series'
  const [fileId, setFileId] = useState(0) // 0: every file (Re-match…)
  const [scope, setScope] = useState<'same' | 'other'>('other')
  const [draft, setDraft] = useState(item.title)
  const [query, setQuery] = useState('')
  const [found, setFound] = useState<{ library: MediaItemSummary[]; search: SearchResult[]; error: string; loading: boolean }>({ library: [], search: [], error: '', loading: false })
  const [picked, setPicked] = useState<RematchCandidate | null>(null)
  const [choices, setChoices] = useState<Record<number, RematchChoice>>({})
  const [blocklist, setBlocklist] = useState<boolean | null>(null)
  const [preview, setPreview] = useState<{ data: RematchPreview | null; error: string; loading: boolean }>({ data: null, error: '', loading: false })
  const [nonce, setNonce] = useState(0)
  const [starting, setStarting] = useState(false)
  const [startError, setStartError] = useState('')
  const [op, setOp] = useState<RematchOperation | null>(null)

  useEffect(() => {
    if (!query) return
    let live = true
    setFound((prev) => ({ ...prev, loading: true, error: '' }))
    void Promise.allSettled([client.getLibrary(item.kind), client.searchMetadata(item.kind, query)]).then(([lib, meta]) => {
      if (!live) return
      setFound({ library: lib.status === 'fulfilled' ? lib.value : [], search: meta.status === 'fulfilled' ? meta.value : [],
        error: meta.status === 'rejected' ? 'Provider search unavailable; library titles are still listed.' : '', loading: false })
    })
    return () => { live = false }
  }, [client, item.kind, query])
  const candidates = useMemo(() => rematchCandidates(found.library, found.search, query, item.id), [found, query, item.id])

  const target: RematchTarget | null = scope === 'same' ? { itemId: item.id } : picked?.target ?? null
  const requestKey = target ? JSON.stringify({ target, fileIds: fileId ? [fileId] : undefined, choices: Object.values(choices), blocklist: blocklist ?? undefined }) : ''
  useEffect(() => {
    if (!requestKey || op) return
    let live = true
    setPreview((prev) => ({ ...prev, loading: true, error: '' }))
    client.previewRematch(item.id, JSON.parse(requestKey))
      .then((data) => { if (live) setPreview({ data, error: '', loading: false }) })
      .catch((cause) => { if (live) setPreview({ data: null, error: cause instanceof Error ? cause.message : 'Could not preview this move.', loading: false }) })
    return () => { live = false }
  }, [client, item.id, requestKey, op, nonce])

  useEffect(() => {
    if (!op || !RUNNING.has(op.state)) return
    const timer = setTimeout(() => { void client.getRematch(op.operationId).then(setOp).catch(() => undefined) }, 1000)
    return () => clearTimeout(timer)
  }, [client, op])

  const start = async (p: RematchPreview) => {
    setStarting(true)
    setStartError('')
    try {
      setOp(await client.startRematch(item.id, { ...JSON.parse(requestKey), requestId: `rematch-${item.id}-${p.fingerprint}-${Date.now()}`,
        sourceRevision: p.sourceRevision, targetRevision: p.targetRevision, fingerprint: p.fingerprint }))
    } catch (cause) {
      setStartError(cause instanceof Error ? cause.message : 'Could not start the move.')
      setNonce((n) => n + 1) // blocked or stale: the preview says what changed
    } finally {
      setStarting(false)
    }
  }
  const stop = async () => {
    if (!op) return
    try {
      await client.cancelRematch(op.operationId)
      setOp(await client.getRematch(op.operationId))
    } catch (cause) {
      setStartError(cause instanceof Error ? cause.message : 'Could not stop the move.')
    }
  }

  const p = preview.data
  const file = item.files.find((f) => f.id === fileId)
  const pickEpisode = (m: RematchMove, value: string) => setChoices((prev) => {
    const next = { ...prev }
    if (value === 'leave') next[m.fileId] = { fileId: m.fileId, leave: true }
    else {
      const [s, e] = value.split(':').map(Number)
      next[m.fileId] = { fileId: m.fileId, season: s, episodes: [e ?? 0] }
    }
    return next
  })
  const episodeChips = (m: RematchMove) => series && p?.episodes ? (
    <ScrollView horizontal showsHorizontalScrollIndicator={false} contentContainerStyle={styles.chips}>
      {p.episodes.map((ep) => {
        const value = `${ep.season}:${ep.episode}`
        const chosen = choices[m.fileId]
        const selected = chosen?.episodes ? `${chosen.season}:${chosen.episodes[0]}` === value : !chosen?.leave && m.season === ep.season && m.episodes[0] === ep.episode
        return <Chip key={value} label={episodeToken(ep.season, [ep.episode])} selected={selected} onPress={() => pickEpisode(m, value)} />
      })}
      <Chip label="Leave it out" selected={!!choices[m.fileId]?.leave} onPress={() => pickEpisode(m, 'leave')} />
    </ScrollView>
  ) : null

  const title = file ? `Move ${file.path.split('/').pop()}` : `Re-match ${item.title}`
  return (
    <Modal animationType="slide" presentationStyle="fullScreen" onRequestClose={onClose}>
      <SafeAreaProvider>
        <AppScreen forceTopInset>
          <Header title={title} subtitle={op ? 'Moving' : p ? 'Preview' : 'Choose'} left={<IconButton label="Close" glyph="×" onPress={onClose} />} />
          <ScrollView style={styles.flex} contentContainerStyle={styles.content} keyboardShouldPersistTaps="handled">
            {op ? <Progress op={op} /> : <>
              <SectionTitle>Files</SectionTitle>
              <View style={styles.wrap}>
                <Chip label="All files (Re-match…)" selected={fileId === 0} onPress={() => { setFileId(0); setScope('other'); setChoices({}) }} />
                {item.files.map((f) => <Chip key={f.id} label={`Move ${f.path.split('/').pop()}`} selected={fileId === f.id}
                  onPress={() => { setFileId(f.id); setScope(series ? 'same' : 'other'); setChoices({}) }} />)}
              </View>
              {fileId && series ? (
                <View style={styles.wrap}>
                  <Chip label="Same show, different episode" selected={scope === 'same'} onPress={() => setScope('same')} />
                  <Chip label="A different show" selected={scope === 'other'} onPress={() => setScope('other')} />
                </View>
              ) : null}
              {scope === 'other' ? <>
                <View style={styles.row}>
                  <Field style={styles.flex} value={draft} onChangeText={setDraft} placeholder="Title" autoCorrect={false} accessibilityLabel="Title to search for" />
                  <Button compact label="Search" disabled={!draft.trim()} onPress={() => { setPicked(null); setQuery(draft.trim()) }} />
                </View>
                {found.loading ? <LoadingState label="Searching…" /> : null}
                {found.error ? <Text style={{ color: theme.muted }}>{found.error}</Text> : null}
                {candidates.map((c) => (
                  <Panel key={c.key} style={styles.line}>
                    <Text style={{ color: theme.text }}>{c.title}{c.year ? ` (${c.year})` : ''}</Text>
                    <Text style={{ color: theme.muted }}>{c.detail}</Text>
                    {c.current && !fileId ? <Badge label="current" /> : (
                      <Button secondary compact label={picked?.key === c.key ? 'Selected' : 'Choose'} onPress={() => { setPicked(c); setChoices({}) }} />
                    )}
                  </Panel>
                ))}
              </> : null}

              {preview.loading && !p ? <LoadingState label="Working out what moves where…" /> : null}
              <InlineError message={preview.error} />
              {p && target ? <>
                <SectionTitle>What will change</SectionTitle>
                {!p.target.inLibrary ? <Text style={{ color: theme.text }}>{p.target.title} is added to the library at {p.target.folder}</Text> : null}
                {[...p.moves, ...p.leftOut].map((m) => (
                  <Panel key={m.fileId} style={styles.line}>
                    <Text style={[styles.mono, { color: theme.muted }]}>{m.from}</Text>
                    {m.to ? <Text style={[styles.mono, { color: theme.text }]}>→ {m.to}</Text> : null}
                    {m.problem ? <Text style={{ color: theme.warning }}>{m.problem}</Text> : null}
                    {episodeChips(m)}
                    {m.mode ? <Text style={{ color: theme.muted }}>{MODE_TEXT[m.mode]} · {formatBytes(m.bytes)}</Text> : null}
                    {m.note ? <Text style={{ color: theme.warning }}>{m.note}</Text> : null}
                  </Panel>
                ))}
                {p.source.id !== p.target.id ? <Text style={{ color: theme.text }}>{p.source.title}: {p.sourceReason}</Text> : null}
                {p.blocklist.length > 0 ? (
                  <View style={styles.row}>
                    <Switch accessibilityLabel="Blocklist the release" value={blocklist ?? p.blocklistOn} onValueChange={setBlocklist} />
                    <Text style={[styles.flex, { color: theme.text }]}>Blocklist {p.blocklist.map((b) => b.release).join(', ')} for {p.source.title}</Text>
                  </View>
                ) : null}
                {p.blocklistNote ? <Text style={{ color: theme.muted }}>{p.blocklistNote}.</Text> : null}
                {p.cinema ? <Text style={{ color: p.cinema.includes('does not support') ? theme.warning : theme.muted }}>{p.cinema}.</Text> : null}
                {p.blocked ? <>
                  <InlineError message={p.blocked.message} />
                  <Button secondary compact label="Wait for it" onPress={() => setNonce((n) => n + 1)} />
                </> : null}
              </> : null}
              <InlineError message={startError} />
            </>}
          </ScrollView>
          <SafeAreaView edges={{ bottom: 'maximum' }} style={[styles.footer, { borderColor: theme.border }]}>
            {op ? <>
              {['planned', 'moving', 'moved'].includes(op.state) ? <Button secondary label="Stop" onPress={() => void stop()} /> : null}
              <Button label={RUNNING.has(op.state) ? 'Close (keeps running)' : 'Done'} onPress={onClose} />
            </> : (
              <Button label={starting ? 'Starting…' : fileId ? 'Move' : p?.summary ?? 'Move'}
                disabled={!p || !!p.blocked || starting || preview.loading || (p.moves.length === 0 && p.leftOut.length === 0)}
                onPress={() => { if (p) void start(p) }} />
            )}
          </SafeAreaView>
        </AppScreen>
      </SafeAreaProvider>
    </Modal>
  )
}

function Progress({ op }: { op: RematchOperation }) {
  const theme = useTheme()
  const r = op.result ?? {}
  const cinema = op.cinema ?? []
  let text: string
  switch (op.state) {
    case 'complete': text = `Moved ${r.moved?.length ?? 0} file${r.moved?.length === 1 ? '' : 's'}.`; break
    case 'cancelled': text = 'Stopped. Every file is back where it was.'; break
    case 'failed': text = `Nothing moved: ${op.lastError ?? 'the move failed'}. Every file is back where it was.`; break
    case 'cancel_pending': text = 'Stopping…'; break
    default: text = op.bytesTotal > 0 ? `Copying ${formatBytes(op.bytesDone)} of ${formatBytes(op.bytesTotal)}…` : 'Moving…'
  }
  return (
    <Panel style={styles.line}>
      <Text accessibilityRole="alert" style={{ color: op.state === 'failed' ? theme.error : theme.text }}>{text}</Text>
      {r.failed?.length ? <Text style={{ color: theme.warning }}>Not moved, needs review: {r.failed.map((f) => f.from).join('; ')}</Text> : null}
      {r.leftOut?.length ? <Text style={{ color: theme.muted }}>Left out (still on disk): {r.leftOut.join(', ')}</Text> : null}
      {r.blocklisted?.length ? <Text style={{ color: theme.muted }}>Blocklisted: {r.blocklisted.map((b) => b.release).join(', ')}</Text> : null}
      {op.state === 'complete' && cinema.length > 0 ? <Text style={{ color: theme.muted }}>{cinema.every((c) => c.visibleAt) ? 'In cinema.' : 'Moved, cinema confirming…'}</Text> : null}
    </Panel>
  )
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  content: { padding: 16, gap: 12 },
  wrap: { flexDirection: 'row', flexWrap: 'wrap', gap: 6 },
  chips: { gap: 6 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  mono: { fontFamily: 'Menlo', fontSize: 12 },
  line: { gap: 6 },
  footer: { padding: 16, gap: 8, borderTopWidth: StyleSheet.hairlineWidth },
})
