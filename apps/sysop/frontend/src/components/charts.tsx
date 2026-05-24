import { cn } from '@hollis-labs/sysop-ui'
import type { NameCount } from '../api/client'

/**
 * Sparkbars — a lightweight CSS bar chart for a time-bucketed series
 * (oldest → newest). No chart library; just flex columns scaled to the max.
 */
export function Sparkbars({ data, className }: { data: number[]; className?: string }) {
  const max = Math.max(1, ...data)
  const rows = 6
  if (data.length === 0) {
    return <div className={cn('h-9 text-[11px] text-text-subtle', className)}>no data</div>
  }
  return (
    <div className={cn('flex h-9 items-end gap-px overflow-hidden', className)} aria-hidden>
      {data.map((v, i) => {
        const filled = v > 0 ? Math.max(1, Math.ceil((v / max) * rows)) : 0
        return (
          <div key={i} className="flex min-w-0 flex-1 flex-col-reverse gap-px" title={String(v)}>
            {Array.from({ length: rows }, (_, row) => (
              <div
                key={row}
                className={cn(
                  'aspect-square w-full min-h-[2px]',
                  row < filled ? 'bg-text-soft/65' : 'bg-text-subtle/10',
                )}
              />
            ))}
          </div>
        )
      })}
    </div>
  )
}

/**
 * Dense, grid-backed column chart for instrumentation dashboards. The
 * translucent full-height bars keep low-volume periods visible while the
 * solid bars carry the actual value.
 */
export function SignalBars({
  data,
  secondaryData,
  className,
  heightClassName = 'h-44',
  primaryLabel = 'Primary',
  secondaryLabel = 'Secondary',
}: {
  data: number[]
  secondaryData?: number[]
  className?: string
  heightClassName?: string
  primaryLabel?: string
  secondaryLabel?: string
}) {
  const rows = 18
  const subColumns = 2
  const totals = data.map((v, i) => v + (secondaryData?.[i] ?? 0))
  const peak = Math.max(1, ...totals)
  const ceiling = Math.ceil(peak * 1.25)
  const active = totals.filter((v) => v > 0).length
  const primaryTotal = data.reduce((sum, v) => sum + v, 0)
  const secondaryTotal = (secondaryData ?? []).reduce((sum, v) => sum + v, 0)
  if (data.length === 0) {
    return <div className={cn(heightClassName, 'text-[11px] text-text-subtle', className)}>no data</div>
  }
  return (
    <div className={cn('bg-bg', className)}>
      <div className="flex h-7 items-center justify-between gap-3 bg-bg px-2 text-[10px] font-mono tabular-nums text-text-subtle">
        <span>peak {peak}</span>
        <span className="flex items-center gap-3">
          <span className="inline-flex items-center gap-1">
            <span className="h-2 w-2 bg-text-soft/50" />
            {primaryLabel} {primaryTotal}
          </span>
          {secondaryData && (
            <span className="inline-flex items-center gap-1">
              <span className="h-2 w-2 bg-text-soft/30" />
              {secondaryLabel} {secondaryTotal}
            </span>
          )}
          <span>{active}/{data.length} active</span>
        </span>
      </div>
      <div className={cn('border-y border-border-strong px-2 py-2', heightClassName)}>
        <div className="relative flex h-full items-end gap-px overflow-hidden" aria-hidden>
          {Array.from({ length: data.length * subColumns }, (_, col) => {
            const i = Math.floor(col / subColumns)
            const v = data[i] ?? 0
            const secondary = secondaryData?.[i] ?? 0
            const total = v + secondary
            const filled = total > 0 ? Math.max(1, Math.ceil((total / ceiling) * rows)) : 0
            const secondaryCells =
              total > 0 && secondary > 0 ? Math.max(1, Math.round((secondary / total) * filled)) : 0
            const primaryCells = Math.max(0, filled - secondaryCells)
            return (
              <div key={col} className="flex min-w-0 flex-1 flex-col-reverse gap-px" title={`${total}`}>
                {Array.from({ length: rows }, (_, row) => {
                  const filledCell = row < filled
                  const secondaryCell = row < secondaryCells
                  const primaryCell = row >= secondaryCells && row < secondaryCells + primaryCells
                  return (
                    <div
                      key={row}
                      className={cn(
                        'aspect-square w-full min-h-[4px]',
                        !filledCell && 'bg-text-subtle/10',
                        primaryCell && 'bg-text-soft/50',
                        secondaryCell && 'bg-text-soft/30',
                      )}
                    />
                  )
                })}
              </div>
            )
          })}
        </div>
      </div>
    </div>
  )
}

/**
 * BarList — a horizontal name/count bar list (e.g. top tools, by-kind).
 */
export function BarList({ items, className }: { items: NameCount[]; className?: string }) {
  if (items.length === 0) {
    return <div className={cn('text-[11px] text-text-subtle', className)}>none</div>
  }
  const max = Math.max(1, ...items.map((i) => i.count))
  return (
    <div className={cn('space-y-1.5', className)}>
      {items.map((it) => (
        <div key={it.name} className="flex items-center gap-2 text-[11px]">
          <span className="w-32 shrink-0 truncate font-mono text-text-soft" title={it.name}>
            {it.name}
          </span>
          <div className="relative h-3 flex-1 overflow-hidden bg-panel-2">
            <div
              className="absolute inset-y-0 left-0 bg-text-soft/55"
              style={{ width: `${(it.count / max) * 100}%` }}
            />
          </div>
          <span className="w-10 shrink-0 text-right font-mono tabular-nums text-text">
            {it.count}
          </span>
        </div>
      ))}
    </div>
  )
}

export function CompositionBars({
  items,
  className,
}: {
  items: NameCount[]
  className?: string
}) {
  const total = items.reduce((sum, item) => sum + item.count, 0)
  if (items.length === 0 || total === 0) {
    return <div className={cn('text-[11px] text-text-subtle', className)}>none</div>
  }
  return (
    <div className={cn('space-y-2', className)}>
      <div className="flex h-3 overflow-hidden bg-panel-2">
        {items.map((item, index) => (
          <div
            key={item.name}
            style={{
              width: `${(item.count / total) * 100}%`,
              backgroundColor: `color-mix(in oklab, var(--color-text) ${Math.max(30, 88 - index * 12)}%, transparent)`,
            }}
            title={`${item.name}: ${item.count}`}
          />
        ))}
      </div>
      <div className="grid grid-cols-2 gap-x-3 gap-y-1">
        {items.map((item) => (
          <div key={item.name} className="flex items-center justify-between gap-2 text-[11px]">
            <span className="truncate text-text-soft" title={item.name}>
              {item.name}
            </span>
            <span className="font-mono tabular-nums text-text-subtle">{item.count}</span>
          </div>
        ))}
      </div>
    </div>
  )
}
