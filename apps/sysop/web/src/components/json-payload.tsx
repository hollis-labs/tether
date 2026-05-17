import { useCallback, useMemo, useState, type ReactNode } from 'react'
import { Check, Copy, Eye } from 'lucide-react'
import { Button, DetailDialog } from '@hollis-labs/sysop-ui'

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

/** Pretty-print a JSON string; returns the input unchanged when not JSON. */
function prettyJson(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
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

// Token regex: quoted string (maybe a key, when followed by `:`), keyword,
// or number. Gaps between matches are punctuation / whitespace.
const TOKEN = /("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false|null)\b|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g

/** Render pretty-printed JSON as colorized React nodes (theme-token colors). */
export function highlightJson(pretty: string): ReactNode[] {
  const nodes: ReactNode[] = []
  let last = 0
  let key = 0
  let m: RegExpExecArray | null
  TOKEN.lastIndex = 0
  while ((m = TOKEN.exec(pretty)) !== null) {
    if (m.index > last) nodes.push(pretty.slice(last, m.index))
    if (m[1] !== undefined) {
      if (m[2] !== undefined) {
        nodes.push(
          <span key={key++} className="text-status-routed">
            {m[1]}
          </span>,
        )
        nodes.push(m[2])
      } else {
        nodes.push(
          <span key={key++} className="text-status-done">
            {m[1]}
          </span>,
        )
      }
    } else if (m[3] !== undefined) {
      nodes.push(
        <span key={key++} className="text-status-blocked">
          {m[3]}
        </span>,
      )
    } else if (m[4] !== undefined) {
      nodes.push(
        <span key={key++} className="text-status-inbox">
          {m[4]}
        </span>,
      )
    }
    last = TOKEN.lastIndex
  }
  if (last < pretty.length) nodes.push(pretty.slice(last))
  return nodes
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

/** Modal showing a colorized, pretty-printed payload with a copy action. */
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
  const pretty = useMemo(() => prettyJson(raw), [raw])
  return (
    <DetailDialog
      open={open}
      onClose={onClose}
      title={title}
      widthClassName="max-w-3xl"
      footer={
        <div className="flex justify-end">
          <CopyButton text={raw} label="Copy payload" />
        </div>
      }
    >
      <pre className="overflow-x-auto rounded-md border border-border bg-panel px-4 py-3 font-mono text-[12px] leading-5 text-text-subtle">
        {highlightJson(pretty)}
      </pre>
    </DetailDialog>
  )
}
