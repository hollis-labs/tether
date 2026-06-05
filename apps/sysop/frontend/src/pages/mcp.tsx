import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Plus, Plug, RefreshCw, Trash2, Wrench } from 'lucide-react'
import {
  Button,
  CopyableId,
  DetailDialog,
  DetailSection,
  EmptyState,
  Pill,
  SummaryCards,
  cn,
  formatRelativeTime,
} from '@hollis-labs/sysop-ui/ui'
import { DataTable, type ColumnDef } from '@hollis-labs/sysop-ui/data'
import { ListPageLayout, TabStrip, type TabStripItem } from '@hollis-labs/sysop-ui/layout'
import { useApi } from '../api/context'
import type { MCPServerInfo, MCPServerSaveRequest, MCPToolInfo, SettingsMCPInfo } from '../api/client'

type TabKey = 'servers' | 'tools'
type DetailView =
  | { kind: 'server'; item: MCPServerInfo }
  | { kind: 'tool'; item: MCPToolInfo }

interface ServerFormState {
  id: string
  transport: string
  command: string
  args: string
  url: string
  token: string
  env: string
  scopes: string
  tags: string
  enabled: boolean
}

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

function emptyServerForm(): ServerFormState {
  return {
    id: '',
    transport: 'stdio',
    command: '',
    args: '',
    url: '',
    token: '',
    env: '',
    scopes: '',
    tags: '',
    enabled: true,
  }
}

function serverToForm(server: MCPServerInfo): ServerFormState {
  return {
    id: server.id,
    transport: server.transport || 'stdio',
    command: server.command ?? '',
    args: server.args?.join('\n') ?? '',
    url: server.url ?? '',
    token: server.has_token ? '__PRESERVE__' : '',
    env: server.env_keys?.map((key) => `${key}=__PRESERVE__`).join('\n') ?? '',
    scopes: server.scopes?.join('\n') ?? '',
    tags: server.tags?.join('\n') ?? '',
    enabled: server.enabled,
  }
}

function splitList(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean)
}

function formToRequest(form: ServerFormState): MCPServerSaveRequest {
  return {
    id: form.id.trim(),
    transport: form.transport,
    command: form.transport === 'stdio' ? form.command.trim() : undefined,
    args: form.transport === 'stdio' ? splitList(form.args) : undefined,
    url: form.transport === 'sse' ? form.url.trim() : undefined,
    token: form.token.trim() || undefined,
    env: parseEnvLines(form.env),
    scopes: splitList(form.scopes),
    tags: splitList(form.tags),
    enabled: form.enabled,
  }
}

function parseEnvLines(raw: string): Record<string, string> {
  const env: Record<string, string> = {}
  for (const line of raw.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed) continue
    const idx = trimmed.indexOf('=')
    if (idx <= 0) continue
    const key = trimmed.slice(0, idx).trim()
    if (!key) continue
    env[key] = trimmed.slice(idx + 1).trim()
  }
  return env
}

function visibilityLabel(visibility: string): string {
  switch (visibility) {
    case 'disabled':
      return 'disabled'
    case 'launch_allowlist':
      return 'launch allowlist'
    case 'project_allowlist':
      return 'project allowlist'
    default:
      return 'catalog available'
  }
}

function visibilityTone(
  visibility: string,
): 'success' | 'warning' | 'neutral' {
  switch (visibility) {
    case 'disabled':
      return 'neutral'
    case 'launch_allowlist':
      return 'success'
    case 'project_allowlist':
      return 'warning'
    default:
      return 'neutral'
  }
}

function toolSourceLabel(tool: MCPToolInfo): string {
  if (tool.live) {
    if (tool.server_status === 'failed') return 'failed'
    return 'live'
  }
  return 'usage only'
}

