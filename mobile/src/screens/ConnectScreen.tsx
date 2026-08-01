import { useState } from 'react'
import { KeyboardAvoidingView, Platform, StyleSheet, Text, View } from 'react-native'
import { MonarrClient } from '../api'
import { sanitizeConnection } from '../connection'
import { useTheme } from '../theme'
import type { Connection, SystemStatus } from '../types'
import { AppScreen, Button, Field, InlineError, Panel, Wordmark } from '../components/UI'

export function ConnectScreen({
  initial,
  onConnected,
}: {
  initial?: Connection | null
  onConnected: (connection: Connection, status: SystemStatus) => Promise<void>
}) {
  const theme = useTheme()
  const [server, setServer] = useState(initial?.baseUrl ?? '')
  const [apiKey, setApiKey] = useState(initial?.apiKey ?? '')
  const [error, setError] = useState('')
  const [connecting, setConnecting] = useState(false)

  const connect = async () => {
    setConnecting(true)
    setError('')
    try {
      const connection = sanitizeConnection({ baseUrl: server, apiKey })
      const status = await new MonarrClient(connection).getStatus()
      await onConnected(connection, status)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not connect to Monarr.')
    } finally {
      setConnecting(false)
    }
  }

  return (
    <AppScreen scroll>
      <KeyboardAvoidingView behavior={Platform.OS === 'ios' ? 'padding' : undefined}>
        <View style={styles.hero}>
          <Wordmark size={38} />
          <Text style={[styles.title, { color: theme.text }]}>Your media, from your pocket.</Text>
          <Text style={[styles.subtitle, { color: theme.muted }]}>Connect this device to the Monarr server you already run.</Text>
        </View>

        <Panel style={styles.form}>
          <View style={styles.labelBlock}>
            <Text style={[styles.label, { color: theme.text }]}>Server address</Text>
            <Text style={[styles.hint, { color: theme.muted }]}>LAN, Tailscale, or HTTPS reverse-proxy address</Text>
          </View>
          <Field
            autoCapitalize="none"
            autoCorrect={false}
            keyboardType="url"
            placeholder="http://192.168.1.20:7676"
            returnKeyType="next"
            value={server}
            onChangeText={setServer}
          />

          <View style={styles.labelBlock}>
            <Text style={[styles.label, { color: theme.text }]}>API key</Text>
            <Text style={[styles.hint, { color: theme.muted }]}>Settings → Security → Reveal; optional when auth is off</Text>
          </View>
          <Field
            autoCapitalize="none"
            autoCorrect={false}
            placeholder="Monarr API key"
            secureTextEntry
            returnKeyType="go"
            value={apiKey}
            onChangeText={setApiKey}
            onSubmitEditing={() => void connect()}
          />

          <InlineError message={error} />
          <Button label={connecting ? 'Connecting…' : 'Connect'} disabled={connecting} onPress={() => void connect()} />
        </Panel>

        <Text style={[styles.privacy, { color: theme.muted }]}>
          The API key stays in this device's keychain or encrypted keystore. Monarr never sends it anywhere except the server address above.
        </Text>
      </KeyboardAvoidingView>
    </AppScreen>
  )
}

const styles = StyleSheet.create({
  hero: { alignItems: 'center', paddingTop: 50, paddingBottom: 34, paddingHorizontal: 16 },
  title: { fontSize: 25, lineHeight: 31, fontWeight: '800', textAlign: 'center', marginTop: 20 },
  subtitle: { fontSize: 15, lineHeight: 22, textAlign: 'center', maxWidth: 340, marginTop: 9 },
  form: { gap: 14 },
  labelBlock: { gap: 3, marginTop: 2 },
  label: { fontSize: 14, fontWeight: '700' },
  hint: { fontSize: 12, lineHeight: 17 },
  privacy: { fontSize: 12, lineHeight: 18, textAlign: 'center', paddingHorizontal: 20, marginTop: 18 },
})
