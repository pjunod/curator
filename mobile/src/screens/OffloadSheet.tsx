import { useEffect, useRef, useState } from 'react'
import { Modal, ScrollView, StyleSheet, Switch, Text, View } from 'react-native'
import { SafeAreaProvider, SafeAreaView } from 'react-native-safe-area-context'
import type { MonarrClient } from '../api'
import { AppScreen, Button, Header, IconButton, InlineError, LoadingState, Panel, SectionTitle } from '../components/UI'
import { formatBytes } from '../format'
import { OFFLOAD_RUNNING, freeText, offloadText } from '../offload'
import { useTheme } from '../theme'
import type { OffloadOperation, OffloadPreview, OffloadRequest } from '../types'

/**
 * Offload… (ADR 0028): free the space, keep the title. Says exactly what goes
 * and how much comes back before anything is deleted; progress and Stop stay
 * here until it is done. Same endpoints as web.
 */
export function OffloadSheet({ client, itemId, label, request, initialOperation, onClose }: {
  client: MonarrClient
  itemId: number
  label: string
  request: OffloadRequest
  initialOperation?: OffloadOperation
  onClose: () => void
}) {
  const theme = useTheme()
  const [preview, setPreview] = useState<{ data: OffloadPreview | null; error: string; loading: boolean }>({ data: null, error: '', loading: true })
  const [nonce, setNonce] = useState(0)
  const [sure, setSure] = useState(false)
  const [starting, setStarting] = useState(false)
  const [startError, setStartError] = useState('')
  const [op, setOp] = useState<OffloadOperation | null>(initialOperation ?? null)
  const requestKey = JSON.stringify(request)

  useEffect(() => {
    if (op) return
    let live = true
    setPreview((prev) => ({ ...prev, loading: true, error: '' }))
    client.previewOffload(itemId, JSON.parse(requestKey))
      .then((data) => { if (live) setPreview({ data, error: '', loading: false }) })
      .catch((cause) => { if (live) setPreview({ data: null, error: cause instanceof Error ? cause.message : 'Could not preview this Offload.', loading: false }) })
    return () => { live = false }
  }, [client, itemId, requestKey, op, nonce])

  const [reviewing, setReviewing] = useState(false)
  useEffect(() => {
    if (initialOperation || reviewing) return
    let live = true
    const scope: OffloadRequest = JSON.parse(requestKey)
    void client.listLibraryOffloads(itemId).then((operations) => {
      const selected = operations.find((entry) => OFFLOAD_RUNNING.has(entry.state) && entry.scope === scope.scope
        && (entry.scope !== 'season' || entry.season === scope.season) && (entry.scope !== 'episode' || entry.episodeId === scope.episodeId))
      if (live && selected) setOp((cur) => cur ?? selected)
    }).catch(() => undefined)
    return () => { live = false }
  }, [client, itemId, requestKey, initialOperation, reviewing])
  const operationId = op?.operationId
  const polling = op != null && OFFLOAD_RUNNING.has(op.state)
  useEffect(() => {
    if (!operationId || !polling) return
    let live = true
    const poll = () => { void client.getOffload(operationId).then((status) => { if (live) setOp(status) }).catch(() => undefined) }
    const timer = setInterval(poll, 1000)
    return () => { live = false; clearInterval(timer) }
  }, [client, operationId, polling])

  // One request id per preview, so a retried tap is the same request.
  const requestIds = useRef(new Map<string, string>())
  const requestIdFor = (fingerprint: string) => {
    let id = requestIds.current.get(fingerprint)
    if (!id) {
      id = `offload-${itemId}-${fingerprint}-${Math.random().toString(36).slice(2, 10)}`
      requestIds.current.set(fingerprint, id)
    }
    return id
  }
  const start = async (p: OffloadPreview) => {
    setStarting(true)
    setStartError('')
    try {
      setOp(await client.startOffload(itemId, { ...request, requestId: requestIdFor(p.fingerprint), fingerprint: p.fingerprint }))
    } catch (cause) {
      setStartError(cause instanceof Error ? cause.message : 'Could not start the Offload.')
      setNonce((n) => n + 1) // blocked or stale: the preview says what changed
    } finally {
      setStarting(false)
    }
  }
  const stop = async () => {
    if (!op) return
    try {
      await client.cancelOffload(op.operationId)
      setOp(await client.getOffload(op.operationId))
    } catch (cause) {
      setStartError(cause instanceof Error ? cause.message : 'Could not stop the Offload.')
    }
  }

  const p = preview.data
  const running = op ? OFFLOAD_RUNNING.has(op.state) : false
  return (
    <Modal animationType="slide" presentationStyle="fullScreen" onRequestClose={onClose}>
      <SafeAreaProvider>
        <AppScreen forceTopInset>
          <Header title={`Offload ${label}`} subtitle={op ? 'Progress' : p ? 'Preview' : 'Working it out'} left={<IconButton label="Close" glyph="×" onPress={onClose} />} />
          <ScrollView style={styles.flex} contentContainerStyle={styles.content} keyboardShouldPersistTaps="handled">
            {op ? (
              <Panel style={styles.line}>
                <Text accessibilityRole="alert" style={{ color: op.state === 'failed' ? theme.error : theme.text }}>{offloadText(op)}</Text>
                {(op.failed ?? []).map((f) => <Text key={f} style={[styles.mono, { color: theme.warning }]}>{f}</Text>)}
                {op.cinemaLine ? <Text style={{ color: theme.muted }}>{op.cinemaLine}</Text> : null}
                <Text style={{ color: theme.muted }}>Operation {op.operationId}</Text>
              </Panel>
            ) : (
              <>
                {preview.loading && !p ? <LoadingState label="Working out what goes…" /> : null}
                <InlineError message={preview.error} />
                {p ? (
                  <>
                    <Text style={{ color: theme.text }}>
                      Deletes {p.files.length} file{p.files.length === 1 ? '' : 's'} of {p.label} and keeps the title: its identity, profile, history and blocklist stay, and nothing searches for it again until you Re-acquire it.
                    </Text>
                    {p.widened && p.widened.length > 0 ? <Text style={{ color: theme.warning }}>A file covers more than you chose, so the Offload widens to {p.widened.join(', ')} as well.</Text> : null}
                    <SectionTitle>What will be deleted</SectionTitle>
                    {p.files.map((f) => (
                      <Panel key={f.path} style={styles.line}>
                        <Text style={[styles.mono, { color: theme.text }]}>{f.path}</Text>
                        <Text style={{ color: theme.muted }}>{f.kind === 'sidecar' ? 'sidecar' : 'video'} · {formatBytes(f.bytes)}{f.episodes?.length ? ` · ${f.episodes.join(', ')}` : ''}</Text>
                        {f.shared ? <Text style={{ color: theme.muted }}>another hard link (a seeding copy) keeps these bytes until it goes too</Text> : null}
                      </Panel>
                    ))}
                    {p.files.length === 0 ? <Text style={{ color: theme.muted }}>There are no files on disk. The title is only marked offloaded.</Text> : null}
                    <Text style={{ color: theme.text }}>{formatBytes(p.freed)} freed{p.freed < p.bytes ? ` (${formatBytes(p.bytes)} deleted)` : ''}</Text>
                    <Text style={{ color: theme.text }}>Free space {freeText(p.freeBefore)} → {freeText(p.freeAfter)}{p.root ? ` on ${p.root}` : ''}</Text>
                    {p.cinema ? <Text style={{ color: p.cinemaWarning ? theme.warning : theme.muted }}>{p.cinema}.</Text> : null}
                    {p.blocked ? (
                      <>
                        <InlineError message={p.blocked.message} />
                        {p.blocked.reason === 'unmounted'
                          ? <Button secondary compact label="Refresh preview" onPress={() => setNonce((n) => n + 1)} />
                          : <Text style={{ color: theme.text }}>You can queue this Offload while it waits.</Text>}
                      </>
                    ) : null}
                    <View style={styles.row}>
                      <Switch accessibilityLabel="I understand the files are deleted, not moved to a trash" value={sure} onValueChange={setSure} />
                      <Text style={[styles.flex, { color: theme.text }]}>I understand the files are deleted, not moved to a trash</Text>
                    </View>
                  </>
                ) : null}
                <InlineError message={startError} />
              </>
            )}
            {op ? <InlineError message={startError} /> : null}
          </ScrollView>
          <SafeAreaView edges={{ bottom: 'maximum' }} style={[styles.footer, { borderColor: theme.border }]}>
            {op ? (
              <>
                {op.state === 'needs_review' ? <Button secondary label="Refresh preview" onPress={() => {
                  setReviewing(true)
                  setSure(false)
                  setOp(null)
                  setNonce((n) => n + 1)
                }} /> : null}
                {op.stoppable ? <Button secondary label="Stop" onPress={() => void stop()} /> : null}
                <Button label={running ? 'Close (keeps running)' : 'Done'} onPress={onClose} />
              </>
            ) : (
              <Button label={starting ? 'Queuing…' : 'Queue Offload'} disabled={!p || p.blocked?.reason === 'unmounted' || !sure || starting || preview.loading}
                onPress={() => { if (p) void start(p) }} />
            )}
          </SafeAreaView>
        </AppScreen>
      </SafeAreaProvider>
    </Modal>
  )
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  content: { padding: 16, gap: 12 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  mono: { fontFamily: 'Menlo', fontSize: 12 },
  line: { gap: 6 },
  footer: { padding: 16, gap: 8, borderTopWidth: StyleSheet.hairlineWidth },
})

