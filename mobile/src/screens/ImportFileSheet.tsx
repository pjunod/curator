import { useEffect, useMemo, useState } from 'react'
import { Modal, Pressable, ScrollView, StyleSheet, Switch, Text, View } from 'react-native'
import { SafeAreaProvider, SafeAreaView } from 'react-native-safe-area-context'
import type { MonarrClient } from '../api'
import { AppScreen, Badge, Button, Chip, Field, Header, IconButton, InlineError, LoadingState, Panel, SectionTitle } from '../components/UI'
import { formatBytes } from '../format'
import { MODE_TEXT, choiceOf, episodeLabel, isTerminal, originalsNote, planRequest, startImport, type EpisodeChoice, type StartedImport } from '../itemImport'
import { useTheme } from '../theme'
import type { BrowseResult, ImportPlanFile, MediaItemDetail, QueueItem, RootFolder, ScannedFile } from '../types'

/**
 * Import file… on the item page: browse a folder, pick files, confirm the
 * plan, then watch (and stop) the jobs it started. Same endpoints as web.
 */
export function ImportFileSheet({ client, item, onClose }: { client: MonarrClient; item: MediaItemDetail; onClose: () => void }) {
  const theme = useTheme()
  const [step, setStep] = useState<'choose' | 'confirm' | 'started'>('choose')
  const [path, setPath] = useState('')
  const [draft, setDraft] = useState('')
  const [roots, setRoots] = useState<RootFolder[]>([])
  const [defaultPath, setDefaultPath] = useState('')
  const [listing, setListing] = useState<{ dirs: BrowseResult | null; files: ScannedFile[]; error: string; loading: boolean }>({ dirs: null, files: [], error: '', loading: false })
  const [selected, setSelected] = useState<Record<string, ScannedFile>>({})
  const [choices, setChoices] = useState<Record<string, EpisodeChoice>>({})
  const [copyId, setCopyId] = useState(0)
  const [removeOriginal, setRemoveOriginal] = useState(false)
  const [plan, setPlan] = useState<{ lines: ImportPlanFile[]; error: string; loading: boolean }>({ lines: [], error: '', loading: false })
  const [submitting, setSubmitting] = useState(false)
  const [started, setStarted] = useState<StartedImport[]>([])

  useEffect(() => {
    void client.getRootFolders().then(setRoots).catch(() => setRoots([]))
    void client.getManualImportDefaultPath().then((res) => { setDefaultPath(res.path); setPath((p) => p || res.path) }).catch(() => undefined)
  }, [client])

  useEffect(() => {
    if (!path) return
    let live = true
    setListing((prev) => ({ ...prev, loading: true, error: '' }))
    const dir = path.endsWith('/') ? path : `${path}/`
    void Promise.allSettled([client.browseFilesystem(dir), client.scanImportFolder(path)]).then(([dirs, files]) => {
      if (!live) return
      setListing({
        dirs: dirs.status === 'fulfilled' ? dirs.value : null,
        files: files.status === 'fulfilled' ? files.value : [],
        error: files.status === 'rejected' ? (files.reason instanceof Error ? files.reason.message : 'Curator cannot read this folder.') : '',
        loading: false,
      })
    })
    return () => { live = false }
  }, [client, path])

  const chosen = Object.values(selected)
  const effectiveChoices = useMemo(() => {
    const out: Record<string, EpisodeChoice> = {}
    for (const f of chosen) out[f.path] = choices[f.path] ?? (item.kind === 'series' ? choiceOf(f.season, f.episodes) : '')
    return out
  }, [chosen, choices, item.kind])

  const planKey = JSON.stringify(planRequest(item, chosen, effectiveChoices, copyId, removeOriginal))
  useEffect(() => {
    if (step !== 'confirm' || chosen.length === 0) return
    let live = true
    setPlan((prev) => ({ ...prev, loading: true, error: '' }))
    client.planImport(JSON.parse(planKey))
      .then((lines) => { if (live) setPlan({ lines, error: '', loading: false }) })
      .catch((cause) => { if (live) setPlan({ lines: [], error: cause instanceof Error ? cause.message : 'Could not plan this import.', loading: false }) })
    return () => { live = false }
  }, [client, step, planKey])

  const go = (next: string) => { setPath(next.length > 1 ? next.replace(/\/+$/, '') : next); setDraft('') }
  const toggle = (f: ScannedFile) => setSelected((prev) => {
    const next = { ...prev }
    if (next[f.path]) delete next[f.path]
    else next[f.path] = f
    return next
  })
  const episodes = item.seasons.flatMap((s) => s.episodes.map((e) => ({ value: choiceOf(s.number, [e.episodeNumber]), label: episodeLabel(s.number, [e.episodeNumber]) })))
  const blocked = plan.loading || plan.lines.length === 0 || plan.lines.some((line) => line.error)
  const loose = chosen.filter((f) => !f.publication)

  const submit = async () => {
    setSubmitting(true)
    try {
      setStarted(await startImport(client, item, chosen, effectiveChoices, copyId, removeOriginal))
      setStep('started')
    } finally {
      setSubmitting(false)
    }
  }

  const close = step === 'confirm' ? () => setStep('choose') : onClose
  return (
    <Modal animationType="slide" presentationStyle="fullScreen" onRequestClose={close}>
      <SafeAreaProvider>
        <AppScreen forceTopInset>
          <Header title={`Import into ${item.title}`} subtitle={step === 'choose' ? '1 · Choose files' : step === 'confirm' ? '2 · Confirm' : 'Started'}
            left={<IconButton label={step === 'confirm' ? 'Back' : 'Close'} glyph={step === 'confirm' ? '‹' : '×'} onPress={close} />} />
          <ScrollView key={step} style={styles.flex} contentContainerStyle={styles.content} keyboardShouldPersistTaps="handled">
            {step === 'choose' ? <>
              <View style={styles.wrap}>
                {defaultPath ? <Chip label="Completed downloads" selected={path === defaultPath} onPress={() => go(defaultPath)} /> : null}
                {item.path ? <Chip label="This item's folder" selected={path === item.path} onPress={() => go(item.path)} /> : null}
                {roots.map((root) => <Chip key={root.id} label={root.path} selected={path === root.path} onPress={() => go(root.path)} />)}
              </View>
              <View style={styles.row}>
                <Field style={styles.flex} value={draft} onChangeText={setDraft} placeholder="Type a path…" autoCapitalize="none" autoCorrect={false} accessibilityLabel="Folder path" />
                <Button compact label="Open" disabled={!draft.trim()} onPress={() => go(draft.trim())} />
              </View>
              <Text style={[styles.mono, { color: theme.muted }]}>{path || '…'}</Text>
              <InlineError message={listing.error} />
              {listing.loading ? <LoadingState label="Reading the folder…" /> : null}
              <Panel style={styles.list}>
                {listing.dirs?.parent && listing.dirs.parent !== path ? <Row label="↰ .." onPress={() => go(listing.dirs!.parent)} /> : null}
                {(listing.dirs?.dirs ?? []).map((d) => <Row key={d.path} label={`▸ ${d.name}/`} onPress={() => go(d.path)} />)}
                {listing.files.map((f) => f.skipped ? (
                  <View key={f.path} style={[styles.file, { borderColor: theme.border }]}>
                    <View style={styles.flex}>
                      <Text style={{ color: theme.muted }}>{f.name}</Text>
                      <Text style={{ color: theme.muted }}>{f.skipped}</Text>
                    </View>
                  </View>
                ) : (
                  <View key={f.path} style={[styles.file, { borderColor: theme.border }]}>
                    <Switch accessibilityLabel={`Select ${f.name}`} value={!!selected[f.path]} onValueChange={() => toggle(f)} />
                    <View style={styles.flex}>
                      <Text style={{ color: theme.text }}>{f.name}</Text>
                      <View style={styles.wrap}>
                        <Badge label={formatBytes(f.size)} />
                        {item.kind === 'series' ? (f.episodes.length ? <Badge tone="ok" label={episodeLabel(f.season, f.episodes)} /> : <Badge tone="warning" label="no episode in name" />) : null}
                        {f.quality ? <Badge label={f.quality} /> : null}
                        {f.subtitles ? <Badge label={`${f.subtitles} sub${f.subtitles === 1 ? '' : 's'}`} /> : null}
                        {f.publication ? <Badge label="runner delivery" /> : null}
                      </View>
                    </View>
                  </View>
                ))}
                {!listing.loading && !listing.error && listing.files.length === 0 && (listing.dirs?.dirs.length ?? 0) === 0
                  ? <Text style={{ color: theme.muted }}>Nothing importable in this folder.</Text> : null}
              </Panel>
            </> : null}

            {step === 'confirm' ? <>
              {item.copies.length > 0 ? <>
                <SectionTitle>{item.kind === 'book' ? 'Edition' : 'Copy'}</SectionTitle>
                <View style={styles.wrap}>
                  <Chip label="Primary" selected={copyId === 0} onPress={() => setCopyId(0)} />
                  {item.copies.map((c) => <Chip key={c.id} label={c.name || c.bookType || `Copy ${c.id}`} selected={copyId === c.id} onPress={() => setCopyId(c.id)} />)}
                </View>
              </> : null}
              <InlineError message={plan.error} />
              {chosen.map((f) => {
                const line = plan.lines.find((l) => l.path === f.path)
                return (
                  <Panel key={f.path} style={styles.planLine}>
                    <Text style={{ color: theme.text }}>{f.name}{line?.subtitles ? ` + ${line.subtitles} subtitle${line.subtitles === 1 ? '' : 's'}` : ''}</Text>
                    {item.kind === 'series' ? (
                      <ScrollView horizontal showsHorizontalScrollIndicator={false} contentContainerStyle={styles.chips}>
                        {episodes.map((e) => <Chip key={e.value} label={e.label} selected={effectiveChoices[f.path] === e.value}
                          onPress={() => setChoices((prev) => ({ ...prev, [f.path]: e.value }))} />)}
                      </ScrollView>
                    ) : null}
                    {line?.error ? <InlineError message={line.error} /> : null}
                    {line?.destination && !line.error ? <>
                      <Text style={[styles.mono, { color: theme.text }]}>{line.destination}</Text>
                      <View style={styles.wrap}>
                        <Badge tone={line.action === 'replace' ? 'warning' : 'ok'} label={line.action === 'replace' ? `replaces ${line.existing}` : line.action === 'beside' ? `kept beside ${line.existing}` : 'new file'} />
                      </View>
                      {line.reason ? <Text style={{ color: theme.muted }}>{line.reason}</Text> : null}
                      {line.mode ? <Text style={{ color: theme.muted }}>{MODE_TEXT[line.mode]}</Text> : null}
                      {line.note ? <Text style={{ color: theme.warning }}>{line.note}</Text> : null}
                    </> : null}
                  </Panel>
                )
              })}
              {loose.length > 0 ? (
                <View style={styles.row}>
                  <Switch accessibilityLabel="Remove original" value={removeOriginal} onValueChange={setRemoveOriginal} />
                  <Text style={[styles.flex, { color: theme.text }]}>Remove original{loose.length < chosen.length ? ' (loose files only)' : ''} — renamed on the same volume; copied, verified and removed across volumes</Text>
                </View>
              ) : null}
            </> : null}

            {step === 'started' ? <StartedJobs client={client} started={started} /> : null}
          </ScrollView>
          <SafeAreaView edges={{ bottom: 'maximum' }} style={[styles.footer, { borderColor: theme.border }]}>
            {step === 'choose' ? <Button label={`Next · ${chosen.length} selected`} disabled={chosen.length === 0} onPress={() => setStep('confirm')} /> : null}
            {step === 'confirm' ? <Button label={submitting ? 'Starting…' : `Import ${chosen.length} file${chosen.length === 1 ? '' : 's'}`} disabled={blocked || submitting} onPress={() => void submit()} /> : null}
            {step === 'started' ? <Button label="Done" onPress={onClose} /> : null}
          </SafeAreaView>
        </AppScreen>
      </SafeAreaProvider>
    </Modal>
  )
}

