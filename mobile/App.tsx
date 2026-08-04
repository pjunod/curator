import { useEffect, useMemo, useState } from 'react'
import { ActivityIndicator, BackHandler, Pressable, StyleSheet, Text, useWindowDimensions, View } from 'react-native'
import { StatusBar } from 'expo-status-bar'
import { SafeAreaProvider, SafeAreaView, initialWindowMetrics } from 'react-native-safe-area-context'
import { MonarrClient } from './src/api'
import { PreferencesProvider } from './src/preferences-context'
import { usePreferences } from './src/preferences-context'
import { TopNavigationProvider } from './src/navigation-context'
import { clearConnection, loadConnection, saveConnection } from './src/storage'
import { useTheme } from './src/theme'
import type { Connection, SystemStatus } from './src/types'
import { ConnectScreen } from './src/screens/ConnectScreen'
import { LibraryScreen } from './src/screens/LibraryScreen'
import { DiscoverScreen } from './src/screens/DiscoverScreen'
import { WantedScreen } from './src/screens/WantedScreen'
import { ActivityScreen } from './src/screens/ActivityScreen'
import { MoreScreen } from './src/screens/MoreScreen'
import { MediaDetailScreen } from './src/screens/MediaDetailScreen'
import { CalendarScreen } from './src/screens/CalendarScreen'

type Tab = 'library' | 'discover' | 'wanted' | 'activity' | 'more'
type Overlay = { name: 'detail'; id: number } | { name: 'calendar' }

export default function App() {
  return (
    <PreferencesProvider>
      <SafeAreaProvider initialMetrics={initialWindowMetrics}>
        <MonarrApp />
      </SafeAreaProvider>
    </PreferencesProvider>
  )
}

function MonarrApp() {
  const theme = useTheme()
  const { layout } = usePreferences()
  const { width } = useWindowDimensions()
  const [booting, setBooting] = useState(true)
  const [connection, setConnection] = useState<Connection | null>(null)
  const [tab, setTab] = useState<Tab>('library')
  const [overlay, setOverlay] = useState<Overlay | null>(null)
  const [libraryVersion, setLibraryVersion] = useState(0)
  const client = useMemo(() => (connection ? new MonarrClient(connection) : null), [connection])

  useEffect(() => {
    void loadConnection().then((saved) => {
      setConnection(saved)
      setBooting(false)
    })
  }, [])

  useEffect(() => {
    if (!overlay) return
    const subscription = BackHandler.addEventListener('hardwareBackPress', () => {
      setOverlay(null)
      return true
    })
    return () => subscription.remove()
  }, [overlay])

  const connected = async (next: Connection, _status: SystemStatus) => {
    const saved = await saveConnection(next)
    setConnection(saved)
    setTab('library')
  }

  const disconnect = async () => {
    await clearConnection()
    setOverlay(null)
    setConnection(null)
  }

  const openMedia = (id: number) => setOverlay({ name: 'detail', id })

  if (booting) {
    return (
      <View style={[styles.boot, { backgroundColor: theme.bg }]}>
        <StatusBar style={theme.dark ? 'light' : 'dark'} />
        <ActivityIndicator size="large" color={theme.accent} />
      </View>
    )
  }
  if (!connection || !client) {
    return (
      <>
        <StatusBar style={theme.dark ? 'light' : 'dark'} />
        <ConnectScreen initial={connection} onConnected={connected} />
      </>
    )
  }

  let content
  if (overlay?.name === 'detail') {
    content = <MediaDetailScreen client={client} id={overlay.id} onBack={() => setOverlay(null)} />
  } else if (overlay?.name === 'calendar') {
    content = <CalendarScreen client={client} onBack={() => setOverlay(null)} onOpen={openMedia} />
  } else if (tab === 'library') {
    content = <LibraryScreen client={client} refreshKey={libraryVersion} onOpen={openMedia} />
  } else if (tab === 'discover') {
    content = <DiscoverScreen client={client} onAdded={(id) => { setLibraryVersion((value) => value + 1); openMedia(id) }} />
  } else if (tab === 'wanted') {
    content = <WantedScreen client={client} onOpen={openMedia} />
  } else if (tab === 'activity') {
    content = <ActivityScreen client={client} onOpen={openMedia} />
  } else {
    content = <MoreScreen client={client} connection={connection} onCalendar={() => setOverlay({ name: 'calendar' })} onDisconnect={() => void disconnect()} />
  }

  const screen = (
    <TopNavigationProvider active={!overlay && layout === 'theater'}>
      <View style={styles.content}>{content}</View>
    </TopNavigationProvider>
  )
  const showPlexRail = !overlay && layout === 'plex' && width >= 720

  return (
    <View style={[styles.app, { backgroundColor: theme.bg }]}>
      <StatusBar style={theme.dark ? 'light' : 'dark'} />
      {showPlexRail ? (
        <View style={styles.plexShell}>
          <NavigationBar variant="rail" selected={tab} onSelect={setTab} />
          {screen}
        </View>
      ) : (
        <>
          {!overlay && layout === 'theater' ? <NavigationBar variant="theater" selected={tab} onSelect={setTab} /> : null}
          {screen}
          {!overlay && layout !== 'theater' ? (
            <NavigationBar variant={layout === 'plex' ? 'plex' : 'classic'} selected={tab} onSelect={setTab} />
          ) : null}
        </>
      )}
    </View>
  )
}

