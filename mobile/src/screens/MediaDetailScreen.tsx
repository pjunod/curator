import { useEffect, useState } from 'react'
import { Alert, ScrollView, StyleSheet, Switch, Text, View } from 'react-native'
import type { MonarrClient } from '../api'
import { formatBytes } from '../format'
import { Poster } from '../components/Media'
import {
  AppScreen,
  Badge,
  Button,
  Chip,
  Field,
  Header,
  IconButton,
  InlineError,
  LoadingState,
  MessageState,
  Panel,
  SectionTitle,
} from '../components/UI'
import { useTheme } from '../theme'
import type { MediaCopy, QualityProfile, UpdateMediaItemRequest } from '../types'
import { useResource } from '../useResource'

export function MediaDetailScreen({ client, id, onBack }: { client: MonarrClient; id: number; onBack: () => void }) {
  const theme = useTheme()
  const resource = useResource(() => client.getLibraryItem(id), [client, id])
  const profilesResource = useResource(() => client.getProfiles(), [client])
  const rootsResource = useResource(() => client.getRootFolders(), [client])
  const [actionError, setActionError] = useState('')
  const [actionMessage, setActionMessage] = useState('')
  const [saving, setSaving] = useState(false)
  const [searching, setSearching] = useState(false)
  const [seasonSaving, setSeasonSaving] = useState<number | null>(null)
  const [draftMonitored, setDraftMonitored] = useState(false)
  const [draftProfileId, setDraftProfileId] = useState(0)
  const [draftRootId, setDraftRootId] = useState(0)
  const [draftPath, setDraftPath] = useState('')
  const [pathEdited, setPathEdited] = useState(false)
  const [addingCopy, setAddingCopy] = useState(false)
  const [copyName, setCopyName] = useState('')
  const [copyProfileId, setCopyProfileId] = useState(0)
  const [copyRootId, setCopyRootId] = useState(0)
  const [copyMonitored, setCopyMonitored] = useState(true)
  const item = resource.data
  const profiles = profilesResource.data ?? []
  const roots = rootsResource.data ?? []

  useEffect(() => {
    if (!item) return
    setDraftMonitored(item.monitored)
    setDraftProfileId(item.qualityProfileId)
    setDraftRootId(item.rootFolderId)
    setDraftPath(item.path)
    setPathEdited(false)
  }, [item])

  useEffect(() => {
    if (copyProfileId !== 0 || profiles.length === 0) return
    const alternate = profiles.find((profile) => profile.id !== item?.qualityProfileId)
    setCopyProfileId(alternate?.id ?? item?.qualityProfileId ?? profiles[0]?.id ?? 0)
  }, [copyProfileId, item?.qualityProfileId, profiles])

  const saveItem = async () => {
    if (!item) return
    const patch: UpdateMediaItemRequest = {}
    if (draftMonitored !== item.monitored) patch.monitored = draftMonitored
    if (draftProfileId !== item.qualityProfileId) patch.qualityProfileId = draftProfileId
    if (draftRootId !== item.rootFolderId) patch.rootFolderId = draftRootId
    if (pathEdited && draftPath !== item.path) patch.path = draftPath
    if (Object.keys(patch).length === 0) {
      setActionError('')
      setActionMessage('No item changes to save.')
      return
    }

    setSaving(true)
    setActionError('')
    setActionMessage('')
    try {
      const profileChanged = patch.qualityProfileId !== undefined
      await client.updateLibraryItem(id, patch)
      await resource.refresh()
      setActionMessage(
        profileChanged && draftMonitored
          ? 'Profile changed — Monarr is searching for the new target in the background.'
          : 'Item changes saved.',
      )
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : 'Could not update this item.')
    } finally {
      setSaving(false)
    }
  }

  const changeRoot = (rootId: number) => {
    if (!item) return
    setDraftRootId(rootId)
    if (rootId === item.rootFolderId) {
      setDraftPath(item.path)
    } else {
      setDraftPath('')
    }
    setPathEdited(false)
  }

  const searchNow = async () => {
    setSearching(true)
    setActionError('')
    setActionMessage('')
    try {
      await client.autoSearchItem(id)
      setActionMessage('Search started.')
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : 'Could not start a search.')
    } finally {
      setSearching(false)
    }
  }

  const setSeasonMonitored = async (season: number, monitored: boolean) => {
    setSeasonSaving(season)
    setActionError('')
    setActionMessage('')
    try {
      await client.setSeasonMonitored(id, season, monitored)
      await resource.refresh()
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : 'Could not update this season.')
    } finally {
      setSeasonSaving(null)
    }
  }

  const addCopy = async () => {
    if (copyProfileId === 0) {
      setActionError('Choose a quality profile for the copy.')
      return
    }
    setAddingCopy(true)
    setActionError('')
    setActionMessage('')
    try {
      await client.addMediaCopy(id, {
        qualityProfileId: copyProfileId,
        rootFolderId: copyRootId || undefined,
        name: copyName.trim() || undefined,
        monitored: copyMonitored,
      })
      await resource.refresh()
      setCopyName('')
      setCopyRootId(0)
      setCopyMonitored(true)
      setActionMessage('Additional copy added — Monarr will hunt it like any other wanted item.')
    } catch (cause) {
      setActionError(cause instanceof Error ? cause.message : 'Could not add this copy.')
    } finally {
      setAddingCopy(false)
    }
  }

  if (resource.loading && !item) {
    return <AppScreen><Header title="Details" left={<IconButton label="Back" glyph="‹" onPress={onBack} />} /><LoadingState /></AppScreen>
  }
  if (!item) {
    return <AppScreen><Header title="Details" left={<IconButton label="Back" glyph="‹" onPress={onBack} />} /><MessageState title="Title unavailable" message={resource.error} retry={() => void resource.refresh()} /></AppScreen>
  }

  const episodes = item.seasons.flatMap((season) => season.episodes)
  const aired = episodes.filter((episode) => !episode.airDate || new Date(episode.airDate) <= new Date())
  const onDisk = aired.filter((episode) => episode.hasFile).length
  const copies = item.copies ?? []
  const selectedProfileName = profileName(profiles, item.qualityProfileId)

  return (
    <AppScreen>
      <Header title={item.title} subtitle={item.kind === 'series' ? 'TV series' : item.kind} left={<IconButton label="Back" glyph="‹" onPress={onBack} />} />
      <ScrollView contentContainerStyle={styles.content} keyboardShouldPersistTaps="handled">
        <View style={styles.hero}>
          <Poster path={item.posterPath} title={item.title} width={126} />
          <View style={styles.heroBody}>
            <Text style={[styles.title, { color: theme.text }]}>{item.title}</Text>
            <Text style={[styles.meta, { color: theme.muted }]}>{[item.author, item.year, item.status, item.runtime ? `${item.runtime} min` : ''].filter(Boolean).join(' · ')}</Text>
            <View style={styles.badges}>
              <Badge label={item.monitored ? 'Monitored' : 'Unmonitored'} tone={item.monitored ? 'ok' : 'neutral'} />
              <Badge label={selectedProfileName} />
              {item.quality ? <Badge label={item.quality} tone={item.qualityVerified ? 'ok' : 'neutral'} /> : null}
              {item.upgrade ? <Badge label={item.upgrade} tone={item.upgrade === 'met' ? 'ok' : 'warning'} /> : null}
            </View>
          </View>
        </View>

        {item.overview ? <Text style={[styles.overview, { color: theme.text }]}>{item.overview}</Text> : null}
        <InlineError message={actionError || resource.error} />
        {actionMessage ? <Text style={[styles.success, { color: theme.ok }]}>{actionMessage}</Text> : null}

        <Panel style={styles.actions}>
          <Button label={searching ? 'Starting search…' : 'Search now'} disabled={searching} onPress={() => void searchNow()} />
        </Panel>

        <SectionTitle>Edit item</SectionTitle>
        <Panel style={styles.formPanel}>
          <View style={styles.monitorRow}>
            <View style={styles.flex}>
              <Text style={[styles.actionTitle, { color: theme.text }]}>Monitored</Text>
              <Text style={[styles.actionHint, { color: theme.muted }]}>Let automation keep this title complete.</Text>
            </View>
            <Switch disabled={saving} value={draftMonitored} onValueChange={setDraftMonitored} trackColor={{ true: theme.accent }} />
          </View>

          <ChoiceGroup
            label="Quality profile"
            options={profiles.map((profile) => ({ id: profile.id, label: profile.name }))}
            selected={draftProfileId}
            onChange={setDraftProfileId}
            disabled={saving}
            empty={profilesResource.loading ? 'Loading profiles…' : profilesResource.error || 'No quality profiles are available.'}
          />

          <ChoiceGroup
            label="Root folder"
            options={[{ id: 0, label: 'None' }, ...roots.map((root) => ({ id: root.id, label: root.path }))]}
            selected={draftRootId}
            onChange={changeRoot}
            disabled={saving}
            empty={rootsResource.loading ? 'Loading root folders…' : rootsResource.error || 'No root folders are available.'}
          />

          <View style={styles.fieldGroup}>
            <Text style={[styles.fieldLabel, { color: theme.text }]}>Folder path</Text>
            <Field
              autoCapitalize="none"
              autoCorrect={false}
              editable={!saving}
              placeholder={draftRootId !== item.rootFolderId && !pathEdited ? 'Recomputed from the selected root folder' : 'Absolute folder path'}
              value={draftPath}
              onChangeText={(value) => {
                setDraftPath(value)
                setPathEdited(true)
              }}
            />
            <Text style={[styles.actionHint, { color: theme.muted }]}>Changing this record never moves files already on disk.</Text>
          </View>

          <Button label={saving ? 'Saving…' : 'Save item'} disabled={saving || draftProfileId === 0} onPress={() => void saveItem()} />
        </Panel>

        {item.kind === 'series' ? (
          <>
            <SectionTitle>Episodes</SectionTitle>
            <Panel>
              <Text style={[styles.stat, { color: theme.text }]}>{onDisk} of {aired.length} aired episodes on disk</Text>
              {item.seasons.map((season) => {
                const present = season.episodes.filter((episode) => episode.hasFile).length
                return (
                  <View key={season.number} style={[styles.row, { borderTopColor: theme.border }]}>
                    <View style={styles.flex}>
                      <Text style={[styles.rowTitle, { color: theme.text }]}>Season {season.number}</Text>
                      <Text style={[styles.meta, { color: theme.muted }]}>{present}/{season.episodes.length} episodes on disk</Text>
                    </View>
                    <Switch
                      accessibilityLabel={`Monitor season ${season.number}`}
                      disabled={seasonSaving !== null}
                      value={season.monitored}
                      onValueChange={(value) => void setSeasonMonitored(season.number, value)}
                      trackColor={{ true: theme.accent }}
                    />
                  </View>
                )
              })}
            </Panel>
          </>
        ) : null}

        {item.kind !== 'book' ? (
          <>
            <SectionTitle>Quality copies</SectionTitle>
            <Text style={[styles.sectionHint, { color: theme.muted }]}>Keep another quality target for this title. Each copy is monitored and upgraded independently.</Text>
            {copies.map((copy) => (
              <CopyEditor
                key={copy.id}
                client={client}
                itemId={id}
                copy={copy}
                profiles={profiles}
                fileCount={item.files.filter((file) => file.copyId === copy.id).length}
                onRefresh={resource.refresh}
                onMessage={(message) => {
                  setActionError('')
                  setActionMessage(message)
                }}
              />
            ))}

            <Panel style={styles.formPanel}>
              <Text style={[styles.actionTitle, { color: theme.text }]}>Add another copy</Text>
              <View style={styles.fieldGroup}>
                <Text style={[styles.fieldLabel, { color: theme.text }]}>Name (optional)</Text>
                <Field editable={!addingCopy} placeholder="e.g. 720p for dad" value={copyName} onChangeText={setCopyName} />
              </View>
              <ChoiceGroup
                label="Quality profile"
                options={profiles.map((profile) => ({ id: profile.id, label: profile.name }))}
                selected={copyProfileId}
                onChange={setCopyProfileId}
                disabled={addingCopy}
                empty={profilesResource.loading ? 'Loading profiles…' : profilesResource.error || 'No quality profiles are available.'}
              />
              <ChoiceGroup
                label="Location"
                options={[{ id: 0, label: 'Same folder as main copy' }, ...roots.map((root) => ({ id: root.id, label: root.path }))]}
                selected={copyRootId}
                onChange={setCopyRootId}
                disabled={addingCopy}
                empty={rootsResource.loading ? 'Loading root folders…' : rootsResource.error || 'No separate root folders are available.'}
              />
              <View style={styles.monitorRow}>
                <Text style={[styles.fieldLabel, styles.flex, { color: theme.text }]}>Monitor this copy</Text>
                <Switch disabled={addingCopy} value={copyMonitored} onValueChange={setCopyMonitored} trackColor={{ true: theme.accent }} />
              </View>
              <Button label={addingCopy ? 'Adding copy…' : 'Add copy'} disabled={addingCopy || copyProfileId === 0} onPress={() => void addCopy()} />
            </Panel>
          </>
        ) : null}

        <SectionTitle>Files</SectionTitle>
        {item.files.length === 0 ? (
          <Panel><Text style={[styles.meta, { color: theme.muted }]}>Nothing on disk yet.</Text></Panel>
        ) : item.files.map((file) => {
          const copy = file.copyId ? copies.find((candidate) => candidate.id === file.copyId) : undefined
          return (
            <Panel key={file.id} style={styles.file}>
              <View style={styles.rowBetween}>
                <Text numberOfLines={1} style={[styles.fileName, { color: theme.text }]}>{file.path.split('/').pop()}</Text>
                <Text style={[styles.meta, { color: theme.muted }]}>{formatBytes(file.size)}</Text>
              </View>
              <Text style={[styles.meta, { color: theme.muted }]}>{file.facts || file.quality || 'Quality unknown'}</Text>
              {copy ? <Badge label={copy.name || profileName(profiles, copy.qualityProfileId)} /> : null}
              {file.implausible ? <Badge label="Implausible metadata" tone="error" /> : file.provenanceLabel ? <Badge label={file.provenanceLabel} tone={file.verified ? 'ok' : 'neutral'} /> : null}
            </Panel>
          )
        })}

        {item.path ? (
          <>
            <SectionTitle>Location</SectionTitle>
            <Text selectable style={[styles.path, { color: theme.muted, backgroundColor: theme.raised, borderColor: theme.border }]}>{item.path}</Text>
          </>
        ) : null}
      </ScrollView>
    </AppScreen>
  )
}

