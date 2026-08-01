import * as SecureStore from 'expo-secure-store'
import { sanitizeConnection } from './connection'
import type { Connection } from './types'

const connectionKey = 'monarr.connection.v1'

export async function loadConnection(): Promise<Connection | null> {
  const encoded = await SecureStore.getItemAsync(connectionKey)
  if (!encoded) return null
  try {
    const parsed = JSON.parse(encoded) as Partial<Connection>
    if (typeof parsed.baseUrl !== 'string' || typeof parsed.apiKey !== 'string') return null
    return sanitizeConnection({ baseUrl: parsed.baseUrl, apiKey: parsed.apiKey })
  } catch {
    return null
  }
}

export async function saveConnection(connection: Connection): Promise<Connection> {
  const clean = sanitizeConnection(connection)
  await SecureStore.setItemAsync(connectionKey, JSON.stringify(clean), {
    keychainAccessible: SecureStore.WHEN_UNLOCKED_THIS_DEVICE_ONLY,
  })
  return clean
}

export async function clearConnection(): Promise<void> {
  await SecureStore.deleteItemAsync(connectionKey)
}
