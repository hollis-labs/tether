import { useCallback, useEffect, useMemo, useState } from 'react'
import { Activity, Database, Gauge, Mail, Plug, RefreshCw, TerminalSquare } from 'lucide-react'
import {
  Button,
  EmptyState,
  StatusBadge,
  cn,
} from '@hollis-labs/sysop-ui/ui'
import {
  BarList,
  CompositionBars,
  SignalBars,
  IntelligenceRow,
  Kpi,
  KpiGrid,
  MiniTrend,
  Panel,
} from '@hollis-labs/sysop-ui/widgets'
import { useApi } from '../api/context'
import type { NameCount, OverviewInfo } from '../api/client'

function formatDuration(s: number): string {
  if (s <= 0) return '0s'
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  if (h) return `${h}h ${m}m`
  if (m) return `${m}m ${sec}s`
  return `${sec}s`
}

function compact(n: number): string {
  return new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 }).format(n)
}

function rate(part: number, total: number): string {
  if (!total) return '0%'
  return `${Math.round((part / total) * 100)}%`
}

function sumSeries(series: number[]): number {
  return series.reduce((sum, n) => sum + n, 0)
}

function mergeSeries(...series: number[][]): number[] {
  const n = Math.max(0, ...series.map((s) => s.length))
  return Array.from({ length: n }, (_, i) => series.reduce((sum, s) => sum + (s[i] ?? 0), 0))
}

function topLabel(items?: NameCount[]): string {
  return items && items.length > 0 ? items[0].name : 'none'
}

function toBarItems(items?: NameCount[]) {
  return (items ?? []).map((item) => ({ label: item.name, value: item.count }))
}


function DataList({ title, items }: { title: string; items: NameCount[] }) {
  return (
    <div className="min-h-0 p-3">
      <div className="mb-2 text-[10px] uppercase tracking-[.16em] text-text-subtle">{title}</div>
      <BarList items={toBarItems(items)} />
    </div>
  )
}

