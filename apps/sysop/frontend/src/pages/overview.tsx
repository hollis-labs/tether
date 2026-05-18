import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { RefreshCw } from 'lucide-react'
import {
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  EmptyState,
  Metric,
  cn,
} from '@hollis-labs/sysop-ui'
import { useApi } from '../api/context'
import type { OverviewInfo } from '../api/client'
import { BarList, Sparkbars } from '../components/charts'

/** Compact human duration from a second count. */
function formatDuration(s: number): string {
  if (s <= 0) return '0s'
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  if (h) return `${h}h ${m}m`
  if (m) return `${m}m ${sec}s`
  return `${sec}s`
}

/** Dashboard panel — a titled content block on the kit's Card. */
function Panel({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-[11px] font-semibold uppercase tracking-[.18em] text-text-muted">
          {title}
        </CardTitle>
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  )
}

function MetricRow({ children }: { children: ReactNode }) {
  return <div className="flex flex-wrap gap-x-5 gap-y-3">{children}</div>
}

function TrendCaption({ label }: { label: string }) {
  return <p className="mt-1 text-[10px] uppercase tracking-[.14em] text-text-subtle">{label}</p>
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

  if (error && !data) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not load overview" description={error} />
      </div>
    )
  }

  const s = data?.sessions
  const t = data?.tool_calls
  const m = data?.messages
  const e = data?.events
  const c = data?.catalog
  const h = data?.health

  return (
    <div className="flex min-h-0 flex-1 flex-col bg-bg">
      <div className="flex shrink-0 items-center justify-between border-b border-border-strong bg-bg px-4 py-2">
        <p className="text-[11px] uppercase tracking-[.18em] text-text-subtle">
          Recent activity overview
        </p>
        <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
          <RefreshCw className={cn('h-3.5 w-3.5', loading && 'animate-spin')} />
          Refresh
        </Button>
      </div>

      <div className="min-h-0 flex-1 overflow-auto p-4">
        <div className="grid gap-4 lg:grid-cols-2 xl:grid-cols-3">
          <Panel title="Sessions">
            <MetricRow>
              <Metric label="Total" value={s?.total ?? '...'} />
              <Metric
                label="Running"
                value={s?.running ?? '...'}
                accentColor="var(--color-status-doing)"
              />
              <Metric label="Ended" value={s?.ended ?? '...'} />
              <Metric label="Success" value={s ? `${s.success_pct}%` : '...'} />
              <Metric label="Avg" value={s ? formatDuration(s.avg_seconds) : '...'} />
            </MetricRow>
            <div className="mt-3">
              <Sparkbars data={s?.trend ?? []} />
              <TrendCaption label="Session starts" />
            </div>
          </Panel>

          <Panel title="Tool Calls">
            <MetricRow>
              <Metric label="Total" value={t?.total ?? '...'} />
              <Metric label="Success" value={t ? `${t.success_pct}%` : '...'} />
              <Metric
                label="Errors"
                value={t?.errors ?? '...'}
                accentColor={t && t.errors > 0 ? 'var(--color-status-blocked)' : undefined}
              />
              <Metric label="p50" value={t ? `${t.p50_ms}ms` : '...'} />
              <Metric label="p95" value={t ? `${t.p95_ms}ms` : '...'} />
            </MetricRow>
            <div className="mt-3">
              <Sparkbars data={t?.trend ?? []} />
              <TrendCaption label="Tool-call volume" />
            </div>
            <div className="mt-3">
              <TrendCaption label="Top tools" />
              <BarList className="mt-1" items={t?.top_tools ?? []} />
            </div>
          </Panel>

          <Panel title="Messages">
            <MetricRow>
              <Metric label="Total" value={m?.total ?? '...'} />
              <Metric
                label="Unread"
                value={m?.unread ?? '...'}
                accentColor={m && m.unread > 0 ? 'var(--color-status-inbox)' : undefined}
              />
              <Metric label="Archived" value={m?.archived ?? '...'} />
            </MetricRow>
            <div className="mt-3">
              <TrendCaption label="By kind" />
              <BarList className="mt-1" items={m?.by_kind ?? []} />
            </div>
            <div className="mt-3">
              <Sparkbars data={m?.trend ?? []} />
              <TrendCaption label="Message volume" />
            </div>
          </Panel>

          <Panel title="Events">
            <MetricRow>
              <Metric label="Total" value={e?.total ?? '...'} />
            </MetricRow>
            <div className="mt-3">
              <TrendCaption label="By scope" />
              <BarList className="mt-1" items={e?.by_scope ?? []} />
            </div>
            <div className="mt-3">
              <Sparkbars data={e?.trend ?? []} />
              <TrendCaption label="Event volume" />
            </div>
          </Panel>

          <Panel title="Catalog">
            <MetricRow>
              <Metric label="Projects" value={c?.projects ?? '...'} />
              <Metric label="Agents" value={c?.agents ?? '...'} />
              <Metric label="Providers" value={c?.providers ?? '...'} />
              <Metric label="Launches" value={c?.launches ?? '...'} />
            </MetricRow>
          </Panel>

          <Panel title="Health">
            <MetricRow>
              <Metric
                label="Catalog"
                value={h?.status ?? '...'}
                accentColor={
                  h?.status === 'ok'
                    ? 'var(--color-status-done)'
                    : h
                      ? 'var(--color-status-blocked)'
                      : undefined
                }
              />
            </MetricRow>
            <p className="mt-3 break-all font-mono text-[11px] text-text-subtle">
              {h?.catalog_root ?? ''}
            </p>
            {h?.error && <p className="mt-1 text-[11px] text-status-blocked">{h.error}</p>}
          </Panel>
        </div>
      </div>
    </div>
  )
}
