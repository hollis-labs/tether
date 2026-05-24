import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Activity, RefreshCw, Tags, Wrench } from 'lucide-react'
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
  JsonViewer,
} from '@hollis-labs/sysop-ui/ui'
import { DataTable, type ColumnDef } from '@hollis-labs/sysop-ui/data'
import { ListPageLayout, TabStrip, type TabStripItem } from '@hollis-labs/sysop-ui/layout'
import { useApi } from '../api/context'
import type { EventInfo, ToolCallInfo } from '../api/client'
import { JsonModal, PayloadActions, PayloadSummary } from '../components/json-payload'

type TabKey = 'events' | 'tool-calls' | 'scopes'

/** Open payload in the shared JSON modal. */
type ViewPayload = { title: string; raw: string }
type DetailView =
  | { kind: 'event'; item: EventInfo }
  | { kind: 'tool-call'; item: ToolCallInfo }
  | { kind: 'scope'; item: ScopeInfo }

interface ScopeInfo {
  scope: string
  event_count: number
  session_count: number
  kind_count: number
  latest_at: string
  latest_kind: string
  latest_seq: number
  kinds: { name: string; count: number }[]
}

function DetailField({ label, children }: { label: string; children: ReactNode }) {
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

function parsePayload(raw?: string): unknown {
  if (!raw) return null
  try {
    return JSON.parse(raw)
  } catch {
    return raw
  }
}

export function ActivityPage() {
  const api = useApi()
  const [tab, setTab] = useState<TabKey>('events')
  const [events, setEvents] = useState<EventInfo[] | null>(null)
  const [toolCalls, setToolCalls] = useState<ToolCallInfo[] | null>(null)
  const [eventTotal, setEventTotal] = useState<number | null>(null)
  const [toolCallTotal, setToolCallTotal] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [payloadView, setPayloadView] = useState<ViewPayload | null>(null)
  const [detailView, setDetailView] = useState<DetailView | null>(null)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const view = useCallback((v: ViewPayload) => setPayloadView(v), [])

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    Promise.all([api.getEvents(), api.getToolCalls()])
      .then(([eventsInfo, toolCallsInfo]) => {
        if (cancelled) return
        setEvents(eventsInfo.events ?? [])
        setToolCalls(toolCallsInfo.tool_calls ?? [])
        setEventTotal(eventsInfo.total ?? eventsInfo.events?.length ?? 0)
        setToolCallTotal(toolCallsInfo.total ?? toolCallsInfo.tool_calls?.length ?? 0)
        setError(eventsInfo.error ?? toolCallsInfo.error ?? null)
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

  const eventColumns = useMemo<ColumnDef<EventInfo>[]>(
    () => [
      {
        key: 'at',
        header: 'When',
        cell: (e) => <span className="text-[11px] text-text-soft">{formatRelativeTime(e.at)}</span>,
        sortValue: (e) => e.at,
      },
      {
        key: 'scope',
        header: 'Scope',
        cell: (e) => (
          <span className="text-[11px] uppercase tracking-[.12em] text-text-soft">{e.scope}</span>
        ),
        sortValue: (e) => e.scope,
      },
      {
        key: 'kind',
        header: 'Kind',
        cell: (e) => <span className="font-mono text-[12px] text-text">{e.kind}</span>,
        sortValue: (e) => e.kind,
      },
      {
        key: 'session',
        header: 'Session',
        cell: (e) =>
          e.session_id ? (
            <CopyableId id={e.session_id} label={e.session_id.slice(0, 12)} />
          ) : (
            <span className="text-[11px] text-text-subtle">—</span>
          ),
        sortValue: (e) => e.session_id ?? '',
      },
      {
        key: 'summary',
        header: 'Detail',
        width: 'fill',
        cell: (e) => <PayloadSummary raw={e.payload ?? ''} />,
        sortValue: (e) => e.payload ?? '',
      },
      {
        key: 'payload',
        header: 'Payload',
        cell: (e) => (
          <PayloadActions
            raw={e.payload ?? ''}
            onView={() => view({ title: `Event ${e.seq} — ${e.kind}`, raw: e.payload ?? '' })}
          />
        ),
      },
    ],
    [view],
  )

  const toolCallColumns = useMemo<ColumnDef<ToolCallInfo>[]>(
    () => [
      {
        key: 'timestamp',
        header: 'When',
        cell: (t) => (
          <span className="text-[11px] text-text-soft">{formatRelativeTime(t.timestamp)}</span>
        ),
        sortValue: (t) => t.timestamp,
      },
      {
        key: 'tool_name',
        header: 'Tool',
        width: 'fill',
        cell: (t) => <span className="font-mono text-[12px] text-text">{t.tool_name}</span>,
        sortValue: (t) => t.tool_name,
      },
      {
        key: 'server',
        header: 'Server',
        cell: (t) => <span className="text-[11px] text-text-soft">{t.server || 'native'}</span>,
        sortValue: (t) => t.server ?? '',
      },
      {
        key: 'duration',
        header: 'Duration',
        align: 'right',
        cell: (t) => (
          <span className="font-mono text-[11px] tabular-nums text-text-soft">{t.duration_ms} ms</span>
        ),
        sortValue: (t) => t.duration_ms,
      },
      {
        key: 'ok',
        header: 'Result',
        cell: (t) => (
          <Pill tone={t.ok ? 'success' : 'danger'}>{t.ok ? 'ok' : 'error'}</Pill>
        ),
        sortValue: (t) => (t.ok ? 1 : 0),
      },
      {
        key: 'error',
        header: 'Error',
        cell: (t) => (
          <PayloadActions
            raw={t.error ?? ''}
            viewLabel="View error"
            onView={() => view({ title: `Tool call error — ${t.tool_name}`, raw: t.error ?? '' })}
          />
        ),
      },
    ],
    [view],
  )

  const eventList = events ?? []
  const toolCallList = toolCalls ?? []
  const scopeList = useMemo<ScopeInfo[]>(() => {
    const byScope = new Map<
      string,
      {
        events: EventInfo[]
        sessions: Set<string>
        kinds: Map<string, number>
      }
    >()
    for (const event of eventList) {
      const scope = event.scope || 'unknown'
      let entry = byScope.get(scope)
      if (!entry) {
        entry = { events: [], sessions: new Set(), kinds: new Map() }
        byScope.set(scope, entry)
      }
      entry.events.push(event)
      if (event.session_id) entry.sessions.add(event.session_id)
      entry.kinds.set(event.kind, (entry.kinds.get(event.kind) ?? 0) + 1)
    }
    return [...byScope.entries()]
      .map(([scope, entry]) => {
        const latest = [...entry.events].sort((a, b) => b.at.localeCompare(a.at))[0]
        const kinds = [...entry.kinds.entries()]
          .map(([name, count]) => ({ name, count }))
          .sort((a, b) => b.count - a.count || a.name.localeCompare(b.name))
        return {
          scope,
          event_count: entry.events.length,
          session_count: entry.sessions.size,
          kind_count: entry.kinds.size,
          latest_at: latest?.at ?? '',
          latest_kind: latest?.kind ?? '',
          latest_seq: latest?.seq ?? 0,
          kinds,
        }
      })
      .sort((a, b) => b.event_count - a.event_count || a.scope.localeCompare(b.scope))
  }, [eventList])

  const scopeColumns = useMemo<ColumnDef<ScopeInfo>[]>(
    () => [
      {
        key: 'scope',
        header: 'Scope',
        width: 'fill',
        cell: (s) => <CopyableId id={s.scope} />,
        sortValue: (s) => s.scope,
      },
      {
        key: 'events',
        header: 'Events',
        align: 'right',
        cell: (s) => (
          <span className="font-mono text-[11px] tabular-nums text-text-soft">
            {s.event_count}
          </span>
        ),
        sortValue: (s) => s.event_count,
      },
      {
        key: 'sessions',
        header: 'Sessions',
        align: 'right',
        cell: (s) => (
          <span className="font-mono text-[11px] tabular-nums text-text-soft">
            {s.session_count}
          </span>
        ),
        sortValue: (s) => s.session_count,
      },
      {
        key: 'kinds',
        header: 'Kinds',
        align: 'right',
        cell: (s) => (
          <span className="font-mono text-[11px] tabular-nums text-text-soft">
            {s.kind_count}
          </span>
        ),
        sortValue: (s) => s.kind_count,
      },
      {
        key: 'latest_kind',
        header: 'Latest Kind',
        cell: (s) => <span className="font-mono text-[12px] text-text">{s.latest_kind}</span>,
        sortValue: (s) => s.latest_kind,
      },
      {
        key: 'latest_at',
        header: 'Latest',
        cell: (s) => (
          <span className="text-[11px] text-text-soft">
            {s.latest_at ? formatRelativeTime(s.latest_at) : '—'}
          </span>
        ),
        sortValue: (s) => s.latest_at,
      },
    ],
    [],
  )

  const toolErrors = toolCallList.filter((t) => !t.ok).length

  const tabs: TabStripItem<TabKey>[] = [
    {
      key: 'events',
      label: 'Events',
      icon: <Activity className="h-3.5 w-3.5" />,
      count: eventTotal ?? eventList.length,
    },
    {
      key: 'tool-calls',
      label: 'Tool Calls',
      icon: <Wrench className="h-3.5 w-3.5" />,
      count: toolCallTotal ?? toolCallList.length,
    },
    {
      key: 'scopes',
      label: 'Scopes',
      icon: <Tags className="h-3.5 w-3.5" />,
      count: scopeList.length,
    },
  ]

  const summaryCards =
    tab === 'events'
      ? [
          { label: 'Events', value: eventTotal ?? (events ? eventList.length : '...') },
          {
            label: 'Sessions',
            value: events
              ? new Set(eventList.map((e) => e.session_id).filter(Boolean)).size
              : '...',
          },
          {
            label: 'Scopes',
            value: events ? new Set(eventList.map((e) => e.scope)).size : '...',
          },
        ]
      : tab === 'tool-calls'
        ? [
            { label: 'Tool Calls', value: toolCallTotal ?? (toolCalls ? toolCallList.length : '...') },
            {
              label: 'OK',
              value: toolCalls ? toolCallList.length - toolErrors : '...',
              accentColor: 'var(--color-status-done)',
            },
            {
              label: 'Errors',
              value: toolCalls ? toolErrors : '...',
              accentColor: 'var(--color-status-blocked)',
            },
          ]
        : [
            { label: 'Scopes', value: events ? scopeList.length : '...' },
            { label: 'Events', value: events ? eventList.length : '...' },
            {
              label: 'Sessions',
              value: events
                ? new Set(eventList.map((e) => e.session_id).filter(Boolean)).size
                : '...',
            },
          ]

  if (error) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not load activity" description={error} />
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
            {tab === 'events'
              ? 'Session, daemon, and broker lifecycle events — newest first (latest 500).'
              : tab === 'tool-calls'
                ? 'MCP proxy tool-call telemetry — newest first (ring buffer, latest 500).'
                : 'Event scopes aggregated from the latest 500 activity events.'}
          </p>
        }
      >
        {tab === 'events' ? (
          <DataTable
            items={eventList}
            columns={eventColumns}
            getRowId={(e) => String(e.seq)}
            initialSort={{ key: 'at', dir: 'desc' }}
            onRowOpen={(_, item) => setDetailView({ kind: 'event', item })}
            rowAriaLabel={(e) => `Open event ${e.seq}`}
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading events...' : 'No events'}
                description={
                  loading
                    ? 'Reading the configured Tether state DB.'
                    : 'The events table has no rows yet.'
                }
              />
            }
          />
        ) : tab === 'tool-calls' ? (
          <DataTable
            items={toolCallList}
            columns={toolCallColumns}
            getRowId={(t) => String(t.id)}
            initialSort={{ key: 'timestamp', dir: 'desc' }}
            onRowOpen={(_, item) => setDetailView({ kind: 'tool-call', item })}
            rowAriaLabel={(t) => `Open tool call ${t.id}`}
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading tool calls...' : 'No tool calls'}
                description={
                  loading
                    ? 'Reading the configured Tether state DB.'
                    : 'The proxy_events table has no rows yet.'
                }
              />
            }
          />
        ) : (
          <DataTable
            items={scopeList}
            columns={scopeColumns}
            getRowId={(s) => s.scope}
            initialSort={{ key: 'events', dir: 'desc' }}
            onRowOpen={(_, item) => setDetailView({ kind: 'scope', item })}
            rowAriaLabel={(s) => `Open scope ${s.scope}`}
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading scopes...' : 'No scopes'}
                description={
                  loading
                    ? 'Reading the configured Tether state DB.'
                    : 'No event scopes were found in the loaded activity window.'
                }
              />
            }
          />
        )}
      </ListPageLayout>

      <JsonModal
        open={payloadView !== null}
        onClose={() => setPayloadView(null)}
        title={payloadView?.title ?? ''}
        raw={payloadView?.raw ?? ''}
      />

      <ActivityDetailDialog detail={detailView} onClose={() => setDetailView(null)} />
    </>
  )
}

