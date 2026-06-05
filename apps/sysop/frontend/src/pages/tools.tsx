import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { BrainCircuit, Plug, RefreshCw, Wrench } from 'lucide-react'
import {
  Button,
  CopyableId,
  DetailDialog,
  DetailSection,
  EmptyState,
  Pill,
  SettingsGrid as ValueGrid,
  SettingsField as Field,
  SummaryCards,
  cn,
} from '@hollis-labs/sysop-ui/ui'
import { DataTable, type ColumnDef } from '@hollis-labs/sysop-ui/data'
import { ListPageLayout, TabStrip, type TabStripItem } from '@hollis-labs/sysop-ui/layout'
import { useApi } from '../api/context'
import type { AISettingsInfo, MCPServerInfo } from '../api/client'

type TabKey = 'servers' | 'policy'

const textAreaClass =
  'min-h-20 w-full resize-y border border-border bg-bg px-2 py-1.5 font-mono text-[12px] text-text outline-none focus:border-border-strong'

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

function splitList(raw: string): string[] {
  return raw.split(/[\n,]/).map((s) => s.trim()).filter(Boolean)
}

function visibilityLabel(s: MCPServerInfo): string {
  const n = (s.project_refs?.length ?? 0) + (s.launch_refs?.length ?? 0)
  if (!s.enabled) return 'disabled'
  if (s.visibility === 'launch_allowlist') return `launch allowlist (${n})`
  if (s.visibility === 'project_allowlist') return `project allowlist (${n})`
  return 'catalog (open)'
}

function visibilityTone(s: MCPServerInfo): 'success' | 'warning' | 'neutral' {
  if (!s.enabled) return 'neutral'
  if (s.visibility === 'launch_allowlist') return 'success'
  if (s.visibility === 'project_allowlist') return 'warning'
  return 'neutral'
}

