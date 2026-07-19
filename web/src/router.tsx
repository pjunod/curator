import { useState } from 'react'
import {
  Link,
  Outlet,
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  useRouterState,
} from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { GlobalSearch } from './globalsearch'
import { useIsMobile } from './useIsMobile'
import { Dashboard } from './pages/Dashboard'
import { SystemPage } from './pages/System'
import { LibraryPage } from './pages/Library'
import { AddMediaPage } from './pages/AddMedia'
import { MediaDetailPage } from './pages/MediaDetail'
import { SettingsPage } from './pages/Settings'
import { ActivityPage } from './pages/Activity'
import { CalendarPage } from './pages/Calendar'
import { LoginPage } from './pages/Login'
import { WantedPage } from './pages/Wanted'
import type { MediaKind } from './api'
import { getHealth, getStatus } from './api'

type Theme = 'auto' | 'light' | 'dark'

function savedTheme(): Theme {
  try {
    const t = localStorage.getItem('monarr-theme')
    if (t === 'light' || t === 'dark') return t
  } catch {
    /* storage unavailable (private mode) — fall through to auto */
  }
  return 'auto'
}

// ThemePicker: manual light/dark override on top of the OS preference.
// 'auto' clears the override so prefers-color-scheme decides again.
function ThemePicker() {
  const [theme, setTheme] = useState<Theme>(savedTheme)
  const pick = (t: Theme) => {
    setTheme(t)
    if (t === 'auto') delete document.documentElement.dataset.theme
    else document.documentElement.dataset.theme = t
    try {
      if (t === 'auto') localStorage.removeItem('monarr-theme')
      else localStorage.setItem('monarr-theme', t)
    } catch {
      /* still applies for this page */
    }
  }
  return (
    <div className="theme-picker" role="group" aria-label="Theme">
      {(['auto', 'light', 'dark'] as const).map((t) => (
        <button
          key={t}
          className={t === theme ? 'active' : ''}
          aria-pressed={t === theme}
          onClick={() => pick(t)}
        >
          {t}
        </button>
      ))}
    </div>
  )
}

// MobileShell: the phone chrome — top bar, bottom tab bar, and a More
// sheet holding everything the desktop sidebar carries.
function MobileShell(props: { overall: string; version: string; children: React.ReactNode }) {
  const [sheet, setSheet] = useState(false)
  // Any navigation closes the sheet.
  const path = useRouterState({ select: (s) => s.location.pathname })
  const [lastPath, setLastPath] = useState(path)
  if (path !== lastPath) {
    setLastPath(path)
    if (sheet) setSheet(false)
  }

  const tab = (to: string, label: string, exact = false) => (
    <Link
      to={to}
      activeOptions={{ exact }}
      activeProps={{ className: 'active' }}
      className="mobile-tab"
    >
      {label}
    </Link>
  )

  return (
    <div className="app-mobile">
      <header className="mobile-top">
        <div className="wordmark">
          mon<span>arr</span>
        </div>
      </header>
      <main className="content mobile-content">{props.children}</main>

      <nav className="mobile-tabs" aria-label="Primary">
        {tab('/', 'Library', true)}
        {tab('/wanted', 'Wanted')}
        {tab('/calendar', 'Calendar')}
        {tab('/activity', 'Activity')}
        <button
          className={`mobile-tab${sheet ? ' active' : ''}`}
          aria-label="More"
          aria-expanded={sheet}
          onClick={() => setSheet(!sheet)}
        >
          More
          {props.overall !== 'ok' && <span className={`dot dot-${props.overall}`} aria-label={props.overall} />}
        </button>
      </nav>

      {sheet && (
        <>
          <div className="sheet-backdrop" onClick={() => setSheet(false)} />
          <div className="more-sheet" role="dialog" aria-label="More">
            <GlobalSearch />
            <nav className="sheet-nav">
              <Link to="/dashboard" activeProps={{ className: 'active' }}>
                Dashboard
              </Link>
              <Link to="/system" activeProps={{ className: 'active' }}>
                System
                {props.overall !== 'ok' && <span className={`dot dot-${props.overall}`} aria-label={props.overall} />}
              </Link>
              <Link to="/settings" activeProps={{ className: 'active' }}>
                Settings
              </Link>
              <Link to="/add" activeProps={{ className: 'active' }}>
                + Add media
              </Link>
            </nav>
            <ThemePicker />
            <div className="muted sheet-foot">v{props.version} · movies · series · books</div>
          </div>
        </>
      )}
    </div>
  )
}

function Layout() {
  const status = useQuery({ queryKey: ['status'], queryFn: getStatus, refetchInterval: 30_000 })
  const health = useQuery({ queryKey: ['health'], queryFn: getHealth, refetchInterval: 30_000 })
  const overall = health.data?.overall ?? 'ok'
  const isMobile = useIsMobile()

  if (isMobile) {
    return (
      <MobileShell overall={overall} version={status.data?.version ?? '…'}>
        <Outlet />
      </MobileShell>
    )
  }

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="wordmark">
          mon<span>arr</span>
        </div>
        <nav>
          <Link
            to="/"
            activeOptions={{ exact: true }}
            activeProps={{ className: 'active' }}
          >
            Library
          </Link>
          <Link to="/wanted" activeProps={{ className: 'active' }}>
            Wanted
          </Link>
          <Link to="/calendar" activeProps={{ className: 'active' }}>
            Calendar
          </Link>
          <Link to="/activity" activeProps={{ className: 'active' }}>
            Activity
          </Link>
          <Link to="/dashboard" activeProps={{ className: 'active' }}>
            Dashboard
          </Link>
          <Link to="/system" activeProps={{ className: 'active' }}>
            System
            {overall !== 'ok' && <span className={`dot dot-${overall}`} aria-label={overall} />}
          </Link>
          <Link to="/settings" activeProps={{ className: 'active' }}>
            Settings
          </Link>
        </nav>
        <GlobalSearch />
        <div className="sidebar-foot">
          <ThemePicker />
          <div>v{status.data?.version ?? '…'}</div>
          <div className="muted">movies · series · books</div>
        </div>
      </aside>
      <main className="content">
        <Outlet />
      </main>
    </div>
  )
}

const rootRoute = createRootRoute({ component: Layout })

const libraryRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: LibraryPage,
})

const dashboardRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/dashboard',
  component: Dashboard,
})

const mediaDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/library/$id',
  component: MediaDetailPage,
})

interface AddSearch {
  q?: string
  kind?: MediaKind
}

const addRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/add',
  component: AddMediaPage,
  validateSearch: (search: Record<string, unknown>): AddSearch => ({
    q: typeof search.q === 'string' ? search.q : undefined,
    kind:
      search.kind === 'movie' || search.kind === 'series' || search.kind === 'book'
        ? search.kind
        : undefined,
  }),
})

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/login',
  component: LoginPage,
})

const wantedRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/wanted',
  component: WantedPage,
})

const calendarRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/calendar',
  component: CalendarPage,
})

const activityRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/activity',
  component: ActivityPage,
})

const systemRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/system',
  component: SystemPage,
})

const settingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/settings',
  component: SettingsPage,
})

const routeTree = rootRoute.addChildren([
  libraryRoute,
  dashboardRoute,
  mediaDetailRoute,
  addRoute,
  loginRoute,
  wantedRoute,
  calendarRoute,
  activityRoute,
  systemRoute,
  settingsRoute,
])

export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

export { RouterProvider }
