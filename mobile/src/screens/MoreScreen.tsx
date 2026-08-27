import { Alert, Linking, Pressable, ScrollView, StyleSheet, Text, View } from 'react-native'
import type { MonarrClient } from '../api'
import { APP_VERSION } from '../app-version'
import { formatUptime } from '../format'
import { AppScreen, Badge, Button, Chip, Header, InlineError, LoadingState, Panel, SectionTitle, Wordmark } from '../components/UI'
import { LAYOUT_OPTIONS, PALETTE_OPTIONS } from '../preferences'
import { usePreferences } from '../preferences-context'
import { useTheme } from '../theme'
import type { Connection } from '../types'
import { useResource } from '../useResource'

export function MoreScreen({
  client,
  connection,
  onCalendar,
  onDisconnect,
}: {
  client: MonarrClient
  connection: Connection
  onCalendar: () => void
  onDisconnect: () => void
}) {
  const theme = useTheme()
  const {
    theme: themePreference,
    itemSize,
    layout,
    palette,
    setTheme,
    setItemSize,
    setLayout,
    setPalette,
  } = usePreferences()
  const resource = useResource(async () => {
    const [status, health] = await Promise.all([client.getStatus(), client.getHealth()])
    return { status, health }
  }, [client])

  const confirmDisconnect = () => {
    Alert.alert('Disconnect this server?', 'The saved address and API key will be removed from this device.', [
      { text: 'Cancel', style: 'cancel' },
      { text: 'Disconnect', style: 'destructive', onPress: onDisconnect },
    ])
  }

  return (
    <AppScreen>
      <Header title="More" left={<Wordmark size={19} />} subtitle={connection.baseUrl.replace(/^https?:\/\//, '')} />
      <ScrollView contentContainerStyle={styles.content}>
        <Pressable accessibilityRole="button" onPress={onCalendar}>
          <Panel style={styles.linkPanel}>
            <View style={styles.flex}>
              <Text style={[styles.linkTitle, { color: theme.text }]}>Calendar</Text>
              <Text style={[styles.linkHint, { color: theme.muted }]}>Upcoming episodes, releases, and availability</Text>
            </View>
            <Text style={[styles.chevron, { color: theme.accent }]}>›</Text>
          </Panel>
        </Pressable>

        <SectionTitle>Display</SectionTitle>
        <Panel style={styles.appearancePanel}>
          <View style={styles.preferenceBlock}>
            <Text style={[styles.preferenceLabel, { color: theme.text }]}>Layout</Text>
            <Text style={[styles.linkHint, { color: theme.muted }]}>Classic is the original mobile interface. Plex emphasizes pinned navigation; Theater moves navigation into a top deck.</Text>
            <View style={styles.choiceRow}>
              {LAYOUT_OPTIONS.map((option) => (
                <Chip key={option.id} label={option.name} selected={layout === option.id} onPress={() => setLayout(option.id)} />
              ))}
            </View>
          </View>
          <View style={[styles.preferenceBlock, styles.preferenceDivider, { borderTopColor: theme.border }]}>
            <Text style={[styles.preferenceLabel, { color: theme.text }]}>Color scheme</Text>
            <Text style={[styles.linkHint, { color: theme.muted }]}>Void, VHS, Panoptic, Redline, and Panovic are midnight-only; the other schemes follow the selected appearance.</Text>
            <View style={styles.choiceRow}>
              {PALETTE_OPTIONS.map((option) => (
                <Chip key={option.id} label={option.name} selected={palette === option.id} onPress={() => setPalette(option.id)} />
              ))}
            </View>
          </View>
          <View style={[styles.preferenceBlock, styles.preferenceDivider, { borderTopColor: theme.border }]}>
            <Text style={[styles.preferenceLabel, { color: theme.text }]}>Appearance</Text>
            <Text style={[styles.linkHint, { color: theme.muted }]}>Auto follows this device. If it reports no preference, Curator uses dark.</Text>
            <View style={styles.choiceRow}>
              {(['auto', 'light', 'dark'] as const).map((value) => (
                <Chip key={value} label={`${value[0]?.toUpperCase()}${value.slice(1)}`} selected={themePreference === value} onPress={() => setTheme(value)} />
              ))}
            </View>
          </View>
          <View style={[styles.preferenceBlock, styles.preferenceDivider, { borderTopColor: theme.border }]}>
            <Text style={[styles.preferenceLabel, { color: theme.text }]}>Item size</Text>
            <Text style={[styles.linkHint, { color: theme.muted }]}>Medium matches the previous layout. Small fits more titles on screen.</Text>
            <View style={styles.choiceRow}>
              {(['small', 'medium', 'large'] as const).map((value) => (
                <Chip key={value} label={`${value[0]?.toUpperCase()}${value.slice(1)}`} selected={itemSize === value} onPress={() => setItemSize(value)} />
              ))}
            </View>
          </View>
        </Panel>

        <SectionTitle>App</SectionTitle>
        <Panel>
          <InfoRow label="Version" value={APP_VERSION} />
        </Panel>

        <SectionTitle>Server</SectionTitle>
        {resource.loading && !resource.data ? <LoadingState /> : null}
        <InlineError message={resource.error} />
        {resource.data ? (
          <Panel style={styles.serverPanel}>
            <View style={styles.serverHead}>
              <View>
                <Text style={[styles.serverName, { color: theme.text }]}>{resource.data.status.appName}</Text>
                <Text style={[styles.linkHint, { color: theme.muted }]}>v{resource.data.status.version} · {resource.data.status.commit}</Text>
              </View>
              <Badge label={resource.data.health.overall} tone={resource.data.health.overall} />
            </View>
            <InfoRow label="Uptime" value={formatUptime(resource.data.status.uptimeSeconds)} />
            <InfoRow label="Runtime" value={`${resource.data.status.os}/${resource.data.status.arch}`} />
            <InfoRow label="Database" value={`schema ${resource.data.status.dbSchemaVersion}`} />
          </Panel>
        ) : null}

        {(resource.data?.health.checks.length ?? 0) > 0 ? (
          <>
            <SectionTitle>Health</SectionTitle>
            <Panel style={styles.healthPanel}>
              {resource.data?.health.checks.map((check, index) => (
                <View key={check.name} style={[styles.healthRow, index > 0 && { borderTopColor: theme.border, borderTopWidth: StyleSheet.hairlineWidth }]}>
                  <View style={styles.flex}>
                    <Text style={[styles.healthName, { color: theme.text }]}>{check.name}</Text>
                    {check.message ? <Text style={[styles.linkHint, { color: theme.muted }]}>{check.message}</Text> : null}
                  </View>
                  <Badge label={check.status} tone={check.status} />
                </View>
              ))}
            </Panel>
          </>
        ) : null}

        <SectionTitle>Administration</SectionTitle>
        <Text style={[styles.adminCopy, { color: theme.muted }]}>Indexer, download client, root folder, and security configuration stay in the full web interface.</Text>
        <View style={styles.actions}>
          <Button label="Open web interface" onPress={() => void Linking.openURL(connection.baseUrl)} />
          <Button secondary label="Change server" onPress={confirmDisconnect} />
        </View>
      </ScrollView>
    </AppScreen>
  )
}

function InfoRow({ label, value }: { label: string; value: string }) {
  const theme = useTheme()
  return (
    <View style={styles.infoRow}>
      <Text style={[styles.infoLabel, { color: theme.muted }]}>{label}</Text>
      <Text style={[styles.infoValue, { color: theme.text }]}>{value}</Text>
    </View>
  )
}

const styles = StyleSheet.create({
  content: { padding: 16, paddingBottom: 42 },
  linkPanel: { flexDirection: 'row', alignItems: 'center' },
  flex: { flex: 1 },
  linkTitle: { fontSize: 16, fontWeight: '700' },
  linkHint: { fontSize: 12, lineHeight: 17, marginTop: 2 },
  appearancePanel: { gap: 16 },
  preferenceBlock: { gap: 8 },
  preferenceDivider: { borderTopWidth: StyleSheet.hairlineWidth, paddingTop: 16 },
  preferenceLabel: { fontSize: 14, fontWeight: '700' },
  choiceRow: { flexDirection: 'row', flexWrap: 'wrap', gap: 7 },
  chevron: { fontSize: 32, lineHeight: 34 },
  serverPanel: { gap: 10 },
  serverHead: { flexDirection: 'row', alignItems: 'flex-start', justifyContent: 'space-between', gap: 12, marginBottom: 4 },
  serverName: { fontSize: 17, fontWeight: '800' },
  infoRow: { flexDirection: 'row', justifyContent: 'space-between', gap: 16 },
  infoLabel: { fontSize: 13 },
  infoValue: { fontSize: 13, fontWeight: '600' },
  healthPanel: { paddingVertical: 2 },
  healthRow: { flexDirection: 'row', alignItems: 'center', gap: 12, paddingVertical: 11 },
  healthName: { fontSize: 13, fontWeight: '700' },
  adminCopy: { fontSize: 13, lineHeight: 19, marginBottom: 13 },
  actions: { gap: 9 },
})
