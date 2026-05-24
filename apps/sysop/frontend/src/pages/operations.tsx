import { useCallback, useEffect, useMemo, useRef, useState, type MouseEvent, type ReactNode } from 'react'
import { Activity, Boxes, Hourglass, Keyboard, Maximize2, Play, RefreshCw, RotateCw, Save, Send, Square } from 'lucide-react'
import {
  Button,
  CopyButton,
  CopyableId,
  DetailDialog,
  DetailSection,
  EmptyState,
  JsonViewer,
  StatusBadge,
  SummaryCards,
  cn,
  formatRelativeTime,
} from '@hollis-labs/sysop-ui/ui'
import { DataTable, type ColumnDef } from '@hollis-labs/sysop-ui/data'
import { ListPageLayout, TabStrip, type TabStripItem } from '@hollis-labs/sysop-ui/layout'
import { useApi } from '../api/context'
import type {
  CatalogInfo,
  HealthInfo,
  InjectedFileInfo,
  LaunchInfo,
  LaunchPreviewInfo,
  LogicalAgentPolicyInfo,
  MCPServerInfo,
  SessionDetailInfo,
  SessionInfo,
  SessionsInfo,
} from '../api/client'
import {
  SessionDetailDialog,
  isLiveSessionState,
  isTerminalSessionState,
  sessionLifecycleHint,
  sessionLifecycleLabel,
  type SessionActionKind,
} from '../components/session-detail-dialog'

type TabKey = 'launches' | 'sessions'

interface LaunchFormState {
  id: string
  project: string
  agent: string
  provider: string
  workspaceMode: string
  worktreeName: string
  includeProjectBoot: boolean
  includeAgentBoot: boolean
  includeKnowledgeBase: boolean
  mcpServers: string[]
  envOverrides: string
  nativeFiles: InjectedFileFormState[]
  bootDirOverlay: InjectedFileFormState[]
}

interface InjectedFileFormState {
  kind: string
  id: string
  relPath: string
  content: string
  source: string
  mode: string
}

interface SessionActionState {
  kind: SessionActionKind
  detail: SessionDetailInfo
  text: string
  status: string
  summary: string
  rows: number
  cols: number
}

interface CleanupFormState {
  olderThanDays: number
  limit: number
  previewCount: number | null
}

interface StopConfirmState {
  session: SessionInfo
  detail?: SessionDetailInfo
}

interface LogicalAgentPolicyFormState {
  logicalAgentId: string
  name: string
  launchId: string
  checkpointPolicy: string
  checkpointStatus: string
  updatedAt: string
}

function parseJson(raw?: string): unknown {
  if (!raw) return null
  try {
    return JSON.parse(raw)
  } catch {
    return raw
  }
}

function logicalAgentPolicyToForm(policy: LogicalAgentPolicyInfo): LogicalAgentPolicyFormState {
  return {
    logicalAgentId: policy.logical_agent_id,
    name: policy.name ?? '',
    launchId: policy.launch_id ?? '',
    checkpointPolicy: policy.checkpoint_policy || 'manual',
    checkpointStatus: policy.checkpoint_status ?? '',
    updatedAt: policy.updated_at ?? '',
  }
}

function emptyInjectedFile(kind = 'raw'): InjectedFileFormState {
  return {
    kind,
    id: '',
    relPath: '',
    content: '',
    source: '',
    mode: '',
  }
}

function fileToForm(file?: InjectedFileInfo | null): InjectedFileFormState {
  return {
    kind: file?.kind ?? 'raw',
    id: file?.id ?? '',
    relPath: file?.rel_path ?? '',
    content: file?.content ?? '',
    source: file?.source ?? '',
    mode: file?.mode ? file.mode.toString(8) : '',
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
    env[key] = trimmed.slice(idx + 1)
  }
  return env
}

function parseMode(raw: string): number | undefined {
  const trimmed = raw.trim()
  if (!trimmed) return undefined
  if (/^0[0-7]+$/.test(trimmed)) return Number.parseInt(trimmed, 8)
  const parsed = Number.parseInt(trimmed, 10)
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : undefined
}

function formFilesToRequest(files: InjectedFileFormState[]): InjectedFileInfo[] {
  return files
    .map((file) => ({
      kind: file.kind.trim() || undefined,
      id: file.id.trim() || undefined,
      rel_path: file.relPath.trim() || undefined,
      content: file.content || undefined,
      source: file.source.trim() || undefined,
      mode: parseMode(file.mode),
    }))
    .filter((file) => Object.values(file).some((value) => value !== undefined))
}

function launchToForm(launch: LaunchInfo, clone = false): LaunchFormState {
  const profile = parseJson(launch.profile) as Partial<{
    id: string
    workspace: { mode?: string; worktree_name?: string }
    prompt: {
      include_project_boot?: boolean
      include_agent_boot?: boolean
      include_knowledge_base?: boolean
    }
    mcp: { servers?: string[] }
    overrides: { env?: Record<string, string> }
    injection: { native_files?: InjectedFileInfo[]; boot_dir_overlay?: InjectedFileInfo[] }
  }> | null
  return {
    id: clone ? `${launch.id}-copy` : launch.id,
    project: launch.project,
    agent: launch.agent,
    provider: launch.provider,
    workspaceMode: profile?.workspace?.mode ?? launch.workspace_mode ?? 'default',
    worktreeName: profile?.workspace?.worktree_name ?? '',
    includeProjectBoot: profile?.prompt?.include_project_boot ?? true,
    includeAgentBoot: profile?.prompt?.include_agent_boot ?? true,
    includeKnowledgeBase: profile?.prompt?.include_knowledge_base ?? true,
    mcpServers: profile?.mcp?.servers ?? [],
    envOverrides: Object.entries(profile?.overrides?.env ?? {})
      .map(([key, value]) => `${key}=${value}`)
      .join('\n'),
    nativeFiles: (profile?.injection?.native_files ?? []).map((file) => fileToForm(file)),
    bootDirOverlay: (profile?.injection?.boot_dir_overlay ?? []).map((file) => fileToForm(file)),
  }
}

function emptyLaunchForm(catalog: CatalogInfo | null): LaunchFormState {
  return {
    id: '',
    project: catalog?.projects[0]?.id ?? '',
    agent: catalog?.agents[0]?.id ?? '',
    provider: catalog?.providers[0]?.id ?? '',
    workspaceMode: 'default',
    worktreeName: '',
    includeProjectBoot: true,
    includeAgentBoot: true,
    includeKnowledgeBase: true,
    mcpServers: [],
    envOverrides: '',
    nativeFiles: [],
    bootDirOverlay: [],
  }
}

function sessionActionTitle(kind: SessionActionKind): string {
  switch (kind) {
    case 'turn':
      return 'Send Turn'
    case 'input':
      return 'Send Raw Input'
    case 'checkpoint':
      return 'Create Checkpoint'
    case 'resume':
      return 'Start From Checkpoint'
    case 'resize':
      return 'Resize Session'
    case 'wait':
      return 'Wait For Exit'
  }
}

function diffLineClass(line: string): string {
  if (line.startsWith('+')) return 'bg-status-done/10 text-status-done'
  if (line.startsWith('-')) return 'bg-status-blocked/10 text-status-blocked'
  return 'text-text-subtle'
}

function DiffViewer({
  title,
  diff,
  emptyLabel,
}: {
  title: string
  diff?: string
  emptyLabel: string
}) {
  const lines = diff?.split('\n') ?? []
  return (
    <div className="min-w-0 rounded border border-border bg-panel/20">
      <div className="border-b border-border px-3 py-2 text-[11px] uppercase tracking-[.14em] text-text-subtle">
        {title}
      </div>
      {lines.length === 0 ? (
        <p className="px-3 py-3 text-[12px] text-text-subtle">{emptyLabel}</p>
      ) : (
        <pre className="max-h-80 overflow-auto px-3 py-3 text-[11px] leading-5">
          {lines.map((line, index) => (
            <div key={`${title}-${index}`} className={cn('whitespace-pre-wrap break-all rounded px-1', diffLineClass(line))}>
              {line || ' '}
            </div>
          ))}
        </pre>
      )}
    </div>
  )
}