export function ToolsPage() {
  const api = useApi()
  const [tab, setTab] = useState<TabKey>('servers')
  const [servers, setServers] = useState<MCPServerInfo[] | null>(null)
  const [aiSettings, setAISettings] = useState<AISettingsInfo | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [actionMessage, setActionMessage] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const [editingScopes, setEditingScopes] = useState<MCPServerInfo | null>(null)
  const [savingScopes, setSavingScopes] = useState(false)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    Promise.all([api.getMCPServers(), api.getAISettings()])
      .then(([serversInfo, aiInfo]) => {
        if (cancelled) return
        setServers(serversInfo.servers ?? [])
        setAISettings(aiInfo)
        setError(serversInfo.error ?? aiInfo.error ?? null)
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => { cancelled = true }
  }, [api])

  useEffect(() => load(), [load])

  function toggleServer(server: MCPServerInfo) {
    setActionError(null)
    setActionMessage(null)
    api
      .toggleMCPServer(server.id, !server.enabled)
      .then((info) => {
        const verb = !server.enabled ? 'Enabled' : 'Disabled'
        setActionMessage(
          info.backup_path
            ? `${verb} ${server.id}. Backup: ${info.backup_path}.`
            : `${verb} ${server.id}.`,
        )
        load()
      })
      .catch((err: unknown) => setActionError(err instanceof Error ? err.message : String(err)))
  }

  function saveScopes(server: MCPServerInfo, rawScopes: string) {
    setSavingScopes(true)
    setActionError(null)
    setActionMessage(null)
    api
      .saveMCPServer({
        id: server.id,
        transport: server.transport,
        command: server.command,
        args: server.args,
        url: server.url,
        scopes: splitList(rawScopes),
        tags: server.tags,
        enabled: server.enabled,
      })
      .then((info) => {
        setActionMessage(
          info.backup_path
            ? `Updated scopes for ${server.id}. Backup: ${info.backup_path}.`
            : `Updated scopes for ${server.id}.`,
        )
        setEditingScopes(null)
        load()
      })
      .catch((err: unknown) => setActionError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSavingScopes(false))
  }

  const serverList = servers ?? []
  const enabled = serverList.filter((s) => s.enabled).length
  const failed = serverList.filter((s) => s.server_status === 'failed').length

  const tabs: TabStripItem<TabKey>[] = [
    { key: 'servers', label: 'MCP Servers', icon: <Plug className="h-3.5 w-3.5" />, count: serverList.length },
    { key: 'policy', label: 'Policy', icon: <BrainCircuit className="h-3.5 w-3.5" /> },
  ]

  const summaryCards =
    tab === 'servers'
      ? [
          { label: 'Servers', value: servers ? serverList.length : '...' },
          { label: 'Enabled', value: servers ? enabled : '...', accentColor: 'var(--color-status-done)' },
          {
            label: 'Failed',
            value: servers ? failed : '...',
            accentColor: failed > 0 ? 'var(--color-status-blocked)' : undefined,
          },
        ]
      : []

  const serverColumns: ColumnDef<MCPServerInfo>[] = [
    {
      key: 'id',
      header: 'Server',
      width: 'fill',
      cell: (s) => <CopyableId id={s.id} />,
      sortValue: (s) => s.id,
    },
    {
      key: 'enabled',
      header: 'Enable',
      cell: (s) => (
        <Button
          variant="outline"
          size="sm"
          onClick={(e) => { e.stopPropagation(); toggleServer(s) }}
          title={s.enabled ? 'Click to disable' : 'Click to enable'}
        >
          <Pill tone={s.enabled ? 'success' : 'neutral'}>
            {s.enabled ? 'on' : 'off'}
          </Pill>
        </Button>
      ),
      sortValue: (s) => (s.enabled ? 1 : 0),
    },
    {
      key: 'visibility',
      header: 'Visibility',
      cell: (s) => <Pill tone={visibilityTone(s)}>{visibilityLabel(s)}</Pill>,
      sortValue: (s) => s.visibility,
    },
    {
      key: 'scopes',
      header: 'Scopes',
      width: 'fill',
      cell: (s) => (
        <div className="flex items-center gap-2">
          {s.scopes?.length ? (
            <span className="truncate font-mono text-[11px] text-text-soft">
              {s.scopes.join(', ')}
            </span>
          ) : (
            <span className="text-[11px] text-text-subtle">none</span>
          )}
          <Button
            variant="ghost"
            size="sm"
            onClick={(e) => { e.stopPropagation(); setEditingScopes(s) }}
            title="Edit scope grants"
          >
            <Wrench className="h-3.5 w-3.5" />
          </Button>
        </div>
      ),
      sortValue: (s) => s.scopes?.join(',') ?? '',
    },
    {
      key: 'status',
      header: 'Runtime',
      cell: (s) =>
        s.server_status ? (
          <div className="flex flex-col gap-0.5">
            <Pill tone={s.server_status === 'failed' ? 'danger' : 'success'}>
              {s.server_status}
            </Pill>
            {s.server_status === 'failed' && s.server_error && (
              <span className="max-w-[14rem] truncate text-[10px] text-status-blocked" title={s.server_error}>
                {s.server_error}
              </span>
            )}
          </div>
        ) : (
          <span className="text-[11px] text-text-subtle">—</span>
        ),
      sortValue: (s) => s.server_status ?? '',
    },
  ]

  const allowTools = aiSettings?.config?.policy?.allow_tools

  if (error && !servers) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not load tools data" description={error} />
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
        summary={summaryCards.length ? <SummaryCards cards={summaryCards} /> : null}
        filters={
          <div className="shrink-0 border-b border-border-strong bg-bg px-4 py-1.5 text-[11px]">
            <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
              <span className="text-text-subtle">
                {tab === 'servers'
                  ? 'Per-server enable/disable and scope grants. Visibility (project/launch allowlists) is set on the project or launch profile — shown here read-only.'
                  : 'Global AI tool policy (read-only). Per-tool policy is coming soon.'}
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
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading servers...' : 'No MCP servers'}
                description={
                  loading ? 'Reading the catalog.' : 'The catalog has no mcp-servers/*.yaml entries.'
                }
              />
            }
          />
        ) : (
          <div className="min-h-0 flex-1 overflow-y-auto">
            <div className="mx-auto max-w-2xl space-y-4 p-6">
              <PolicySection title="AI Tools (global policy)" icon={<BrainCircuit className="h-4 w-4" />}>
                <ValueGrid>
                  <Field label="allow_tools">
                    {allowTools === undefined ? (
                      <span className="text-text-subtle">unset (defaults to allowed)</span>
                    ) : (
                      <Pill tone={allowTools ? 'success' : 'warning'}>
                        {allowTools ? 'allowed' : 'blocked'}
                      </Pill>
                    )}
                  </Field>
                </ValueGrid>
                <p className="px-4 pb-3 text-[11px] text-text-subtle">
                  Edit via the{' '}
                  <span className="font-medium text-text">AI Gateway → Policy</span> page.
                  The value shown here is the global policy; per-route overrides are on the AI page.
                </p>
              </PolicySection>

              <PolicySection
                title="Per-Tool Policy"
                icon={<Wrench className="h-4 w-4" />}
                disabled
              >
                <p className="px-4 pb-3 text-[11px] text-text-subtle">
                  Per-tool enable/deny lists and deny-lists are not available in this beta.
                  Use MCP server-level enable/disable and scope grants (Servers tab) to
                  control tool access until the policy engine ships.
                </p>
                <div className="mx-4 mb-3 rounded border border-border bg-bg-subtle px-3 py-2 text-[11px] text-text-subtle">
                  Coming soon: per-tool allow/deny, deny-lists, and per-session policy overrides.
                </div>
              </PolicySection>
            </div>
          </div>
        )}
      </ListPageLayout>

      <ScopesDialog
        server={editingScopes}
        saving={savingScopes}
        error={actionError}
        onClose={() => setEditingScopes(null)}
        onSave={saveScopes}
      />
    </>
  )
}

