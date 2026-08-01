import { Platform } from 'react-native'
import * as Network from 'expo-network'
import Zeroconf, { ImplType, type ZeroconfService } from 'react-native-zeroconf'
import { probeMonarrServer, serviceBaseUrls, type DiscoveredServer } from './pairing'

const SCAN_DURATION_MS = 5000

export async function discoverMonarrServers(
  apiKey: string,
  onResolved: (count: number) => void,
): Promise<DiscoveredServer[]> {
  const state = await Network.getNetworkStateAsync()
  if (state.type !== Network.NetworkStateType.WIFI || !state.isConnected) {
    throw new Error('Connect this device to Wi-Fi before searching for Monarr.')
  }

  const services = await browseServices(onResolved)
  const found = await Promise.all(services.map(async (service) => {
    const candidates = serviceBaseUrls(service)
    const probes = await Promise.all(
      candidates.map((baseUrl) => probeMonarrServer(service.name, baseUrl, apiKey)),
    )
    return probes.find((result) => result !== null) ?? null
  }))
  return found
    .filter((result): result is DiscoveredServer => result !== null)
    .sort((left, right) => left.name.localeCompare(right.name))
}

function browseServices(onResolved: (count: number) => void): Promise<ZeroconfService[]> {
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
      else resolve([...services.values()])
    }

    zeroconf.on('resolved', (service: ZeroconfService) => {
      services.set(service.fullName ?? service.name, service)
      onResolved(services.size)
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
