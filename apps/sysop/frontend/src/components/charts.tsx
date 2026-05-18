import { cn } from '@hollis-labs/sysop-ui'
import type { NameCount } from '../api/client'

/**
 * Sparkbars — a lightweight CSS bar chart for a time-bucketed series
 * (oldest → newest). No chart library; just flex columns scaled to the max.
 */
export function Sparkbars({ data, className }: { data: number[]; className?: string }) {
  const max = Math.max(1, ...data)
  if (data.length === 0) {
    return <div className={cn('h-9 text-[11px] text-text-subtle', className)}>no data</div>
  }
  return (
    <div className={cn('flex h-9 items-end gap-px', className)} aria-hidden>
      {data.map((v, i) => (
        <div
          key={i}
          className="flex-1 rounded-sm bg-text-soft/55"
          style={{ height: `${Math.max(3, (v / max) * 100)}%` }}
          title={String(v)}
        />
      ))}
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
    <div className={cn('space-y-1', className)}>
      {items.map((it) => (
        <div key={it.name} className="flex items-center gap-2 text-[11px]">
          <span className="w-32 shrink-0 truncate font-mono text-text-soft" title={it.name}>
            {it.name}
          </span>
          <div className="relative h-3 flex-1 overflow-hidden rounded-sm bg-panel-2">
            <div
              className="absolute inset-y-0 left-0 rounded-sm bg-text-soft/50"
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