function launchColumns(
  onLaunch: (id: string) => void,
  pendingID: string | null,
): ColumnDef<LaunchInfo>[] {
  return [
    {
      key: 'id',
      header: 'Launch',
      width: 'fill',
      cell: (launch) => <CopyableId id={launch.id} />,
      sortValue: (launch) => launch.id,
    },
    {
      key: 'project',
      header: 'Project',
      cell: (launch) => launch.project,
      sortValue: (launch) => launch.project,
    },
    {
      key: 'agent',
      header: 'Agent',
      cell: (launch) => launch.agent,
      sortValue: (launch) => launch.agent,
    },
    {
      key: 'provider',
      header: 'Provider',
      cell: (launch) => launch.provider,
      sortValue: (launch) => launch.provider,
    },
    {
      key: 'workspace',
      header: 'Workspace',
      cell: (launch) => launch.workspace_mode || 'default',
      sortValue: (launch) => launch.workspace_mode,
    },
    {
      key: 'injection',
      header: 'Files',
      align: 'right',
      cell: (launch) => launch.native_files + launch.boot_overlay,
      sortValue: (launch) => launch.native_files + launch.boot_overlay,
    },
    {
      key: 'actions',
      header: '',
      align: 'right',
      cell: (launch) => {
        const pending = pendingID === launch.id
        return (
          <Button
            variant="outline"
            size="sm"
            onClick={(event: MouseEvent<HTMLButtonElement>) => {
              event.stopPropagation()
              onLaunch(launch.id)
            }}
            disabled={pending || Boolean(launch.plan_error)}
            title={launch.plan_error ? 'Resolve the launch plan error before launching.' : 'Launch'}
          >
            <Play className={cn('h-3.5 w-3.5', pending && 'animate-pulse')} />
            {pending ? 'Launching' : 'Launch'}
          </Button>
        )
      },
      sortValue: () => 0,
    },
  ]
}

function sessionColumns(
  onStop: (session: SessionInfo) => void,
  pendingID: string | null,
): ColumnDef<SessionInfo>[] {
  return [
    {
      key: 'id',
      header: 'Session',
      width: 'fill',
      cell: (session) => <CopyableId id={session.id} label={session.id.slice(0, 12)} />,
      sortValue: (session) => session.id,
    },
    {
      key: 'state',
      header: 'State',
      cell: (session) => (
        <div className="flex flex-col gap-1">
          <StatusBadge status={session.state} />
          <span className="text-[10px] uppercase tracking-[.12em] text-text-subtle">
            {sessionLifecycleLabel(session.state)}
          </span>
        </div>
      ),
      sortValue: (session) => session.state,
    },
    {
      key: 'launch',
      header: 'Launch',
      cell: (session) => session.launch_id || 'manual',
      sortValue: (session) => session.launch_id,
    },
    {
      key: 'provider',
      header: 'Provider',
      cell: (session) => session.provider_id,
      sortValue: (session) => session.provider_id,
    },
    {
      key: 'updated',
      header: 'Updated',
      cell: (session) => formatRelativeTime(session.updated_at),
      sortValue: (session) => session.updated_at,
    },
    {
      key: 'actions',
      header: '',
      align: 'right',
      cell: (session) => {
        const canStop = isLiveSessionState(session.state)
        const pending = pendingID === session.id
        if (!canStop) {
          return (
            <span
              className="text-[11px] text-text-subtle"
              title={sessionLifecycleHint(session.state)}
            >
              {isTerminalSessionState(session.state) ? 'Ended' : 'Not live'}
            </span>
          )
        }
        return (
          <Button
            variant="outline"
            size="sm"
            onClick={(event: MouseEvent<HTMLButtonElement>) => {
              event.stopPropagation()
              onStop(session)
            }}
            disabled={pending}
            title="Stop live runtime"
          >
            <Square className={cn('h-3.5 w-3.5', pending && 'animate-pulse')} />
            {pending ? 'Stopping' : 'Stop Live'}
          </Button>
        )
      },
      sortValue: () => 0,
    },
  ]
}

function launchRequestFromForm(form: LaunchFormState) {
  return {
    id: form.id,
    project: form.project,
    agent: form.agent,
    provider: form.provider,
    workspace_mode: form.workspaceMode,
    worktree_name: form.worktreeName || undefined,
    include_project_boot: form.includeProjectBoot,
    include_agent_boot: form.includeAgentBoot,
    include_knowledge_base: form.includeKnowledgeBase,
    mcp_servers: form.mcpServers,
    env_overrides: parseEnvLines(form.envOverrides),
    native_files: formFilesToRequest(form.nativeFiles),
    boot_dir_overlay: formFilesToRequest(form.bootDirOverlay),
  }
}

