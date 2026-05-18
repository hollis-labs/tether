import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Archive, Bot, RefreshCw, Send, User } from 'lucide-react'
import {
  Button,
  CopyableId,
  DataTable,
  DetailDialog,
  DetailSection,
  EmptyState,
  ListPageLayout,
  StatusBadge,
  SummaryCards,
  TabStrip,
  Textarea,
  cn,
  formatRelativeTime,
  type ColumnDef,
  type TabStripItem,
} from '@hollis-labs/sysop-ui'
import { useApi } from '../api/context'
import type { MessageInfo } from '../api/client'
import { CopyButton, safeParseObject, scalarStr } from '../components/json-payload'

type ScopeKey = 'user' | 'agent'

// Payload keys the backend's projectPayload already folds into subject/body —
// excluded from the modal's "Details" decomposition.
const PROJECTED_KEYS = new Set(['subject', 'title', 'body', 'summary', 'text', 'message'])

/** Structured payload fields not already shown as subject/body. */
function extraFields(payload?: string): [string, unknown][] {
  if (!payload) return []
  const obj = safeParseObject(payload)
  if (!obj) return []
  return Object.entries(obj).filter(([k]) => !PROJECTED_KEYS.has(k))
}

/** One-line label for the table — backend subject, else first line of body. */
function messageHeadline(m: MessageInfo): string {
  if (m.subject) return m.subject
  return m.body.split('\n').find((l) => l.trim()) ?? ''
}

/** Lifecycle status of a message, most salient state wins. */
function messageStatus(m: MessageInfo): string {
  if (m.canceled_at) return 'canceled'
  if (m.archived_at) return 'archived'
  if (m.consumed_at) return 'consumed'
  if (m.read_at) return 'read'
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
      const unread = messageStatus(m) === 'unread'
      return headline ? (
        <span className={cn('block truncate text-[12px]', unread ? 'font-medium text-text' : 'text-text-soft')}>
          {headline}
        </span>
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
  const [dialogError, setDialogError] = useState<string | null>(null)
  const [sending, setSending] = useState(false)
  const [archiving, setArchiving] = useState(false)
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
  const details = useMemo(() => (selected ? extraFields(selected.payload) : []), [selected])

  const unread = scoped.filter((m) => messageStatus(m) === 'unread').length
  const archived = scoped.filter((m) => m.archived_at).length

  const tabs: TabStripItem<ScopeKey>[] = [
    { key: 'user', label: 'User', icon: <User className="h-3.5 w-3.5" />, count: userCount },
    { key: 'agent', label: 'Agents', icon: <Bot className="h-3.5 w-3.5" />, count: agentCount },
  ]

  function closeDialog() {
    setSelectedId(null)
    setReplyText('')
    setDialogError(null)
  }

  // Opening a message marks it read (fire-and-forget; reload reflects it).
  function openMessage(id: string) {
    setSelectedId(id)
    setReplyText('')
    setDialogError(null)
    const m = all.find((x) => x.id === id)
    if (m && !m.read_at && !m.canceled_at) {
      api
        .markRead(m.id, m.to)
        .then(() => load())
        .catch(() => {})
    }
  }

  async function sendReply() {
    if (!selected || !replyText.trim()) return
    setSending(true)
    setDialogError(null)
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
      setDialogError(err instanceof Error ? err.message : String(err))
    } finally {
      setSending(false)
    }
  }

  async function archiveMessage() {
    if (!selected) return
    setArchiving(true)
    setDialogError(null)
    try {
      await api.archiveMessage(selected.id, selected.to)
      closeDialog()
      load()
    } catch (err: unknown) {
      setDialogError(err instanceof Error ? err.message : String(err))
    } finally {
      setArchiving(false)
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
    <>
      <ListPageLayout
        header={null}
        scrollRef={scrollRef}
        tabs={
          <TabStrip
            tabs={tabs}
            value={scope}
            onChange={setScope}
            actions={
              <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
                <RefreshCw className={cn('h-3.5 w-3.5', loading && 'animate-spin')} />
                Refresh
              </Button>
            }
          />
        }
        summary={
          <SummaryCards
            cards={[
              { label: scope === 'user' ? 'User Messages' : 'Agent Messages', value: scoped.length },
              { label: 'Unread', value: unread, accentColor: 'var(--color-status-inbox)' },
              { label: 'Archived', value: archived, accentColor: 'var(--color-status-archived)' },
            ]}
          />
        }
        filters={
          <p className="shrink-0 border-b border-border-strong bg-bg px-4 py-1.5 text-[11px] text-text-subtle">
            {scope === 'user' ? 'Messages addressed to users.' : 'Messages addressed to agents.'}{' '}
            Opening a message marks it read; Archive soft-deletes it.
          </p>
        }
      >
        <DataTable
          items={scoped}
          columns={columns}
          getRowId={(m) => m.id}
          initialSort={{ key: 'created', dir: 'desc' }}
          scrollRootRef={scrollRef}
          onRowOpen={(id) => openMessage(id)}
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
      </ListPageLayout>

      <DetailDialog
        open={selected !== null}
        onClose={closeDialog}
        widthClassName="max-w-4xl"
        title={selected ? messageHeadline(selected) || `${selected.kind} message` : ''}
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
            <div className="flex items-center justify-between gap-3">
              <div className="flex items-center gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={archiveMessage}
                  disabled={archiving || selected.archived_at !== undefined}
                >
                  <Archive className="h-3.5 w-3.5" />
                  {selected.archived_at ? 'Archived' : archiving ? 'Archiving...' : 'Archive'}
                </Button>
                <CopyButton text={selected.payload || selected.body} label="Copy payload" />
              </div>
              <div className="flex items-center gap-3">
                {dialogError && (
                  <span className="text-[12px] text-status-blocked">{dialogError}</span>
                )}
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
            </div>
          ) : null
        }
      >
        {selected && (
          <>
            <DetailSection title="Message">
              <div className="whitespace-pre-wrap break-words rounded-md border border-border bg-panel px-4 py-3 text-[13px] leading-6 text-text">
                {selected.body || '(no message text)'}
              </div>
            </DetailSection>

            {details.length > 0 && (
              <DetailSection title="Details">
                <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
                  {details.map(([k, v]) => (
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
            </DetailSection>
          </>
        )}
      </DetailDialog>
    </>
  )
}
