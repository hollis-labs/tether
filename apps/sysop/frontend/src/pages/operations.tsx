import { useCallback, useEffect, useRef, useState } from 'react'
import { Activity, Boxes, RefreshCw } from 'lucide-react'
import {
  Button,
  CopyableId,
  DataTable,
  DetailDialog,
  DetailSection,
  EmptyState,
  JsonViewer,
  ListPageLayout,
  StatusBadge,
  SummaryCards,
  TabStrip,
  cn,
  formatRelativeTime,
  type ColumnDef,
  type TabStripItem,
} from '@hollis-labs/sysop-ui'
import { useApi } from '../api/context'
import type {
  CatalogInfo,
  HealthInfo,
  LaunchInfo,
  SessionDetailInfo,
  SessionInfo,
  SessionsInfo,
} from '../api/client'
import { CopyButton } from '../components/json-payload'
import { SessionDetailDialog } from '../components/session-detail-dialog'

type TabKey = 'launches' | 'sessions'

function parseJson(raw?: string): unknown {
  if (!raw) return null
  try {
    return JSON.parse(raw)
  } catch {
    return raw
  }
}

const launchColumns: ColumnDef<LaunchInfo>[] = [
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
]

const sessionColumns: ColumnDef<SessionInfo>[] = [
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
    cell: (session) => <StatusBadge status={session.state} />,
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
]

export function OperationsPage() {
  const api = useApi()
  const [tab, setTab] = useState<TabKey>('launches')
  const [health, setHealth] = useState<HealthInfo | null>(null)
  const [catalog, setCatalog] = useState<CatalogInfo | null>(null)
  const [sessions, setSessions] = useState<SessionsInfo | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [detail, setDetail] = useState<SessionDetailInfo | null>(null)
  const [launchDetail, setLaunchDetail] = useState<LaunchInfo | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [detailError, setDetailError] = useState<string | null>(null)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    Promise.all([api.getHealth(), api.getCatalog(), api.getSessions()])
      .then(([healthInfo, catalogInfo, sessionsInfo]) => {
        if (cancelled) return
        setHealth(healthInfo)
        setCatalog(catalogInfo)
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

  const launches = catalog?.launches ?? []
  const sessionList = sessions?.sessions ?? []
  const sessionTotal = sessions?.total ?? sessionList.length
  const runningSessions = sessions?.running ?? sessionList.filter((s) => s.state === 'running').length
  const endedSessions = sessions?.ended ?? Math.max(0, sessionList.length - runningSessions)

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
            label: 'Running',
            value: sessions ? runningSessions : '...',
            accentColor: 'var(--color-status-doing)',
          },
          {
            label: 'Ended',
            value: sessions ? endedSessions : '...',
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
            {tab === 'launches'
              ? health?.catalog_root ?? 'Loading catalog...'
              : 'Latest 50 entries from the configured Tether state DB.'}
          </p>
        }
      >
        {tab === 'launches' ? (
          <DataTable
            items={launches}
            columns={launchColumns}
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
              columns={sessionColumns}
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
      />
      <LaunchDetailDialog launch={launchDetail} onClose={() => setLaunchDetail(null)} />
    </>
  )
}

function LaunchDetailDialog({
  launch,
  onClose,
}: {
  launch: LaunchInfo | null
  onClose: () => void
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
          <div className="flex justify-end gap-2">
            {launch.profile && <CopyButton text={launch.profile} label="Copy profile" />}
            {launch.launch_plan && <CopyButton text={launch.launch_plan} label="Copy plan" />}
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
