import { useEffect, useMemo, useState } from 'react'
import { ActivityIndicator, BackHandler, Pressable, StyleSheet, Text, View } from 'react-native'
import { StatusBar } from 'expo-status-bar'
import { SafeAreaProvider, SafeAreaView, initialWindowMetrics } from 'react-native-safe-area-context'
import { MonarrClient } from './src/api'
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
    <SafeAreaProvider initialMetrics={initialWindowMetrics}>
      <MonarrApp />
    </SafeAreaProvider>
  )
}

function MonarrApp() {
  const theme = useTheme()
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

  return (
    <View style={[styles.app, { backgroundColor: theme.bg }]}>
      <StatusBar style={theme.dark ? 'light' : 'dark'} />
      <View style={styles.content}>{content}</View>
      {!overlay ? <TabBar selected={tab} onSelect={setTab} /> : null}
    </View>
  )
}

function TabBar({ selected, onSelect }: { selected: Tab; onSelect: (tab: Tab) => void }) {
  const theme = useTheme()
  const tabs: { id: Tab; glyph: string; label: string }[] = [
    { id: 'library', glyph: '▦', label: 'Library' },
    { id: 'discover', glyph: '✦', label: 'Discover' },
    { id: 'wanted', glyph: '⌕', label: 'Wanted' },
    { id: 'activity', glyph: '↓', label: 'Activity' },
    { id: 'more', glyph: '•••', label: 'More' },
  ]
  return (
    <SafeAreaView edges={['bottom', 'left', 'right']} style={[styles.tabSafe, { backgroundColor: theme.raised, borderTopColor: theme.border }]}>
      <View style={styles.tabs}>
        {tabs.map((item) => {
          const active = item.id === selected
          return (
            <Pressable
              key={item.id}
              accessibilityRole="tab"
              accessibilityState={{ selected: active }}
              onPress={() => onSelect(item.id)}
              style={styles.tab}
            >
              <Text style={[styles.tabGlyph, { color: active ? theme.accent : theme.muted }]}>{item.glyph}</Text>
              <Text style={[styles.tabLabel, { color: active ? theme.accent : theme.muted }]}>{item.label}</Text>
            </Pressable>
          )
        })}
      </View>
    </SafeAreaView>
  )
}

const styles = StyleSheet.create({
  app: { flex: 1 },
  boot: { flex: 1, alignItems: 'center', justifyContent: 'center' },
  content: { flex: 1 },
  tabSafe: { borderTopWidth: StyleSheet.hairlineWidth },
  tabs: { height: 58, flexDirection: 'row' },
  tab: { flex: 1, alignItems: 'center', justifyContent: 'center', gap: 1 },
  tabGlyph: { height: 25, fontSize: 20, lineHeight: 24, fontWeight: '600' },
  tabLabel: { fontSize: 10, fontWeight: '700' },
})
