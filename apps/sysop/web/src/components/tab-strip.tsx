import type { ComponentType, ReactNode } from 'react'
import { cn } from '@hollis-labs/sysop-ui'

export interface TabItem<K extends string> {
  key: K
  label: string
  /** Optional leading icon (e.g. a lucide icon component). */
  icon?: ComponentType<{ className?: string }>
  /** Optional count badge rendered after the label. */
  count?: number
}

interface TabStripProps<K extends string> {
  tabs: TabItem<K>[]
  active: K
  onSelect: (key: K) => void
  /** Right-aligned slot for page actions (e.g. a Refresh button). */
  actions?: ReactNode
}

/**
 * Pinned underline-style tab strip — the shared page-section switcher used
 * by Operations, Messaging, and Activity. Mirrors the Fragments Engine
 * operations layout: tabs on the left, actions on the right edge.
 */
export function TabStrip<K extends string>({ tabs, active, onSelect, actions }: TabStripProps<K>) {
  return (
    <div className="flex shrink-0 items-center justify-between border-b border-border-strong bg-bg px-4">
      <div className="flex items-center gap-1">
        {tabs.map((tab) => {
          const Icon = tab.icon
          const isActive = tab.key === active
          return (
            <button
              key={tab.key}
              type="button"
              onClick={() => onSelect(tab.key)}
              className={cn(
                'relative -mb-px flex items-center gap-2 border-b-2 px-3 py-2.5 text-[12px] font-medium tracking-[.02em] transition-colors',
                isActive
                  ? 'border-text text-text'
                  : 'border-transparent text-text-subtle hover:text-text-soft',
              )}
            >
              {Icon && <Icon className="h-3.5 w-3.5" />}
              {tab.label}
              {tab.count !== undefined && (
                <span
                  className={cn(
                    'rounded px-1.5 py-0.5 font-mono text-[10px] tabular-nums',
                    isActive ? 'bg-panel-2 text-text-soft' : 'bg-panel-2/50 text-text-subtle',
                  )}
                >
                  {tab.count}
                </span>
              )}
            </button>
          )
        })}
      </div>
      {actions && <div className="flex items-center gap-2">{actions}</div>}
    </div>
  )
}
