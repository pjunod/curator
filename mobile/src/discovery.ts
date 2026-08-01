import { Platform } from 'react-native'
import * as Network from 'expo-network'
import Zeroconf, { ImplType, type ZeroconfService } from 'react-native-zeroconf'
import { probeMonarrServer, serviceBaseUrls, type DiscoveredServer } from './pairing'

const SCAN_DURATION_MS = 5000

export async function discoverMonarrServers(
  apiKey: string,
  onResolved: (count: number) => void,
  onDiscovered: (server: DiscoveredServer) => void = () => {},
): Promise<DiscoveredServer[]> {
  const state = await Network.getNetworkStateAsync()
  if (state.type !== Network.NetworkStateType.WIFI || !state.isConnected) {
    throw new Error('Connect this device to Wi-Fi before searching for Monarr.')
  }

  const probes: Promise<DiscoveredServer | null>[] = []
  await browseServices(onResolved, (service) => {
    const candidates = serviceBaseUrls(service)
    const probe = Promise.all(
      candidates.map((baseUrl) => probeMonarrServer(service.name, baseUrl, apiKey)),
    ).then((results) => {
      const found = results.find((result) => result !== null) ?? null
      if (found) onDiscovered(found)
      return found
    })
    probes.push(probe)
  })
  const found = await Promise.all(probes)
  return found
    .filter((result): result is DiscoveredServer => result !== null)
    .sort((left, right) => left.name.localeCompare(right.name))
}

function browseServices(
  onResolved: (count: number) => void,
  onService: (service: ZeroconfService) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const zeroconf = new Zeroconf()
    const services = new Map<string, ZeroconfService>()
    const implementation = Platform.OS === 'android' ? ImplType.DNSSD : ImplType.NSD
    let finished = false

    const cleanup = () => {
      try {
        zeroconf.stop(implementation)
      } catch {
        // A platform can report an error after it already stopped the scan.
      }
      zeroconf.removeDeviceListeners()
    }
    const finish = (error?: Error) => {
      if (finished) return
      finished = true
      clearTimeout(timer)
      cleanup()
      if (error) reject(error)
      else resolve()
    }

    zeroconf.on('resolved', (service: ZeroconfService) => {
      const key = service.fullName ?? service.name
      if (services.has(key)) return
      services.set(key, service)
      onResolved(services.size)
      onService(service)
    })
    zeroconf.on('error', (error: Error) => finish(error))
    const timer = setTimeout(() => finish(), SCAN_DURATION_MS)
    try {
      zeroconf.scan('monarr', 'tcp', 'local.', implementation)
    } catch (cause) {
      finish(cause instanceof Error ? cause : new Error('Local discovery could not start.'))
    }
  })
}
