import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { FolderKanban, Network, Pencil, Plus, RefreshCw, RotateCw, Trash2, Users } from 'lucide-react'
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
import type {
  RegistryBootstrapErrorInfo,
  RegistryBootstrapReportInfo,
  RegistryLinkInfo,
  RegistryProfileInfo,
  RegistrySaveRequest,
  RegistrySkillInfo,
} from '../api/client'

type TabKey = 'agents' | 'projects' | 'management'
type RegistryKind = 'agent' | 'project'
type StatusFilter = 'active' | 'deprecated' | '*'

interface RegistryFormState {
  kind: RegistryKind
  urn: string
  displayName: string
  title: string
  role: string
  description: string
  avatar: string
  project: string
  status: string
  healthStatus: string
  hostAddress: string
  lastUpdatedBy: string
  callbackScheme: string
  callbackTarget: string
  capabilities: string
  skills: string
  links: string
}

function emptyForm(kind: RegistryKind): RegistryFormState {
  return {
    kind,
    urn: '',
    displayName: '',
    title: '',
    role: '',
    description: '',
    avatar: '',
    project: '',
    status: 'active',
    healthStatus: '',
    hostAddress: '',
    lastUpdatedBy: 'sysop-ui',
    callbackScheme: 'file',
    callbackTarget: '',
    capabilities: '',
    skills: '',
    links: '',
  }
}

function profileToForm(profile: RegistryProfileInfo): RegistryFormState {
  return {
    kind: profile.kind === 'project' ? 'project' : 'agent',
    urn: profile.urn,
    displayName: profile.display_name,
    title: profile.title ?? '',
    role: profile.role ?? '',
    description: profile.description ?? '',
    avatar: profile.avatar ?? '',
    project: profile.project ?? '',
    status: profile.status || 'active',
    healthStatus: profile.health_status ?? '',
    hostAddress: profile.host_address ?? '',
    lastUpdatedBy: profile.last_updated_by || 'sysop-ui',
    callbackScheme: profile.callback?.scheme ?? '',
    callbackTarget: profile.callback?.target ?? '',
    capabilities: (profile.capabilities ?? []).join('\n'),
    skills: formatSkills(profile.skills),
    links: formatLinks(profile.links),
  }
}

function formatSkills(skills?: RegistrySkillInfo[]): string {
  return (skills ?? [])
    .map((skill) => [skill.name, skill.learned_at, skill.via ?? '', skill.level ?? ''].join(' | '))
    .join('\n')
}

function formatLinks(links?: RegistryLinkInfo[]): string {
  return (links ?? []).map((link) => `${link.kind} | ${link.target}`).join('\n')
}

function splitLines(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean)
}

function parseSkills(raw: string): RegistrySkillInfo[] {
  const now = new Date().toISOString()
  return raw
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line, index) => {
      const [name, learnedAt, via, level] = line.split('|').map((part) => part.trim())
      if (!name) {
        throw new Error(`skill line ${index + 1} requires a name`)
      }
      const ts = learnedAt || now
      if (Number.isNaN(Date.parse(ts))) {
        throw new Error(`skill line ${index + 1} has an invalid learned_at timestamp`)
      }
      return {
        name,
        learned_at: new Date(ts).toISOString(),
        via: via || undefined,
        level: level || undefined,
      }
    })
}

function parseLinks(raw: string): RegistryLinkInfo[] {
  return raw
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line, index) => {
      const [kind, target] = line.split('|').map((part) => part.trim())
      if (!kind || !target) {
        throw new Error(`link line ${index + 1} must be "kind | target"`)
      }
      return { kind, target }
    })
}

function buildSaveRequest(form: RegistryFormState): RegistrySaveRequest {
  return {
    kind: form.kind,
    urn: form.urn || undefined,
    display_name: form.displayName.trim(),
    title: form.title.trim() || undefined,
    role: form.role.trim() || undefined,
    description: form.description.trim() || undefined,
    avatar: form.avatar.trim() || undefined,
    project: form.project.trim() || undefined,
    status: form.status,
    health_status: form.healthStatus.trim() || undefined,
    host_address: form.hostAddress.trim() || undefined,
    last_updated_by: form.lastUpdatedBy.trim() || undefined,
    callback:
      !form.urn && form.callbackScheme.trim() && form.callbackTarget.trim()
        ? {
            scheme: form.callbackScheme.trim(),
            target: form.callbackTarget.trim(),
          }
        : undefined,
    capabilities: splitLines(form.capabilities),
    skills: parseSkills(form.skills),
    links: parseLinks(form.links),
  }
}

