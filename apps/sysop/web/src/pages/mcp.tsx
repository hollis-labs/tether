import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Plug, RefreshCw, Wrench } from 'lucide-react'
import {
  Button,
  CopyableId,
  DataTable,
  EmptyState,
  SummaryCards,
  cn,
  formatRelativeTime,
  type ColumnDef,
} from '@hollis-labs/sysop-ui'
import { useApi } from '../api/context'
import type { MCPServerInfo, MCPToolInfo } from '../api/client'
import { TabStrip, type TabItem } from '../components/tab-strip'

type TabKey = 'servers' | 'tools'

/** Small colored pill from the shared status tokens. */
function Pill({ tone, children }: { tone: 'on' | 'off'; children: React.ReactNode }) {
  return (
    <span
      className={cn(
        'inline-flex items-center rounded border px-2 py-0.5 text-[10px] uppercase tracking-wider',
        tone === 'on'
          ? 'border-status-done/40 bg-status-done/10 text-status-done'
          : 'border-border bg-panel-2/50 text-text-subtle',
      )}
    >
      {children}
    </span>
  )
}

const serverColumns: ColumnDef<MCPServerInfo>[] = [
  {
    key: 'id',
    header: 'Server',
    width: 'fill',
    cell: (s) => <CopyableId id={s.id} />,
    sortValue: (s) => s.id,
  },
  {
    key: 'transport',
    header: 'Transport',
    cell: (s) => (
      <span className="text-[11px] uppercase tracking-[.12em] text-text-soft">{s.transport}</span>
    ),
    sortValue: (s) => s.transport,
  },
  {
    key: 'endpoint',
    header: 'Endpoint',
    width: 'fill',
    cell: (s) => (
      <span className="block truncate font-mono text-[11px] text-text-soft">
        {s.url || s.command || '—'}
      </span>
    ),
    sortValue: (s) => s.url || s.command || '',
  },
  {
    key: 'scopes',
    header: 'Scopes',
    align: 'right',
    cell: (s) => <span className="text-[11px] text-text-soft">{s.scopes?.length ?? 0}</span>,
    sortValue: (s) => s.scopes?.length ?? 0,
  },
  {
    key: 'token',
    header: 'Token',
    cell: (s) => (s.has_token ? <Pill tone="on">set</Pill> : <span className="text-[11px] text-text-subtle">—</span>),
    sortValue: (s) => (s.has_token ? 1 : 0),
  },
  {
    key: 'enabled',
    header: 'State',
    cell: (s) => <Pill tone={s.enabled ? 'on' : 'off'}>{s.enabled ? 'enabled' : 'disabled'}</Pill>,
    sortValue: (s) => (s.enabled ? 1 : 0),
  },
]

const toolColumns: ColumnDef<MCPToolInfo>[] = [
  {
    key: 'name',
    header: 'Tool',
    width: 'fill',
    cell: (t) => <span className="font-mono text-[12px] text-text">{t.name}</span>,
    sortValue: (t) => t.name,
  },
  {
    key: 'server',
    header: 'Server',
    cell: (t) => <span className="text-[11px] text-text-soft">{t.server}</span>,
    sortValue: (t) => t.server,
  },
  {
    key: 'calls',
    header: 'Calls',
    align: 'right',
    cell: (t) => <span className="font-mono text-[11px] tabular-nums text-text">{t.calls}</span>,
    sortValue: (t) => t.calls,
  },
  {
    key: 'success',
    header: 'Success',
    align: 'right',
    cell: (t) => (
      <span
        className="font-mono text-[11px] tabular-nums"
        style={{
          color:
            t.success_pct >= 99
              ? 'var(--color-status-done)'
              : t.success_pct >= 90
                ? 'var(--color-text-soft)'
                : 'var(--color-status-blocked)',
        }}
      >
        {t.success_pct}%
      </span>
    ),
    sortValue: (t) => t.success_pct,
  },
  {
    key: 'avg',
    header: 'Avg',
    align: 'right',
    cell: (t) => (
      <span className="font-mono text-[11px] tabular-nums text-text-soft">{t.avg_ms}ms</span>
    ),
    sortValue: (t) => t.avg_ms,
  },
  {
    key: 'p95',
    header: 'p95',
    align: 'right',
    cell: (t) => (
      <span className="font-mono text-[11px] tabular-nums text-text-soft">{t.p95_ms}ms</span>
    ),
    sortValue: (t) => t.p95_ms,
  },
  {
    key: 'errors',
    header: 'Errors',
    align: 'right',
    cell: (t) => (
      <span
        className={cn(
          'font-mono text-[11px] tabular-nums',
          t.errors > 0 ? 'text-status-blocked' : 'text-text-subtle',
        )}
      >
        {t.errors}
      </span>
    ),
    sortValue: (t) => t.errors,
  },
  {
    key: 'last_seen',
    header: 'Last call',
    align: 'right',
    cell: (t) => <span className="text-[11px] text-text-soft">{formatRelativeTime(t.last_seen)}</span>,
    sortValue: (t) => t.last_seen,
  },
]