function CopyEditor({
  client,
  itemId,
  copy,
  profiles,
  fileCount,
  onRefresh,
  onMessage,
}: {
  client: MonarrClient
  itemId: number
  copy: MediaCopy
  profiles: QualityProfile[]
  fileCount: number
  onRefresh: () => Promise<void>
  onMessage: (message: string) => void
}) {
  const theme = useTheme()
  const [name, setName] = useState(copy.name)
  const [profileId, setProfileId] = useState(copy.qualityProfileId)
  const [monitored, setMonitored] = useState(copy.monitored)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    setName(copy.name)
    setProfileId(copy.qualityProfileId)
    setMonitored(copy.monitored)
  }, [copy])

  const save = async () => {
    setBusy(true)
    setError('')
    try {
      await client.updateMediaCopy(itemId, copy.id, { name: name.trim(), qualityProfileId: profileId, monitored })
      await onRefresh()
      onMessage(`${name.trim() || profileName(profiles, profileId)} copy updated.`)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not update this copy.')
    } finally {
      setBusy(false)
    }
  }

  const remove = async () => {
    setBusy(true)
    setError('')
    try {
      await client.deleteMediaCopy(itemId, copy.id)
      await onRefresh()
      onMessage('Copy removed. Its files on disk were kept.')
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not remove this copy.')
    } finally {
      setBusy(false)
    }
  }

  const confirmRemove = () => {
    Alert.alert(
      'Remove this copy?',
      'Monarr will remove the copy record and stop managing it. Files already on disk will be kept.',
      [
        { text: 'Cancel', style: 'cancel' },
        { text: 'Remove copy', style: 'destructive', onPress: () => void remove() },
      ],
    )
  }

  return (
    <Panel style={styles.copyCard}>
      <View style={styles.copyHeader}>
        <View style={styles.flex}>
          <Text style={[styles.actionTitle, { color: theme.text }]}>{copy.name || profileName(profiles, copy.qualityProfileId)}</Text>
          <Text style={[styles.meta, { color: theme.muted }]}>{fileCount} {fileCount === 1 ? 'file' : 'files'} · {copy.path || 'same folder as main copy'}</Text>
        </View>
        <Badge label={copy.monitored ? 'Monitored' : 'Off'} tone={copy.monitored ? 'ok' : 'neutral'} />
      </View>
      <InlineError message={error} />
      <View style={styles.fieldGroup}>
        <Text style={[styles.fieldLabel, { color: theme.text }]}>Name</Text>
        <Field editable={!busy} placeholder="Optional label" value={name} onChangeText={setName} />
      </View>
      <ChoiceGroup
        label="Quality profile"
        options={profiles.map((profile) => ({ id: profile.id, label: profile.name }))}
        selected={profileId}
        onChange={setProfileId}
        disabled={busy}
        empty="No quality profiles are available."
      />
      <View style={styles.monitorRow}>
        <Text style={[styles.fieldLabel, styles.flex, { color: theme.text }]}>Monitored</Text>
        <Switch disabled={busy} value={monitored} onValueChange={setMonitored} trackColor={{ true: theme.accent }} />
      </View>
      <View style={styles.buttonRow}>
        <Button label={busy ? 'Saving…' : 'Save copy'} compact disabled={busy || profileId === 0} onPress={() => void save()} />
        <Button label="Remove" compact secondary disabled={busy} onPress={confirmRemove} />
      </View>
    </Panel>
  )
}

