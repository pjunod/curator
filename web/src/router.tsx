import { useEffect, useState } from 'react'
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
import { DiscoverPage } from './pages/Discover'
import { MediaDetailPage } from './pages/MediaDetail'
import { SettingsPage } from './pages/Settings'
import { AccessPage } from './pages/Access'
import { ActivityPage } from './pages/Activity'
import { CalendarPage } from './pages/Calendar'
import type { CalendarSearch } from './pages/Calendar'
import { LoginPage } from './pages/Login'
import { WantedPage } from './pages/Wanted'
import type { MediaKind } from './api'
import { getHealth, getStatus } from './api'
import {
  LAYOUTS,
  PALETTES,
  applyDisplaySettings,
  loadDisplaySettings,
  saveDisplaySettings,
  type Appearance,
  type DisplaySettings,
  type LayoutId,
  type PaletteId,
} from './display'

function DisplayPicker() {
  const [settings, setSettings] = useState<DisplaySettings>(loadDisplaySettings)
  const update = (change: Partial<DisplaySettings>) => {
    const next = { ...settings, ...change }
    setSettings(next)
    saveDisplaySettings(next)
  }

  useEffect(() => {
    applyDisplaySettings(settings)
    if (settings.appearance !== 'auto') return
    const media = window.matchMedia('(prefers-color-scheme: light)')
    const changed = () => applyDisplaySettings(settings)
    media.addEventListener('change', changed)
    return () => media.removeEventListener('change', changed)
  }, [settings])

  const palette = PALETTES.find((candidate) => candidate.id === settings.palette)
  return (
    <details className="display-settings">
      <summary>Display</summary>
      <div className="display-menu">
        <label>
          <span>Layout</span>
          <select value={settings.layout} onChange={(event) => update({ layout: event.target.value as LayoutId })}>
            {LAYOUTS.map((layout) => <option key={layout.id} value={layout.id}>{layout.name}</option>)}
          </select>
        </label>
        <label>
          <span>Color scheme</span>
          <select value={settings.palette} onChange={(event) => update({ palette: event.target.value as PaletteId })}>
            {PALETTES.map((candidate) => <option key={candidate.id} value={candidate.id}>{candidate.name}</option>)}
          </select>
        </label>
        <label>
          <span>Appearance</span>
          <select value={settings.appearance} onChange={(event) => update({ appearance: event.target.value as Appearance })}>
            <option value="auto">Auto (system)</option>
            <option value="light">Light</option>
            <option value="dark">Dark</option>
          </select>
        </label>
        {palette?.darkOnly && <p>{palette.name} is a midnight-only scheme and stays dark.</p>}
      </div>
    </details>
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
              <Link to="/discover" activeProps={{ className: 'active' }}>
                Discover
              </Link>
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
              <Link to="/access" activeProps={{ className: 'active' }}>
                Access
              </Link>
              <Link to="/add" activeProps={{ className: 'active' }}>
                + Add media
              </Link>
            </nav>
            <DisplayPicker />
            <div className="muted sheet-foot">v{props.version} · movies · TV · books</div>
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
          <Link to="/discover" activeProps={{ className: 'active' }}>
            Discover
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
          <Link to="/access" activeProps={{ className: 'active' }}>
            Access
          </Link>
        </nav>
        <GlobalSearch />
        <div className="sidebar-foot">
          <DisplayPicker />
          <div>v{status.data?.version ?? '…'}</div>
          <div className="muted">movies · TV · books</div>
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

interface MediaDetailSearch {
  assignFolder?: boolean
}

const mediaDetailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/library/$id',
  component: MediaDetailPage,
  validateSearch: (search: Record<string, unknown>): MediaDetailSearch => ({
    assignFolder: search.assignFolder === true || search.assignFolder === 'true' ? true : undefined,
  }),
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

const discoverRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/discover',
  component: DiscoverPage,
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
  // Both params optional: ?view deep-links a view (and beats the stored
  // choice), ?date anchors the month shown / the day the agenda opens at.
  // Anything else is dropped rather than trusted — these end up in a Date
  // constructor and a render branch.
  validateSearch: (search: Record<string, unknown>): CalendarSearch => ({
    view: search.view === 'month' || search.view === 'agenda' ? search.view : undefined,
    date:
      typeof search.date === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(search.date)
        ? search.date
        : undefined,
  }),
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

const accessRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/access',
  component: AccessPage,
})

const routeTree = rootRoute.addChildren([
  libraryRoute,
  dashboardRoute,
  mediaDetailRoute,
  addRoute,
  discoverRoute,
  loginRoute,
  wantedRoute,
  calendarRoute,
  activityRoute,
  systemRoute,
  settingsRoute,
  accessRoute,
])

export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

export { RouterProvider }