function toolSourceTone(tool: MCPToolInfo): 'success' | 'warning' | 'neutral' {
  if (tool.live) {
    if (tool.server_status === 'failed') return 'warning'
    return 'success'
  }
  return 'neutral'
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
    key: 'visibility',
    header: 'Visibility',
    cell: (s) => <Pill tone={visibilityTone(s.visibility)}>{visibilityLabel(s.visibility)}</Pill>,
    sortValue: (s) => s.visibility,
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
      <div className="flex flex-col gap-0.5">
        <Pill tone={s.enabled ? (s.server_status === 'failed' ? 'danger' : 'success') : 'neutral'}>
          {s.server_status === 'failed' ? 'failed' : s.enabled ? 'enabled' : 'disabled'}
        </Pill>
        {s.server_status === 'failed' && s.server_error && (
          <span className="max-w-[16rem] truncate text-[10px] text-status-blocked" title={s.server_error}>
            {s.server_error}
          </span>
        )}
      </div>
    ),
    sortValue: (s) => (s.server_status === 'failed' ? -1 : s.enabled ? 1 : 0),
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
    key: 'source',
    header: 'Runtime',
    cell: (t) => <Pill tone={toolSourceTone(t)}>{toolSourceLabel(t)}</Pill>,
    sortValue: (t) => `${t.live ? 1 : 0}:${t.server_status ?? ''}:${t.source}`,
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
  const [mcpSettings, setMCPSettings] = useState<SettingsMCPInfo | null>(null)
  const [totalToolCalls, setTotalToolCalls] = useState<number | null>(null)
  const [detailView, setDetailView] = useState<DetailView | null>(null)
  const [editingServer, setEditingServer] = useState<ServerFormState | null>(null)
  const [savingServer, setSavingServer] = useState(false)
  const [reloadingDaemon, setReloadingDaemon] = useState(false)
  const [actionMessage, setActionMessage] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    Promise.all([api.getMCPServers(), api.getMCPTools(), api.getSettings()])
      .then(([serversInfo, toolsInfo, settingsInfo]) => {
        if (cancelled) return
        setServers(serversInfo.servers ?? [])
        setTools(toolsInfo.tools ?? [])
        setMCPSettings(settingsInfo.mcp)
        setTotalToolCalls(toolsInfo.total_calls ?? null)
        setError(serversInfo.error ?? toolsInfo.error ?? settingsInfo.error ?? null)
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

  function openNewServer() {
    setActionError(null)
    setEditingServer(emptyServerForm())
  }

  function openEditServer(server: MCPServerInfo) {
    setActionError(null)
    setEditingServer(serverToForm(server))
  }

  function saveServer() {
    if (!editingServer) return
    setSavingServer(true)
    setActionError(null)
    setActionMessage(null)
    api
      .saveMCPServer(formToRequest(editingServer))
      .then((info) => {
        setActionMessage(
          info.backup_path
            ? `Saved MCP server ${editingServer.id.trim()}. Backup: ${info.backup_path}.`
            : `Saved MCP server ${editingServer.id.trim()}.`,
        )
        setEditingServer(null)
        load()
      })
      .catch((err: unknown) => setActionError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSavingServer(false))
  }

  function deleteServer(server: MCPServerInfo) {
    if (!window.confirm(`Delete MCP server ${server.id}? A timestamped backup will be kept beside the YAML file.`)) return
    setActionError(null)
    setActionMessage(null)
    api
      .deleteMCPServer(server.id)
      .then((info) => {
        setActionMessage(
          info.backup_path
            ? `Deleted MCP server ${server.id}. Backup: ${info.backup_path}.`
            : `Deleted MCP server ${server.id}.`,
        )
        setDetailView(null)
        load()
      })
      .catch((err: unknown) => setActionError(err instanceof Error ? err.message : String(err)))
  }

  function toggleServer(server: MCPServerInfo) {
    setActionError(null)
    setActionMessage(null)
    api
      .toggleMCPServer(server.id, !server.enabled)
      .then((info) => {
        const verb = !server.enabled ? 'Enabled' : 'Disabled'
        setActionMessage(
          info.backup_path
            ? `${verb} MCP server ${server.id}. Backup: ${info.backup_path}.`
            : `${verb} MCP server ${server.id}.`,
        )
        setDetailView(null)
        load()
      })
      .catch((err: unknown) => setActionError(err instanceof Error ? err.message : String(err)))
  }

  function reloadDaemon() {
    if (!window.confirm('Reload tether-daemon-service with Cerberus?')) return
    setReloadingDaemon(true)
    setActionError(null)
    setActionMessage(null)
    api
      .runSystemResourceAction('tether-daemon-service', 'reload')
      .then(() => {
        setActionMessage('Reloaded tether-daemon-service.')
        load()
      })
      .catch((err: unknown) => setActionError(err instanceof Error ? err.message : String(err)))
      .finally(() => setReloadingDaemon(false))
  }

  const serverList = servers ?? []
  const toolList = tools ?? []
  const enabled = serverList.filter((s) => s.enabled).length
  const referenced = serverList.filter(
    (s) => (s.launch_refs?.length ?? 0) > 0 || (s.project_refs?.length ?? 0) > 0,
  ).length
  const liveTools = toolList.filter((t) => t.live).length
  const usageOnlyTools = toolList.filter((t) => !t.live).length
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
          { label: 'Referenced', value: servers ? referenced : '...' },
          {
            label: 'Runtime config',
            value: mcpSettings ? (mcpSettings.restart_required ? 'reload' : 'current') : '...',
            accentColor: mcpSettings?.restart_required
              ? 'var(--color-status-blocked)'
              : 'var(--color-status-done)',
          },
        ]
      : [
          { label: 'Tools', value: tools ? toolList.length : '...' },
          { label: 'Live', value: tools ? liveTools : '...', accentColor: 'var(--color-status-done)' },
          { label: 'Usage only', value: tools ? usageOnlyTools : '...' },
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
              <div className="flex items-center gap-2">
                {tab === 'servers' && (
                  <Button variant="outline" size="sm" onClick={openNewServer}>
                    <Plus className="h-3.5 w-3.5" />
                    Add
                  </Button>
                )}
                {tab === 'servers' && (
                  <Button
                    variant={mcpSettings?.restart_required ? 'default' : 'outline'}
                    size="sm"
                    onClick={reloadDaemon}
                    disabled={reloadingDaemon}
                    title="cerberus resource reload tether-daemon-service"
                  >
                    <RefreshCw className={cn('h-3.5 w-3.5', reloadingDaemon && 'animate-spin')} />
                    {reloadingDaemon ? 'Reloading' : 'Reload daemon'}
                  </Button>
                )}
                <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
                  <RefreshCw className={cn('h-3.5 w-3.5', loading && 'animate-spin')} />
                  Refresh
                </Button>
              </div>
            }
          />
        }
        summary={<SummaryCards cards={summaryCards} />}
        filters={
          <div className="shrink-0 border-b border-border-strong bg-bg px-4 py-1.5 text-[11px]">
            <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
              <span className="text-text-subtle">
                {tab === 'servers'
                  ? mcpSettings?.restart_required
                    ? 'MCP config changed after daemon start. Reload tether-daemon-service to apply server catalog changes.'
                    : 'Upstream MCP servers from the catalog (mcp-servers/*.yaml), including project and launch allowlist visibility. Edits preserve hidden token/env values.'
                  : 'Tool rows merge a live upstream MCP probe with proxy_events usage history. "usage only" means the tool was observed previously but is not available from the current probe.'}
              </span>
              {actionMessage && <span className="text-status-done">{actionMessage}</span>}
              {actionError && <span className="text-status-blocked">{actionError}</span>}
            </div>
          </div>
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
      <MCPDetailDialog
        detail={detailView}
        onClose={() => setDetailView(null)}
        onEdit={openEditServer}
        onDelete={deleteServer}
        onToggle={toggleServer}
      />
      <MCPServerEditDialog
        form={editingServer}
        saving={savingServer}
        error={actionError}
        onChange={setEditingServer}
        onClose={() => setEditingServer(null)}
        onSave={saveServer}
      />
    </>
  )
}