function ChoiceGroup({
  label,
  options,
  selected,
  onChange,
  disabled,
  empty,
}: {
  label: string
  options: { id: number; label: string }[]
  selected: number
  onChange: (id: number) => void
  disabled: boolean
  empty: string
}) {
  const theme = useTheme()
  return (
    <View style={styles.fieldGroup}>
      <Text style={[styles.fieldLabel, { color: theme.text }]}>{label}</Text>
      {options.length > 0 ? (
        <View style={styles.choices}>
          {options.map((option) => (
            <Chip key={option.id} label={option.label} selected={selected === option.id} onPress={disabled ? undefined : () => onChange(option.id)} />
          ))}
        </View>
      ) : (
        <Text style={[styles.actionHint, { color: theme.muted }]}>{empty}</Text>
      )}
    </View>
  )
}

function profileName(profiles: QualityProfile[], id: number): string {
  return profiles.find((profile) => profile.id === id)?.name ?? `Profile #${id}`
}

const styles = StyleSheet.create({
  content: { padding: 16, paddingBottom: 42 },
  hero: { flexDirection: 'row', gap: 16 },
  heroBody: { flex: 1, paddingTop: 4, gap: 7 },
  title: { fontSize: 24, lineHeight: 29, fontWeight: '800' },
  meta: { fontSize: 12, lineHeight: 18 },
  badges: { flexDirection: 'row', flexWrap: 'wrap', gap: 5 },
  overview: { marginTop: 18, fontSize: 14, lineHeight: 22 },
  success: { marginTop: 10, fontSize: 13, lineHeight: 19 },
  actions: { gap: 14, marginTop: 18 },
  formPanel: { gap: 16 },
  monitorRow: { flexDirection: 'row', alignItems: 'center', gap: 12 },
  flex: { flex: 1 },
  actionTitle: { fontSize: 15, fontWeight: '700' },
  actionHint: { fontSize: 12, lineHeight: 17, marginTop: 2 },
  fieldGroup: { gap: 7 },
  fieldLabel: { fontSize: 13, fontWeight: '700' },
  choices: { flexDirection: 'row', flexWrap: 'wrap', gap: 7 },
  sectionHint: { fontSize: 13, lineHeight: 19, marginTop: -4, marginBottom: 10 },
  stat: { fontSize: 14, fontWeight: '700', marginBottom: 5 },
  row: { borderTopWidth: StyleSheet.hairlineWidth, paddingTop: 10, marginTop: 10, flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', gap: 12 },
  rowTitle: { fontSize: 13, fontWeight: '600' },
  copyCard: { gap: 14, marginBottom: 10 },
  copyHeader: { flexDirection: 'row', alignItems: 'flex-start', gap: 12 },
  buttonRow: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
  file: { gap: 7, marginBottom: 9 },
  rowBetween: { flexDirection: 'row', justifyContent: 'space-between', gap: 10 },
  fileName: { flex: 1, fontSize: 13, fontWeight: '700' },
  path: { borderWidth: 1, borderRadius: 12, padding: 12, fontSize: 12, lineHeight: 18 },
})
