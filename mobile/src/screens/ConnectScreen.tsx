import { useCallback, useEffect, useRef, useState } from 'react'
import { KeyboardAvoidingView, Platform, StyleSheet, Text, View } from 'react-native'
import { CameraView, useCameraPermissions, type BarcodeScanningResult } from 'expo-camera'
import { MonarrClient } from '../api'
import { sanitizeConnection } from '../connection'
import { discoverMonarrServers } from '../discovery'
import { parsePairingCode, type DiscoveredServer } from '../pairing'
import { useTheme } from '../theme'
import type { Connection, SystemStatus } from '../types'
import { AppScreen, Badge, Button, Field, InlineError, Panel, Wordmark } from '../components/UI'

export function ConnectScreen({
  initial,
  onConnected,
}: {
  initial?: Connection | null
  onConnected: (connection: Connection, status: SystemStatus) => Promise<void>
}) {
  const theme = useTheme()
  const [cameraPermission, requestCameraPermission] = useCameraPermissions()
  const [server, setServer] = useState(initial?.baseUrl ?? '')
  const [apiKey, setApiKey] = useState(initial?.apiKey ?? '')
  const [error, setError] = useState('')
  const [message, setMessage] = useState('')
  const [connecting, setConnecting] = useState(false)
  const [discovering, setDiscovering] = useState(true)
  const [resolvedCount, setResolvedCount] = useState(0)
  const [discovered, setDiscovered] = useState<DiscoveredServer[]>([])
  const [scanningQr, setScanningQr] = useState(false)
  const [qrHandled, setQrHandled] = useState(false)
  const apiKeyRef = useRef(apiKey)
  const scanGeneration = useRef(0)

  useEffect(() => {
    apiKeyRef.current = apiKey
  }, [apiKey])

  const connectWith = async (candidate: Connection) => {
    setConnecting(true)
    setError('')
    setMessage('')
    try {
      const connection = sanitizeConnection(candidate)
      const status = await new MonarrClient(connection).getStatus()
      await onConnected(connection, status)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not connect to Monarr.')
    } finally {
      setConnecting(false)
    }
  }

  const connect = () => connectWith({ baseUrl: server, apiKey })

  const findOnWifi = useCallback(async (automatic = false) => {
    const generation = ++scanGeneration.current
    setDiscovering(true)
    setResolvedCount(0)
    setDiscovered([])
    setError('')
    setMessage('Looking for Monarr DNS-SD services on this Wi-Fi network…')
    try {
      const servers = await discoverMonarrServers(
        apiKeyRef.current,
        (count) => {
          if (scanGeneration.current === generation) setResolvedCount(count)
        },
        (found) => {
          if (scanGeneration.current !== generation) return
          setDiscovered((current) => {
            if (current.some((server) => server.baseUrl === found.baseUrl)) return current
            return [...current, found].sort((left, right) => left.name.localeCompare(right.name))
          })
        },
      )
      if (scanGeneration.current !== generation) return
      setDiscovered(servers)
      setMessage(
        servers.length === 0
          ? 'No Monarr servers answered. Make sure the server is running v0.18.0 or newer and multicast DNS is allowed on this Wi-Fi network.'
          : `Found ${servers.length} Monarr ${servers.length === 1 ? 'server' : 'servers'}.`,
      )
    } catch (cause) {
      if (scanGeneration.current !== generation) return
      const detail = cause instanceof Error ? cause.message : 'Local discovery could not start.'
      if (automatic) {
        setMessage(detail)
      } else {
        setMessage('')
        setError(detail)
      }
    } finally {
      if (scanGeneration.current === generation) setDiscovering(false)
    }
  }, [])

  useEffect(() => {
    void findOnWifi(true)
    return () => {
      scanGeneration.current += 1
    }
  }, [findOnWifi])

  const useDiscoveredServer = (found: DiscoveredServer) => {
    setServer(found.baseUrl)
    setError('')
    if (found.requiresApiKey) {
      setMessage(`${found.name} requires its API key. Enter it below, then connect.`)
      return
    }
    void connectWith({ baseUrl: found.baseUrl, apiKey })
  }

  const openQrScanner = async () => {
    setError('')
    setMessage('')
    let permission = cameraPermission
    if (!permission?.granted) permission = await requestCameraPermission()
    if (!permission.granted) {
      setError('Camera access is required to scan a Monarr pairing QR code.')
      return
    }
    setQrHandled(false)
    setScanningQr(true)
  }

  const handleQrCode = ({ data }: BarcodeScanningResult) => {
    if (qrHandled) return
    setQrHandled(true)
    try {
      const connection = parsePairingCode(data)
      setServer(connection.baseUrl)
      setApiKey(connection.apiKey)
      setScanningQr(false)
      void connectWith(connection)
    } catch (cause) {
      setScanningQr(false)
      setError(cause instanceof Error ? cause.message : 'This pairing QR code could not be read.')
    }
  }

  if (scanningQr) {
    return (
      <AppScreen>
        <View style={styles.scannerScreen}>
          <View style={styles.scannerHeader}>
            <Wordmark size={30} />
            <Text style={[styles.scannerTitle, { color: theme.text }]}>Scan the pairing code</Text>
            <Text style={[styles.scannerHint, { color: theme.muted }]}>Open Monarr web → Settings → Security and reveal the mobile pairing QR.</Text>
          </View>
          <View style={[styles.cameraFrame, { borderColor: theme.accent }]}>
            <CameraView
              style={styles.camera}
              facing="back"
              barcodeScannerSettings={{ barcodeTypes: ['qr'] }}
              onBarcodeScanned={qrHandled ? undefined : handleQrCode}
            />
            <View pointerEvents="none" style={[styles.scanTarget, { borderColor: theme.accent }]} />
          </View>
          <Button label="Cancel" secondary onPress={() => setScanningQr(false)} />
        </View>
      </AppScreen>
    )
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
          <Text style={[styles.panelTitle, { color: theme.text }]}>Nearby Monarr servers</Text>
          <Text style={[styles.hint, { color: theme.muted }]}>Monarr searches this Wi-Fi automatically. Choose a server when it appears, or use either option below.</Text>
          {discovered.map((found) => (
            <View key={`${found.name}-${found.baseUrl}`} style={[styles.result, { borderColor: theme.border }]}>
              <View style={styles.resultText}>
                <Text style={[styles.resultName, { color: theme.text }]}>{found.name}</Text>
                <Text style={[styles.resultUrl, { color: theme.muted }]}>{found.baseUrl}</Text>
                {found.status ? <Badge label={`Monarr ${found.status.version}`} tone="ok" /> : <Badge label="API key required" tone="warning" />}
              </View>
              <Button label={found.requiresApiKey ? 'Use' : 'Connect'} compact disabled={connecting} onPress={() => useDiscoveredServer(found)} />
            </View>
          ))}
          {message ? <Text style={[styles.message, { color: theme.muted }]}>{message}</Text> : null}
          <Button
            label={discovering ? `Searching…${resolvedCount ? ` ${resolvedCount} announced` : ''}` : 'Search again'}
            disabled={discovering || connecting}
            secondary
            onPress={() => void findOnWifi()}
          />
          <Button label="Scan pairing QR" disabled={connecting} secondary onPress={() => void openQrScanner()} />
          <InlineError message={error} />
        </Panel>

        <Panel style={{ ...styles.form, ...styles.manual }}>
          <Text style={[styles.panelTitle, { color: theme.text }]}>Enter it manually</Text>
          <View style={styles.labelBlock}>
            <Text style={[styles.label, { color: theme.text }]}>Server address</Text>
            <Text style={[styles.hint, { color: theme.muted }]}>LAN IPv4/IPv6, Tailscale, or HTTPS reverse-proxy address</Text>
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

          <Button label={connecting ? 'Connecting…' : 'Connect'} disabled={connecting} onPress={() => void connect()} />
        </Panel>

        <Text style={[styles.privacy, { color: theme.muted }]}>The API key stays in this device's keychain or encrypted keystore. The pairing QR contains that key, so show it only to a device you trust.</Text>
      </KeyboardAvoidingView>
    </AppScreen>
  )
}

