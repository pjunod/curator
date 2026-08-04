import { createContext, useContext, type PropsWithChildren } from 'react'

const TopNavigationContext = createContext(false)

export function TopNavigationProvider({ active, children }: PropsWithChildren<{ active: boolean }>) {
  return <TopNavigationContext.Provider value={active}>{children}</TopNavigationContext.Provider>
}

export function useTopNavigation(): boolean {
  return useContext(TopNavigationContext)
}
