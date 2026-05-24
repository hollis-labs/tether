import { useCallback, useMemo, useState } from 'react'
import { Check, Copy, Eye } from 'lucide-react'
import { Button, DetailDialog, JsonViewer } from '@hollis-labs/sysop-ui/ui'

/** Parse `raw` as a JSON object, or null when it is not one. */
export function safeParseObject(raw: string): Record<string, unknown> | null {
  if (!raw) return null
  try {
    const v = JSON.parse(raw)
    return v && typeof v === 'object' && !Array.isArray(v)
      ? (v as Record<string, unknown>)
      : null
  } catch {
    return null
  }
}

/** Compact display string for a decomposed payload value. */
export function scalarStr(v: unknown): string {
  if (v === null) return 'null'
  if (typeof v === 'string') return v
  if (typeof v === 'number' || typeof v === 'boolean') return String(v)
  const s = JSON.stringify(v)
  return s.length > 48 ? `${s.slice(0, 47)}…` : s
}

/** Clipboard hook — `copied` flips true for 1.5s after a successful copy. */
export function useCopy() {
  const [copied, setCopied] = useState(false)
  const copy = useCallback((text: string) => {
    void navigator.clipboard
      .writeText(text)
      .then(() => {
        setCopied(true)
        setTimeout(() => setCopied(false), 1500)
      })
      .catch(() => {})
  }, [])
  return { copied, copy }
}

/** Outline button that copies `text` and confirms inline. */
export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const { copied, copy } = useCopy()
  return (
    <Button variant="outline" size="sm" onClick={() => copy(text)}>
      {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
      {copied ? 'Copied' : label}
    </Button>
  )
}

/**
 * Inline view + copy icon pair for a table cell. `onView` opens a payload
 * modal; the copy icon copies `raw` without leaving the table. Renders an
 * em-dash when there is nothing to show.
 */
export function PayloadActions({
  raw,
  onView,
  viewLabel = 'View payload',
}: {
  raw: string
  onView: () => void
  viewLabel?: string
}) {
  const { copied, copy } = useCopy()
  if (!raw) return <span className="text-[11px] text-text-subtle">—</span>
  return (
    <div className="flex items-center gap-0.5">
      <button
        type="button"
        title={viewLabel}
        aria-label={viewLabel}
        onClick={(e) => {
          e.stopPropagation()
          onView()
        }}
        className="rounded p-1 text-text-subtle transition-colors hover:bg-panel-hover hover:text-text"
      >
        <Eye className="h-3.5 w-3.5" />
      </button>
      <button
        type="button"
        title="Copy payload"
        aria-label="Copy payload"
        onClick={(e) => {
          e.stopPropagation()
          copy(raw)
        }}
        className="rounded p-1 text-text-subtle transition-colors hover:bg-panel-hover hover:text-text"
      >
        {copied ? (
          <Check className="h-3.5 w-3.5 text-status-done" />
        ) : (
          <Copy className="h-3.5 w-3.5" />
        )}
      </button>
    </div>
  )
}

/** Decomposed payload preview — JSON objects render as inline key:value
 * chips; anything else falls back to a truncated mono string. */
export function PayloadSummary({ raw }: { raw: string }) {
  const parsed = useMemo(() => safeParseObject(raw), [raw])
  if (!parsed) {
    return raw ? (
      <span className="block truncate font-mono text-[11px] text-text-subtle">{raw}</span>
    ) : (
      <span className="text-[11px] text-text-subtle">—</span>
    )
  }
  const entries = Object.entries(parsed).slice(0, 6)
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-0.5">
      {entries.map(([k, v]) => (
        <span key={k} className="whitespace-nowrap text-[11px]">
          <span className="text-text-subtle">{k}:</span>{' '}
          <span className="font-mono text-text-soft">{scalarStr(v)}</span>
        </span>
      ))}
    </div>
  )
}

/** Modal showing a syntax-highlighted payload with a copy action. */
export function JsonModal({
  open,
  onClose,
  title,
  raw,
}: {
  open: boolean
  onClose: () => void
  title: string
  raw: string
}) {
  // The kit's JsonViewer pretty-prints + highlights any value; parse the
  // payload to an object when possible, otherwise hand it the raw string.
  const value = useMemo<unknown>(() => {
    try {
      return JSON.parse(raw)
    } catch {
      return raw
    }
  }, [raw])
  return (
    <DetailDialog
      open={open}
      onClose={onClose}
      title={title}
      footer={
        <div className="flex justify-end">
          <CopyButton text={raw} label="Copy payload" />
        </div>
      }
    >
      <div className="px-4 py-3">
        <JsonViewer value={value} />
      </div>
    </DetailDialog>
  )
}
