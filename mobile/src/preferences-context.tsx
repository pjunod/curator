import { createContext, useCallback, useContext, useEffect, useRef, useState, type PropsWithChildren } from 'react'
import { loadPreferences, savePreferences } from './storage'
import {
  DEFAULT_PREFERENCES,
  type AppPreferences,
  type ItemSize,
  type LayoutPreference,
  type PalettePreference,
  type ThemePreference,
} from './preferences'

interface PreferencesContextValue extends AppPreferences {
  setTheme: (theme: ThemePreference) => void
  setItemSize: (itemSize: ItemSize) => void
  setLayout: (layout: LayoutPreference) => void
  setPalette: (palette: PalettePreference) => void
}

const PreferencesContext = createContext<PreferencesContextValue | null>(null)

export function PreferencesProvider({ children }: PropsWithChildren) {
  const [preferences, setPreferences] = useState(DEFAULT_PREFERENCES)
  const saveQueue = useRef(Promise.resolve())
  const changed = useRef(false)

  useEffect(() => {
    let active = true
    void loadPreferences().then((saved) => {
      if (active && !changed.current) setPreferences(saved)
    })
    return () => {
      active = false
    }
  }, [])

  const setTheme = useCallback((theme: ThemePreference) => {
    changed.current = true
    setPreferences((current) => {
      const next = { ...current, theme }
      saveQueue.current = saveQueue.current
        .then(() => savePreferences(next))
        .catch(() => undefined)
      return next
    })
  }, [])

  const setItemSize = useCallback((itemSize: ItemSize) => {
    changed.current = true
    setPreferences((current) => {
      const next = { ...current, itemSize }
      saveQueue.current = saveQueue.current
        .then(() => savePreferences(next))
        .catch(() => undefined)
      return next
    })
  }, [])

  const setLayout = useCallback((layout: LayoutPreference) => {
    changed.current = true
    setPreferences((current) => {
      const next = { ...current, layout }
      saveQueue.current = saveQueue.current
        .then(() => savePreferences(next))
        .catch(() => undefined)
      return next
    })
  }, [])

  const setPalette = useCallback((palette: PalettePreference) => {
    changed.current = true
    setPreferences((current) => {
      const next = { ...current, palette }
      saveQueue.current = saveQueue.current
        .then(() => savePreferences(next))
        .catch(() => undefined)
      return next
    })
  }, [])

  return (
    <PreferencesContext.Provider value={{ ...preferences, setTheme, setItemSize, setLayout, setPalette }}>
      {children}
    </PreferencesContext.Provider>
  )
}

export function usePreferences(): PreferencesContextValue {
  const value = useContext(PreferencesContext)
  if (!value) throw new Error('usePreferences must be used inside PreferencesProvider')
  return value
}