const styles = StyleSheet.create({
  hero: { alignItems: 'center', paddingTop: 42, paddingBottom: 28, paddingHorizontal: 16 },
  title: { fontSize: 25, lineHeight: 31, fontWeight: '800', textAlign: 'center', marginTop: 20 },
  subtitle: { fontSize: 15, lineHeight: 22, textAlign: 'center', maxWidth: 340, marginTop: 9 },
  form: { gap: 14 },
  manual: { marginTop: 14 },
  panelTitle: { fontSize: 18, fontWeight: '800' },
  labelBlock: { gap: 3, marginTop: 2 },
  label: { fontSize: 14, fontWeight: '700' },
  hint: { fontSize: 12, lineHeight: 17 },
  message: { fontSize: 12, lineHeight: 18 },
  result: { borderTopWidth: StyleSheet.hairlineWidth, paddingTop: 12, flexDirection: 'row', alignItems: 'center', gap: 12 },
  resultText: { flex: 1, gap: 3 },
  resultName: { fontSize: 14, fontWeight: '700' },
  resultUrl: { fontSize: 11, lineHeight: 16 },
  privacy: { fontSize: 12, lineHeight: 18, textAlign: 'center', paddingHorizontal: 20, marginTop: 18 },
  scannerScreen: { flex: 1, padding: 16, gap: 18 },
  scannerHeader: { alignItems: 'center', gap: 7, paddingTop: 12 },
  scannerTitle: { fontSize: 21, fontWeight: '800', marginTop: 5 },
  scannerHint: { fontSize: 13, lineHeight: 19, textAlign: 'center', maxWidth: 370 },
  cameraFrame: { flex: 1, minHeight: 320, maxHeight: 560, overflow: 'hidden', borderWidth: 2, borderRadius: 20 },
  camera: { flex: 1 },
  scanTarget: { position: 'absolute', width: 220, height: 220, top: '50%', left: '50%', marginTop: -110, marginLeft: -110, borderWidth: 3, borderRadius: 18 },
})
