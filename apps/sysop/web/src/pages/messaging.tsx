import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Bot, RefreshCw, Send, Trash2, User } from 'lucide-react'
import {
  Button,
  CopyableId,
  DataTable,
  DetailDialog,
  DetailSection,
  EmptyState,
  StatusBadge,
  SummaryCards,
  Textarea,
  cn,
  formatRelativeTime,
  type ColumnDef,
} from '@hollis-labs/sysop-ui'
import { useApi } from '../api/context'
import type { MessageInfo } from '../api/client'
import { TabStrip, type TabItem } from '../components/tab-strip'
import { CopyButton, safeParseObject, scalarStr } from '../components/json-payload'

type ScopeKey = 'user' | 'agent'

// Payload keys, in priority order, that map onto an email-shaped message.
const SUBJECT_KEYS = ['subject', 'title', 'headline']
const TEXT_KEYS = ['summary', 'body', 'text', 'message', 'detail', 'description', 'content']

interface ParsedMessage {
  subject?: string
  text: string
  fields: [string, unknown][]
  isJson: boolean
}

/** Decompose a message body into an email shape: subject, text, and any
 * remaining structured fields. Plain-text bodies pass through verbatim. */
function parseMessage(body: string): ParsedMessage {
  const obj = safeParseObject(body)
  if (!obj) return { text: body, fields: [], isJson: false }
  const consumed = new Set<string>()
  let subject: string | undefined
  for (const k of SUBJECT_KEYS) {
    if (typeof obj[k] === 'string' && obj[k]) {
      subject = obj[k] as string
      consumed.add(k)
      break
    }
  }
  let text = ''
  for (const k of TEXT_KEYS) {
    if (typeof obj[k] === 'string' && obj[k]) {
      text = obj[k] as string
      consumed.add(k)
      break
    }
  }
  const fields = Object.entries(obj).filter(([k]) => !consumed.has(k))
  return { subject, text, fields, isJson: true }
}

/** One-line label for the table — subject, else the first line of text. */
function messageHeadline(m: MessageInfo): string {
  const p = parseMessage(m.body)
  return p.subject || p.text.split('\n').find((l) => l.trim()) || ''
}

/** Lifecycle status of a message, newest terminal state wins. */
function messageStatus(m: MessageInfo): string {
  if (m.canceled_at) return 'canceled'
  if (m.consumed_at) return 'consumed'
  if (m.delivered_at) return 'delivered'
  return 'unread'
}

