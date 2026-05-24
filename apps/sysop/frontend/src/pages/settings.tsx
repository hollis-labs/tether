import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  Boxes,
  Clock,
  Database,
  Pencil,
  Plus,
  FolderTree,
  Plug,
  RefreshCw,
  Rocket,
  Server,
  Trash2,
  Wrench,
} from 'lucide-react'
import {
  Button,
  CopyableId,
  DetailDialog,
  DetailSection,
  EmptyState,
  Pill,
  SummaryCards,
  cn,
} from '@hollis-labs/sysop-ui/ui'
import { DataTable, type ColumnDef } from '@hollis-labs/sysop-ui/data'
import { ListPageLayout, TabStrip, type TabStripItem } from '@hollis-labs/sysop-ui/layout'
import { useApi } from '../api/context'
import type {
  SettingsInfo,
  SettingsPathInfo,
  SettingsProviderInfo,
  SettingsRoadmapInfo,
} from '../api/client'

type TabKey = 'setup' | 'mcp' | 'server' | 'launches' | 'providers' | 'roadmap'

interface GlobalSettingsForm {
  shutdownTimeout: string
  permissionMode: string
  launchEngine: string
  launchSpecsRoot: string
  workspaceRoot: string
  stateDB: string
  tempRoot: string
}

interface ProviderFormState {
  id: string
  type: string
  provider: string
  runtimeKind: string
  command: string
  args: string
  adapter: string
  bootstrapMode: string
  bootstrapPrefix: string
  envMode: string
  envPassthrough: string
  envRedact: string
}

interface PathRow {
  key: string
  label: string
  info: SettingsPathInfo
}

function providerToForm(provider: SettingsProviderInfo): ProviderFormState {
  return {
    id: provider.id,
    type: provider.type || 'cli',
    provider: provider.provider || '',
    runtimeKind: provider.runtime_kind || '',
    command: provider.command || '',
    args: provider.args?.join('\n') ?? '',
    adapter: provider.adapter ?? '',
    bootstrapMode: provider.bootstrap_mode || '',
    bootstrapPrefix: provider.bootstrap_prefix ?? '',
    envMode: provider.env_mode || 'merge',
    envPassthrough: provider.env_passthrough?.join('\n') ?? '',
    envRedact: provider.env_redact?.join('\n') ?? '',
  }
}

function emptyProviderForm(): ProviderFormState {
  return {
    id: '',
    type: 'cli',
    provider: '',
    runtimeKind: 'subprocess',
    command: '',
    args: '',
    adapter: '',
    bootstrapMode: 'stdin',
    bootstrapPrefix: '',
    envMode: 'merge',
    envPassthrough: 'HOME\nPATH\nSHELL',
    envRedact: '',
  }
}

function splitLines(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean)
}

function ValueGrid({ children }: { children: ReactNode }) {
  return (
    <dl className="grid grid-cols-[10rem_minmax(0,1fr)] gap-x-4 gap-y-2 px-4 py-3 text-[12px]">
      {children}
    </dl>
  )
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="truncate text-text-subtle">{label}</dt>
      <dd className="min-w-0 break-words text-text-soft">{children}</dd>
    </div>
  )
}

function RestartPill({ required }: { required: boolean }) {
  return (
    <Pill tone={required ? 'warning' : 'success'}>
      {required ? 'reload required' : 'current'}
    </Pill>
  )
}

function WarningPanel({
  title,
  description,
}: {
  title: string
  description: string
}) {
  return (
    <div className="rounded border border-status-blocked/30 bg-status-blocked/10 px-3 py-2">
      <div className="text-[11px] font-semibold uppercase tracking-[.14em] text-status-blocked">
        {title}
      </div>
      <div className="mt-1 text-[12px] text-text-soft">{description}</div>
    </div>
  )
}

function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '0s'
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const s = Math.floor(seconds % 60)
  if (d) return `${d}d ${h}h`
  if (h) return `${h}h ${m}m`
  if (m) return `${m}m ${s}s`
  return `${s}s`
}