function Row({ label, onPress }: { label: string; onPress: () => void }) {
  const theme = useTheme()
  return (
    <Pressable accessibilityRole="button" onPress={onPress} style={styles.dir}>
      <Text style={{ color: theme.accent }}>{label}</Text>
    </Pressable>
  )
}

/** The jobs an import started, live through completion: state, Stop, and what happened to the originals. */
function StartedJobs({ client, started }: { client: MonarrClient; started: StartedImport[] }) {
  const theme = useTheme()
  const [rows, setRows] = useState<Record<number, QueueItem>>({})
  const [message, setMessage] = useState('')
  useEffect(() => {
    let live = true
    const ids = started.flatMap((s) => (s.downloadId ? [s.downloadId] : []))
    const done = new Set<number>()
    const poll = async () => {
      for (const id of ids) {
        if (done.has(id)) continue
        try {
          const row = await client.getQueueItem(id)
          if (!live) return
          setRows((prev) => ({ ...prev, [id]: row }))
          if (isTerminal(row)) done.add(id)
        } catch {
          /* the next poll tries again */
        }
      }
    }
    void poll()
    const timer = setInterval(() => { if (done.size < ids.length) void poll() }, 2000)
    return () => { live = false; clearInterval(timer) }
  }, [client, started])
  const stop = async (row: QueueItem) => {
    if (!row.importControl) return
    try {
      await client.cancelQueueImport(row.id, row.importControl)
      setMessage('Stopped. The files stay where they were; retry from Activity.')
      setRows((prev) => ({ ...prev, [row.id]: { ...row, importState: undefined } }))
    } catch (cause) {
      setMessage(cause instanceof Error ? cause.message : 'Could not stop this import.')
    }
  }
  return (
    <>
      {message ? <Text style={{ color: theme.text }}>{message}</Text> : null}
      {started.map((s, i) => {
        const row = s.downloadId ? rows[s.downloadId] : undefined
        const originals = row ? originalsNote(row) : ''
        return (
          <Panel key={i} style={styles.planLine}>
            <Text style={{ color: theme.text }}>{s.label}</Text>
            {s.error ? <InlineError message={s.error} /> : (
              <Text style={{ color: theme.muted }}>
                Activity #{s.downloadId}: {row ? `${row.state}${row.state === 'failed' && row.error ? ` — ${row.error}` : ''}` : 'queued'}
              </Text>
            )}
            {originals ? <Text style={{ color: theme.muted }}>{originals}</Text> : null}
            {row && (row.state === 'importing' || row.importState) && row.importControl ? <Button secondary compact label="Stop" onPress={() => void stop(row)} /> : null}
          </Panel>
        )
      })}
    </>
  )
}

const styles = StyleSheet.create({
  flex: { flex: 1 },
  content: { padding: 16, gap: 12 },
  wrap: { flexDirection: 'row', flexWrap: 'wrap', gap: 6 },
  chips: { gap: 6 },
  row: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  mono: { fontFamily: 'Menlo', fontSize: 12 },
  list: { gap: 4 },
  dir: { paddingVertical: 10 },
  file: { flexDirection: 'row', alignItems: 'center', gap: 10, paddingVertical: 8, borderTopWidth: StyleSheet.hairlineWidth },
  planLine: { gap: 6 },
  footer: { padding: 16, gap: 8, borderTopWidth: StyleSheet.hairlineWidth },
})
