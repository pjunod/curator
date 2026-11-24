import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

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
  const generation = useRef(0)
  const epoch = useMemo(() => ({}), dependencies)
  const currentEpoch = useRef(epoch)
  currentEpoch.current = epoch

  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  // Callers pass stable primitive dependencies. The loader itself is omitted
  // intentionally so an inline closure cannot turn one request into a loop.
  const refresh = useCallback(async () => {
    if (!alive.current || currentEpoch.current !== epoch) return
    const ticket=++generation.current
    setRefreshing(true)
    setError('')
    try {
      const value = await load()
      if (alive.current && ticket===generation.current && currentEpoch.current === epoch) setData(value)
    } catch (cause) {
      if (alive.current && ticket===generation.current && currentEpoch.current === epoch) setError(cause instanceof Error ? cause.message : 'Something went wrong.')
    } finally {
      if (alive.current && ticket===generation.current && currentEpoch.current === epoch) {
        setLoading(false)
        setRefreshing(false)
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, dependencies)

  useEffect(() => {
    setData(null)
    setLoading(true)
    void refresh()
    return ()=>{generation.current++}
  }, [refresh])

  return { data, error, loading, refreshing, refresh }
}