function settingsToForm(settings: SettingsInfo): GlobalSettingsForm {
  return {
    shutdownTimeout: settings.daemon.shutdown_timeout,
    permissionMode: settings.daemon.permission_mode || 'default',
    launchEngine: settings.daemon.launch_engine || 'catalog',
    launchSpecsRoot: settings.daemon.launch_specs_root,
    workspaceRoot: settings.paths.workspace_root.path,
    stateDB: settings.paths.state_db.path,
    tempRoot: settings.paths.temp_root.path,
  }
}

function ConfigPanel({
  title,
  icon,
  children,
}: {
  title: string
  icon: ReactNode
  children: ReactNode
}) {
  return (
    <section className="min-w-0 border-b border-border-strong bg-panel last:border-b-0">
      <div className="flex h-9 items-center gap-2 border-b border-border px-4 text-text-subtle">
        {icon}
        <h2 className="text-[11px] font-semibold uppercase tracking-[.18em] text-text-muted">
          {title}
        </h2>
      </div>
      {children}
    </section>
  )
}

function ResourceActionRow({
  label,
  resource,
  pending,
  onRun,
}: {
  label: string
  resource: string
  pending: string | null
  onRun: (resource: string, action: string) => void
}) {
  const actions = ['status', 'reload', 'apply', 'deploy']
  return (
    <div className="contents">
      <dt className="truncate text-text-subtle">{label}</dt>
      <dd className="flex min-w-0 flex-wrap items-center gap-2 text-text-soft">
        <CopyableId id={resource} label={resource} />
        <span className="mx-1 h-4 w-px bg-border" />
        {actions.map((action) => {
          const key = `${resource}:${action}`
          return (
            <Button
              key={action}
              variant="outline"
              size="sm"
              onClick={() => onRun(resource, action)}
              disabled={pending !== null}
              title={`cerberus resource ${action} ${resource}`}
            >
              {pending === key ? 'Working' : action}
            </Button>
          )
        })}
      </dd>
    </div>
  )
}

const pathColumns: ColumnDef<PathRow>[] = [
  {
    key: 'label',
    header: 'Path',
    cell: (row) => row.label,
    sortValue: (row) => row.label,
  },
  {
    key: 'value',
    header: 'Location',
    width: 'fill',
    cell: (row) =>
      row.info.path ? (
        <CopyableId id={row.info.path} label={row.info.path} />
      ) : (
        <span className="text-text-subtle">unset</span>
      ),
    sortValue: (row) => row.info.path,
  },
  {
    key: 'exists',
    header: 'State',
    cell: (row) => (
      <Pill tone={row.info.exists ? 'success' : 'warning'}>
        {row.info.exists ? 'present' : 'missing'}
      </Pill>
    ),
    sortValue: (row) => (row.info.exists ? 1 : 0),
  },
]

function providerColumns(
  onEdit: (provider: SettingsProviderInfo) => void,
  onDelete: (provider: SettingsProviderInfo) => void,
  deletingID: string | null,
): ColumnDef<SettingsProviderInfo>[] {
  return [
    {
      key: 'id',
      header: 'Provider',
      width: 'fill',
      cell: (provider) => <CopyableId id={provider.id} />,
      sortValue: (provider) => provider.id,
    },
    {
      key: 'brand',
      header: 'Brand',
      cell: (provider) => provider.provider || 'default',
      sortValue: (provider) => provider.provider,
    },
    {
      key: 'type',
      header: 'Type',
      cell: (provider) => provider.type,
      sortValue: (provider) => provider.type,
    },
    {
      key: 'runtime',
      header: 'Runtime',
      cell: (provider) => provider.runtime_kind || 'subprocess',
      sortValue: (provider) => provider.runtime_kind,
    },
    {
      key: 'env',
      header: 'Env',
      cell: (provider) => provider.env_mode || 'merge',
      sortValue: (provider) => provider.env_mode,
    },
    {
      key: 'launches',
      header: 'Launches',
      align: 'right',
      cell: (provider) => provider.referenced_launches,
      sortValue: (provider) => provider.referenced_launches,
    },
    {
      key: 'command',
      header: 'Command',
      width: 'fill',
      cell: (provider) => (
        <span className="block truncate font-mono text-[11px] text-text-soft">
          {provider.command || provider.adapter || 'auto'}
        </span>
      ),
      sortValue: (provider) => provider.command || provider.adapter || '',
    },
    {
      key: 'actions',
      header: '',
      align: 'right',
      cell: (provider) => {
        const deleting = deletingID === provider.id
        return (
          <div className="flex justify-end gap-1">
            <Button variant="ghost" size="sm" onClick={(event) => { event.stopPropagation(); onEdit(provider) }} title="Edit provider">
              <Pencil className="h-3.5 w-3.5" />
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={(event) => { event.stopPropagation(); onDelete(provider) }}
              disabled={deleting || provider.referenced_launches > 0}
              title={provider.referenced_launches > 0 ? 'Provider is used by launch profiles.' : 'Delete provider'}
            >
              <Trash2 className={cn('h-3.5 w-3.5', deleting && 'animate-pulse')} />
            </Button>
          </div>
        )
      },
      sortValue: () => 0,
    },
  ]
}