function PolicySection({
  title,
  icon,
  children,
  disabled,
}: {
  title: string
  icon: ReactNode
  children: ReactNode
  disabled?: boolean
}) {
  return (
    <div className={cn('rounded border border-border bg-bg', disabled && 'opacity-60')}>
      <div className="flex items-center gap-2 border-b border-border px-4 py-2 text-[11px] font-medium uppercase tracking-[.14em] text-text-subtle">
        {icon}
        {title}
        {disabled && (
          <Pill tone="neutral" className="ml-auto">coming soon</Pill>
        )}
      </div>
      {children}
    </div>
  )
}

function ScopesDialog({
  server,
  saving,
  error,
  onClose,
  onSave,
}: {
  server: MCPServerInfo | null
  saving: boolean
  error: string | null
  onClose: () => void
  onSave: (server: MCPServerInfo, rawScopes: string) => void
}) {
  const [rawScopes, setRawScopes] = useState('')

  useEffect(() => {
    if (server) setRawScopes(server.scopes?.join('\n') ?? '')
  }, [server])

  return (
    <DetailDialog
      open={server !== null}
      onClose={onClose}
      title={server ? `Scopes — ${server.id}` : 'Scopes'}
      widthClassName="max-w-lg"
      footer={
        <div className="flex w-full items-center justify-between gap-3">
          <span className="min-w-0 truncate text-[11px] text-status-blocked">{error}</span>
          <div className="flex shrink-0 gap-2">
            <Button variant="outline" size="sm" onClick={onClose} disabled={saving}>
              Cancel
            </Button>
            <Button
              variant="default"
              size="sm"
              onClick={() => server && onSave(server, rawScopes)}
              disabled={saving || !server}
            >
              {saving ? 'Saving' : 'Save'}
            </Button>
          </div>
        </div>
      }
    >
      {server && (
        <DetailSection title="Scope Grants">
          <div className="grid gap-3 px-1 py-1">
            <p className="text-[11px] text-text-subtle">
              Scope gates restrict which capabilities this server can access. One scope per line
              (or comma-separated). Saving writes a backup of the existing YAML file.
            </p>
            <FormField label="Scopes">
              <textarea
                className={textAreaClass}
                value={rawScopes}
                onChange={(e) => setRawScopes(e.target.value)}
                placeholder={'session.write\nmessage.write\nread'}
              />
            </FormField>
            <p className="text-[11px] text-text-subtle">
              Visibility (project/launch allowlists) is set on the project or launch profile,
              not here.
            </p>
          </div>
        </DetailSection>
      )}
    </DetailDialog>
  )
}