export function OverviewPage() {
  const api = useApi()
  const [data, setData] = useState<OverviewInfo | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    api
      .getOverview()
      .then((info) => {
        if (cancelled) return
        setData(info)
        setError(info.error ?? null)
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

  const s = data?.sessions
  const t = data?.tool_calls
  const m = data?.messages
  const e = data?.events
  const c = data?.catalog
  const h = data?.health

  const activitySeries = useMemo(
    () => mergeSeries(s?.trend ?? [], t?.trend ?? [], m?.trend ?? [], e?.trend ?? []),
    [s?.trend, t?.trend, m?.trend, e?.trend],
  )
  const runtimeSeries = useMemo(
    () => mergeSeries(s?.trend ?? [], e?.trend ?? []),
    [s?.trend, e?.trend],
  )
  const interactionSeries = useMemo(
    () => mergeSeries(t?.trend ?? [], m?.trend ?? []),
    [t?.trend, m?.trend],
  )
  const activityTotal = sumSeries(activitySeries)
  const unreadRate = m ? rate(m.unread, m.total) : '0%'
  const errorRate = t ? rate(t.errors, t.total) : '0%'

  if (error && !data) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not load overview" description={error} />
      </div>
    )
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col bg-bg">
      <div className="flex shrink-0 items-center justify-between border-b border-border-strong bg-bg px-4 py-2">
        <div className="min-w-0">
          <p className="text-[11px] uppercase tracking-[.18em] text-text-subtle">
            Agent Ops Control Plane
          </p>
          <p className="mt-0.5 truncate font-mono text-[11px] text-text-subtle">
            {h?.catalog_root ?? 'Loading catalog...'}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {h?.status && <StatusBadge status={h.status === 'ok' ? 'done' : 'blocked'} />}
          <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
            <RefreshCw className={cn('h-3.5 w-3.5', loading && 'animate-spin')} />
            Refresh
          </Button>
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-auto p-3">
        <div className="grid gap-3 xl:grid-cols-[minmax(0,2fr)_minmax(20rem,0.85fr)]">
          <Panel
            title="Activity Signal"
            icon={<Activity className="h-3.5 w-3.5" />}
            meta={`${compact(activityTotal)} sampled events`}
          >
            <KpiGrid cols="grid-cols-2 md:grid-cols-6">
              <Kpi label="Sessions" value={compact(s?.total ?? 0)} sub={`${s?.recent_24h ?? 0} / 24h`} />
              <Kpi label="Tool Calls" value={compact(t?.total ?? 0)} sub={`${t?.recent_1h ?? 0} / 1h`} />
              <Kpi label="Messages" value={compact(m?.total ?? 0)} sub={`${m?.recent_24h ?? 0} / 24h`} />
              <Kpi label="Events" value={compact(e?.total ?? 0)} sub={`${e?.recent_1h ?? 0} / 1h`} />
              <Kpi label="Success" value={t ? `${t.success_pct}%` : '...'} sub={`${errorRate} errors`} />
              <Kpi label="Unread" value={unreadRate} sub={`${m?.unread ?? 0} messages`} />
            </KpiGrid>
            <SignalBars
              data={runtimeSeries}
              secondaryData={interactionSeries}
              heightClassName="h-56"
              primaryLabel="Runtime"
              secondaryLabel="Interaction"
            />
            <div className="grid md:grid-cols-4">
              <MiniTrend label="Sessions" value={sumSeries(s?.trend ?? [])} data={s?.trend ?? []} />
              <MiniTrend label="Tools" value={sumSeries(t?.trend ?? [])} data={t?.trend ?? []} />
              <MiniTrend label="Messages" value={sumSeries(m?.trend ?? [])} data={m?.trend ?? []} />
              <MiniTrend label="Events" value={sumSeries(e?.trend ?? [])} data={e?.trend ?? []} />
            </div>
          </Panel>

          <Panel title="Intelligence" icon={<Gauge className="h-3.5 w-3.5" />}>
            <IntelligenceRow
              label="Tool reliability"
              value={t ? `${t.success_pct}%` : '...'}
              status={t && t.success_pct < 95 ? 'blocked' : 'done'}
            />
            <IntelligenceRow
              label="Session completion"
              value={s ? `${s.success_pct}%` : '...'}
              status={s && s.success_pct < 50 ? 'blocked' : 'done'}
            />
            <IntelligenceRow
              label="Slow tool calls"
              value={t?.slow_calls ?? '...'}
              status={t && t.slow_calls > 0 ? 'doing' : 'done'}
            />
            <IntelligenceRow
              label="Inbox pressure"
              value={unreadRate}
              status={m && m.unread > 0 ? 'inbox' : 'done'}
            />
            <IntelligenceRow label="Top tool" value={topLabel(t?.top_tools)} status="indexed" />
            <IntelligenceRow label="Top event" value={topLabel(e?.by_kind)} status="indexed" />
          </Panel>
        </div>

        <div className="mt-3 grid gap-3 xl:grid-cols-4">
          <Panel
            title="Sessions"
            icon={<TerminalSquare className="h-3.5 w-3.5" />}
            meta={`${s?.running ?? 0} running`}
          >
            <KpiGrid>
              <Kpi label="Ended" value={s?.ended ?? 0} />
              <Kpi label="Failed" value={s ? `${s.failure_pct}%` : '...'} />
              <Kpi label="Avg Time" value={s ? formatDuration(s.avg_seconds) : '...'} />
              <Kpi label="Projects" value={s?.by_project?.length ?? 0} />
            </KpiGrid>
            <div className="grid md:grid-cols-2 xl:grid-cols-1">
              <div className="border-b border-border p-3">
                <div className="mb-2 text-[10px] uppercase tracking-[.16em] text-text-subtle">State mix</div>
                <CompositionBars items={toBarItems(s?.by_state)} />
              </div>
              <DataList title="Providers" items={s?.by_provider ?? []} />
            </div>
          </Panel>

          <Panel
            title="Tool Calls"
            icon={<Plug className="h-3.5 w-3.5" />}
            meta={`${t?.sessions ?? 0} sessions`}
          >
            <KpiGrid>
              <Kpi label="p50" value={t ? `${t.p50_ms}ms` : '...'} />
              <Kpi label="p95" value={t ? `${t.p95_ms}ms` : '...'} />
              <Kpi label="Avg" value={t ? `${t.avg_ms}ms` : '...'} />
              <Kpi label="Errors" value={t?.errors ?? 0} accent={t && t.errors > 0 ? 'var(--color-status-blocked)' : undefined} />
            </KpiGrid>
            <DataList title="Top tools" items={t?.top_tools ?? []} />
            <div className="border-t border-border p-3">
              <div className="mb-2 text-[10px] uppercase tracking-[.16em] text-text-subtle">Latency bands</div>
              <CompositionBars items={toBarItems(t?.latency)} />
            </div>
          </Panel>

          <Panel title="Messaging" icon={<Mail className="h-3.5 w-3.5" />}>
            <KpiGrid>
              <Kpi label="Unread" value={m?.unread ?? 0} accent={m && m.unread > 0 ? 'var(--color-status-inbox)' : undefined} />
              <Kpi label="Archived" value={m?.archived ?? 0} />
              <Kpi label="Recent" value={m?.recent_24h ?? 0} />
              <Kpi label="Kinds" value={m?.by_kind?.length ?? 0} />
            </KpiGrid>
            <div className="border-b border-border p-3">
              <div className="mb-2 text-[10px] uppercase tracking-[.16em] text-text-subtle">Scope mix</div>
              <CompositionBars items={toBarItems(m?.by_scope)} />
            </div>
            <DataList title="Message kinds" items={m?.by_kind ?? []} />
          </Panel>

          <Panel
            title="Event Bus"
            icon={<Database className="h-3.5 w-3.5" />}
            meta={e ? `seq ${e.latest_seq}` : undefined}
          >
            <KpiGrid>
              <Kpi label="Total" value={compact(e?.total ?? 0)} />
              <Kpi label="1h" value={e?.recent_1h ?? 0} />
              <Kpi label="Scopes" value={e?.by_scope?.length ?? 0} />
              <Kpi label="Kinds" value={e?.by_kind?.length ?? 0} />
            </KpiGrid>
            <div className="border-b border-border p-3">
              <div className="mb-2 text-[10px] uppercase tracking-[.16em] text-text-subtle">Scope mix</div>
              <CompositionBars items={toBarItems(e?.by_scope)} />
            </div>
            <DataList title="Event kinds" items={e?.by_kind ?? []} />
          </Panel>
        </div>

        <div className="mt-3 grid gap-3 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
          <Panel title="Catalog Surface" icon={<Database className="h-3.5 w-3.5" />}>
            <KpiGrid cols="grid-cols-2 md:grid-cols-4">
              <Kpi label="Projects" value={c?.projects ?? '...'} />
              <Kpi label="Agents" value={c?.agents ?? '...'} />
              <Kpi label="Providers" value={c?.providers ?? '...'} />
              <Kpi label="Launches" value={c?.launches ?? '...'} />
            </KpiGrid>
            <DataList title="Session projects" items={s?.by_project ?? []} />
          </Panel>

          <Panel title="MCP Servers" icon={<Plug className="h-3.5 w-3.5" />}>
            <KpiGrid cols="grid-cols-2 md:grid-cols-4">
              <Kpi label="Servers" value={t?.by_server?.length ?? 0} />
              <Kpi label="Calls" value={compact(t?.total ?? 0)} />
              <Kpi label="Errors" value={t?.errors ?? 0} />
              <Kpi label="Slow" value={t?.slow_calls ?? 0} />
            </KpiGrid>
            <DataList title="Server volume" items={t?.by_server ?? []} />
          </Panel>
        </div>

        {error && (
          <p className="mt-3 border border-status-blocked/30 bg-status-blocked/10 px-3 py-2 text-[12px] text-status-blocked">
            {error}
          </p>
        )}
      </div>
    </div>
  )
}
