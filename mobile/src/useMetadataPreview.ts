import { useCallback, useEffect, useRef, useState } from 'react'
import { AppState } from 'react-native'
import { ApiError } from './api'
import type { MonarrClient } from './api'
import { previewKey, previewQuery, previewIdentityConflict } from './metadataPreview'
import type { MetadataPreview, SearchResult } from './types'

export function useMetadataPreview(client: MonarrClient, item: SearchResult) {
  const key = previewKey(item)
  const generation = useRef(0)
  const [data, setData] = useState<MetadataPreview>()
  const [error, setError] = useState<Error>()
  const [unsupported, setUnsupported] = useState<string>()
  const [conflict, setConflict] = useState(false)
  const [loading, setLoading] = useState(false)
  const refresh = useCallback(async () => {
    if (!key) return
    const request = ++generation.current
    setLoading(true)
    try {
      const response = await client.getMetadataPreview(previewQuery(key))
      if (request === generation.current) {
        if (previewIdentityConflict(item, response)) throw new ApiError('The provider returned conflicting identities.', 409, 'identity_conflict')
        setData(response); setError(undefined); setConflict(false); setUnsupported(undefined)
      }
    } catch (cause) {
      if (request === generation.current) {
        setError(cause instanceof Error ? cause : new Error('More details are unavailable.'))
        if (cause instanceof ApiError && cause.code === 'unsupported_hydration') setUnsupported(cause.message)
        if (cause instanceof ApiError && cause.code === 'identity_conflict') { setData(undefined); setConflict(true) }
      }
    } finally {
      if (request === generation.current) setLoading(false)
    }
  }, [client, key, item])
  useEffect(() => {
    setData(undefined)
    setError(undefined)
    setConflict(false)
    setUnsupported(undefined)
    void refresh()
    const subscription = AppState.addEventListener('change', (state) => { if (state === 'active') void refresh() })
    return () => { generation.current++; subscription.remove() }
  }, [refresh])
  return { data, error, loading, refresh, conflict, unsupported }
}