function registryColumns(
  onEdit: (profile: RegistryProfileInfo) => void,
  onSync: (urn: string) => void,
  onDeregister: (profile: RegistryProfileInfo) => void,
  pendingURN: string | null,
): ColumnDef<RegistryProfileInfo>[] {
  return [
    {
      key: 'display_name',
      header: 'Name',
      width: 'fill',
      cell: (row) => (
        <div className="min-w-0">
          <div className="truncate text-[12px] text-text">{row.display_name}</div>
          <div className="truncate text-[11px] text-text-subtle">{row.title || row.urn}</div>
        </div>
      ),
      sortValue: (row) => row.display_name,
    },
    {
      key: 'role',
      header: 'Role',
      cell: (row) => <span className="text-[11px] text-text-soft">{row.role || '—'}</span>,
      sortValue: (row) => row.role || '',
    },
    {
      key: 'project',
      header: 'Project',
      cell: (row) => <span className="text-[11px] text-text-soft">{row.project || '—'}</span>,
      sortValue: (row) => row.project || '',
    },
    {
      key: 'callback',
      header: 'Callback',
      width: 'fill',
      cell: (row) => (
        <span className="block truncate font-mono text-[11px] text-text-subtle">
          {row.callback?.target || 'none'}
        </span>
      ),
      sortValue: (row) => row.callback?.target || '',
    },
    {
      key: 'status',
      header: 'Status',
      cell: (row) => (
        <Pill tone={row.status === 'active' ? 'success' : 'neutral'}>{row.status}</Pill>
      ),
      sortValue: (row) => row.status,
    },
    {
      key: 'updated_at',
      header: 'Updated',
      cell: (row) => (
        <span className="text-[11px] text-text-soft">{formatRelativeTime(row.updated_at)}</span>
      ),
      sortValue: (row) => row.updated_at,
    },
    {
      key: 'actions',
      header: '',
      align: 'right',
      cell: (row) => {
        const pending = pendingURN === row.urn
        return (
          <div className="flex justify-end gap-1">
            <Button variant="ghost" size="sm" onClick={() => onEdit(row)} title="Edit registry row">
              <Pencil className="h-3.5 w-3.5" />
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => onSync(row.urn)}
              disabled={pending}
              title="Sync from callback"
            >
              <RotateCw className={cn('h-3.5 w-3.5', pending && 'animate-spin')} />
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => onDeregister(row)}
              disabled={pending || row.status === 'deprecated'}
              title="Soft-delete registry row"
            >
              <Trash2 className="h-3.5 w-3.5" />
            </Button>
          </div>
        )
      },
      sortValue: () => 0,
    },
  ]
}

