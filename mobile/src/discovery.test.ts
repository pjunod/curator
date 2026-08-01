import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  getNetworkStateAsync: vi.fn(),
  probeMonarrServer: vi.fn(),
  service: {
    name: 'Monarr in the rack',
    fullName: 'Monarr in the rack._monarr._tcp.local.',
    host: 'rack.local.',
    port: 7676,
    addresses: ['192.168.50.20', 'fd12:3456::20'],
  },
}))

vi.mock('react-native', () => ({ Platform: { OS: 'ios' } }))
vi.mock('expo-network', () => ({
  NetworkStateType: { WIFI: 'WIFI' },
  getNetworkStateAsync: mocks.getNetworkStateAsync,
}))
vi.mock('./pairing', () => ({
  serviceBaseUrls: () => ['http://rack.local:7676'],
  probeMonarrServer: mocks.probeMonarrServer,
}))
vi.mock('react-native-zeroconf', () => {
  type Listener = (...values: unknown[]) => void

  return {
    ImplType: { NSD: 'NSD', DNSSD: 'DNSSD' },
    default: class MockZeroconf {
      private listeners = new Map<string, Listener>()

      on(event: string, listener: Listener) {
        this.listeners.set(event, listener)
      }

      scan() {
        queueMicrotask(() => this.listeners.get('resolved')?.(mocks.service))
      }

      stop() {}

      removeDeviceListeners() {
        this.listeners.clear()
      }
    },
  }
})

import { discoverMonarrServers } from './discovery'

beforeEach(() => {
  vi.useFakeTimers()
  mocks.getNetworkStateAsync.mockResolvedValue({ type: 'WIFI', isConnected: true })
  mocks.probeMonarrServer.mockResolvedValue({
    name: mocks.service.name,
    baseUrl: 'http://rack.local:7676',
    requiresApiKey: false,
  })
})

afterEach(() => {
  vi.useRealTimers()
  vi.clearAllMocks()
})

describe('discoverMonarrServers', () => {
  it('surfaces a verified server while the automatic browse is still running', async () => {
    const announced = vi.fn()
    const discovered = vi.fn()
    const scan = discoverMonarrServers('', announced, discovered)

    await vi.advanceTimersByTimeAsync(0)

    expect(announced).toHaveBeenCalledWith(1)
    expect(discovered).toHaveBeenCalledWith(expect.objectContaining({
      baseUrl: 'http://rack.local:7676',
    }))

    await vi.advanceTimersByTimeAsync(5000)
    await expect(scan).resolves.toEqual([
      expect.objectContaining({ baseUrl: 'http://rack.local:7676' }),
    ])
  })
})