export function MCPPage() {
  const api = useApi()
  const [tab, setTab] = useState<TabKey>('servers')
  const [servers, setServers] = useState<MCPServerInfo[] | null>(null)
  const [tools, setTools] = useState<MCPToolInfo[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    Promise.all([api.getMCPServers(), api.getMCPTools()])
      .then(([serversInfo, toolsInfo]) => {
        if (cancelled) return
        setServers(serversInfo.servers ?? [])
        setTools(toolsInfo.tools ?? [])
        setError(serversInfo.error ?? toolsInfo.error ?? null)
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [api])

  useEffect(() => load(), [load])

  const serverList = servers ?? []
  const toolList = tools ?? []
  const enabled = serverList.filter((s) => s.enabled).length
  const totalCalls = useMemo(() => toolList.reduce((n, t) => n + t.calls, 0), [toolList])
  const totalErrors = useMemo(() => toolList.reduce((n, t) => n + t.errors, 0), [toolList])

  const tabs: TabItem<TabKey>[] = [
    { key: 'servers', label: 'Servers', icon: Plug, count: serverList.length },
    { key: 'tools', label: 'Tools', icon: Wrench, count: toolList.length },
  ]

  const summaryCards =
    tab === 'servers'
      ? [
          { label: 'Servers', value: servers ? serverList.length : '...' },
          {
            label: 'Enabled',
            value: servers ? enabled : '...',
            accentColor: 'var(--color-status-done)',
          },
          { label: 'Disabled', value: servers ? serverList.length - enabled : '...' },
        ]
      : [
          { label: 'Tools', value: tools ? toolList.length : '...' },
          { label: 'Calls', value: tools ? totalCalls : '...' },
          {
            label: 'Errors',
            value: tools ? totalErrors : '...',
            accentColor: 'var(--color-status-blocked)',
          },
        ]

  if (error && !servers && !tools) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not load MCP data" description={error} />
      </div>
    )
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col bg-bg">
      <TabStrip
        tabs={tabs}
        active={tab}
        onSelect={setTab}
        actions={
          <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
            <RefreshCw className={cn('h-3.5 w-3.5', loading && 'animate-spin')} />
            Refresh
          </Button>
        }
      />

      <SummaryCards cards={summaryCards} />

      <p className="shrink-0 border-b border-border-strong bg-bg px-4 py-1.5 text-[11px] text-text-subtle">
        {tab === 'servers'
          ? 'Upstream MCP servers from the catalog (mcp-servers/*.yaml). Live connection status is daemon-only — pending CW-20260517-0047.'
          : 'Tool usage aggregated from the MCP proxy ring buffer (latest 500 calls).'}
      </p>

      <div ref={scrollRef} className="min-h-0 flex-1 overflow-auto">
        {tab === 'servers' ? (
          <DataTable
            items={serverList}
            columns={serverColumns}
            getRowId={(s) => s.id}
            initialSort={{ key: 'id', dir: 'asc' }}
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading servers...' : 'No MCP servers'}
                description={
                  loading
                    ? 'Reading the catalog.'
                    : 'The catalog has no mcp-servers/*.yaml entries.'
                }
              />
            }
          />
        ) : (
          <DataTable
            items={toolList}
            columns={toolColumns}
            getRowId={(t) => t.name}
            initialSort={{ key: 'calls', dir: 'desc' }}
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading tools...' : 'No tool activity'}
                description={
                  loading
                    ? 'Reading the proxy event ring buffer.'
                    : 'The proxy_events table has no tool calls yet.'
                }
              />
            }
          />
        )}
      </div>
    </div>
  )
}
