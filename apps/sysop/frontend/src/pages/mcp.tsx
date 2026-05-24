import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Plug, RefreshCw, Wrench } from 'lucide-react'
import {
  Button,
  CopyableId,
  DataTable,
  DetailDialog,
  DetailSection,
  EmptyState,
  ListPageLayout,
  Pill,
  SummaryCards,
  TabStrip,
  cn,
  formatRelativeTime,
  type ColumnDef,
  type TabStripItem,
} from '@hollis-labs/sysop-ui'
import { useApi } from '../api/context'
import type { MCPServerInfo, MCPToolInfo } from '../api/client'

type TabKey = 'servers' | 'tools'
type DetailView =
  | { kind: 'server'; item: MCPServerInfo }
  | { kind: 'tool'; item: MCPToolInfo }

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="truncate text-text-subtle">{label}</dt>
      <dd className="break-words text-text-soft">{children}</dd>
    </div>
  )
}

function CopyValue({ value, label }: { value: string | number; label?: string }) {
  const text = String(value)
  return <CopyableId id={text} label={label ?? text} />
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
    cell: (s) =>
      s.has_token ? (
        <Pill tone="success">set</Pill>
      ) : (
        <span className="text-[11px] text-text-subtle">—</span>
      ),
    sortValue: (s) => (s.has_token ? 1 : 0),
  },
  {
    key: 'enabled',
    header: 'State',
    cell: (s) => (
      <Pill tone={s.enabled ? 'success' : 'neutral'}>
        {s.enabled ? 'enabled' : 'disabled'}
      </Pill>
    ),
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
  const [totalToolCalls, setTotalToolCalls] = useState<number | null>(null)
  const [detailView, setDetailView] = useState<DetailView | null>(null)
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
        setTotalToolCalls(toolsInfo.total_calls ?? null)
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

  const tabs: TabStripItem<TabKey>[] = [
    {
      key: 'servers',
      label: 'Servers',
      icon: <Plug className="h-3.5 w-3.5" />,
      count: serverList.length,
    },
    {
      key: 'tools',
      label: 'Tools',
      icon: <Wrench className="h-3.5 w-3.5" />,
      count: toolList.length,
    },
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
          { label: 'Calls', value: tools ? (totalToolCalls ?? totalCalls) : '...' },
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
    <>
      <ListPageLayout
        header={null}
        scrollRef={scrollRef}
        tabs={
          <TabStrip
            tabs={tabs}
            value={tab}
            onChange={setTab}
            actions={
              <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
                <RefreshCw className={cn('h-3.5 w-3.5', loading && 'animate-spin')} />
                Refresh
              </Button>
            }
          />
        }
        summary={<SummaryCards cards={summaryCards} />}
        filters={
          <p className="shrink-0 border-b border-border-strong bg-bg px-4 py-1.5 text-[11px] text-text-subtle">
            {tab === 'servers'
              ? 'Upstream MCP servers from the catalog (mcp-servers/*.yaml). Live connection status is daemon-only — pending CW-20260517-0047.'
              : 'Tool usage aggregated from the MCP proxy ring buffer.'}
          </p>
        }
      >
        {tab === 'servers' ? (
          <DataTable
            items={serverList}
            columns={serverColumns}
            getRowId={(s) => s.id}
            initialSort={{ key: 'id', dir: 'asc' }}
            onRowOpen={(_, item) => setDetailView({ kind: 'server', item })}
            rowAriaLabel={(s) => `Open MCP server ${s.id}`}
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
            onRowOpen={(_, item) => setDetailView({ kind: 'tool', item })}
            rowAriaLabel={(t) => `Open MCP tool ${t.name}`}
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
      </ListPageLayout>
      <MCPDetailDialog detail={detailView} onClose={() => setDetailView(null)} />
    </>
  )
}

function MCPDetailDialog({
  detail,
  onClose,
}: {
  detail: DetailView | null
  onClose: () => void
}) {
  const server = detail?.kind === 'server' ? detail.item : null
  const tool = detail?.kind === 'tool' ? detail.item : null
  return (
    <DetailDialog
      open={detail !== null}
      onClose={onClose}
      title={server ? `Server ${server.id}` : tool ? `Tool ${tool.name}` : ''}
      badge={
        server ? (
          <Pill tone={server.enabled ? 'success' : 'neutral'}>
            {server.enabled ? 'enabled' : 'disabled'}
          </Pill>
        ) : tool ? (
          <Pill tone={tool.errors > 0 ? 'warning' : 'success'}>
            {tool.errors > 0 ? 'errors' : 'ok'}
          </Pill>
        ) : null
      }
      meta={
        server ? (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span className="inline-flex items-center gap-1">
              <span className="text-text-subtle/70">server</span>
              <CopyValue value={server.id} />
            </span>
            <span>{server.transport}</span>
          </div>
        ) : tool ? (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span className="inline-flex items-center gap-1">
              <span className="text-text-subtle/70">tool</span>
              <CopyValue value={tool.name} />
            </span>
            <span className="inline-flex items-center gap-1">
              <span className="text-text-subtle/70">server</span>
              <CopyValue value={tool.server} />
            </span>
          </div>
        ) : null
      }
    >
      {server && (
        <>
          <DetailSection title="Server">
            <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <Field label="ID"><CopyValue value={server.id} /></Field>
              <Field label="Transport"><CopyValue value={server.transport} /></Field>
              <Field label="Command">
                {server.command ? <CopyValue value={server.command} /> : '—'}
              </Field>
              <Field label="URL">{server.url ? <CopyValue value={server.url} /> : '—'}</Field>
              <Field label="Args">
                {server.args?.length ? server.args.map((arg) => <CopyValue key={arg} value={arg} />) : '—'}
              </Field>
              <Field label="Token">{server.has_token ? 'set' : '—'}</Field>
              <Field label="Enabled">{server.enabled ? 'enabled' : 'disabled'}</Field>
            </dl>
          </DetailSection>
          <DetailSection title="Scopes">
            {server.scopes?.length ? (
              <div className="flex flex-wrap gap-2">
                {server.scopes.map((scope) => <CopyValue key={scope} value={scope} />)}
              </div>
            ) : (
              <p className="text-[12px] text-text-subtle">No scopes configured.</p>
            )}
          </DetailSection>
          <DetailSection title="Environment">
            {server.env_keys?.length ? (
              <div className="flex flex-wrap gap-2">
                {server.env_keys.map((key) => <CopyValue key={key} value={key} />)}
              </div>
            ) : (
              <p className="text-[12px] text-text-subtle">No environment keys configured.</p>
            )}
          </DetailSection>
          <DetailSection title="Tags">
            {server.tags?.length ? (
              <div className="flex flex-wrap gap-2">
                {server.tags.map((tag) => <CopyValue key={tag} value={tag} />)}
              </div>
            ) : (
              <p className="text-[12px] text-text-subtle">No tags configured.</p>
            )}
          </DetailSection>
        </>
      )}

      {tool && (
        <>
          <DetailSection title="Tool">
            <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <Field label="Name"><CopyValue value={tool.name} /></Field>
              <Field label="Server"><CopyValue value={tool.server} /></Field>
              <Field label="Calls">{tool.calls}</Field>
              <Field label="Errors">{tool.errors}</Field>
              <Field label="Success">{tool.success_pct}%</Field>
              <Field label="Average">{tool.avg_ms}ms</Field>
              <Field label="p95">{tool.p95_ms}ms</Field>
              <Field label="Last seen">
                {tool.last_seen ? (
                  <>
                    {formatRelativeTime(tool.last_seen)} <CopyValue value={tool.last_seen} />
                  </>
                ) : (
                  '—'
                )}
              </Field>
            </dl>
          </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}