function MCPDetailDialog({
  detail,
  onClose,
  onEdit,
  onDelete,
  onToggle,
}: {
  detail: DetailView | null
  onClose: () => void
  onEdit: (server: MCPServerInfo) => void
  onDelete: (server: MCPServerInfo) => void
  onToggle: (server: MCPServerInfo) => void
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
            {toolSourceLabel(tool)}
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
      footer={
        server ? (
          <div className="flex justify-between gap-2">
            <Button variant="outline" size="sm" onClick={() => onDelete(server)}>
              <Trash2 className="h-3.5 w-3.5" />
              Delete
            </Button>
            <div className="flex gap-2">
              <Button variant="outline" size="sm" onClick={() => onToggle(server)}>
                {server.enabled ? 'Disable' : 'Enable'}
              </Button>
              <Button variant="default" size="sm" onClick={() => onEdit(server)}>
                Edit
              </Button>
            </div>
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
              <Field label="Visibility">
                <Pill tone={visibilityTone(server.visibility)}>{visibilityLabel(server.visibility)}</Pill>
              </Field>
              <Field label="Command">
                {server.command ? <CopyValue value={server.command} /> : '—'}
              </Field>
              <Field label="URL">{server.url ? <CopyValue value={server.url} /> : '—'}</Field>
              <Field label="Args">
                {server.args?.length ? server.args.map((arg) => <CopyValue key={arg} value={arg} />) : '—'}
              </Field>
              <Field label="Token">{server.has_token ? 'set' : '—'}</Field>
              <Field label="Enabled">{server.enabled ? 'enabled' : 'disabled'}</Field>
              {server.server_status && (
                <Field label="Runtime">
                  <Pill tone={server.server_status === 'failed' ? 'danger' : 'success'}>
                    {server.server_status}
                  </Pill>
                </Field>
              )}
              {server.server_error && (
                <Field label="Error">
                  <span className="font-mono text-[11px] text-status-blocked">{server.server_error}</span>
                </Field>
              )}
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
          <DetailSection title="Project References">
            {server.project_refs?.length ? (
              <div className="flex flex-wrap gap-2">
                {server.project_refs.map((projectID) => <CopyValue key={projectID} value={projectID} />)}
              </div>
            ) : (
              <p className="text-[12px] text-text-subtle">No project defaults explicitly select this server.</p>
            )}
          </DetailSection>
          <DetailSection title="Launch References">
            {server.launch_refs?.length ? (
              <div className="flex flex-wrap gap-2">
                {server.launch_refs.map((launchID) => <CopyValue key={launchID} value={launchID} />)}
              </div>
            ) : (
              <p className="text-[12px] text-text-subtle">No launch profiles explicitly select this server.</p>
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
              <Field label="Runtime">
                <Pill tone={toolSourceTone(tool)}>{toolSourceLabel(tool)}</Pill>
              </Field>
              <Field label="Server state">{tool.server_status || '—'}</Field>
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
              <Field label="Server error">{tool.server_error || '—'}</Field>
            </dl>
          </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}

function FormField({
  label,
  children,
}: {
  label: string
  children: ReactNode
}) {
  return (
    <label className="block">
      <span className="mb-1 block text-[11px] uppercase tracking-[.14em] text-text-subtle">
        {label}
      </span>
      {children}
    </label>
  )
}

const inputClass =
  'h-8 w-full border border-border bg-bg px-2 font-mono text-[12px] text-text outline-none focus:border-border-strong'
const textAreaClass =
  'min-h-20 w-full resize-y border border-border bg-bg px-2 py-1.5 font-mono text-[12px] text-text outline-none focus:border-border-strong'

function MCPServerEditDialog({
  form,
  saving,
  error,
  onChange,
  onClose,
  onSave,
}: {
  form: ServerFormState | null
  saving: boolean
  error: string | null
  onChange: (next: ServerFormState | null) => void
  onClose: () => void
  onSave: () => void
}) {
  function update(patch: Partial<ServerFormState>) {
    if (form) onChange({ ...form, ...patch })
  }

  return (
    <DetailDialog
      open={form !== null}
      onClose={onClose}
      title={form?.id ? `Edit ${form.id}` : 'Add MCP Server'}
      widthClassName="max-w-2xl"
      footer={
        <div className="flex items-center justify-between gap-3">
          <span className="min-w-0 text-[11px] text-status-blocked">{error}</span>
          <div className="flex shrink-0 gap-2">
            <Button variant="outline" size="sm" onClick={onClose} disabled={saving}>
              Cancel
            </Button>
            <Button variant="default" size="sm" onClick={onSave} disabled={saving || !form}>
              {saving ? 'Saving' : 'Save'}
            </Button>
          </div>
        </div>
      }
    >
      {form && (
        <DetailSection title="Server">
          <div className="grid gap-3 px-1 py-1">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_9rem_7rem]">
              <FormField label="ID">
                <input
                  className={inputClass}
                  value={form.id}
                  onChange={(event) => update({ id: event.target.value })}
                  placeholder="memory-server"
                />
              </FormField>
              <FormField label="Transport">
                <select
                  className={inputClass}
                  value={form.transport}
                  onChange={(event) => update({ transport: event.target.value })}
                >
                  <option value="stdio">stdio</option>
                  <option value="sse">sse</option>
                </select>
              </FormField>
              <FormField label="Enabled">
                <label className="flex h-8 items-center gap-2 border border-border bg-bg px-2 text-[12px] text-text-soft">
                  <input
                    type="checkbox"
                    checked={form.enabled}
                    onChange={(event) => update({ enabled: event.target.checked })}
                  />
                  Enabled
                </label>
              </FormField>
            </div>

            {form.transport === 'stdio' ? (
              <>
                <FormField label="Command">
                  <input
                    className={inputClass}
                    value={form.command}
                    onChange={(event) => update({ command: event.target.value })}
                    placeholder="/path/to/server"
                  />
                </FormField>
                <FormField label="Args">
                  <textarea
                    className={textAreaClass}
                    value={form.args}
                    onChange={(event) => update({ args: event.target.value })}
                    placeholder={'one argument per line\nor comma-separated'}
                  />
                </FormField>
              </>
            ) : (
              <FormField label="URL">
                <input
                  className={inputClass}
                  value={form.url}
                  onChange={(event) => update({ url: event.target.value })}
                  placeholder="http://127.0.0.1:9000/sse"
                />
              </FormField>
            )}

            <FormField label="Token">
              <input
                className={inputClass}
                value={form.token}
                onChange={(event) => update({ token: event.target.value })}
                placeholder="${MCP_SERVER_TOKEN}"
              />
            </FormField>
            <FormField label="Environment">
              <textarea
                className={textAreaClass}
                value={form.env}
                onChange={(event) => update({ env: event.target.value })}
                placeholder={'KEY=${ENV_VAR}\nEXISTING=__PRESERVE__'}
              />
            </FormField>
            <FormField label="Scopes">
              <textarea
                className={textAreaClass}
                value={form.scopes}
                onChange={(event) => update({ scopes: event.target.value })}
                placeholder="session.write, message.write"
              />
            </FormField>
            <FormField label="Tags">
              <textarea
                className={textAreaClass}
                value={form.tags}
                onChange={(event) => update({ tags: event.target.value })}
                placeholder="memory, automation"
              />
            </FormField>
            <p className="text-[11px] text-text-subtle">
              Use __PRESERVE__ to keep an existing hidden token or environment value. Use
              __DELETE__ to remove a token. Remove an environment line to delete it.
            </p>
          </div>
        </DetailSection>
      )}
    </DetailDialog>
  )
}
