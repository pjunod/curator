import {
  Link,
  Outlet,
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Dashboard } from './pages/Dashboard'
import { SystemPage } from './pages/System'
import { getHealth, getStatus } from './api'

function Layout() {
  const status = useQuery({ queryKey: ['status'], queryFn: getStatus, refetchInterval: 30_000 })
  const health = useQuery({ queryKey: ['health'], queryFn: getHealth, refetchInterval: 30_000 })
  const overall = health.data?.overall ?? 'ok'

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="wordmark">
          mon<span>arr</span>
        </div>
        <nav>
          <Link to="/" activeOptions={{ exact: true }} activeProps={{ className: 'active' }}>
            Dashboard
          </Link>
          <Link to="/system" activeProps={{ className: 'active' }}>
            System
            {overall !== 'ok' && <span className={`dot dot-${overall}`} aria-label={overall} />}
          </Link>
        </nav>
        <div className="sidebar-foot">
          <div>v{status.data?.version ?? '…'}</div>
          <div className="muted">phase 0 · walking skeleton</div>
        </div>
      </aside>
      <main className="content">
        <Outlet />
      </main>
    </div>
  )
}

const rootRoute = createRootRoute({ component: Layout })

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: Dashboard,
})

const systemRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/system',
  component: SystemPage,
})

const routeTree = rootRoute.addChildren([indexRoute, systemRoute])

export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

export { RouterProvider }
