import { Suspense, lazy, useEffect, useState } from 'react'
import { Activity, Boxes, BrainCircuit, Gauge, LayoutDashboard, Mail, Network, Plug, Settings } from 'lucide-react'
import { NavRail, PageHeader, ThemeSwitcher, type NavRailItem } from '@hollis-labs/sysop-ui/ui'

/**
 * App shell — the icon nav rail on the left, a pinned page header, and the
 * active page. Add pages by extending `Route`, `routeComponents`, `nav`,
 * and `TITLES`.
 */
type Route = 'overview' | 'operations' | 'messaging' | 'mcp' | 'ai' | 'activity' | 'registry' | 'settings'

const routeComponents = {
  overview: lazy(() => import('./pages/overview').then((module) => ({ default: module.OverviewPage }))),
  operations: lazy(() =>
    import('./pages/operations').then((module) => ({ default: module.OperationsPage })),
  ),
  messaging: lazy(() =>
    import('./pages/messaging').then((module) => ({ default: module.MessagingPage })),
  ),
  mcp: lazy(() => import('./pages/mcp').then((module) => ({ default: module.MCPPage }))),
  ai: lazy(() => import('./pages/ai').then((module) => ({ default: module.AIPage }))),
  activity: lazy(() => import('./pages/activity').then((module) => ({ default: module.ActivityPage }))),
  registry: lazy(() => import('./pages/registry').then((module) => ({ default: module.RegistryPage }))),
  settings: lazy(() => import('./pages/settings').then((module) => ({ default: module.SettingsPage }))),
} satisfies Record<Route, ReturnType<typeof lazy>>

const BASE_PATH = '/operations'
const ROUTE_PATHS: Record<Route, string> = {
  overview: '',
  operations: 'operations',
  messaging: 'messaging',
  mcp: 'mcp',
  ai: 'ai',
  activity: 'activity',
  registry: 'registry',
  settings: 'settings',
}

const TITLES: Record<Route, string> = {
  overview: 'Agent Ops',
  operations: 'Operations',
  messaging: 'Messaging',
  mcp: 'MCP',
  ai: 'AI Gateway',
  activity: 'Activity Monitor',
  registry: 'Registry',
  settings: 'Settings',
}

function routeFromPath(pathname: string): Route {
  const trimmed = pathname.replace(/\/+$/, '')
  const rest = trimmed === BASE_PATH ? '' : trimmed.replace(new RegExp(`^${BASE_PATH}/?`), '')
  const match = (Object.entries(ROUTE_PATHS) as Array<[Route, string]>).find(
    ([, path]) => path === rest,
  )
  return match?.[0] ?? 'overview'
}

export function App() {
  const [route, setRoute] = useState<Route>(() => routeFromPath(window.location.pathname))
  const ActivePage = routeComponents[route]

  useEffect(() => {
    function onPopState() {
      setRoute(routeFromPath(window.location.pathname))
    }
    window.addEventListener('popstate', onPopState)
    return () => window.removeEventListener('popstate', onPopState)
  }, [])

  function navigate(next: Route) {
    setRoute(next)
    const path = ROUTE_PATHS[next]
    const nextPath = path ? `${BASE_PATH}/${path}` : `${BASE_PATH}/`
    if (window.location.pathname !== nextPath) {
      window.history.pushState(null, '', nextPath)
    }
  }

  const nav: NavRailItem[] = [
    {
      key: 'overview',
      label: 'Overview',
      icon: <Gauge className="h-4 w-4" />,
      active: route === 'overview',
      onSelect: () => navigate('overview'),
    },
    {
      key: 'operations',
      label: 'Operations',
      icon: <LayoutDashboard className="h-4 w-4" />,
      active: route === 'operations',
      onSelect: () => navigate('operations'),
    },
    {
      key: 'messaging',
      label: 'Messaging',
      icon: <Mail className="h-4 w-4" />,
      active: route === 'messaging',
      onSelect: () => navigate('messaging'),
    },
    {
      key: 'mcp',
      label: 'MCP',
      icon: <Plug className="h-4 w-4" />,
      active: route === 'mcp',
      onSelect: () => navigate('mcp'),
    },
    {
      key: 'ai',
      label: 'AI',
      icon: <BrainCircuit className="h-4 w-4" />,
      active: route === 'ai',
      onSelect: () => navigate('ai'),
    },
    {
      key: 'activity',
      label: 'Activity Monitor',
      icon: <Activity className="h-4 w-4" />,
      active: route === 'activity',
      onSelect: () => navigate('activity'),
    },
    {
      key: 'registry',
      label: 'Registry',
      icon: <Network className="h-4 w-4" />,
      active: route === 'registry',
      onSelect: () => navigate('registry'),
    },
    {
      key: 'settings',
      label: 'Settings',
      icon: <Settings className="h-4 w-4" />,
      active: route === 'settings',
      onSelect: () => navigate('settings'),
      footer: true,
    },
  ]

  return (
    <div className="flex h-screen bg-bg text-text">
      <NavRail
        items={nav}
        logo={<Boxes className="h-4 w-4" />}
        logoLabel="Tether"
        footerExtra={<ThemeSwitcher />}
      />
      <div className="flex min-w-0 flex-1 flex-col">
        <PageHeader title={TITLES[route]} />
        <main className="flex min-h-0 flex-1 flex-col">
          <Suspense fallback={<div className="flex flex-1 items-center justify-center text-sm text-text-subtle">Loading...</div>}>
            <ActivePage />
          </Suspense>
        </main>
      </div>
    </div>
  )
}