function ActivityDetailDialog({
  detail,
  onClose,
}: {
  detail: DetailView | null
  onClose: () => void
}) {
  const isEvent = detail?.kind === 'event'
  const event = isEvent ? detail.item : null
  const toolCall = detail?.kind === 'tool-call' ? detail.item : null
  const scope = detail?.kind === 'scope' ? detail.item : null

  return (
    <DetailDialog
      open={detail !== null}
      onClose={onClose}
      title={
        event
          ? `Event ${event.seq}`
          : toolCall
            ? `Tool call ${toolCall.id}`
            : scope
              ? `Scope ${scope.scope}`
              : ''
      }
      badge={
        toolCall ? <Pill tone={toolCall.ok ? 'success' : 'danger'}>{toolCall.ok ? 'ok' : 'error'}</Pill> : null
      }
      meta={
        event ? (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span className="inline-flex items-center gap-1">
              <span className="text-text-subtle/70">id</span>
              <CopyValue value={event.seq} />
            </span>
            <span className="inline-flex items-center gap-1">
              <span className="text-text-subtle/70">kind</span>
              <CopyValue value={event.kind} />
            </span>
            {event.session_id && (
              <span className="inline-flex items-center gap-1">
                <span className="text-text-subtle/70">session</span>
                <CopyValue value={event.session_id} label={event.session_id.slice(0, 12)} />
              </span>
            )}
          </div>
        ) : toolCall ? (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span className="inline-flex items-center gap-1">
              <span className="text-text-subtle/70">id</span>
              <CopyValue value={toolCall.id} />
            </span>
            <span className="inline-flex items-center gap-1">
              <span className="text-text-subtle/70">tool</span>
              <CopyValue value={toolCall.tool_name} />
            </span>
            {toolCall.session_id && (
              <span className="inline-flex items-center gap-1">
                <span className="text-text-subtle/70">session</span>
                <CopyValue value={toolCall.session_id} label={toolCall.session_id.slice(0, 12)} />
              </span>
            )}
          </div>
        ) : scope ? (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span className="inline-flex items-center gap-1">
              <span className="text-text-subtle/70">scope</span>
              <CopyValue value={scope.scope} />
            </span>
            <span>{scope.event_count} events</span>
            <span>{scope.kind_count} kinds</span>
          </div>
        ) : null
      }
    >
      {event && (
        <>
          <DetailSection title="Event">
            <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <DetailField label="Event ID"><CopyValue value={event.seq} /></DetailField>
              <DetailField label="Sequence"><CopyValue value={event.seq} /></DetailField>
              <DetailField label="When">{formatRelativeTime(event.at)}</DetailField>
              <DetailField label="Timestamp"><CopyValue value={event.at} /></DetailField>
              <DetailField label="Scope"><CopyValue value={event.scope} /></DetailField>
              <DetailField label="Kind"><CopyValue value={event.kind} /></DetailField>
              <DetailField label="Session">
                {event.session_id ? (
                  <CopyValue value={event.session_id} label={event.session_id.slice(0, 12)} />
                ) : (
                  '—'
                )}
              </DetailField>
            </dl>
          </DetailSection>
          <DetailSection title="Payload">
            {event.payload ? (
              <JsonViewer
                value={parsePayload(event.payload)}
                className="rounded-none border-0 bg-transparent px-0 py-0"
              />
            ) : (
              <p className="text-[12px] text-text-subtle">No payload recorded for this event.</p>
            )}
          </DetailSection>
        </>
      )}

      {toolCall && (
        <>
          <DetailSection title="Tool Call">
            <dl className="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <DetailField label="ID"><CopyValue value={toolCall.id} /></DetailField>
              <DetailField label="When">{formatRelativeTime(toolCall.timestamp)}</DetailField>
              <DetailField label="Timestamp"><CopyValue value={toolCall.timestamp} /></DetailField>
              <DetailField label="Tool"><CopyValue value={toolCall.tool_name} /></DetailField>
              <DetailField label="Server"><CopyValue value={toolCall.server || 'native'} /></DetailField>
              <DetailField label="Session">
                {toolCall.session_id ? (
                  <CopyValue value={toolCall.session_id} label={toolCall.session_id.slice(0, 12)} />
                ) : (
                  '—'
                )}
              </DetailField>
              <DetailField label="Duration"><CopyValue value={`${toolCall.duration_ms} ms`} /></DetailField>
              <DetailField label="Result"><CopyValue value={toolCall.ok ? 'ok' : 'error'} /></DetailField>
              <DetailField label="Args fingerprint">
                {toolCall.args_schema_fp ? <CopyValue value={toolCall.args_schema_fp} /> : '—'}
              </DetailField>
            </dl>
          </DetailSection>
          <DetailSection title="Payload">
            {toolCall.payload ? (
              <JsonViewer
                value={parsePayload(toolCall.payload)}
                className="rounded-none border-0 bg-transparent px-0 py-0"
              />
            ) : (
              <p className="text-[12px] text-text-subtle">
                No raw tool payload is persisted; only sanitized tool-call metadata is stored.
              </p>
            )}
          </DetailSection>
          <DetailSection title="Error">
            {toolCall.error ? (
              <pre className="whitespace-pre-wrap break-words bg-transparent px-0 py-0 font-mono text-xs leading-5 text-text-muted">
                {toolCall.error}
              </pre>
            ) : (
              <p className="text-[12px] text-text-subtle">No error recorded for this tool call.</p>
            )}
          </DetailSection>
        </>
      )}

      {scope && (
        <>
          <DetailSection title="Scope">
            <dl className="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <DetailField label="Scope"><CopyValue value={scope.scope} /></DetailField>
              <DetailField label="Events">{scope.event_count}</DetailField>
              <DetailField label="Sessions">{scope.session_count}</DetailField>
              <DetailField label="Kinds">{scope.kind_count}</DetailField>
              <DetailField label="Latest kind"><CopyValue value={scope.latest_kind} /></DetailField>
              <DetailField label="Latest event"><CopyValue value={scope.latest_seq} /></DetailField>
              <DetailField label="Latest timestamp">
                {scope.latest_at ? <CopyValue value={scope.latest_at} /> : '—'}
              </DetailField>
            </dl>
          </DetailSection>
          <DetailSection title="Kinds">
            <div className="space-y-1">
              {scope.kinds.map((kind) => (
                <div key={kind.name} className="flex items-center justify-between gap-3 text-[12px]">
                  <CopyValue value={kind.name} />
                  <span className="font-mono text-[11px] tabular-nums text-text-soft">
                    {kind.count}
                  </span>
                </div>
              ))}
            </div>
          </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}
