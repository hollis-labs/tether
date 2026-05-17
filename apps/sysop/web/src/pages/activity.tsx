import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Activity, RefreshCw, Wrench } from 'lucide-react'
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
import type { EventInfo, ToolCallInfo } from '../api/client'
import { TabStrip, type TabItem } from '../components/tab-strip'
import { JsonModal, PayloadActions, PayloadSummary } from '../components/json-payload'

type TabKey = 'events' | 'tool-calls'

/** Open payload in the shared JSON modal. */
type ViewPayload = { title: string; raw: string }

/** Small ok/error pill — colored from the shared status tokens. */
function OkPill({ ok }: { ok: boolean }) {
  return (
    <span
      className={cn(
        'inline-flex items-center rounded border px-2 py-0.5 text-[10px] uppercase tracking-wider',
        ok
          ? 'border-status-done/40 bg-status-done/10 text-status-done'
          : 'border-status-blocked/40 bg-status-blocked/10 text-status-blocked',
      )}
    >
      {ok ? 'ok' : 'error'}
    </span>
  )
}

export function ActivityPage() {
  const api = useApi()
  const [tab, setTab] = useState<TabKey>('events')
  const [events, setEvents] = useState<EventInfo[] | null>(null)
  const [toolCalls, setToolCalls] = useState<ToolCallInfo[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [payloadView, setPayloadView] = useState<ViewPayload | null>(null)
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
        cell: (t) => <OkPill ok={t.ok} />,
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
  const toolErrors = toolCallList.filter((t) => !t.ok).length

  const tabs: TabItem<TabKey>[] = [
    { key: 'events', label: 'Events', icon: Activity, count: eventList.length },
    { key: 'tool-calls', label: 'Tool Calls', icon: Wrench, count: toolCallList.length },
  ]

  const summaryCards =
    tab === 'events'
      ? [
          { label: 'Events', value: events ? eventList.length : '...' },
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
      : [
          { label: 'Tool Calls', value: toolCalls ? toolCallList.length : '...' },
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

  if (error) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not load activity" description={error} />
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
        {tab === 'events'
          ? 'Session, daemon, and broker lifecycle events — newest first (latest 500).'
          : 'MCP proxy tool-call telemetry — newest first (ring buffer, latest 500).'}
      </p>

      <div ref={scrollRef} className="min-h-0 flex-1 overflow-auto">
        {tab === 'events' ? (
          <DataTable
            items={eventList}
            columns={eventColumns}
            getRowId={(e) => String(e.seq)}
            initialSort={{ key: 'at', dir: 'desc' }}
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
        ) : (
          <DataTable
            items={toolCallList}
            columns={toolCallColumns}
            getRowId={(t) => String(t.id)}
            initialSort={{ key: 'timestamp', dir: 'desc' }}
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
        )}
      </div>

      <JsonModal
        open={payloadView !== null}
        onClose={() => setPayloadView(null)}
        title={payloadView?.title ?? ''}
        raw={payloadView?.raw ?? ''}
      />
    </div>
  )
}
