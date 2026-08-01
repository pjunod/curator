declare module 'react-native-zeroconf' {
  export interface ZeroconfService {
    name: string
    fullName?: string
    host?: string
    port: number
    addresses?: string[]
    txt?: Record<string, string>
  }

  export const ImplType: {
    NSD: 'NSD'
    DNSSD: 'DNSSD'
  }

  export default class Zeroconf {
    on(event: 'resolved', listener: (service: ZeroconfService) => void): this
    on(event: 'error', listener: (error: Error) => void): this
    scan(type?: string, protocol?: string, domain?: string, implType?: 'NSD' | 'DNSSD'): void
    stop(implType?: 'NSD' | 'DNSSD'): void
    getServices(): Record<string, ZeroconfService>
    removeDeviceListeners(): void
  }
}