export function OperationsPage() {
  const api = useApi()
  const [tab, setTab] = useState<TabKey>('launches')
  const [health, setHealth] = useState<HealthInfo | null>(null)
  const [catalog, setCatalog] = useState<CatalogInfo | null>(null)
  const [mcpServers, setMCPServers] = useState<MCPServerInfo[]>([])
  const [sessions, setSessions] = useState<SessionsInfo | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [detail, setDetail] = useState<SessionDetailInfo | null>(null)
  const [launchDetail, setLaunchDetail] = useState<LaunchInfo | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [detailError, setDetailError] = useState<string | null>(null)
  const [launchForm, setLaunchForm] = useState<LaunchFormState | null>(null)
  const [sessionAction, setSessionAction] = useState<SessionActionState | null>(null)
  const [streamDetail, setStreamDetail] = useState<SessionDetailInfo | null>(null)
  const [cleanupForm, setCleanupForm] = useState<CleanupFormState | null>(null)
  const [stopConfirm, setStopConfirm] = useState<StopConfirmState | null>(null)
  const [cleanupConfirmOpen, setCleanupConfirmOpen] = useState(false)
  const [cleanupConfirmText, setCleanupConfirmText] = useState('')
  const [policyForm, setPolicyForm] = useState<LogicalAgentPolicyFormState | null>(null)
  const [policySaving, setPolicySaving] = useState(false)
  const [savingLaunch, setSavingLaunch] = useState(false)
  const [previewingLaunch, setPreviewingLaunch] = useState(false)
  const [launchPreview, setLaunchPreview] = useState<LaunchPreviewInfo | null>(null)
  const [sessionActionSaving, setSessionActionSaving] = useState(false)
  const [cleanupSaving, setCleanupSaving] = useState(false)
  const [launchingID, setLaunchingID] = useState<string | null>(null)
  const [stoppingID, setStoppingID] = useState<string | null>(null)
  const [actionMessage, setActionMessage] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    Promise.all([api.getHealth(), api.getCatalog(), api.getMCPServers(), api.getSessions()])
      .then(([healthInfo, catalogInfo, mcpInfo, sessionsInfo]) => {
        if (cancelled) return
        setHealth(healthInfo)
        setCatalog(catalogInfo)
        setMCPServers(mcpInfo.servers ?? [])
        setSessions(sessionsInfo)
        setError(null)
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

  function openSession(id: string) {
    setDetail(null)
    setDetailError(null)
    setDetailLoading(true)
    api
      .getSessionDetail(id)
      .then((info) => {
        setDetail(info)
        setDetailError(info.error ?? null)
      })
      .catch((err: unknown) => {
        setDetailError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => setDetailLoading(false))
  }

  function closeSession() {
    setDetail(null)
    setDetailError(null)
    setDetailLoading(false)
  }

  function launchProfile(id: string) {
    setLaunchingID(id)
    setActionError(null)
    setActionMessage(null)
    api
      .launchProfile(id)
      .then((info) => {
        setActionMessage(
          info.session_id ? `Launched ${id} as session ${info.session_id.slice(0, 12)}.` : `Launched ${id}.`,
        )
        setTab('sessions')
        load()
      })
      .catch((err: unknown) => {
        setActionError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => setLaunchingID(null))
  }

  function requestStopSession(session: SessionInfo, detail?: SessionDetailInfo) {
    setStopConfirm({ session, detail })
  }

  function stopSession(confirmState: StopConfirmState) {
    const { session, detail } = confirmState
    setStoppingID(session.id)
    setActionError(null)
    setActionMessage(null)
    api
      .stopSession(session.id)
      .then(() => {
        setActionMessage(`Stopped session ${session.id.slice(0, 12)}.`)
        if (detail) openSession(session.id)
        setStopConfirm(null)
        load()
      })
      .catch((err: unknown) => {
        setActionError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => setStoppingID(null))
  }

  function openSessionAction(kind: SessionActionKind, actionDetail: SessionDetailInfo) {
    setActionError(null)
    setActionMessage(null)
    setSessionAction({
      kind,
      detail: actionDetail,
      text: kind === 'input' ? '\n' : '',
      status: 'manual',
      summary: '',
      rows: 40,
      cols: 120,
    })
  }

  function openLogicalAgentPolicy(detail: SessionDetailInfo) {
    const logicalAgentId = detail.session.logical_agent_id
    if (!logicalAgentId) return
    setActionError(null)
    setActionMessage(null)
    setPolicySaving(true)
    api
      .getLogicalAgentPolicy(logicalAgentId)
      .then((info) => {
        setPolicyForm(logicalAgentPolicyToForm(info))
      })
      .catch((err: unknown) => {
        setActionError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => setPolicySaving(false))
  }

  function saveLogicalAgentPolicy() {
    if (!policyForm) return
    setPolicySaving(true)
    setActionError(null)
    setActionMessage(null)
    api
      .saveLogicalAgentPolicy({
        logical_agent_id: policyForm.logicalAgentId,
        checkpoint_policy: policyForm.checkpointPolicy,
        checkpoint_status: policyForm.checkpointStatus,
      })
      .then((info) => {
        setPolicyForm(logicalAgentPolicyToForm(info))
        setActionMessage(`Saved policy for ${policyForm.logicalAgentId}.`)
      })
      .catch((err: unknown) => {
        setActionError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => setPolicySaving(false))
  }

  function updateSessionAction(patch: Partial<SessionActionState>) {
    setSessionAction((current) => (current ? { ...current, ...patch } : current))
  }

  function submitSessionAction() {
    if (!sessionAction) return
    const session = sessionAction.detail.session
    setSessionActionSaving(true)
    setActionError(null)
    setActionMessage(null)

    let request: Promise<unknown>
    if (sessionAction.kind === 'turn') {
      request = api.sendSessionTurn(session.id, sessionAction.text)
    } else if (sessionAction.kind === 'input') {
      request = api.sendSessionInput(session.id, sessionAction.text)
    } else if (sessionAction.kind === 'checkpoint') {
      request = api.checkpointSession(session.id, sessionAction.status, sessionAction.summary)
    } else if (sessionAction.kind === 'resize') {
      request = api.resizeSession(session.id, sessionAction.rows, sessionAction.cols)
    } else if (sessionAction.kind === 'wait') {
      request = api.waitSession(session.id)
    } else {
      request = api.resumeLogicalAgent(session.logical_agent_id)
    }

    request
      .then((info) => {
        if (sessionAction.kind === 'resume') {
          const action = info as { session_id?: string }
          setActionMessage(
            action.session_id
              ? `Started new checkpoint-based session ${action.session_id.slice(0, 12)} for ${session.logical_agent_id.slice(0, 12)}.`
              : `Started a new checkpoint-based session for ${session.logical_agent_id.slice(0, 12)}.`,
          )
          setTab('sessions')
          closeSession()
        } else if (sessionAction.kind === 'wait') {
          const action = info as { exit_code?: number }
          setActionMessage(
            typeof action.exit_code === 'number'
              ? `Session ${session.id.slice(0, 12)} exited with code ${action.exit_code}.`
              : `Session ${session.id.slice(0, 12)} exited.`,
          )
          openSession(session.id)
        } else {
          setActionMessage(`${sessionActionTitle(sessionAction.kind)} completed for ${session.id.slice(0, 12)}.`)
          openSession(session.id)
        }
        setSessionAction(null)
        load()
      })
      .catch((err: unknown) => {
        setActionError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => setSessionActionSaving(false))
  }

  function previewCleanup() {
    if (!cleanupForm) return
    setCleanupSaving(true)
    setActionError(null)
    setActionMessage(null)
    api
      .cleanupSessions(cleanupForm.olderThanDays, cleanupForm.limit, true)
      .then((info) => {
        setCleanupForm({ ...cleanupForm, previewCount: info.count ?? 0 })
        setActionMessage(`Cleanup preview found ${info.count ?? 0} ended sessions.`)
      })
      .catch((err: unknown) => setActionError(err instanceof Error ? err.message : String(err)))
      .finally(() => setCleanupSaving(false))
  }

  function requestCleanupConfirm() {
    if (!cleanupForm || cleanupForm.previewCount === null || cleanupForm.previewCount === 0) return
    setCleanupConfirmText('')
    setCleanupConfirmOpen(true)
    setActionError(null)
    setActionMessage(null)
  }

  function runCleanup() {
    if (!cleanupForm) return
    setCleanupSaving(true)
    setActionError(null)
    setActionMessage(null)
    api
      .cleanupSessions(cleanupForm.olderThanDays, cleanupForm.limit, false)
      .then((info) => {
        setActionMessage(`Deleted ${info.count ?? 0} ended sessions.`)
        setCleanupConfirmOpen(false)
        setCleanupForm(null)
        setCleanupConfirmText('')
        load()
      })
      .catch((err: unknown) => setActionError(err instanceof Error ? err.message : String(err)))
      .finally(() => setCleanupSaving(false))
  }

  function saveLaunch() {
    if (!launchForm) return
    setSavingLaunch(true)
    setActionError(null)
    setActionMessage(null)
    api
      .saveLaunch(launchRequestFromForm(launchForm))
      .then((info) => {
        setActionMessage(
          info.backup_path
            ? `Saved launch ${launchForm.id}. Backup: ${info.backup_path}.`
            : `Saved launch ${launchForm.id}.`,
        )
        setLaunchForm(null)
        setLaunchPreview(null)
        setLaunchDetail(null)
        load()
      })
      .catch((err: unknown) => setActionError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSavingLaunch(false))
  }

  function previewLaunch() {
    if (!launchForm) return
    setPreviewingLaunch(true)
    setActionError(null)
    setActionMessage(null)
    api
      .previewLaunch(launchRequestFromForm(launchForm))
      .then((info) => {
        setLaunchPreview(info)
        setActionMessage(
          info.existing ? `Previewed changes for launch ${launchForm.id}.` : `Previewed new launch ${launchForm.id}.`,
        )
      })
      .catch((err: unknown) => {
        setLaunchPreview(null)
        setActionError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => setPreviewingLaunch(false))
  }

  function deleteLaunch(launch: LaunchInfo) {
    if (!window.confirm(`Delete launch profile ${launch.id}? A timestamped backup will be kept beside the YAML file.`)) return
    setActionError(null)
    setActionMessage(null)
    api
      .deleteLaunch(launch.id)
      .then((info) => {
        setActionMessage(
          info.backup_path
            ? `Deleted launch ${launch.id}. Backup: ${info.backup_path}.`
            : `Deleted launch ${launch.id}.`,
        )
        setLaunchDetail(null)
        load()
      })
      .catch((err: unknown) => setActionError(err instanceof Error ? err.message : String(err)))
  }

  const launches = catalog?.launches ?? []
  const sessionList = sessions?.sessions ?? []
  const sessionTotal = sessions?.total ?? sessionList.length
  const liveSessions = sessionList.filter((session) => isLiveSessionState(session.state)).length
  const prelaunchSessions = sessionList.filter((session) => session.state === 'created').length
  const terminalSessions =
    sessions?.ended ?? sessionList.filter((session) => isTerminalSessionState(session.state)).length

  const tabs: TabStripItem<TabKey>[] = [
    {
      key: 'launches',
      label: 'Launch Profiles',
      icon: <Boxes className="h-3.5 w-3.5" />,
      count: launches.length,
    },
    {
      key: 'sessions',
      label: 'Sessions',
      icon: <Activity className="h-3.5 w-3.5" />,
      count: sessionTotal,
    },
  ]

  const launchTableColumns = useMemo(
    () => launchColumns(launchProfile, launchingID),
    [launchingID],
  )
  const sessionTableColumns = useMemo(
    () => sessionColumns((session) => requestStopSession(session), stoppingID),
    [stoppingID],
  )
  const summaryCards =
    tab === 'launches'
      ? [
          { label: 'Catalog', value: health ? health.status : '...' },
          { label: 'Projects', value: catalog?.projects.length ?? '...' },
          { label: 'Launches', value: catalog?.launches.length ?? '...' },
          { label: 'Providers', value: catalog?.providers.length ?? '...' },
        ]
      : [
          { label: 'Sessions', value: sessions ? sessionTotal : '...' },
          {
            label: 'Pre-launch',
            value: sessions ? prelaunchSessions : '...',
          },
          {
            label: 'Live',
            value: sessions ? liveSessions : '...',
            accentColor: 'var(--color-status-doing)',
          },
          {
            label: 'Ended',
            value: sessions ? terminalSessions : '...',
            accentColor: 'var(--color-status-done)',
          },
        ]

  if (error) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not reach the server" description={error} />
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
                {tab === 'launches' && (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      setLaunchPreview(null)
                      setLaunchForm(emptyLaunchForm(catalog))
                    }}
                  >
                    Add
                  </Button>
                )}
                {tab === 'sessions' && (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      setCleanupConfirmOpen(false)
                      setCleanupConfirmText('')
                      setCleanupForm({ olderThanDays: 30, limit: 100, previewCount: null })
                    }}
                  >
                    Cleanup
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
                {tab === 'launches'
                  ? health?.catalog_root ?? 'Loading catalog...'
                  : 'Latest 50 state DB rows. Quick stop only applies to live runtime-backed sessions; checkpoint resume creates a new session from the logical agent\'s latest checkpoint. Session policy knobs stay backend-defined until the daemon exposes a real operator policy surface.'}
              </span>
              {actionMessage && <span className="text-status-done">{actionMessage}</span>}
              {actionError && <span className="text-status-blocked">{actionError}</span>}
            </div>
          </div>
        }
      >
        {tab === 'launches' ? (
          <DataTable
            items={launches}
            columns={launchTableColumns}
            getRowId={(launch) => launch.id}
            initialSort={{ key: 'id', dir: 'asc' }}
            scrollRootRef={scrollRef}
            onRowOpen={(_, launch) => setLaunchDetail(launch)}
            rowAriaLabel={(launch) => `Open launch profile ${launch.id}`}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading launch profiles...' : 'No launch profiles found'}
                description={
                  loading
                    ? 'Reading the configured catalog.'
                    : 'The configured catalog loaded, but it does not contain launch profiles.'
                }
              />
            }
          />
        ) : (
          <>
            <DataTable
              items={sessionList}
              columns={sessionTableColumns}
              getRowId={(session) => session.id}
              initialSort={{ key: 'updated', dir: 'desc' }}
              scrollRootRef={scrollRef}
              onRowOpen={(id) => openSession(id)}
              rowAriaLabel={(session) => `Open session ${session.id.slice(0, 12)}`}
              emptyState={
                <EmptyState
                  variant="empty"
                  title={loading ? 'Loading sessions...' : 'No sessions found'}
                  description={
                    loading
                      ? 'Reading the configured state database.'
                      : 'The configured state database has no session rows yet.'
                  }
                />
              }
            />
            {sessions?.error && (
              <p className="px-4 py-3 text-[12px] text-status-blocked">{sessions.error}</p>
            )}
          </>
        )}
      </ListPageLayout>

      <SessionDetailDialog
        detail={detail}
        loading={detailLoading}
        error={detailError}
        onClose={closeSession}
        onAction={openSessionAction}
        onStop={(sessionDetail) => requestStopSession(sessionDetail.session, sessionDetail)}
        onStream={setStreamDetail}
        onPolicy={openLogicalAgentPolicy}
      />
      <LaunchDetailDialog
        launch={launchDetail}
        onClose={() => setLaunchDetail(null)}
        onEdit={(launch) => {
          setLaunchPreview(null)
          setLaunchForm(launchToForm(launch))
        }}
        onClone={(launch) => {
          setLaunchPreview(null)
          setLaunchForm(launchToForm(launch, true))
        }}
        onDelete={deleteLaunch}
      />
      <LaunchEditDialog
        form={launchForm}
        catalog={catalog}
        mcpServers={mcpServers}
        saving={savingLaunch}
        previewing={previewingLaunch}
        preview={launchPreview}
        error={actionError}
        onChange={(next) => {
          setLaunchPreview(null)
          setLaunchForm(next)
        }}
        onClose={() => {
          setLaunchForm(null)
          setLaunchPreview(null)
        }}
        onPreview={previewLaunch}
        onSave={saveLaunch}
      />
      <SessionActionDialog
        action={sessionAction}
        saving={sessionActionSaving}
        error={actionError}
        onChange={updateSessionAction}
        onClose={() => setSessionAction(null)}
        onSubmit={submitSessionAction}
      />
      <SessionStreamDialog detail={streamDetail} onClose={() => setStreamDetail(null)} />
      <SessionCleanupDialog
        form={cleanupForm}
        saving={cleanupSaving}
        error={actionError}
        onChange={setCleanupForm}
        onClose={() => {
          setCleanupConfirmOpen(false)
          setCleanupConfirmText('')
          setCleanupForm(null)
        }}
        onPreview={previewCleanup}
        onCleanup={requestCleanupConfirm}
      />
      <SessionStopConfirmDialog
        state={stopConfirm}
        saving={stoppingID !== null}
        error={actionError}
        onClose={() => setStopConfirm(null)}
        onConfirm={stopSession}
      />
      <SessionCleanupConfirmDialog
        open={cleanupConfirmOpen}
        form={cleanupForm}
        confirmText={cleanupConfirmText}
        saving={cleanupSaving}
        error={actionError}
        onChange={setCleanupConfirmText}
        onClose={() => {
          setCleanupConfirmOpen(false)
          setCleanupConfirmText('')
        }}
        onConfirm={runCleanup}
      />
      <LogicalAgentPolicyDialog
        form={policyForm}
        saving={policySaving}
        error={actionError}
        onChange={setPolicyForm}
        onClose={() => setPolicyForm(null)}
        onSave={saveLogicalAgentPolicy}
      />
    </>
  )
}

function LaunchDetailDialog({
  launch,
  onClose,
  onEdit,
  onClone,
  onDelete,
}: {
  launch: LaunchInfo | null
  onClose: () => void
  onEdit: (launch: LaunchInfo) => void
  onClone: (launch: LaunchInfo) => void
  onDelete: (launch: LaunchInfo) => void
}) {
  const plan = parseJson(launch?.launch_plan)
  const profile = parseJson(launch?.profile)
  return (
    <DetailDialog
      open={launch !== null}
      onClose={onClose}
      title={launch ? `Launch ${launch.id}` : 'Launch'}
      meta={
        launch ? (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-text-subtle">
            <CopyableId id={launch.id} label={launch.id} />
            <span>{launch.project}</span>
            <span>{launch.agent}</span>
          </div>
        ) : null
      }
      footer={
        launch ? (
          <div className="flex justify-between gap-2">
            <Button variant="outline" size="sm" onClick={() => onDelete(launch)}>
              Delete
            </Button>
            <div className="flex gap-2">
              <Button variant="outline" size="sm" onClick={() => onClone(launch)}>
                Clone
              </Button>
              <Button variant="outline" size="sm" onClick={() => onEdit(launch)}>
                Edit
              </Button>
              {launch.profile && <CopyButton text={launch.profile} label="Copy profile" />}
              {launch.launch_plan && <CopyButton text={launch.launch_plan} label="Copy plan" />}
            </div>
          </div>
        ) : null
      }
    >
      {launch && (
        <>
          <DetailSection title="Launch Profile">
            <dl className="grid grid-cols-[7rem_minmax(0,1fr)] gap-x-4 gap-y-2 text-[12px]">
              <dt className="text-text-subtle">Project</dt>
              <dd className="break-words text-text-soft">{launch.project}</dd>
              <dt className="text-text-subtle">Agent</dt>
              <dd className="break-words text-text-soft">{launch.agent}</dd>
              <dt className="text-text-subtle">Provider</dt>
              <dd className="break-words text-text-soft">{launch.provider}</dd>
              <dt className="text-text-subtle">Workspace</dt>
              <dd className="break-words text-text-soft">{launch.workspace_mode || 'default'}</dd>
              <dt className="text-text-subtle">Files</dt>
              <dd className="break-words text-text-soft">
                {launch.native_files} native, {launch.boot_overlay} boot overlay
              </dd>
            </dl>
          </DetailSection>

          {launch.plan_error && (
            <DetailSection title="Plan Error">
              <p className="text-[12px] text-status-blocked">{launch.plan_error}</p>
            </DetailSection>
          )}

          {launch.launch_plan && (
            <DetailSection title="Actual Launch Plan">
              <div className="mb-2 flex justify-end">
                <CopyButton text={launch.launch_plan} label="Copy plan" />
              </div>
              <JsonViewer value={plan} className="rounded-none border-0 bg-transparent px-0 py-0" />
            </DetailSection>
          )}

          {launch.profile && (
            <DetailSection title="Catalog Profile">
              <div className="mb-2 flex justify-end">
                <CopyButton text={launch.profile} label="Copy profile" />
              </div>
              <JsonViewer value={profile} className="rounded-none border-0 bg-transparent px-0 py-0" />
            </DetailSection>
          )}
        </>
      )}
    </DetailDialog>
  )
}

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

const inputClass =
  'h-8 w-full border border-border bg-bg px-2 font-mono text-[12px] text-text outline-none focus:border-border-strong'
const textAreaClass =
  'min-h-20 w-full resize-y border border-border bg-bg px-2 py-1.5 font-mono text-[12px] text-text outline-none focus:border-border-strong'

function LaunchEditDialog({
  form,
  catalog,
  mcpServers,
  saving,
  previewing,
  preview,
  error,
  onChange,
  onClose,
  onPreview,
  onSave,
}: {
  form: LaunchFormState | null
  catalog: CatalogInfo | null
  mcpServers: MCPServerInfo[]
  saving: boolean
  previewing: boolean
  preview: LaunchPreviewInfo | null
  error: string | null
  onChange: (next: LaunchFormState | null) => void
  onClose: () => void
  onPreview: () => void
  onSave: () => void
}) {
  function update(patch: Partial<LaunchFormState>) {
    if (form) onChange({ ...form, ...patch })
  }
  function toggleMCPServer(id: string) {
    if (!form) return
    const next = form.mcpServers.includes(id)
      ? form.mcpServers.filter((item) => item !== id)
      : [...form.mcpServers, id].sort()
    update({ mcpServers: next })
  }
  function updateFiles(key: 'nativeFiles' | 'bootDirOverlay', next: InjectedFileFormState[]) {
    if (form) onChange({ ...form, [key]: next })
  }
  return (
    <DetailDialog
      open={form !== null}
      onClose={onClose}
      title={form?.id ? `Launch ${form.id}` : 'Add Launch'}
      widthClassName="w-[1100px] max-w-[calc(100vw-2rem)]"
      footer={
        <div className="flex items-center justify-between gap-3">
          <span className="min-w-0 text-[11px] text-status-blocked">{error}</span>
          <div className="flex shrink-0 gap-2">
            <Button variant="outline" size="sm" onClick={onClose} disabled={saving}>
              Cancel
            </Button>
            <Button variant="outline" size="sm" onClick={onPreview} disabled={saving || previewing || !form}>
              {previewing ? 'Previewing' : 'Preview'}
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
        <DetailSection title="Launch Profile">
          <div className="grid gap-3 px-1 py-1">
            <FormField label="ID">
              <input className={inputClass} value={form.id} onChange={(e) => update({ id: e.target.value })} />
            </FormField>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
              <FormField label="Project">
                <select className={inputClass} value={form.project} onChange={(e) => update({ project: e.target.value })}>
                  {(catalog?.projects ?? []).map((project) => <option key={project.id} value={project.id}>{project.id}</option>)}
                </select>
              </FormField>
              <FormField label="Agent">
                <select className={inputClass} value={form.agent} onChange={(e) => update({ agent: e.target.value })}>
                  {(catalog?.agents ?? []).map((agent) => <option key={agent.id} value={agent.id}>{agent.id}</option>)}
                </select>
              </FormField>
              <FormField label="Provider">
                <select className={inputClass} value={form.provider} onChange={(e) => update({ provider: e.target.value })}>
                  {(catalog?.providers ?? []).map((provider) => <option key={provider.id} value={provider.id}>{provider.id}</option>)}
                </select>
              </FormField>
            </div>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <FormField label="Workspace mode">
                <select className={inputClass} value={form.workspaceMode} onChange={(e) => update({ workspaceMode: e.target.value })}>
                  <option value="default">default</option>
                  <option value="worktree">worktree</option>
                  <option value="shared">shared</option>
                </select>
              </FormField>
              <FormField label="Worktree name">
                <input className={inputClass} value={form.worktreeName} onChange={(e) => update({ worktreeName: e.target.value })} />
              </FormField>
            </div>
            <div className="grid grid-cols-1 gap-2 text-[12px] text-text-soft sm:grid-cols-3">
              <label className="flex items-center gap-2"><input type="checkbox" checked={form.includeProjectBoot} onChange={(e) => update({ includeProjectBoot: e.target.checked })} /> Project boot</label>
              <label className="flex items-center gap-2"><input type="checkbox" checked={form.includeAgentBoot} onChange={(e) => update({ includeAgentBoot: e.target.checked })} /> Agent boot</label>
              <label className="flex items-center gap-2"><input type="checkbox" checked={form.includeKnowledgeBase} onChange={(e) => update({ includeKnowledgeBase: e.target.checked })} /> Knowledge base</label>
            </div>
          </div>
        </DetailSection>

        <DetailSection title="MCP Servers">
          <div className="grid gap-2 px-1 py-1 sm:grid-cols-2">
            {mcpServers.length === 0 ? (
              <p className="text-[12px] text-text-subtle">No MCP servers are configured in the catalog.</p>
            ) : (
              mcpServers.map((server) => (
                <label
                  key={server.id}
                  className={cn(
                    'flex items-start gap-3 rounded border px-3 py-2 text-[12px]',
                    form.mcpServers.includes(server.id)
                      ? 'border-border-strong bg-panel/60'
                      : 'border-border bg-panel/20',
                  )}
                >
                  <input
                    type="checkbox"
                    checked={form.mcpServers.includes(server.id)}
                    onChange={() => toggleMCPServer(server.id)}
                  />
                  <span className="min-w-0">
                    <span className="block font-medium text-text">{server.id}</span>
                    <span className="block text-text-subtle">
                      {server.transport} {server.enabled ? 'enabled' : 'disabled'}
                    </span>
                  </span>
                </label>
              ))
            )}
          </div>
        </DetailSection>

        <DetailSection title="Env Overrides">
          <div className="grid gap-3 px-1 py-1">
            <FormField label="KEY=value per line">
              <textarea
                className={textAreaClass}
                value={form.envOverrides}
                onChange={(e) => update({ envOverrides: e.target.value })}
                placeholder={'FOO=bar\nDEBUG=1'}
              />
            </FormField>
            <p className="text-[11px] text-text-subtle">
              Launch env overrides are persisted in catalog YAML. Keep secrets in provider env passthrough, not here.
            </p>
          </div>
        </DetailSection>

        <DetailSection title="Native Files">
          <InjectionListEditor
            items={form.nativeFiles}
            allowSkill
            addLabel="Add native file"
            onChange={(next) => updateFiles('nativeFiles', next)}
          />
        </DetailSection>

        <DetailSection title="Boot Overlay">
          <InjectionListEditor
            items={form.bootDirOverlay}
            addLabel="Add overlay file"
            onChange={(next) => updateFiles('bootDirOverlay', next)}
          />
        </DetailSection>

        <DetailSection title="Preview">
          <div className="grid gap-3 px-1 py-1">
            {!preview ? (
              <p className="text-[12px] text-text-subtle">
                Run Preview to validate the launch profile and inspect the resulting launch plan before saving.
              </p>
            ) : (
              <>
                <div className="grid gap-3 md:grid-cols-3">
                  <div className="rounded border border-border bg-panel/20 p-3">
                    <div className="text-[11px] uppercase tracking-[.14em] text-text-subtle">Write Target</div>
                    <p className="mt-2 break-all font-mono text-[12px] text-text">
                      {preview.target_path ?? 'Unavailable'}
                    </p>
                  </div>
                  <div className="rounded border border-border bg-panel/20 p-3">
                    <div className="text-[11px] uppercase tracking-[.14em] text-text-subtle">Backup On Save</div>
                    <p className="mt-2 text-[12px] text-text">
                      {preview.will_create_backup ? 'Yes, overwrite will create a timestamped .bak file.' : 'No, this is a new launch file.'}
                    </p>
                  </div>
                  <div className="rounded border border-border bg-panel/20 p-3">
                    <div className="text-[11px] uppercase tracking-[.14em] text-text-subtle">Comment Preservation</div>
                    <p className="mt-2 text-[12px] text-text">
                      {preview.comment_loss_risk ? 'Comments and hand formatting will be rewritten on save.' : 'No existing YAML comments are at risk.'}
                    </p>
                  </div>
                </div>
                {(preview.warnings ?? []).length > 0 && (
                  <div className="rounded border border-border bg-panel/30 p-3 text-[12px] text-text-soft">
                    {(preview.warnings ?? []).map((warning) => (
                      <p key={warning}>{warning}</p>
                    ))}
                  </div>
                )}
                <div className="grid gap-3 xl:grid-cols-2">
                  <DiffViewer
                    title="Launch YAML Diff"
                    diff={preview.profile_diff}
                    emptyLabel={preview.existing ? 'No launch YAML changes detected.' : 'Preview unavailable.'}
                  />
                  <div className="min-w-0 rounded border border-border bg-panel/20">
                    <div className="border-b border-border px-3 py-2 text-[11px] uppercase tracking-[.14em] text-text-subtle">
                      Proposed Launch YAML
                    </div>
                    {preview.next_profile_yaml ? (
                      <pre className="max-h-80 overflow-auto px-3 py-3 text-[11px] leading-5 text-text">
                        <code>{preview.next_profile_yaml}</code>
                      </pre>
                    ) : (
                      <p className="px-3 py-3 text-[12px] text-text-subtle">Preview unavailable.</p>
                    )}
                  </div>
                  <DiffViewer
                    title="Resolved Plan Diff"
                    diff={preview.current_plan_error || preview.next_plan_error ? undefined : preview.plan_diff}
                    emptyLabel={preview.current_plan_error || preview.next_plan_error ? 'Plan diff unavailable because plan resolution failed.' : 'No launch plan changes detected.'}
                  />
                  <div className="min-w-0 rounded border border-border bg-panel/20">
                    <div className="border-b border-border px-3 py-2 text-[11px] uppercase tracking-[.14em] text-text-subtle">
                      Proposed Resolved Plan
                    </div>
                    {preview.next_plan_error ? (
                      <p className="px-3 py-3 text-[12px] text-status-blocked">{preview.next_plan_error}</p>
                    ) : preview.next_plan ? (
                      <pre className="max-h-80 overflow-auto px-3 py-3 text-[11px] leading-5 text-text">
                        <code>{preview.next_plan}</code>
                      </pre>
                    ) : (
                      <p className="px-3 py-3 text-[12px] text-text-subtle">Preview unavailable.</p>
                    )}
                  </div>
                </div>
                {preview.current_plan_error && (
                  <p className="text-[12px] text-status-blocked">Current plan resolution failed: {preview.current_plan_error}</p>
                )}
              </>
            )}
          </div>
        </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}

function InjectionListEditor({
  items,
  allowSkill = false,
  addLabel,
  onChange,
}: {
  items: InjectedFileFormState[]
  allowSkill?: boolean
  addLabel: string
  onChange: (next: InjectedFileFormState[]) => void
}) {
  function updateAt(index: number, patch: Partial<InjectedFileFormState>) {
    onChange(items.map((item, itemIndex) => (itemIndex === index ? { ...item, ...patch } : item)))
  }
  function removeAt(index: number) {
    onChange(items.filter((_, itemIndex) => itemIndex !== index))
  }
  function addItem() {
    onChange([...items, emptyInjectedFile()])
  }
  return (
    <div className="grid gap-3 px-1 py-1">
      {items.length === 0 ? (
        <p className="text-[12px] text-text-subtle">No entries configured.</p>
      ) : (
        items.map((item, index) => (
          <div key={`${index}-${item.relPath}-${item.id}`} className="rounded border border-border bg-panel/30 p-3">
            <div className="mb-3 flex items-center justify-between gap-3">
              <span className="text-[11px] uppercase tracking-[.14em] text-text-subtle">Entry {index + 1}</span>
              <Button variant="outline" size="sm" onClick={() => removeAt(index)}>
                Remove
              </Button>
            </div>
            <div className="grid gap-3 sm:grid-cols-2">
              <FormField label="Kind">
                <select
                  className={inputClass}
                  value={item.kind}
                  onChange={(e) =>
                    updateAt(index, {
                      kind: e.target.value,
                      id: e.target.value === 'skill' ? item.id : '',
                      relPath: e.target.value === 'skill' ? '' : item.relPath,
                    })
                  }
                >
                  <option value="raw">raw</option>
                  {allowSkill && <option value="skill">skill</option>}
                </select>
              </FormField>
              <FormField label="Mode">
                <input
                  className={inputClass}
                  value={item.mode}
                  onChange={(e) => updateAt(index, { mode: e.target.value })}
                  placeholder="644 or 0644"
                />
              </FormField>
            </div>
            {item.kind === 'skill' ? (
              <FormField label="Skill ID">
                <input
                  className={inputClass}
                  value={item.id}
                  onChange={(e) => updateAt(index, { id: e.target.value })}
                  placeholder="skill id"
                />
              </FormField>
            ) : (
              <FormField label="Relative path">
                <input
                  className={inputClass}
                  value={item.relPath}
                  onChange={(e) => updateAt(index, { relPath: e.target.value })}
                  placeholder="notes/context.md"
                />
              </FormField>
            )}
            <div className="grid gap-3 sm:grid-cols-2">
              <FormField label="Source file">
                <input
                  className={inputClass}
                  value={item.source}
                  onChange={(e) => updateAt(index, { source: e.target.value })}
                  placeholder="relative/or/absolute/path"
                />
              </FormField>
              <FormField label="Inline content">
                <textarea
                  className={textAreaClass}
                  value={item.content}
                  onChange={(e) => updateAt(index, { content: e.target.value })}
                  placeholder="inline non-secret content"
                />
              </FormField>
            </div>
          </div>
        ))
      )}
      <div className="flex justify-start">
        <Button variant="outline" size="sm" onClick={addItem}>
          {addLabel}
        </Button>
      </div>
      <p className="text-[11px] text-text-subtle">
        Injected file content is persisted at rest via launch plans. Keep secrets out of both inline content and source files.
      </p>
    </div>
  )
}

function SessionActionDialog({
  action,
  saving,
  error,
  onChange,
  onClose,
  onSubmit,
}: {
  action: SessionActionState | null
  saving: boolean
  error: string | null
  onChange: (patch: Partial<SessionActionState>) => void
  onClose: () => void
  onSubmit: () => void
}) {
  const session = action?.detail.session
  const requiresText = action?.kind === 'turn' || action?.kind === 'input'
  const disabled =
    saving ||
    !action ||
    (requiresText && action.text.length === 0) ||
    (action.kind === 'resize' && (action.rows < 1 || action.cols < 1)) ||
    (action.kind === 'resume' && !session?.logical_agent_id)
  const icon =
    action?.kind === 'turn' ? (
      <Send className="h-3.5 w-3.5" />
    ) : action?.kind === 'input' ? (
      <Keyboard className="h-3.5 w-3.5" />
    ) : action?.kind === 'checkpoint' ? (
      <Save className="h-3.5 w-3.5" />
    ) : action?.kind === 'resize' ? (
      <Maximize2 className="h-3.5 w-3.5" />
    ) : action?.kind === 'wait' ? (
      <Hourglass className="h-3.5 w-3.5" />
    ) : (
      <RotateCw className="h-3.5 w-3.5" />
    )

  return (
    <DetailDialog
      open={action !== null}
      onClose={onClose}
      title={action ? sessionActionTitle(action.kind) : 'Session Action'}
      widthClassName="max-w-xl"
      meta={
        session ? (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-text-subtle">
            <CopyableId id={session.id} label={session.id.slice(0, 16)} />
            <span>{session.state}</span>
            {session.logical_agent_id && <span>agent {session.logical_agent_id.slice(0, 12)}</span>}
          </div>
        ) : null
      }
      footer={
        <div className="flex w-full items-center justify-between gap-3">
          <span className="min-w-0 truncate text-[11px] text-status-blocked">{error}</span>
          <div className="flex shrink-0 gap-2">
            <Button variant="outline" size="sm" onClick={onClose} disabled={saving}>
              Cancel
            </Button>
            <Button variant="default" size="sm" onClick={onSubmit} disabled={disabled}>
              {icon}
              {saving ? 'Working' : action ? sessionActionTitle(action.kind) : 'Submit'}
            </Button>
          </div>
        </div>
      }
    >
      {action && session && (
        <DetailSection title="Action">
          <div className="grid gap-3 px-1 py-1">
            {action.kind === 'turn' && (
              <FormField label="Turn text">
                <textarea
                  className={textAreaClass}
                  value={action.text}
                  onChange={(e) => onChange({ text: e.target.value })}
                  placeholder="Instruction to send through the agent turn endpoint"
                />
              </FormField>
            )}

            {action.kind === 'input' && (
              <FormField label="Raw input">
                <textarea
                  className={textAreaClass}
                  value={action.text}
                  onChange={(e) => onChange({ text: e.target.value })}
                  placeholder="Raw PTY bytes, e.g. /status followed by Enter"
                />
              </FormField>
            )}

            {action.kind === 'checkpoint' && (
              <>
                <FormField label="Status">
                  <input
                    className={inputClass}
                    value={action.status}
                    onChange={(e) => onChange({ status: e.target.value })}
                    placeholder="manual"
                  />
                </FormField>
                <FormField label="Summary">
                  <textarea
                    className={textAreaClass}
                    value={action.summary}
                    onChange={(e) => onChange({ summary: e.target.value })}
                    placeholder="What should be captured about this session state?"
                  />
                </FormField>
              </>
            )}

            {action.kind === 'resize' && (
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <FormField label="Rows">
                  <input
                    className={inputClass}
                    type="number"
                    min={1}
                    max={999}
                    value={action.rows}
                    onChange={(e) => onChange({ rows: Number(e.target.value) })}
                  />
                </FormField>
                <FormField label="Columns">
                  <input
                    className={inputClass}
                    type="number"
                    min={1}
                    max={999}
                    value={action.cols}
                    onChange={(e) => onChange({ cols: Number(e.target.value) })}
                  />
                </FormField>
              </div>
            )}

            {action.kind === 'wait' && (
              <div className="rounded border border-border bg-panel/40 p-3 text-[12px] text-text-soft">
                <p>
                  Wait for session <span className="font-mono text-text">{session.id}</span> to
                  exit. This request stays open until the daemon reports a terminal state.
                </p>
              </div>
            )}

            {action.kind === 'resume' && (
              <div className="rounded border border-border bg-panel/40 p-3 text-[12px] text-text-soft">
                <p>
                  Start a new session for logical agent{' '}
                  <span className="font-mono text-text">{session.logical_agent_id || 'unknown'}</span>
                  {' '}from its latest checkpoint and stored launch profile.
                </p>
                <p className="mt-2 text-text-subtle">
                  This does not reopen session <span className="font-mono text-text">{session.id}</span>.
                </p>
              </div>
            )}
          </div>
        </DetailSection>
      )}
    </DetailDialog>
  )
}

function SessionStreamDialog({
  detail,
  onClose,
}: {
  detail: SessionDetailInfo | null
  onClose: () => void
}) {
  const api = useApi()
  const session = detail?.session
  const [output, setOutput] = useState('')
  const [input, setInput] = useState('')
  const [history, setHistory] = useState<string[]>([])
  const [historyIndex, setHistoryIndex] = useState<number | null>(null)
  const [bytes, setBytes] = useState(0)
  const [size, setSize] = useState({ rows: 0, cols: 0 })
  const [error, setError] = useState<string | null>(null)
  const [sending, setSending] = useState(false)
  const [connected, setConnected] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const outputRef = useRef<HTMLPreElement | null>(null)
  const lastResizeRef = useRef('')

  useEffect(() => {
    if (!session) return
    const sessionID = session.id
    const controller = new AbortController()
    const decoder = new TextDecoder()
    abortRef.current = controller
    setOutput('')
    setBytes(0)
    setError(null)
    setConnected(false)

    async function stream() {
      try {
        const response = await fetch(`/api/sessions/attach?id=${encodeURIComponent(sessionID)}`, {
          signal: controller.signal,
        })
        if (!response.ok) {
          throw new Error(await response.text())
        }
        if (!response.body) {
          throw new Error('stream body unavailable')
        }
        setConnected(true)
        const reader = response.body.getReader()
        while (true) {
          const { value, done } = await reader.read()
          if (done) break
          if (!value) continue
          setBytes((current) => current + value.byteLength)
          const text = decoder.decode(value, { stream: true })
          if (text) {
            setOutput((current) => (current + text).slice(-160000))
          }
        }
        const rest = decoder.decode()
        if (rest) setOutput((current) => (current + rest).slice(-160000))
        setConnected(false)
      } catch (err) {
        if (!controller.signal.aborted) {
          setError(err instanceof Error ? err.message : String(err))
          setConnected(false)
        }
      }
    }

    void stream()
    return () => {
      controller.abort()
      if (abortRef.current === controller) abortRef.current = null
    }
  }, [session])

	  useEffect(() => {
    const el = outputRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [output])

  useEffect(() => {
    if (!session || session.state !== 'running') return
    const el = outputRef.current
    if (!el || typeof ResizeObserver === 'undefined') return
    let resizeTimer: number | null = null
    const observer = new ResizeObserver(([entry]) => {
      const { width, height } = entry.contentRect
      const cols = Math.max(20, Math.min(240, Math.floor(width / 7)))
      const rows = Math.max(8, Math.min(80, Math.floor(height / 16)))
      const key = `${rows}x${cols}`
      setSize({ rows, cols })
      if (key === lastResizeRef.current) return
      if (resizeTimer !== null) window.clearTimeout(resizeTimer)
      resizeTimer = window.setTimeout(() => {
        lastResizeRef.current = key
        api.resizeSession(session.id, rows, cols).catch((err: unknown) => {
          setError(err instanceof Error ? err.message : String(err))
        })
      }, 350)
    })
    observer.observe(el)
    return () => {
      observer.disconnect()
      if (resizeTimer !== null) window.clearTimeout(resizeTimer)
    }
  }, [api, session])

  function stopStream() {
    abortRef.current?.abort()
    abortRef.current = null
    setConnected(false)
  }

  function sendInput(raw = input) {
    if (!session || raw.length === 0) return
    const historyEntry = raw.endsWith('\n') ? raw.slice(0, -1) : raw
    setSending(true)
    setError(null)
    api
      .sendSessionInput(session.id, raw)
      .then(() => {
        if (historyEntry.trim()) {
          setHistory((current) => [historyEntry, ...current.filter((item) => item !== historyEntry)].slice(0, 50))
        }
        setHistoryIndex(null)
        setInput('')
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSending(false))
  }

  function recallHistory(direction: 'prev' | 'next') {
    if (history.length === 0) return
    if (direction === 'prev') {
      const nextIndex = historyIndex === null ? 0 : Math.min(history.length - 1, historyIndex + 1)
      setHistoryIndex(nextIndex)
      setInput(history[nextIndex] ?? '')
      return
    }
    if (historyIndex === null) return
    const nextIndex = historyIndex - 1
    if (nextIndex < 0) {
      setHistoryIndex(null)
      setInput('')
      return
    }
    setHistoryIndex(nextIndex)
    setInput(history[nextIndex] ?? '')
  }

  return (
    <DetailDialog
      open={detail !== null}
      onClose={onClose}
      title={session ? `Live ${session.id.slice(0, 16)}` : 'Live Session'}
      widthClassName="w-[820px] max-w-[calc(100vw-2rem)]"
      badge={session ? <StatusBadge status={connected ? 'running' : session.state} /> : null}
      meta={
        session ? (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-text-subtle">
            <CopyableId id={session.id} label={session.id.slice(0, 16)} />
            <span>{bytes} bytes</span>
            {size.rows > 0 && size.cols > 0 && <span>{size.cols}x{size.rows}</span>}
            <span>{connected ? 'connected' : 'closed'}</span>
          </div>
        ) : null
      }
      footer={
        <div className="flex w-full min-w-0 items-center gap-2">
          <input
            className="h-8 min-w-0 flex-1 border border-border bg-bg px-2 font-mono text-[12px] text-text outline-none focus:border-border-strong"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault()
                sendInput(`${input}\n`)
              } else if (e.key === 'ArrowUp') {
                e.preventDefault()
                recallHistory('prev')
              } else if (e.key === 'ArrowDown') {
                e.preventDefault()
                recallHistory('next')
              }
            }}
            placeholder="raw input; Enter sends newline"
            disabled={!session || session.state !== 'running' || sending}
          />
          <div className="flex shrink-0 gap-1">
            <Button
              variant="outline"
              size="sm"
              onClick={() => sendInput('\u0003')}
              disabled={!session || session.state !== 'running' || sending}
              title="Send Ctrl-C"
            >
              C-c
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => sendInput('\u0004')}
              disabled={!session || session.state !== 'running' || sending}
              title="Send Ctrl-D"
            >
              C-d
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => sendInput('\u000c')}
              disabled={!session || session.state !== 'running' || sending}
              title="Send Ctrl-L"
            >
              C-l
            </Button>
          </div>
          <Button
            variant="default"
            size="sm"
            onClick={() => sendInput(input)}
            disabled={!session || session.state !== 'running' || input.length === 0 || sending}
          >
            <Send className="h-3.5 w-3.5" />
            {sending ? 'Sending' : 'Send'}
          </Button>
          <div className="flex shrink-0 gap-2">
            <Button variant="outline" size="sm" onClick={stopStream} disabled={!connected}>
              Stop
            </Button>
            <Button variant="outline" size="sm" onClick={onClose}>
              Close
            </Button>
          </div>
        </div>
      }
    >
      <section className="flex h-full min-h-0 flex-col border-t border-border-strong bg-bg">
        <pre
          ref={outputRef}
          className="min-h-[292px] flex-1 overflow-auto whitespace-pre-wrap break-words px-4 py-3 font-mono text-[11px] leading-relaxed text-text-soft"
        >
          {output || (connected ? 'Waiting for session output...' : 'Opening stream...')}
        </pre>
        {error && <p className="border-t border-border-strong px-4 py-2 text-[11px] text-status-blocked">{error}</p>}
      </section>
    </DetailDialog>
  )
}

function SessionCleanupDialog({
  form,
  saving,
  error,
  onChange,
  onClose,
  onPreview,
  onCleanup,
}: {
  form: CleanupFormState | null
  saving: boolean
  error: string | null
  onChange: (next: CleanupFormState | null) => void
  onClose: () => void
  onPreview: () => void
  onCleanup: () => void
}) {
  function update(patch: Partial<CleanupFormState>) {
    if (form) onChange({ ...form, ...patch, previewCount: patch.previewCount ?? null })
  }
  return (
    <DetailDialog
      open={form !== null}
      onClose={onClose}
      title="Session Cleanup"
      widthClassName="max-w-xl"
      footer={
        <div className="flex w-full items-center justify-between gap-3">
          <span className="min-w-0 truncate text-[11px] text-status-blocked">{error}</span>
          <div className="flex shrink-0 gap-2">
            <Button variant="outline" size="sm" onClick={onClose} disabled={saving}>
              Cancel
            </Button>
            <Button variant="outline" size="sm" onClick={onPreview} disabled={saving || !form}>
              {saving ? 'Checking' : 'Preview'}
            </Button>
            <Button
              variant="default"
              size="sm"
              onClick={onCleanup}
              disabled={saving || !form || form.previewCount === null || form.previewCount === 0}
            >
              Review Delete
            </Button>
          </div>
        </div>
      }
    >
      {form && (
        <DetailSection title="Retention">
          <div className="grid gap-3 px-1 py-1">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <FormField label="Older than days">
                <input
                  className={inputClass}
                  type="number"
                  min={1}
                  max={3650}
                  value={form.olderThanDays}
                  onChange={(e) => update({ olderThanDays: Number(e.target.value) })}
                />
              </FormField>
              <FormField label="Limit">
                <input
                  className={inputClass}
                  type="number"
                  min={1}
                  max={1000}
                  value={form.limit}
                  onChange={(e) => update({ limit: Number(e.target.value) })}
                />
              </FormField>
            </div>
            <div className="rounded border border-border bg-panel/40 p-3 text-[12px] text-text-soft">
              {form.previewCount === null ? (
                <p>Preview checks terminal sessions before deleting anything.</p>
              ) : (
                <p>
                  Preview found <span className="font-mono text-text">{form.previewCount}</span>{' '}
                  terminal sessions eligible for cleanup.
                </p>
              )}
            </div>
          </div>
        </DetailSection>
      )}
    </DetailDialog>
  )
}

function SessionStopConfirmDialog({
  state,
  saving,
  error,
  onClose,
  onConfirm,
}: {
  state: StopConfirmState | null
  saving: boolean
  error: string | null
  onClose: () => void
  onConfirm: (state: StopConfirmState) => void
}) {
  const [typed, setTyped] = useState('')
  useEffect(() => {
    setTyped('')
  }, [state?.session.id])

  const activeAttachments = state?.detail?.attachments.filter((attachment) => !attachment.detached_at).length ?? 0
  const expected = state?.session.id.slice(0, 12) ?? ''
  const canConfirm = state !== null && typed.trim() === expected

  return (
    <DetailDialog
      open={state !== null}
      onClose={onClose}
      title={state ? `Stop ${state.session.id.slice(0, 12)}` : 'Stop Session'}
      widthClassName="max-w-xl"
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
              onClick={() => state && onConfirm(state)}
              disabled={saving || !canConfirm}
            >
              {saving ? 'Stopping' : 'Stop Live Runtime'}
            </Button>
          </div>
        </div>
      }
    >
      {state && (
        <DetailSection title="Confirm Stop">
          <div className="grid gap-3 px-1 py-1">
            <div className="rounded border border-border bg-panel/40 p-3 text-[12px] text-text-soft">
              <p>
                This asks the daemon to stop the live runtime for session{' '}
                <span className="font-mono text-text">{state.session.id}</span>. If successful, the
                session moves to a terminal state and attach/live input ends.
              </p>
              {activeAttachments > 0 ? (
                <p className="mt-2 text-status-blocked">
                  {activeAttachments} active client attachment(s) will lose the live runtime.
                </p>
              ) : (
                <p className="mt-2 text-text-subtle">
                  Attachment count is {state.detail ? '0 active' : 'unavailable from table view'}.
                </p>
              )}
            </div>
            <div className="grid grid-cols-[8rem_minmax(0,1fr)] gap-x-4 gap-y-2 text-[12px]">
              <span className="text-text-subtle">State</span>
              <span className="text-text-soft">{sessionLifecycleLabel(state.session.state)}</span>
              <span className="text-text-subtle">Provider</span>
              <span className="text-text-soft">{state.session.provider_id || '—'}</span>
              <span className="text-text-subtle">Logical agent</span>
              <span className="text-text-soft">{state.session.logical_agent_id || '—'}</span>
            </div>
            <FormField label={`Type ${expected} to confirm`}>
              <input className={inputClass} value={typed} onChange={(e) => setTyped(e.target.value)} />
            </FormField>
          </div>
        </DetailSection>
      )}
    </DetailDialog>
  )
}

function SessionCleanupConfirmDialog({
  open,
  form,
  confirmText,
  saving,
  error,
  onChange,
  onClose,
  onConfirm,
}: {
  open: boolean
  form: CleanupFormState | null
  confirmText: string
  saving: boolean
  error: string | null
  onChange: (value: string) => void
  onClose: () => void
  onConfirm: () => void
}) {
  const previewCount = form?.previewCount ?? 0
  const expected = `DELETE ${previewCount}`
  const canConfirm = confirmText.trim() === expected

  return (
    <DetailDialog
      open={open}
      onClose={onClose}
      title="Confirm Session Cleanup"
      widthClassName="max-w-xl"
      footer={
        <div className="flex w-full items-center justify-between gap-3">
          <span className="min-w-0 truncate text-[11px] text-status-blocked">{error}</span>
          <div className="flex shrink-0 gap-2">
            <Button variant="outline" size="sm" onClick={onClose} disabled={saving}>
              Back
            </Button>
            <Button variant="default" size="sm" onClick={onConfirm} disabled={saving || !canConfirm}>
              {saving ? 'Deleting' : 'Delete Sessions'}
            </Button>
          </div>
        </div>
      }
    >
      {form && (
        <DetailSection title="Delete Ended Sessions">
          <div className="grid gap-3 px-1 py-1">
            <div className="rounded border border-border bg-panel/40 p-3 text-[12px] text-text-soft">
              <p>
                Delete <span className="font-mono text-text">{previewCount}</span> terminal session
                row(s) older than <span className="font-mono text-text">{form.olderThanDays}</span> days,
                up to the current limit of <span className="font-mono text-text">{form.limit}</span>.
              </p>
              <p className="mt-2 text-text-subtle">
                This removes session rows and their persisted events, launch plans, proxy events, and attachments from the state DB.
              </p>
            </div>
            <FormField label={`Type ${expected} to confirm`}>
              <input className={inputClass} value={confirmText} onChange={(e) => onChange(e.target.value)} />
            </FormField>
          </div>
        </DetailSection>
      )}
    </DetailDialog>
  )
}

function LogicalAgentPolicyDialog({
  form,
  saving,
  error,
  onChange,
  onClose,
  onSave,
}: {
  form: LogicalAgentPolicyFormState | null
  saving: boolean
  error: string | null
  onChange: (next: LogicalAgentPolicyFormState | null) => void
  onClose: () => void
  onSave: () => void
}) {
  function update(patch: Partial<LogicalAgentPolicyFormState>) {
    if (form) onChange({ ...form, ...patch })
  }
  return (
    <DetailDialog
      open={form !== null}
      onClose={onClose}
      title={form ? `Policy ${form.logicalAgentId}` : 'Logical Agent Policy'}
      widthClassName="max-w-xl"
      footer={
        <div className="flex w-full items-center justify-between gap-3">
          <span className="min-w-0 truncate text-[11px] text-status-blocked">{error}</span>
          <div className="flex shrink-0 gap-2">
            <Button variant="outline" size="sm" onClick={onClose} disabled={saving}>
              Close
            </Button>
            <Button variant="default" size="sm" onClick={onSave} disabled={saving || !form}>
              {saving ? 'Saving' : 'Save Policy'}
            </Button>
          </div>
        </div>
      }
    >
      {form && (
        <>
          <DetailSection title="Checkpoint Policy">
            <div className="grid gap-3 px-1 py-1">
              <div className="grid grid-cols-[8rem_minmax(0,1fr)] gap-x-4 gap-y-2 text-[12px]">
                <span className="text-text-subtle">Name</span>
                <span className="text-text-soft">{form.name || '—'}</span>
                <span className="text-text-subtle">Launch</span>
                <span className="text-text-soft">{form.launchId || '—'}</span>
                <span className="text-text-subtle">Updated</span>
                <span className="text-text-soft">{form.updatedAt ? formatRelativeTime(form.updatedAt) : '—'}</span>
              </div>
              <FormField label="Policy">
                <select
                  className={inputClass}
                  value={form.checkpointPolicy}
                  onChange={(e) => update({ checkpointPolicy: e.target.value })}
                >
                  <option value="manual">manual</option>
                  <option value="on_stop">on_stop</option>
                </select>
              </FormField>
              <FormField label="Auto-checkpoint status">
                <input
                  className={inputClass}
                  value={form.checkpointStatus}
                  onChange={(e) => update({ checkpointStatus: e.target.value })}
                  disabled={form.checkpointPolicy !== 'on_stop'}
                  placeholder={form.checkpointPolicy === 'on_stop' ? 'auto-stop' : 'Only used with on_stop'}
                />
              </FormField>
              <div className="rounded border border-border bg-panel/40 p-3 text-[12px] text-text-soft">
                {form.checkpointPolicy === 'on_stop' ? (
                  <p>
                    The daemon will create a checkpoint before stopping a live runtime. If the checkpoint write fails, stop is blocked.
                  </p>
                ) : (
                  <p>Stop does not create a checkpoint automatically. Operators can still create checkpoints manually.</p>
                )}
              </div>
            </div>
          </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}
