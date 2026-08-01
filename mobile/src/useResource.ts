import { useCallback, useEffect, useRef, useState } from 'react'

interface Resource<T> {
  data: T | null
  error: string
  loading: boolean
  refreshing: boolean
  refresh: () => Promise<void>
}

export function useResource<T>(load: () => Promise<T>, dependencies: readonly unknown[]): Resource<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const alive = useRef(true)

  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  // Callers pass stable primitive dependencies. The loader itself is omitted
  // intentionally so an inline closure cannot turn one request into a loop.
  const refresh = useCallback(async () => {
    setRefreshing(true)
    setError('')
    try {
      const value = await load()
      if (alive.current) setData(value)
    } catch (cause) {
      if (alive.current) setError(cause instanceof Error ? cause.message : 'Something went wrong.')
    } finally {
      if (alive.current) {
        setLoading(false)
        setRefreshing(false)
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, dependencies)

  useEffect(() => {
    setLoading(true)
    void refresh()
  }, [refresh])

  return { data, error, loading, refreshing, refresh }
}