/** Strip `msg://<kind>/` to the readable `authority/id[/sub]` tail. */
function shortUrn(urn: string): string {
  const tail = urn.replace(/^msg:\/\//, '').split('/')
  return tail.length > 1 ? tail.slice(1).join('/') : urn
}

const columns: ColumnDef<MessageInfo>[] = [
  {
    key: 'from',
    header: 'From',
    cell: (m) => <span className="text-[12px] text-text">{shortUrn(m.from)}</span>,
    sortValue: (m) => m.from,
  },
  {
    key: 'to',
    header: 'To',
    cell: (m) => <span className="text-[12px] text-text-soft">{shortUrn(m.to)}</span>,
    sortValue: (m) => m.to,
  },
  {
    key: 'kind',
    header: 'Kind',
    cell: (m) => (
      <span className="text-[11px] uppercase tracking-[.12em] text-text-soft">{m.kind}</span>
    ),
    sortValue: (m) => m.kind,
  },
  {
    key: 'subject',
    header: 'Subject',
    width: 'fill',
    cell: (m) => {
      const headline = messageHeadline(m)
      return headline ? (
        <span className="block truncate text-[12px] text-text">{headline}</span>
      ) : (
        <span className="text-[12px] text-text-subtle">(no subject)</span>
      )
    },
    sortValue: (m) => messageHeadline(m),
  },
  {
    key: 'status',
    header: 'Status',
    cell: (m) => <StatusBadge status={messageStatus(m)} />,
    sortValue: (m) => messageStatus(m),
  },
  {
    key: 'created',
    header: 'Received',
    align: 'right',
    cell: (m) => <span className="text-[11px] text-text-soft">{formatRelativeTime(m.created_at)}</span>,
    sortValue: (m) => m.created_at,
  },
]

export function MessagingPage() {
  const api = useApi()
  const [scope, setScope] = useState<ScopeKey>('user')
  const [messages, setMessages] = useState<MessageInfo[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [replyText, setReplyText] = useState('')
  const [replyError, setReplyError] = useState<string | null>(null)
  const [sending, setSending] = useState(false)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    api
      .getMessages()
      .then((info) => {
        if (cancelled) return
        setMessages(info.messages ?? [])
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

  const all = messages ?? []
  const scoped = useMemo(() => all.filter((m) => m.scope === scope), [all, scope])
  const userCount = useMemo(() => all.filter((m) => m.scope === 'user').length, [all])
  const agentCount = useMemo(() => all.filter((m) => m.scope === 'agent').length, [all])

  const selected = selectedId ? all.find((m) => m.id === selectedId) ?? null : null
  const parsed = useMemo(() => (selected ? parseMessage(selected.body) : null), [selected])

  const unread = scoped.filter((m) => messageStatus(m) === 'unread').length
  const consumed = scoped.filter((m) => m.consumed_at).length

  const tabs: TabItem<ScopeKey>[] = [
    { key: 'user', label: 'User', icon: User, count: userCount },
    { key: 'agent', label: 'Agents', icon: Bot, count: agentCount },
  ]

  function closeDialog() {
    setSelectedId(null)
    setReplyText('')
    setReplyError(null)
  }

  async function sendReply() {
    if (!selected || !replyText.trim()) return
    setSending(true)
    setReplyError(null)
    try {
      await api.sendReply({
        from: selected.to,
        to: selected.from,
        kind: 'response',
        body: replyText.trim(),
        in_reply_to: selected.id,
        thread_id: selected.thread_id || selected.id,
      })
      closeDialog()
      load()
    } catch (err: unknown) {
      setReplyError(err instanceof Error ? err.message : String(err))
    } finally {
      setSending(false)
    }
  }

  if (error) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not load messages" description={error} />
      </div>
    )
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col bg-bg">
      <TabStrip
        tabs={tabs}
        active={scope}
        onSelect={setScope}
        actions={
          <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
            <RefreshCw className={cn('h-3.5 w-3.5', loading && 'animate-spin')} />
            Refresh
          </Button>
        }
      />

      <SummaryCards
        cards={[
          { label: scope === 'user' ? 'User Messages' : 'Agent Messages', value: scoped.length },
          { label: 'Unread', value: unread, accentColor: 'var(--color-status-inbox)' },
          { label: 'Consumed', value: consumed, accentColor: 'var(--color-status-done)' },
        ]}
      />

      <p className="shrink-0 border-b border-border-strong bg-bg px-4 py-1.5 text-[11px] text-text-subtle">
        {scope === 'user' ? 'Messages addressed to users.' : 'Messages addressed to agents.'} Non-destructive
        read-only view — delete is pending backend inbox semantics (Torque CW-20260517-0003).
      </p>

      <div ref={scrollRef} className="min-h-0 flex-1 overflow-auto">
        <DataTable
          items={scoped}
          columns={columns}
          getRowId={(m) => m.id}
          initialSort={{ key: 'created', dir: 'desc' }}
          scrollRootRef={scrollRef}
          onRowOpen={(id) => setSelectedId(id)}
          rowAriaLabel={(m) => `Open message from ${shortUrn(m.from)}`}
          emptyState={
            <EmptyState
              variant="empty"
              title={loading ? 'Loading messages...' : 'No messages'}
              description={
                loading
                  ? 'Reading the configured Tether state DB.'
                  : `No ${scope === 'user' ? 'user' : 'agent'}-scoped messages in the state DB.`
              }
            />
          }
        />
      </div>

      <DetailDialog
        open={selected !== null}
        onClose={closeDialog}
        widthClassName="max-w-4xl"
        title={selected ? parsed?.subject || `${selected.kind} message` : ''}
        badge={selected ? <StatusBadge status={messageStatus(selected)} /> : null}
        meta={
          selected ? (
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-text-subtle">
              <span>
                {shortUrn(selected.from)} <span className="text-text-subtle/60">→</span>{' '}
                {shortUrn(selected.to)}
              </span>
              <span className="uppercase tracking-[.12em]">{selected.kind}</span>
              <span>{formatRelativeTime(selected.created_at)}</span>
              <CopyableId id={selected.id} label={selected.id.slice(0, 12)} />
            </div>
          ) : null
        }
        footer={
          selected ? (
            <div className="flex items-center justify-between gap-2">
              <div className="flex items-center gap-2">
                <span title="Delete is pending backend inbox semantics — Torque CW-20260517-0003">
                  <Button variant="outline" size="sm" disabled>
                    <Trash2 className="h-3.5 w-3.5" />
                    Delete
                  </Button>
                </span>
                <CopyButton text={selected.body} label="Copy payload" />
              </div>
              <Button
                variant="default"
                size="sm"
                onClick={sendReply}
                disabled={sending || !replyText.trim()}
              >
                <Send className={cn('h-3.5 w-3.5', sending && 'animate-pulse')} />
                {sending ? 'Sending...' : 'Send reply'}
              </Button>
            </div>
          ) : null
        }
      >
        {selected && parsed && (
          <>
            <DetailSection title="Message">
              <div className="whitespace-pre-wrap break-words rounded-md border border-border bg-panel px-4 py-3 text-[13px] leading-6 text-text">
                {parsed.text || (parsed.isJson ? '(no message text)' : '(no body)')}
              </div>
            </DetailSection>

            {parsed.fields.length > 0 && (
              <DetailSection title="Details">
                <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
                  {parsed.fields.map(([k, v]) => (
                    <div key={k} className="contents">
                      <dt className="truncate text-text-subtle">{k}</dt>
                      <dd className="break-words font-mono text-text-soft">
                        {typeof v === 'string' ? v : scalarStr(v)}
                      </dd>
                    </div>
                  ))}
                </dl>
              </DetailSection>
            )}

            <DetailSection title={`Reply to ${shortUrn(selected.from)}`}>
              <Textarea
                value={replyText}
                onChange={(e) => setReplyText(e.target.value)}
                placeholder="Write a reply..."
                rows={5}
                className="w-full"
              />
              {replyError && <p className="mt-2 text-[12px] text-status-blocked">{replyError}</p>}
            </DetailSection>
          </>
        )}
      </DetailDialog>
    </div>
  )
}