export function RegistryPage() {
  const api = useApi()
  const [tab, setTab] = useState<TabKey>('agents')
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('active')
  const [query, setQuery] = useState('')
  const [agents, setAgents] = useState<RegistryProfileInfo[]>([])
  const [projects, setProjects] = useState<RegistryProfileInfo[]>([])
  const [report, setReport] = useState<RegistryBootstrapReportInfo | null>(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [pendingURN, setPendingURN] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [message, setMessage] = useState<string | null>(null)
  const [form, setForm] = useState<RegistryFormState | null>(null)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    Promise.all([api.getRegistry('agent', { status: '*' }), api.getRegistry('project', { status: '*' })])
      .then(([agentsInfo, projectsInfo]) => {
        if (cancelled) return
        setAgents(agentsInfo.rows ?? [])
        setProjects(projectsInfo.rows ?? [])
        setError(agentsInfo.error ?? projectsInfo.error ?? null)
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

  const filteredAgents = useMemo(
    () => filterRows(agents, statusFilter, query),
    [agents, statusFilter, query],
  )
  const filteredProjects = useMemo(
    () => filterRows(projects, statusFilter, query),
    [projects, statusFilter, query],
  )
  const visibleRows = tab === 'projects' ? filteredProjects : filteredAgents

  const summaryCards = [
    { label: 'Agents', value: agents.filter((row) => row.status === 'active').length },
    { label: 'Projects', value: projects.filter((row) => row.status === 'active').length },
    { label: 'Deprecated', value: [...agents, ...projects].filter((row) => row.status === 'deprecated').length },
    {
      label: 'Callbacks',
      value: [...agents, ...projects].filter((row) => Boolean(row.callback?.target)).length,
    },
  ]

  const tabs: TabStripItem<TabKey>[] = [
    { key: 'agents', label: 'Agents', icon: <Users className="h-3.5 w-3.5" />, count: filteredAgents.length },
    { key: 'projects', label: 'Projects', icon: <FolderKanban className="h-3.5 w-3.5" />, count: filteredProjects.length },
    { key: 'management', label: 'Management', icon: <Network className="h-3.5 w-3.5" /> },
  ]

  function saveForm() {
    if (!form) return
    setSaving(true)
    setError(null)
    setMessage(null)
    let body: RegistrySaveRequest
    try {
      body = buildSaveRequest(form)
    } catch (err) {
      setSaving(false)
      setError(err instanceof Error ? err.message : String(err))
      return
    }
    api
      .saveRegistry(body)
      .then(() => {
        setMessage(form.urn ? `Updated ${form.displayName}.` : `Registered ${form.displayName}.`)
        setForm(null)
        load()
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSaving(false))
  }

  function syncURN(urn: string) {
    setPendingURN(urn)
    setError(null)
    setMessage(null)
    api
      .syncRegistry(urn)
      .then((info) => {
        setMessage(info.status === 'noop' ? `No callback configured for ${urn}.` : `Synced ${urn}.`)
        load()
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setPendingURN(null))
  }

  function deregisterRow(row: RegistryProfileInfo) {
    if (!window.confirm(`Soft-delete ${row.display_name}?`)) return
    setPendingURN(row.urn)
    setError(null)
    setMessage(null)
    api
      .deregisterRegistry(row.urn)
      .then(() => {
        setMessage(`Deprecated ${row.display_name}.`)
        load()
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setPendingURN(null))
  }

  function runBootstrap(force: boolean) {
    const label = force ? 'force refresh' : 'bootstrap'
    if (force && !window.confirm('Re-run registry bootstrap with force=true?')) return
    setSaving(true)
    setError(null)
    setMessage(null)
    api
      .bootstrapRegistry(force)
      .then((nextReport) => {
        setReport(nextReport)
        setMessage(`Registry ${label} completed.`)
        load()
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSaving(false))
  }

  if (error && agents.length === 0 && projects.length === 0 && !loading) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not load registry" description={error} />
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
                {tab !== 'management' && (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setForm(emptyForm(tab === 'projects' ? 'project' : 'agent'))}
                  >
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
          <div className="shrink-0 border-b border-border-strong bg-bg px-4 py-2">
            <div className="flex flex-wrap items-center gap-2">
              <select
                className={inputClass}
                value={statusFilter}
                onChange={(e) => setStatusFilter(e.target.value as StatusFilter)}
              >
                <option value="active">active</option>
                <option value="deprecated">deprecated</option>
                <option value="*">all</option>
              </select>
              <input
                className={inputClass}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="search display name, urn, role, project"
              />
              {message && <span className="text-[11px] text-status-done">{message}</span>}
              {error && <span className="text-[11px] text-status-blocked">{error}</span>}
            </div>
          </div>
        }
      >
        {tab !== 'management' ? (
          <DataTable
            items={visibleRows}
            columns={registryColumns(
              (profile) => setForm(profileToForm(profile)),
              syncURN,
              deregisterRow,
              pendingURN,
            )}
            getRowId={(row) => row.urn}
            initialSort={{ key: 'display_name', dir: 'asc' }}
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading registry...' : 'No registry rows'}
                description="No registry rows matched the current filter."
              />
            }
          />
        ) : (
          <div className="grid gap-3 p-3">
            <section className="border border-border-strong bg-panel">
              <div className="flex items-center justify-between border-b border-border-strong px-4 py-2">
                <div>
                  <h2 className="text-[11px] font-semibold uppercase tracking-[.18em] text-text-muted">
                    Bootstrap
                  </h2>
                  <p className="mt-1 text-[12px] text-text-soft">
                    Re-run the daemon-side importer that projects catalog agents and projects into the registry.
                  </p>
                </div>
                <div className="flex gap-2">
                  <Button variant="outline" size="sm" onClick={() => runBootstrap(false)} disabled={saving}>
                    Bootstrap
                  </Button>
                  <Button variant="default" size="sm" onClick={() => runBootstrap(true)} disabled={saving}>
                    Force Refresh
                  </Button>
                </div>
              </div>
              <div className="grid gap-3 px-4 py-3 text-[12px] text-text-soft md:grid-cols-4">
                <Stat label="Active agents" value={agents.filter((row) => row.status === 'active').length} />
                <Stat label="Active projects" value={projects.filter((row) => row.status === 'active').length} />
                <Stat label="Deprecated rows" value={[...agents, ...projects].filter((row) => row.status === 'deprecated').length} />
                <Stat label="Callback-backed rows" value={[...agents, ...projects].filter((row) => Boolean(row.callback?.target)).length} />
              </div>
            </section>

            <section className="border border-border-strong bg-panel">
              <div className="border-b border-border-strong px-4 py-2">
                <h2 className="text-[11px] font-semibold uppercase tracking-[.18em] text-text-muted">
                  Last Bootstrap Report
                </h2>
              </div>
              {report ? (
                <>
                  <div className="grid gap-3 px-4 py-3 text-[12px] text-text-soft md:grid-cols-4">
                    <Stat label="Imported" value={report.Imported ?? 0} />
                    <Stat label="Skipped" value={report.Skipped ?? 0} />
                    <Stat label="Refreshed" value={report.Refreshed ?? 0} />
                    <Stat label="Errors" value={report.Errors?.length ?? 0} />
                  </div>
                  <div className="border-t border-border-strong">
                    {(report.Errors ?? []).length > 0 ? (
                      report.Errors?.map((item, index) => (
                        <BootstrapErrorRow key={`${item.path}-${index}`} item={item} />
                      ))
                    ) : (
                      <div className="px-4 py-3 text-[12px] text-text-subtle">No importer errors on the last run.</div>
                    )}
                  </div>
                </>
              ) : (
                <div className="px-4 py-3 text-[12px] text-text-subtle">No bootstrap report yet in this session.</div>
              )}
            </section>
          </div>
        )}
      </ListPageLayout>
      <RegistryDialog
        form={form}
        saving={saving}
        error={error}
        onChange={setForm}
        onClose={() => setForm(null)}
        onSave={saveForm}
      />
    </>
  )
}

function filterRows(rows: RegistryProfileInfo[], status: StatusFilter, query: string): RegistryProfileInfo[] {
  const q = query.trim().toLowerCase()
  return rows.filter((row) => {
    if (status !== '*' && row.status !== status) return false
    if (!q) return true
    return [
      row.display_name,
      row.urn,
      row.title,
      row.role,
      row.project,
      row.callback?.target,
    ]
      .filter(Boolean)
      .some((value) => value!.toLowerCase().includes(q))
  })
}

function Stat({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div>
      <div className="text-[10px] uppercase tracking-[.14em] text-text-subtle">{label}</div>
      <div className="mt-1 font-mono text-[20px] text-text">{value}</div>
    </div>
  )
}

function BootstrapErrorRow({ item }: { item: RegistryBootstrapErrorInfo }) {
  return (
    <div className="border-b border-border px-4 py-3 last:border-b-0">
      <div className="text-[12px] text-text">
        <CopyableId id={item.path} label={item.path} />
      </div>
      <div className="mt-1 text-[11px] text-status-blocked">{item.reason}</div>
    </div>
  )
}

const inputClass =
  'h-8 border border-border bg-bg px-2 font-mono text-[12px] text-text outline-none focus:border-border-strong'

function FormField({ label, children, hint }: { label: string; children: ReactNode; hint?: string }) {
  return (
    <label className="block">
      <span className="mb-1 block text-[11px] uppercase tracking-[.14em] text-text-subtle">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-[11px] text-text-subtle">{hint}</span>}
    </label>
  )
}

function RegistryDialog({
  form,
  saving,
  error,
  onChange,
  onClose,
  onSave,
}: {
  form: RegistryFormState | null
  saving: boolean
  error: string | null
  onChange: (next: RegistryFormState | null) => void
  onClose: () => void
  onSave: () => void
}) {
  function update(patch: Partial<RegistryFormState>) {
    if (form) onChange({ ...form, ...patch })
  }

  return (
    <DetailDialog
      open={form !== null}
      onClose={onClose}
      title={form?.urn ? form.displayName || form.urn : `New ${form?.kind ?? 'registry'} row`}
      widthClassName="max-w-3xl"
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
          <DetailSection title="Identity">
            <div className="grid gap-3 px-1 py-1">
              {form.urn && (
                <FormField label="URN">
                  <div className="flex h-8 items-center border border-border bg-bg px-2 text-[12px] text-text-soft">
                    <CopyableId id={form.urn} label={form.urn} />
                  </div>
                </FormField>
              )}
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <FormField label="Kind">
                  <select
                    className={cn(inputClass, 'w-full')}
                    value={form.kind}
                    onChange={(e) => update({ kind: e.target.value as RegistryKind })}
                    disabled={Boolean(form.urn)}
                  >
                    <option value="agent">agent</option>
                    <option value="project">project</option>
                  </select>
                </FormField>
                <FormField label="Status">
                  <select
                    className={cn(inputClass, 'w-full')}
                    value={form.status}
                    onChange={(e) => update({ status: e.target.value })}
                  >
                    <option value="active">active</option>
                    <option value="deprecated">deprecated</option>
                  </select>
                </FormField>
              </div>
              <FormField label="Display name">
                <input className={cn(inputClass, 'w-full')} value={form.displayName} onChange={(e) => update({ displayName: e.target.value })} />
              </FormField>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <FormField label="Title">
                  <input className={cn(inputClass, 'w-full')} value={form.title} onChange={(e) => update({ title: e.target.value })} />
                </FormField>
                <FormField label="Role">
                  <input className={cn(inputClass, 'w-full')} value={form.role} onChange={(e) => update({ role: e.target.value })} />
                </FormField>
              </div>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <FormField label="Project">
                  <input className={cn(inputClass, 'w-full')} value={form.project} onChange={(e) => update({ project: e.target.value })} />
                </FormField>
                <FormField label="Last updated by">
                  <input className={cn(inputClass, 'w-full')} value={form.lastUpdatedBy} onChange={(e) => update({ lastUpdatedBy: e.target.value })} />
                </FormField>
              </div>
              <FormField label="Description">
                <textarea className={cn(inputClass, 'h-24 w-full resize-y py-2')} value={form.description} onChange={(e) => update({ description: e.target.value })} />
              </FormField>
            </div>
          </DetailSection>

          <DetailSection title="Runtime Identity">
            <div className="grid gap-3 px-1 py-1">
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <FormField label="Health status">
                  <input className={cn(inputClass, 'w-full')} value={form.healthStatus} onChange={(e) => update({ healthStatus: e.target.value })} />
                </FormField>
                <FormField label="Host address">
                  <input className={cn(inputClass, 'w-full')} value={form.hostAddress} onChange={(e) => update({ hostAddress: e.target.value })} />
                </FormField>
              </div>
              <FormField label="Avatar">
                <input className={cn(inputClass, 'w-full')} value={form.avatar} onChange={(e) => update({ avatar: e.target.value })} />
              </FormField>
            </div>
          </DetailSection>

          <DetailSection title="Callback">
            <div className="grid gap-3 px-1 py-1">
              {form.urn ? (
                <>
                  <FormField label="Scheme">
                    <input className={cn(inputClass, 'w-full')} value={form.callbackScheme} readOnly />
                  </FormField>
                  <FormField label="Target" hint="Callback edits are not supported by the current registry patch API.">
                    <input className={cn(inputClass, 'w-full')} value={form.callbackTarget} readOnly />
                  </FormField>
                </>
              ) : (
                <div className="grid grid-cols-1 gap-3 sm:grid-cols-[12rem_minmax(0,1fr)]">
                  <FormField label="Scheme">
                    <select className={cn(inputClass, 'w-full')} value={form.callbackScheme} onChange={(e) => update({ callbackScheme: e.target.value })}>
                      <option value="file">file</option>
                      <option value="cli">cli</option>
                    </select>
                  </FormField>
                  <FormField label="Target">
                    <input className={cn(inputClass, 'w-full')} value={form.callbackTarget} onChange={(e) => update({ callbackTarget: e.target.value })} placeholder="file:///... or cli://..." />
                  </FormField>
                </div>
              )}
            </div>
          </DetailSection>

          <DetailSection title="Arrays">
            <div className="grid gap-3 px-1 py-1">
              <FormField label="Capabilities" hint="One capability per line.">
                <textarea className={cn(inputClass, 'h-24 w-full resize-y py-2')} value={form.capabilities} onChange={(e) => update({ capabilities: e.target.value })} />
              </FormField>
              <FormField label="Skills" hint='One skill per line: "name | learned_at | via | level". learned_at defaults to now if omitted.'>
                <textarea className={cn(inputClass, 'h-28 w-full resize-y py-2')} value={form.skills} onChange={(e) => update({ skills: e.target.value })} />
              </FormField>
              <FormField label="Links" hint='One link per line: "kind | target".'>
                <textarea className={cn(inputClass, 'h-24 w-full resize-y py-2')} value={form.links} onChange={(e) => update({ links: e.target.value })} />
              </FormField>
            </div>
          </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}
