import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Archive, Bot, MessageSquarePlus, Plus, RefreshCw, Send, User, Users } from 'lucide-react'
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
import type {
  GroupInfo,
  GroupMessageInfo,
  MessageAgentInfo,
  MessageInfo,
  MessageTotals,
} from '../api/client'
import { CopyButton, safeParseObject, scalarStr } from '../components/json-payload'

type ScopeKey = 'user' | 'agent' | 'groups'

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

function groupMessageHeadline(m: GroupMessageInfo): string {
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

function agentLabel(agent: MessageAgentInfo): string {
  return agent.display_name ? `${agent.display_name} (${shortUrn(agent.urn)})` : shortUrn(agent.urn)
}

const DEFAULT_SENDER = 'msg://user/agent-mux/operator'

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

interface GroupsBoardProps {
  groups: GroupInfo[]
  loading: boolean
  selectedGroup: GroupInfo | null
  selectedFrom: string
  replyText: string
  sending: boolean
  error: string | null
  onSelectGroup: (urn: string) => void
  onSelectFrom: (urn: string) => void
  onReplyText: (text: string) => void
  onSendReply: () => void
}

function GroupsBoard({
  groups,
  loading,
  selectedGroup,
  selectedFrom,
  replyText,
  sending,
  error,
  onSelectGroup,
  onSelectFrom,
  onReplyText,
  onSendReply,
}: GroupsBoardProps) {
  if (groups.length === 0) {
    return (
      <EmptyState
        variant="empty"
        title={loading ? 'Loading groups...' : 'No groups'}
        description={
          loading
            ? 'Reading the configured Tether state DB.'
            : 'The registry has no group profiles yet.'
        }
      />
    )
  }

  const members = selectedGroup?.members ?? []
  const archived = selectedGroup?.status === 'archived'
  const canSend = Boolean(selectedGroup && selectedFrom && replyText.trim() && !archived && members.length > 0)

  return (
    <div className="grid min-h-0 flex-1 grid-cols-1 bg-bg md:grid-cols-[minmax(14rem,18rem)_1fr]">
      <div className="min-h-0 border-b border-border-strong md:border-b-0 md:border-r">
        <div className="max-h-64 overflow-auto md:max-h-none">
          {groups.map((g) => {
            const selected = selectedGroup?.urn === g.urn
            const latest = g.messages[g.messages.length - 1]
            return (
              <button
                key={g.urn}
                type="button"
                onClick={() => onSelectGroup(g.urn)}
                className={cn(
                  'block w-full border-b border-border px-4 py-3 text-left transition-colors',
                  selected ? 'bg-panel' : 'hover:bg-panel/60',
                )}
              >
                <span className="flex items-center justify-between gap-3">
                  <span className="min-w-0 truncate text-[13px] font-medium text-text">
                    {g.display_name || shortUrn(g.urn)}
                  </span>
                  <span className="font-mono text-[11px] tabular-nums text-text-subtle">
                    {g.messages.length}
                  </span>
                </span>
                <span className="mt-1 block truncate text-[11px] text-text-subtle">
                  {latest ? groupMessageHeadline(latest) || shortUrn(latest.from_urn) : 'No posts yet'}
                </span>
              </button>
            )
          })}
        </div>
      </div>

      <div className="flex min-h-0 flex-col">
        {selectedGroup ? (
          <>
            <div className="shrink-0 border-b border-border-strong px-4 py-3">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <h2 className="truncate text-[14px] font-medium text-text">
                      {selectedGroup.display_name || shortUrn(selectedGroup.urn)}
                    </h2>
                    <StatusBadge status={selectedGroup.status} />
                  </div>
                  <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-text-subtle">
                    <CopyableId id={selectedGroup.urn} label={shortUrn(selectedGroup.urn)} />
                    <span>{members.length} members</span>
                    <span>{selectedGroup.messages.length} posts</span>
                  </div>
                </div>
              </div>
              {selectedGroup.description && (
                <p className="mt-2 line-clamp-2 text-[12px] text-text-soft">
                  {selectedGroup.description}
                </p>
              )}
            </div>

            <div className="min-h-0 flex-1 overflow-auto">
              {selectedGroup.messages.length === 0 ? (
                <EmptyState
                  variant="empty"
                  title="No group messages"
                  description="This group has not received any posts yet."
                />
              ) : (
                selectedGroup.messages.map((m) => (
                  <article key={m.id} className="border-b border-border px-4 py-3">
                    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
                      <div className="flex min-w-0 items-center gap-2">
                        <span className="truncate text-[12px] font-medium text-text">
                          {shortUrn(m.from_urn)}
                        </span>
                        <span className="text-[11px] uppercase tracking-[.12em] text-text-subtle">
                          {m.kind}
                        </span>
                      </div>
                      <div className="flex items-center gap-2 text-[11px] text-text-subtle">
                        <span className="font-mono tabular-nums">#{m.group_seq}</span>
                        <span>{formatRelativeTime(m.created_at)}</span>
                      </div>
                    </div>
                    {m.subject && (
                      <div className="mt-2 text-[12px] font-medium text-text">{m.subject}</div>
                    )}
                    <div className="mt-2 whitespace-pre-wrap break-words text-[13px] leading-6 text-text-soft">
                      {m.body || '(no message text)'}
                    </div>
                  </article>
                ))
              )}
            </div>

            <div className="shrink-0 border-t border-border-strong bg-bg px-4 py-3">
              <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
                <label className="flex items-center gap-2 text-[11px] text-text-subtle">
                  <span>Reply as</span>
                  <select
                    value={selectedFrom}
                    onChange={(e) => onSelectFrom(e.target.value)}
                    disabled={archived || members.length === 0}
                    className="h-8 rounded-md border border-border bg-panel px-2 text-[12px] text-text outline-none focus:border-border-strong disabled:opacity-60"
                  >
                    {members.map((m) => (
                      <option key={m.member_urn} value={m.member_urn}>
                        {(m.display_name || shortUrn(m.member_urn)) + ` (${m.role})`}
                      </option>
                    ))}
                  </select>
                </label>
                {error && <span className="text-[12px] text-status-blocked">{error}</span>}
              </div>
              <div className="flex flex-col gap-2 sm:flex-row sm:items-end">
                <Textarea
                  value={replyText}
                  onChange={(e) => onReplyText(e.target.value)}
                  placeholder={archived ? 'Archived groups are read-only.' : 'Write a group reply...'}
                  disabled={archived || members.length === 0}
                  className="h-22 min-h-0 flex-1 resize-none border-0 bg-input/30 focus-visible:border-transparent focus-visible:ring-0"
                />
                <Button
                  variant="default"
                  size="sm"
                  onClick={onSendReply}
                  disabled={sending || !canSend}
                  className="shrink-0"
                >
                  <Send className={cn('h-3.5 w-3.5', sending && 'animate-pulse')} />
                  {sending ? 'Sending...' : 'Reply'}
                </Button>
              </div>
            </div>
          </>
        ) : null}
      </div>
    </div>
  )
}

export function MessagingPage() {
  const api = useApi()
  const [scope, setScope] = useState<ScopeKey>('user')
  const [messages, setMessages] = useState<MessageInfo[] | null>(null)
  const [messageTotals, setMessageTotals] = useState<MessageTotals | null>(null)
  const [groups, setGroups] = useState<GroupInfo[] | null>(null)
  const [agents, setAgents] = useState<MessageAgentInfo[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [selectedGroupUrn, setSelectedGroupUrn] = useState<string | null>(null)
  const [replyText, setReplyText] = useState('')
  const [groupReplyText, setGroupReplyText] = useState('')
  const [groupFrom, setGroupFrom] = useState('')
  const [newMessageOpen, setNewMessageOpen] = useState(false)
  const [newMessageFrom, setNewMessageFrom] = useState(DEFAULT_SENDER)
  const [newMessageTo, setNewMessageTo] = useState('')
  const [newMessageBody, setNewMessageBody] = useState('')
  const [newGroupOpen, setNewGroupOpen] = useState(false)
  const [newGroupName, setNewGroupName] = useState('')
  const [newGroupDescription, setNewGroupDescription] = useState('')
  const [newGroupCreator, setNewGroupCreator] = useState('')
  const [dialogError, setDialogError] = useState<string | null>(null)
  const [groupError, setGroupError] = useState<string | null>(null)
  const [composeError, setComposeError] = useState<string | null>(null)
  const [sending, setSending] = useState(false)
  const [sendingGroup, setSendingGroup] = useState(false)
  const [creatingGroup, setCreatingGroup] = useState(false)
  const [archiving, setArchiving] = useState(false)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    Promise.allSettled([api.getMessages(), api.getGroups(), api.getMessageAgents()])
      .then(([messageResult, groupResult, agentResult]) => {
        if (cancelled) return
        if (messageResult.status === 'rejected') {
          setError(
            messageResult.reason instanceof Error
              ? messageResult.reason.message
              : String(messageResult.reason),
          )
          setGroups([])
          return
        }
        setMessages(messageResult.value.messages ?? [])
        setMessageTotals(messageResult.value.totals ?? null)
        setError(messageResult.value.error ?? null)
        if (groupResult.status === 'fulfilled') {
          setGroups(groupResult.value.groups ?? [])
          setGroupError(groupResult.value.error ?? null)
        } else {
          setGroups([])
          setGroupError(
            groupResult.reason instanceof Error
              ? groupResult.reason.message
              : String(groupResult.reason),
          )
        }
        if (agentResult.status === 'fulfilled') {
          const nextAgents = agentResult.value.agents ?? []
          setAgents(nextAgents)
          setNewMessageTo((current) => current || nextAgents[0]?.urn || '')
          setNewGroupCreator((current) => current || nextAgents[0]?.urn || '')
        } else {
          setAgents([])
        }
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
  const groupList = groups ?? []
  const scoped = useMemo(() => all.filter((m) => m.scope === scope), [all, scope])
  const userCount = messageTotals?.user.total ?? all.filter((m) => m.scope === 'user').length
  const agentCount = messageTotals?.agent.total ?? all.filter((m) => m.scope === 'agent').length
  const groupPostCount = useMemo(
    () => groupList.reduce((n, g) => n + g.messages.length, 0),
    [groupList],
  )
  const totalGroupPosts = messageTotals?.groups.total ?? groupPostCount

  const selected = selectedId ? all.find((m) => m.id === selectedId) ?? null : null
  const selectedGroup =
    (selectedGroupUrn ? groupList.find((g) => g.urn === selectedGroupUrn) : null) ?? groupList[0] ?? null
  const details = useMemo(() => (selected ? extraFields(selected.payload) : []), [selected])

  const scopeTotals = scope === 'user' ? messageTotals?.user : scope === 'agent' ? messageTotals?.agent : null
  const unread = scopeTotals?.unread ?? scoped.filter((m) => messageStatus(m) === 'unread').length
  const archived = scopeTotals?.archived ?? scoped.filter((m) => m.archived_at).length
  const groupMembers = selectedGroup?.members ?? []
  const selectedGroupFrom = groupFrom || groupMembers[0]?.member_urn || ''

  useEffect(() => {
    if (!selectedGroup) return
    if (!selectedGroupUrn || selectedGroupUrn !== selectedGroup.urn) {
      setSelectedGroupUrn(selectedGroup.urn)
    }
    if (!groupMembers.some((m) => m.member_urn === groupFrom)) {
      setGroupFrom(groupMembers[0]?.member_urn ?? '')
    }
  }, [groupFrom, groupMembers, selectedGroup, selectedGroupUrn])

  const tabs: TabStripItem<ScopeKey>[] = [
    { key: 'user', label: 'User', icon: <User className="h-3.5 w-3.5" />, count: userCount },
    { key: 'agent', label: 'Agents', icon: <Bot className="h-3.5 w-3.5" />, count: agentCount },
    { key: 'groups', label: 'Groups', icon: <Users className="h-3.5 w-3.5" />, count: groupList.length },
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

  async function sendGroupReply() {
    if (!selectedGroup || !selectedGroupFrom || !groupReplyText.trim()) return
    setSendingGroup(true)
    setGroupError(null)
    try {
      await api.sendGroupReply({
        group_urn: selectedGroup.urn,
        from: selectedGroupFrom,
        kind: 'message',
        body: groupReplyText.trim(),
      })
      setGroupReplyText('')
      load()
    } catch (err: unknown) {
      setGroupError(err instanceof Error ? err.message : String(err))
    } finally {
      setSendingGroup(false)
    }
  }

  function closeNewMessage() {
    setNewMessageOpen(false)
    setNewMessageBody('')
    setComposeError(null)
  }

  async function sendNewMessage() {
    if (!newMessageFrom.trim() || !newMessageTo.trim() || !newMessageBody.trim()) return
    setSending(true)
    setComposeError(null)
    try {
      await api.sendReply({
        from: newMessageFrom.trim(),
        to: newMessageTo.trim(),
        kind: 'notice',
        body: newMessageBody.trim(),
      })
      closeNewMessage()
      load()
    } catch (err: unknown) {
      setComposeError(err instanceof Error ? err.message : String(err))
    } finally {
      setSending(false)
    }
  }

  function closeNewGroup() {
    setNewGroupOpen(false)
    setNewGroupName('')
    setNewGroupDescription('')
    setComposeError(null)
  }

  async function createGroup() {
    if (!newGroupName.trim() || !newGroupCreator.trim()) return
    setCreatingGroup(true)
    setComposeError(null)
    try {
      const group = await api.createGroup({
        display_name: newGroupName.trim(),
        description: newGroupDescription.trim() || undefined,
        creator_urn: newGroupCreator.trim(),
      })
      setSelectedGroupUrn(group.urn)
      setScope('groups')
      closeNewGroup()
      load()
    } catch (err: unknown) {
      setComposeError(err instanceof Error ? err.message : String(err))
    } finally {
      setCreatingGroup(false)
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
              <div className="flex items-center gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    setComposeError(null)
                    setNewMessageOpen(true)
                  }}
                >
                  <MessageSquarePlus className="h-3.5 w-3.5" />
                  New Message
                </Button>
                {scope === 'groups' && (
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => {
                      setComposeError(null)
                      setNewGroupOpen(true)
                    }}
                  >
                    <Plus className="h-3.5 w-3.5" />
                    Add Group
                  </Button>
                )}
                <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
                  <RefreshCw className={cn('h-3.5 w-3.5', loading && 'animate-spin')} />
                  Refresh
                </Button>
              </div>
            }
          />
        }
        summary={
          <SummaryCards
            cards={
              scope === 'groups'
                ? [
                    { label: 'Groups', value: groupList.length },
                    { label: 'Posts', value: totalGroupPosts },
                    {
                      label: 'Members',
                      value: selectedGroup ? selectedGroup.members.length : 0,
                      accentColor: 'var(--color-status-inbox)',
                    },
                  ]
                : [
                    {
                      label: scope === 'user' ? 'User Messages' : 'Agent Messages',
                      value: scopeTotals?.total ?? scoped.length,
                    },
                    { label: 'Unread', value: unread, accentColor: 'var(--color-status-inbox)' },
                    { label: 'Archived', value: archived, accentColor: 'var(--color-status-archived)' },
                  ]
            }
          />
        }
        filters={
          <p className="shrink-0 border-b border-border-strong bg-bg px-4 py-1.5 text-[11px] text-text-subtle">
            {scope === 'groups'
              ? 'Group message boards with inline replies.'
              : scope === 'user'
                ? 'Messages addressed to users. Opening a message marks it read; Archive soft-deletes it.'
                : 'Messages addressed to agents. Opening a message marks it read; Archive soft-deletes it.'}
          </p>
        }
      >
        {scope === 'groups' ? (
          <GroupsBoard
            groups={groupList}
            loading={loading}
            selectedGroup={selectedGroup}
            selectedFrom={selectedGroupFrom}
            replyText={groupReplyText}
            sending={sendingGroup}
            error={groupError}
            onSelectGroup={(urn) => {
              setSelectedGroupUrn(urn)
              setGroupReplyText('')
              setGroupError(null)
            }}
            onSelectFrom={setGroupFrom}
            onReplyText={setGroupReplyText}
            onSendReply={sendGroupReply}
          />
        ) : (
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
        )}
      </ListPageLayout>

      <DetailDialog
        open={newMessageOpen}
        onClose={closeNewMessage}
        title="New Message"
        footer={
          <div className="flex items-center justify-between gap-3">
            <div className="min-w-0 text-[12px] text-status-blocked">{composeError}</div>
            <Button
              variant="default"
              size="sm"
              onClick={sendNewMessage}
              disabled={sending || !newMessageFrom.trim() || !newMessageTo.trim() || !newMessageBody.trim()}
            >
              <Send className={cn('h-3.5 w-3.5', sending && 'animate-pulse')} />
              {sending ? 'Sending...' : 'Send'}
            </Button>
          </div>
        }
      >
        <div className="h-full overflow-y-auto px-4 py-3">
          <div className="space-y-4">
          <div className="grid gap-3 sm:grid-cols-2">
            <label className="space-y-1 text-[11px] text-text-subtle">
              <span>From</span>
              <input
                value={newMessageFrom}
                onChange={(e) => setNewMessageFrom(e.target.value)}
                className="h-9 w-full rounded-md border border-border bg-panel px-3 font-mono text-[12px] text-text outline-none focus:border-border-strong"
              />
            </label>
            <label className="space-y-1 text-[11px] text-text-subtle">
              <span>To</span>
              <input
                list="message-agent-options"
                value={newMessageTo}
                onChange={(e) => setNewMessageTo(e.target.value)}
                className="h-9 w-full rounded-md border border-border bg-panel px-3 font-mono text-[12px] text-text outline-none focus:border-border-strong"
              />
            </label>
          </div>
          <datalist id="message-agent-options">
            {agents.map((a) => (
              <option key={a.urn} value={a.urn} label={agentLabel(a)} />
            ))}
          </datalist>
          <label className="space-y-1 text-[11px] text-text-subtle">
            <span>Message</span>
            <Textarea
              value={newMessageBody}
              onChange={(e) => setNewMessageBody(e.target.value)}
              rows={10}
              className="min-h-[18rem] w-full"
            />
          </label>
          </div>
        </div>
      </DetailDialog>

      <DetailDialog
        open={newGroupOpen}
        onClose={closeNewGroup}
        title="Add Group"
        footer={
          <div className="flex items-center justify-between gap-3">
            <div className="min-w-0 text-[12px] text-status-blocked">{composeError}</div>
            <Button
              variant="default"
              size="sm"
              onClick={createGroup}
              disabled={creatingGroup || !newGroupName.trim() || !newGroupCreator.trim()}
            >
              <Plus className={cn('h-3.5 w-3.5', creatingGroup && 'animate-pulse')} />
              {creatingGroup ? 'Creating...' : 'Create'}
            </Button>
          </div>
        }
      >
        <div className="h-full overflow-y-auto px-4 py-3">
          <div className="space-y-4">
          <label className="space-y-1 text-[11px] text-text-subtle">
            <span>Name</span>
            <input
              value={newGroupName}
              onChange={(e) => setNewGroupName(e.target.value)}
              className="h-9 w-full rounded-md border border-border bg-panel px-3 text-[12px] text-text outline-none focus:border-border-strong"
            />
          </label>
          <label className="space-y-1 text-[11px] text-text-subtle">
            <span>Owner</span>
            <input
              list="group-owner-options"
              value={newGroupCreator}
              onChange={(e) => setNewGroupCreator(e.target.value)}
              className="h-9 w-full rounded-md border border-border bg-panel px-3 font-mono text-[12px] text-text outline-none focus:border-border-strong"
            />
          </label>
          <datalist id="group-owner-options">
            {agents.map((a) => (
              <option key={a.urn} value={a.urn} label={agentLabel(a)} />
            ))}
          </datalist>
          <label className="space-y-1 text-[11px] text-text-subtle">
            <span>Description</span>
            <Textarea
              value={newGroupDescription}
              onChange={(e) => setNewGroupDescription(e.target.value)}
              rows={10}
              className="min-h-[16rem] w-full"
            />
          </label>
          </div>
        </div>
      </DetailDialog>

      <DetailDialog
        open={selected !== null}
        onClose={closeDialog}
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
          <div className="flex h-full min-h-0 flex-col">
            <div className="min-h-0 flex-1 overflow-y-auto">
              <section className="border-t border-border-strong">
                <div className="border-b border-border-strong bg-panel px-4 py-2 text-[10px] font-semibold uppercase tracking-[.18em] text-text-subtle">
                  Message:
                </div>
                <div className="whitespace-pre-wrap break-words px-3 py-2 text-[13px] leading-6 text-text">
                  {selected.body || '(no message text)'}
                </div>
              </section>

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
            </div>

            <div className="h-28 shrink-0 border-t border-border-strong bg-bg p-3">
              <Textarea
                value={replyText}
                onChange={(e) => setReplyText(e.target.value)}
                placeholder="Write a reply..."
                className="h-full min-h-0 resize-none border-0 bg-input/30 focus-visible:border-transparent focus-visible:ring-0"
              />
            </div>
          </div>
        )}
      </DetailDialog>
    </>
  )
}