const roadmapColumns: ColumnDef<SettingsRoadmapInfo>[] = [
  {
    key: 'area',
    header: 'Area',
    cell: (item) => item.area,
    sortValue: (item) => item.area,
  },
  {
    key: 'status',
    header: 'Status',
    cell: (item) => (
      <Pill tone={item.status === 'now' ? 'success' : 'neutral'}>{item.status}</Pill>
    ),
    sortValue: (item) => item.status,
  },
  {
    key: 'next',
    header: 'Next',
    width: 'fill',
    cell: (item) => <span className="text-text-soft">{item.next}</span>,
    sortValue: (item) => item.next,
  },
]

export function SettingsPage() {
  const api = useApi()
  const [tab, setTab] = useState<TabKey>('setup')
  const [settings, setSettings] = useState<SettingsInfo | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [message, setMessage] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [systemAction, setSystemAction] = useState<string | null>(null)
  const [globalForm, setGlobalForm] = useState<GlobalSettingsForm | null>(null)
  const [providerForm, setProviderForm] = useState<ProviderFormState | null>(null)
  const [deletingProviderID, setDeletingProviderID] = useState<string | null>(null)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    api
      .getSettings()
      .then((info) => {
        if (cancelled) return
        setSettings(info)
        setError(info.error ?? null)
        setMessage(null)
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

  const pathRows = useMemo<PathRow[]>(() => {
    const paths = settings?.paths
    if (!paths) return []
    return [
      { key: 'catalog_root', label: 'Catalog root', info: paths.catalog_root },
      { key: 'state_db', label: 'State DB', info: paths.state_db },
      { key: 'workspace_root', label: 'Workspace root', info: paths.workspace_root },
      { key: 'temp_root', label: 'Temp root', info: paths.temp_root },
      { key: 'launch_specs_root', label: 'Launch specs', info: paths.launch_specs_root },
      { key: 'projects_root', label: 'Projects', info: paths.projects_root },
      { key: 'agents_root', label: 'Agents', info: paths.agents_root },
      { key: 'providers_root', label: 'Providers', info: paths.providers_root },
      { key: 'launches_root', label: 'Launches', info: paths.launches_root },
      { key: 'boot_root', label: 'Boot', info: paths.boot_root },
      { key: 'mcp_servers_root', label: 'MCP servers', info: paths.mcp_servers_root },
    ]
  }, [settings?.paths])

  const missingPaths = pathRows.filter((row) => row.info.path && !row.info.exists).length
  const tabs: TabStripItem<TabKey>[] = [
    { key: 'setup', label: 'Setup', icon: <FolderTree className="h-3.5 w-3.5" /> },
    { key: 'mcp', label: 'MCP', icon: <Plug className="h-3.5 w-3.5" /> },
    { key: 'server', label: 'System', icon: <Server className="h-3.5 w-3.5" /> },
    { key: 'launches', label: 'Launches', icon: <Rocket className="h-3.5 w-3.5" /> },
    {
      key: 'providers',
      label: 'Providers',
      icon: <Wrench className="h-3.5 w-3.5" />,
      count: settings?.providers.length,
    },
    { key: 'roadmap', label: 'Roadmap', icon: <Boxes className="h-3.5 w-3.5" /> },
  ]

  const summaryCards = [
    { label: 'Catalog', value: settings?.catalog.version || '...' },
    { label: 'Providers', value: settings?.catalog.providers ?? '...' },
    { label: 'Launches', value: settings?.catalog.launches ?? '...' },
    {
      label: 'Runtime drift',
      value: settings ? Number(settings.daemon.restart_required) + Number(settings.mcp.restart_required) : '...',
      accentColor:
        settings && (settings.daemon.restart_required || settings.mcp.restart_required)
          ? 'var(--color-status-blocked)'
          : 'var(--color-status-done)',
    },
    {
      label: 'Missing paths',
      value: settings ? missingPaths : '...',
      accentColor: missingPaths > 0 ? 'var(--color-status-blocked)' : 'var(--color-status-done)',
    },
  ]

  function saveGlobalSettings() {
    if (!globalForm) return
    setSaving(true)
    setError(null)
    setMessage(null)
    api
      .saveGlobalSettings({
        shutdown_timeout: globalForm.shutdownTimeout,
        permission_mode: globalForm.permissionMode,
        launch_engine: globalForm.launchEngine,
        launch_specs_root: globalForm.launchSpecsRoot,
        workspace_root: globalForm.workspaceRoot,
        state_db: globalForm.stateDB,
        temp_root: globalForm.tempRoot,
      })
      .then((info) => {
        setMessage(
          info.backup_path
            ? `Saved global settings. Backup: ${info.backup_path}. Restart/reload may be required.`
            : 'Saved global settings. Restart/reload may be required.',
        )
        setGlobalForm(null)
        load()
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSaving(false))
  }

  function runSystemAction(resource: string, action: string) {
    const key = `${resource}:${action}`
    const disruptive = action !== 'status'
    if (disruptive && !window.confirm(`Run cerberus resource ${action} ${resource}?`)) return
    setSystemAction(key)
    setError(null)
    setMessage(null)
    api
      .runSystemResourceAction(resource, action)
      .then((info) => {
        const suffix = info.status === 'scheduled' ? 'scheduled' : 'completed'
        setMessage(`${resource} ${action} ${suffix}.`)
        if (info.output) console.info(info.output)
        if (resource !== 'tether-dev' || action === 'status') load()
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSystemAction(null))
  }

  function saveProvider() {
    if (!providerForm) return
    setSaving(true)
    setError(null)
    setMessage(null)
    api
      .saveProvider({
        id: providerForm.id,
        type: providerForm.type,
        provider: providerForm.provider,
        runtime_kind: providerForm.runtimeKind,
        command: providerForm.command,
        args: splitLines(providerForm.args),
        adapter: providerForm.adapter,
        bootstrap_mode: providerForm.bootstrapMode,
        bootstrap_prefix: providerForm.bootstrapPrefix,
        env_mode: providerForm.envMode,
        env_passthrough: splitLines(providerForm.envPassthrough),
        env_redact: splitLines(providerForm.envRedact),
      })
      .then((info) => {
        setMessage(
          info.backup_path
            ? `Saved provider ${providerForm.id}. Backup: ${info.backup_path}. Reload may be required.`
            : `Saved provider ${providerForm.id}. Reload may be required.`,
        )
        setProviderForm(null)
        load()
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSaving(false))
  }

  function deleteProvider(provider: SettingsProviderInfo) {
    if (provider.referenced_launches > 0) return
    if (!window.confirm(`Delete provider ${provider.id}? A timestamped backup will be kept beside the YAML file.`)) return
    setDeletingProviderID(provider.id)
    setError(null)
    setMessage(null)
    api
      .deleteProvider(provider.id)
      .then((info) => {
        setMessage(
          info.backup_path
            ? `Deleted provider ${provider.id}. Backup: ${info.backup_path}. Reload may be required.`
            : `Deleted provider ${provider.id}. Reload may be required.`,
        )
        load()
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setDeletingProviderID(null))
  }

  if (error && !settings) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not load settings" description={error} />
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
              {tab === 'server' && settings && (
                <Button variant="outline" size="sm" onClick={() => setGlobalForm(settingsToForm(settings))}>
                  Edit
                </Button>
              )}
              {tab === 'providers' && (
                <Button variant="outline" size="sm" onClick={() => setProviderForm(emptyProviderForm())}>
                  <Plus className="h-3.5 w-3.5" />
                  Add
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
            <span className="text-text-subtle">{settings?.server.catalog_root ?? 'Loading settings...'}</span>
            {message && <span className="text-status-done">{message}</span>}
            {error && settings && <span className="text-status-blocked">{error}</span>}
          </div>
        </div>
      }
    >
      {tab === 'setup' && (
        <DataTable
          items={pathRows}
          columns={pathColumns}
          getRowId={(row) => row.key}
          initialSort={{ key: 'label', dir: 'asc' }}
          scrollRootRef={scrollRef}
          emptyState={
            <EmptyState
              variant="empty"
              title={loading ? 'Loading setup...' : 'No setup data'}
              description="The settings endpoint returned no path rows."
            />
          }
        />
      )}

      {tab === 'mcp' && settings && (
        <ConfigPanel title="MCP Setup" icon={<Plug className="h-3.5 w-3.5" />}>
          <div className="px-4 py-3">
            {settings.mcp.restart_required ? (
              <WarningPanel
                title="Runtime Drift"
                description="The MCP catalog changed after the daemon started. Reload tether-daemon-service before assuming runtime tool availability matches the catalog shown here."
              />
            ) : (
              <WarningPanel
                title="Runtime Current"
                description="The MCP catalog on disk is not newer than the daemon runtime reference."
              />
            )}
          </div>
          <ValueGrid>
            <Field label="Config surface">{settings.mcp.config_surface}</Field>
            <Field label="Root">
              <CopyableId id={settings.mcp.root} label={settings.mcp.root} />
            </Field>
            <Field label="Root state">
              <Pill tone={settings.mcp.root_exists ? 'success' : 'warning'}>
                {settings.mcp.root_exists ? 'present' : 'missing'}
              </Pill>
            </Field>
            <Field label="Runtime config"><RestartPill required={settings.mcp.restart_required} /></Field>
            {settings.mcp.config_modified_at && (
              <Field label="Config modified">
                <CopyableId id={settings.mcp.config_modified_at} label={settings.mcp.config_modified_at} />
              </Field>
            )}
            <Field label="Servers">{settings.mcp.servers}</Field>
            <Field label="Enabled">{settings.mcp.enabled}</Field>
            <Field label="Token refs">{settings.mcp.with_tokens}</Field>
            <Field label="Transports">
              {settings.mcp.transports.length ? settings.mcp.transports.join(', ') : 'none'}
            </Field>
          </ValueGrid>
        </ConfigPanel>
      )}

      {tab === 'server' && settings && (
        <>
          <ConfigPanel title="Frontend Server" icon={<Server className="h-3.5 w-3.5" />}>
            <ValueGrid>
              <Field label="HTTP addr">{settings.server.http_addr}</Field>
              <Field label="PID">{settings.server.pid}</Field>
              <Field label="Uptime">{formatUptime(settings.server.uptime_sec)}</Field>
              <Field label="Started">
                <CopyableId id={settings.server.started_at} label={settings.server.started_at} />
              </Field>
              <Field label="Catalog root">
                <CopyableId id={settings.server.catalog_root} label={settings.server.catalog_root} />
              </Field>
            </ValueGrid>
          </ConfigPanel>
          <ConfigPanel title="Tether Daemon" icon={<Database className="h-3.5 w-3.5" />}>
            <div className="px-4 py-3">
              {settings.daemon.restart_required ? (
                <WarningPanel
                  title="Daemon Config Drift"
                  description="Catalog-backed daemon settings changed after the daemon reference timestamp. Reload or restart tether-daemon-service before assuming runtime behavior matches these values."
                />
              ) : (
                <WarningPanel
                  title="Daemon Config Current"
                  description="The daemon config on disk is not newer than the running daemon reference."
                />
              )}
            </div>
            <ValueGrid>
              <Field label="Listen addr">{settings.daemon.listen_addr}</Field>
              <Field label="Listen kind">{settings.daemon.listen_kind || 'unset'}</Field>
              <Field label="Endpoint">
                {settings.daemon.listen_endpoint ? (
                  <CopyableId
                    id={settings.daemon.listen_endpoint}
                    label={settings.daemon.listen_endpoint}
                  />
                ) : (
                  'unset'
                )}
              </Field>
              <Field label="Socket">
                <Pill tone={settings.daemon.socket_exists ? 'success' : 'warning'}>
                  {settings.daemon.socket_exists ? 'present' : 'missing'}
                </Pill>
              </Field>
              <Field label="PID file">
                <CopyableId id={settings.daemon.pid_file} label={settings.daemon.pid_file} />
              </Field>
              <Field label="PID file state">
                <Pill tone={settings.daemon.pid_file_exists ? 'success' : 'warning'}>
                  {settings.daemon.pid_file_exists ? 'present' : 'missing'}
                </Pill>
              </Field>
              <Field label="Daemon PID">{settings.daemon.pid || 'unknown'}</Field>
              <Field label="PID running">
                <Pill tone={settings.daemon.pid_running ? 'success' : 'warning'}>
                  {settings.daemon.pid_running ? 'running' : 'not confirmed'}
                </Pill>
              </Field>
              <Field label="Runtime config"><RestartPill required={settings.daemon.restart_required} /></Field>
              {settings.daemon.config_modified_at && (
                <Field label="Config modified">
                  <CopyableId id={settings.daemon.config_modified_at} label={settings.daemon.config_modified_at} />
                </Field>
              )}
              <Field label="Shutdown">{settings.daemon.shutdown_timeout}</Field>
              <Field label="Permissions">{settings.daemon.permission_mode}</Field>
              <Field label="Launch engine">{settings.daemon.launch_engine || 'catalog'}</Field>
              <Field label="Launch specs">
                {settings.daemon.launch_specs_root ? (
                  <CopyableId
                    id={settings.daemon.launch_specs_root}
                    label={settings.daemon.launch_specs_root}
                  />
                ) : (
                  'default'
                )}
              </Field>
            </ValueGrid>
          </ConfigPanel>
          <ConfigPanel title="Management Commands" icon={<Clock className="h-3.5 w-3.5" />}>
            <ValueGrid>
              <ResourceActionRow
                label="Frontend"
                resource="tether-dev"
                pending={systemAction}
                onRun={runSystemAction}
              />
              <ResourceActionRow
                label="Daemon"
                resource="tether-daemon-service"
                pending={systemAction}
                onRun={runSystemAction}
              />
            </ValueGrid>
          </ConfigPanel>
        </>
      )}

      {tab === 'launches' && settings && (
        <ConfigPanel title="Launch And Session Config" icon={<Rocket className="h-3.5 w-3.5" />}>
          <ValueGrid>
            <Field label="Launch profiles">{settings.launches.total}</Field>
            <Field label="Worktree launches">{settings.launches.with_worktree}</Field>
            <Field label="MCP-scoped launches">{settings.launches.with_mcp}</Field>
            <Field label="Injected files">{settings.launches.with_injection}</Field>
            <Field label="Env overrides">{settings.launches.with_env}</Field>
            <Field label="Sessions">{settings.sessions.total}</Field>
            <Field label="Running sessions">{settings.sessions.running}</Field>
            <Field label="Ended sessions">{settings.sessions.ended}</Field>
            {settings.sessions.error && <Field label="Session error">{settings.sessions.error}</Field>}
          </ValueGrid>
        </ConfigPanel>
      )}

      {tab === 'providers' && (
        <DataTable
          items={settings?.providers ?? []}
          columns={providerColumns(
            (provider) => setProviderForm(providerToForm(provider)),
            deleteProvider,
            deletingProviderID,
          )}
          getRowId={(provider) => provider.id}
          initialSort={{ key: 'id', dir: 'asc' }}
          scrollRootRef={scrollRef}
          emptyState={
            <EmptyState
              variant="empty"
              title={loading ? 'Loading providers...' : 'No providers'}
              description="The catalog has no provider entries."
            />
          }
        />
      )}

      {tab === 'roadmap' && (
        <DataTable
          items={settings?.roadmap ?? []}
          columns={roadmapColumns}
          getRowId={(item) => item.area}
          initialSort={{ key: 'status', dir: 'asc' }}
          scrollRootRef={scrollRef}
          emptyState={
            <EmptyState
              variant="empty"
              title={loading ? 'Loading roadmap...' : 'No roadmap'}
              description="The settings endpoint returned no roadmap rows."
            />
          }
        />
      )}
    </ListPageLayout>
    <GlobalSettingsDialog
      form={globalForm}
      saving={saving}
      error={error}
      onChange={setGlobalForm}
      onClose={() => setGlobalForm(null)}
      onSave={saveGlobalSettings}
    />
    <ProviderDialog
      form={providerForm}
      saving={saving}
      error={error}
      onChange={setProviderForm}
      onClose={() => setProviderForm(null)}
      onSave={saveProvider}
    />
    </>
  )
}

const inputClass =
  'h-8 w-full border border-border bg-bg px-2 font-mono text-[12px] text-text outline-none focus:border-border-strong'

function FormField({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="block">
      <span className="mb-1 block text-[11px] uppercase tracking-[.14em] text-text-subtle">
        {label}
      </span>
      {children}
    </label>
  )
}

function GlobalSettingsDialog({
  form,
  saving,
  error,
  onChange,
  onClose,
  onSave,
}: {
  form: GlobalSettingsForm | null
  saving: boolean
  error: string | null
  onChange: (next: GlobalSettingsForm | null) => void
  onClose: () => void
  onSave: () => void
}) {
  function update(patch: Partial<GlobalSettingsForm>) {
    if (form) onChange({ ...form, ...patch })
  }
  return (
    <DetailDialog
      open={form !== null}
      onClose={onClose}
      title="Global Settings"
      widthClassName="max-w-2xl"
      footer={
        <div className="flex w-full items-center justify-between gap-3">
          <span className="min-w-0 truncate text-[11px] text-status-blocked">{error}</span>
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
        <>
          <DetailSection title="Safety">
            <div className="px-1 py-1 text-[12px] text-text-soft">
              Saving writes `global.yaml` and keeps a timestamped backup of the previous file when one exists. This dialog does not offer a diff preview yet.
            </div>
          </DetailSection>
          <DetailSection title="Daemon">
            <div className="grid gap-3 px-1 py-1">
              <FormField label="Shutdown timeout">
                <input
                  className={inputClass}
                  value={form.shutdownTimeout}
                  onChange={(e) => update({ shutdownTimeout: e.target.value })}
                  placeholder="10s"
                />
              </FormField>
            </div>
          </DetailSection>
          <DetailSection title="Launch Policy">
            <div className="grid gap-3 px-1 py-1">
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <FormField label="Permission mode">
                  <select
                    className={inputClass}
                    value={form.permissionMode}
                    onChange={(e) => update({ permissionMode: e.target.value })}
                  >
                    <option value="default">default</option>
                    <option value="bypass">bypass</option>
                  </select>
                </FormField>
                <FormField label="Launch engine">
                  <select
                    className={inputClass}
                    value={form.launchEngine}
                    onChange={(e) => update({ launchEngine: e.target.value })}
                  >
                    <option value="catalog">catalog</option>
                    <option value="spec">spec</option>
                  </select>
                </FormField>
              </div>
              <FormField label="Launch specs root">
                <input
                  className={inputClass}
                  value={form.launchSpecsRoot}
                  onChange={(e) => update({ launchSpecsRoot: e.target.value })}
                />
              </FormField>
            </div>
          </DetailSection>
          <DetailSection title="Storage">
            <div className="grid gap-3 px-1 py-1">
              <FormField label="State DB">
                <input className={inputClass} value={form.stateDB} onChange={(e) => update({ stateDB: e.target.value })} />
              </FormField>
              <FormField label="Workspace root">
                <input className={inputClass} value={form.workspaceRoot} onChange={(e) => update({ workspaceRoot: e.target.value })} />
              </FormField>
              <FormField label="Temp root">
                <input className={inputClass} value={form.tempRoot} onChange={(e) => update({ tempRoot: e.target.value })} />
              </FormField>
            </div>
          </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}

function ProviderDialog({
  form,
  saving,
  error,
  onChange,
  onClose,
  onSave,
}: {
  form: ProviderFormState | null
  saving: boolean
  error: string | null
  onChange: (next: ProviderFormState | null) => void
  onClose: () => void
  onSave: () => void
}) {
  function update(patch: Partial<ProviderFormState>) {
    if (form) onChange({ ...form, ...patch })
  }
  return (
    <DetailDialog
      open={form !== null}
      onClose={onClose}
      title={form?.id ? `Provider ${form.id}` : 'Provider'}
      widthClassName="max-w-2xl"
      footer={
        <div className="flex w-full items-center justify-between gap-3">
          <span className="min-w-0 truncate text-[11px] text-status-blocked">{error}</span>
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
        <>
          <DetailSection title="Safety">
            <div className="px-1 py-1 text-[12px] text-text-soft">
              Saving rewrites the provider YAML and keeps a timestamped backup of the previous file when one exists. This dialog does not offer a diff preview yet.
            </div>
          </DetailSection>
          <DetailSection title="Identity">
            <div className="grid gap-3 px-1 py-1">
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <FormField label="ID">
                  <input className={inputClass} value={form.id} onChange={(e) => update({ id: e.target.value })} />
                </FormField>
                <FormField label="Type">
                  <select className={inputClass} value={form.type} onChange={(e) => update({ type: e.target.value })}>
                    <option value="cli">cli</option>
                    <option value="cli-goprovider">cli-goprovider</option>
                    <option value="api">api</option>
                  </select>
                </FormField>
              </div>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <FormField label="Provider brand">
                  <input className={inputClass} value={form.provider} onChange={(e) => update({ provider: e.target.value })} placeholder="claude, codex, opencode" />
                </FormField>
                <FormField label="Runtime kind">
                  <select className={inputClass} value={form.runtimeKind} onChange={(e) => update({ runtimeKind: e.target.value })}>
                    <option value="">default</option>
                    <option value="subprocess">subprocess</option>
                    <option value="pty">pty</option>
                    <option value="streaming-stdio">streaming-stdio</option>
                    <option value="jsonrpc-stdio">jsonrpc-stdio</option>
                    <option value="api">api</option>
                  </select>
                </FormField>
              </div>
            </div>
          </DetailSection>
          <DetailSection title="Command">
            <div className="grid gap-3 px-1 py-1">
              <FormField label="Command">
                <input className={inputClass} value={form.command} onChange={(e) => update({ command: e.target.value })} placeholder="binary path or empty for adapter auto-detect" />
              </FormField>
              <FormField label="Args">
                <textarea className={`${inputClass} h-20 resize-y py-2`} value={form.args} onChange={(e) => update({ args: e.target.value })} placeholder="one argument per line" />
              </FormField>
              <FormField label="Adapter">
                <select className={inputClass} value={form.adapter} onChange={(e) => update({ adapter: e.target.value })}>
                  <option value="">none</option>
                  <option value="claude">claude</option>
                  <option value="codex">codex</option>
                </select>
              </FormField>
            </div>
          </DetailSection>
          <DetailSection title="Bootstrap And Env">
            <div className="grid gap-3 px-1 py-1">
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <FormField label="Bootstrap mode">
                  <input className={inputClass} value={form.bootstrapMode} onChange={(e) => update({ bootstrapMode: e.target.value })} />
                </FormField>
                <FormField label="Env mode">
                  <select className={inputClass} value={form.envMode} onChange={(e) => update({ envMode: e.target.value })}>
                    <option value="merge">merge</option>
                    <option value="whitelist">whitelist</option>
                  </select>
                </FormField>
              </div>
              <FormField label="Prompt prefix">
                <input className={inputClass} value={form.bootstrapPrefix} onChange={(e) => update({ bootstrapPrefix: e.target.value })} />
              </FormField>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <FormField label="Env passthrough">
                  <textarea className={`${inputClass} h-24 resize-y py-2`} value={form.envPassthrough} onChange={(e) => update({ envPassthrough: e.target.value })} />
                </FormField>
                <FormField label="Env redact">
                  <textarea className={`${inputClass} h-24 resize-y py-2`} value={form.envRedact} onChange={(e) => update({ envRedact: e.target.value })} />
                </FormField>
              </div>
            </div>
          </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}
