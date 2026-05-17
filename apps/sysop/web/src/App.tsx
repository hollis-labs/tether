import { useState } from 'react'
import { Activity, Boxes, Gauge, LayoutDashboard, Mail, Plug } from 'lucide-react'
import { NavRail, PageHeader, ThemeSwitcher, type NavRailItem } from '@hollis-labs/sysop-ui'
import { OverviewPage } from './pages/overview'
import { OperationsPage } from './pages/operations'
import { MessagingPage } from './pages/messaging'
import { MCPPage } from './pages/mcp'
import { ActivityPage } from './pages/activity'

/**
 * App shell — the icon nav rail on the left, a pinned page header, and the
 * active page. Add pages by extending `Route`, `nav`, `TITLES`, and the
 * route switch below.
 */
type Route = 'overview' | 'operations' | 'messaging' | 'mcp' | 'activity'

const TITLES: Record<Route, string> = {
  overview: 'Agent Ops',
  operations: 'Operations',
  messaging: 'Messaging',
  mcp: 'MCP',
  activity: 'Activity Monitor',
}

export function App() {
  const [route, setRoute] = useState<Route>('overview')

  const nav: NavRailItem[] = [
    {
      key: 'overview',
      label: 'Overview',
      icon: <Gauge className="h-4 w-4" />,
      active: route === 'overview',
      onSelect: () => setRoute('overview'),
    },
    {
      key: 'operations',
      label: 'Operations',
      icon: <LayoutDashboard className="h-4 w-4" />,
      active: route === 'operations',
      onSelect: () => setRoute('operations'),
    },
    {
      key: 'messaging',
      label: 'Messaging',
      icon: <Mail className="h-4 w-4" />,
      active: route === 'messaging',
      onSelect: () => setRoute('messaging'),
    },
    {
      key: 'mcp',
      label: 'MCP',
      icon: <Plug className="h-4 w-4" />,
      active: route === 'mcp',
      onSelect: () => setRoute('mcp'),
    },
    {
      key: 'activity',
      label: 'Activity Monitor',
      icon: <Activity className="h-4 w-4" />,
      active: route === 'activity',
      onSelect: () => setRoute('activity'),
    },
  ]

  return (
    <div className="flex h-screen bg-bg text-text">
      <NavRail items={nav} logo={<Boxes className="h-4 w-4" />} logoLabel="Tether" />
      <div className="flex min-w-0 flex-1 flex-col">
        <PageHeader title={TITLES[route]}>
          <ThemeSwitcher />
        </PageHeader>
        <main className="flex min-h-0 flex-1 flex-col">
          {route === 'overview' && <OverviewPage />}
          {route === 'operations' && <OperationsPage />}
          {route === 'messaging' && <MessagingPage />}
          {route === 'mcp' && <MCPPage />}
          {route === 'activity' && <ActivityPage />}
        </main>
      </div>
    </div>
  )
}