type NavigationVariant = 'classic' | 'plex' | 'rail' | 'theater'

function NavigationBar({ variant, selected, onSelect }: { variant: NavigationVariant; selected: Tab; onSelect: (tab: Tab) => void }) {
  const theme = useTheme()
  const tabs: { id: Tab; glyph: string; label: string }[] = [
    { id: 'library', glyph: '▦', label: 'Library' },
    { id: 'discover', glyph: '✦', label: 'Discover' },
    { id: 'wanted', glyph: '⌕', label: 'Wanted' },
    { id: 'activity', glyph: '↓', label: 'Activity' },
    { id: 'more', glyph: '•••', label: 'More' },
  ]
  const buttons = tabs.map((item) => {
    const active = item.id === selected
    const rail = variant === 'rail'
    const theater = variant === 'theater'
    const plex = variant === 'plex'
    return (
      <Pressable
        key={item.id}
        accessibilityRole="tab"
        accessibilityState={{ selected: active }}
        onPress={() => onSelect(item.id)}
        style={[
          rail ? styles.railTab : theater ? styles.theaterTab : styles.tab,
          plex && styles.plexTab,
          active && (rail ? styles.railTabActive : theater ? styles.theaterTabActive : plex ? styles.plexTabActive : undefined),
          active && (rail || theater || plex) ? { backgroundColor: theme.accentSoft } : undefined,
        ]}
      >
        {!theater ? <Text style={[rail ? styles.railGlyph : styles.tabGlyph, { color: active ? theme.accent : theme.muted }]}>{item.glyph}</Text> : null}
        <Text style={[rail ? styles.railLabel : theater ? styles.theaterLabel : styles.tabLabel, { color: active ? theme.accent : theme.muted }]}>{item.label}</Text>
      </Pressable>
    )
  })

  if (variant === 'rail') {
    return (
      <SafeAreaView edges={['top', 'bottom', 'left']} style={[styles.railSafe, { backgroundColor: theme.raised, borderRightColor: theme.border }]}>
        <View style={styles.railTabs}>{buttons}</View>
      </SafeAreaView>
    )
  }
  if (variant === 'theater') {
    return (
      <SafeAreaView edges={['top', 'left', 'right']} style={[styles.theaterSafe, { backgroundColor: theme.raised, borderBottomColor: theme.border }]}>
        <View accessibilityRole="tablist" style={styles.theaterTabs}>{buttons}</View>
      </SafeAreaView>
    )
  }
  return (
    <SafeAreaView edges={['bottom', 'left', 'right']} style={[styles.tabSafe, { backgroundColor: theme.raised, borderTopColor: theme.border }]}>
      <View accessibilityRole="tablist" style={styles.tabs}>{buttons}</View>
    </SafeAreaView>
  )
}

const styles = StyleSheet.create({
  app: { flex: 1 },
  boot: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  content: { flex: 1 },
  plexShell: { flex: 1, flexDirection: 'row' },
  tabSafe: { borderTopWidth: StyleSheet.hairlineWidth },
  tabs: { height: 58, flexDirection: 'row' },
  tab: { flex: 1, alignItems: 'center', justifyContent: 'center', gap: 1 },
  plexTab: { marginHorizontal: 3, marginVertical: 5, borderRadius: 13 },
  plexTabActive: { marginVertical: 4 },
  tabGlyph: { height: 25, fontSize: 20, lineHeight: 24, fontWeight: '600' },
  tabLabel: { fontSize: 10, fontWeight: '700' },
  railSafe: { width: 92, borderRightWidth: StyleSheet.hairlineWidth },
  railTabs: { flex: 1, paddingHorizontal: 8, paddingVertical: 12, gap: 5 },
  railTab: { minHeight: 62, borderRadius: 14, alignItems: 'center', justifyContent: 'center', gap: 3 },
  railTabActive: { minHeight: 64 },
  railGlyph: { fontSize: 21, lineHeight: 24, fontWeight: '700' },
  railLabel: { fontSize: 10, fontWeight: '800' },
  theaterSafe: { borderBottomWidth: StyleSheet.hairlineWidth },
  theaterTabs: { minHeight: 56, flexDirection: 'row', alignItems: 'center', paddingHorizontal: 8, gap: 4 },
  theaterTab: { flex: 1, minHeight: 36, borderRadius: 18, alignItems: 'center', justifyContent: 'center', paddingHorizontal: 5 },
  theaterTabActive: { minHeight: 38 },
  theaterLabel: { fontSize: 11, fontWeight: '800' },
})