export function OffloadOperations({ client, itemId, sheetOpen, onOpen }: {
  client: MonarrClient
  itemId: number
  sheetOpen: boolean
  onOpen: (op: OffloadOperation) => void
}) {
  const theme = useTheme()
  const [operations, setOperations] = useState<OffloadOperation[]>([])
  const running = operations.some((op) => OFFLOAD_RUNNING.has(op.state))
  useEffect(() => {
    let live = true
    const refresh = () => { void client.listLibraryOffloads(itemId).then((rows) => { if (live) setOperations(rows) }).catch(() => undefined) }
    refresh()
    const timer = running ? setInterval(refresh, 1000) : undefined
    return () => { live = false; clearInterval(timer) }
  }, [client, itemId, running, sheetOpen])
  if (!operations.length) return null
  return (
    <Panel style={styles.line}>
      <SectionTitle>Offload operations</SectionTitle>
      {operations.map((op) => (
        <View key={op.operationId} style={styles.row}>
          <View style={styles.flex}>
            <Text style={{ color: theme.text }}>{op.label}</Text>
            <Text style={{ color: theme.muted }}>{offloadText(op)}</Text>
          </View>
          <Button compact secondary label={op.state === 'needs_review' ? 'Review' : 'Details'} onPress={() => onOpen(op)} />
        </View>
      ))}
    </Panel>
  )
}
