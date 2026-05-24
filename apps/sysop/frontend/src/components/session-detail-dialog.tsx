import type { ReactNode } from 'react'
import { Check, Copy, Hourglass, Keyboard, Maximize2, Radio, RotateCw, Save, Send, Square, Terminal } from 'lucide-react'
import {
  Button,
  CopyableId,
  DetailDialog,
  DetailSection,
  JsonViewer,
  StatusBadge,
  cn,
  formatRelativeTime,
} from '@hollis-labs/sysop-ui/ui'
import type { SessionDetailInfo } from '../api/client'
import { useCopy } from './json-payload'

export type SessionActionKind = 'turn' | 'input' | 'checkpoint' | 'resume' | 'resize' | 'wait'

export function isLiveSessionState(state: string): boolean {
  return state === 'ready' || state === 'launching' || state === 'running'
}

export function isTerminalSessionState(state: string): boolean {
  return state === 'completed' || state === 'failed' || state === 'killed'
}

export function sessionLifecycleLabel(state: string): string {
  if (state === 'created') return 'Created only'
  if (state === 'ready' || state === 'launching') return 'Launching'
  if (state === 'running') return 'Live runtime'
  if (isTerminalSessionState(state)) return 'Ended'
  return 'Unknown'
}

export function sessionLifecycleHint(state: string): string {
  if (state === 'created') return 'Row exists, but no live runtime is attached yet.'
  if (state === 'ready') return 'Runtime is allocated and preparing to hand off to normal interaction.'
  if (state === 'launching') return 'Daemon is still bringing the runtime up.'
  if (state === 'running') return 'Session is live; stream, input, wait, and stop apply here.'
  if (state === 'completed') return 'Session exited normally. Resume starts a new session from checkpoint.'
  if (state === 'failed') return 'Session exited with failure. Resume starts a new session from checkpoint.'
  if (state === 'killed') return 'Session was stopped or killed. Resume starts a new session from checkpoint.'
  return 'Lifecycle state is not recognized by this client.'
}

/** Parse a stored JSON string to a value; fall back to the raw string. */
function parseJson(raw: string): unknown {
  try {
    return JSON.parse(raw)
  } catch {
    return raw
  }
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="truncate text-text-subtle">{label}</dt>
      <dd className="break-words text-text-soft">{children}</dd>
    </div>
  )
}

function planField(plan: unknown, key: string): string {
  if (!plan || typeof plan !== 'object' || Array.isArray(plan)) return ''
  const value = (plan as Record<string, unknown>)[key]
  return typeof value === 'string' ? value : ''
}

function CopyText({ text, label = text }: { text: string; label?: string }) {
  const { copied, copy } = useCopy()
  return (
    <button
      type="button"
      onClick={() => copy(text)}
      title={`Copy ${text}`}
      className="inline-flex min-w-0 items-center gap-1 font-mono text-[10px] text-text-subtle transition-colors hover:text-text-muted"
    >
      <span className="truncate">{label}</span>
      {copied ? (
        <Check className="h-2.5 w-2.5 shrink-0 text-status-indexed" />
      ) : (
        <Copy className="h-2.5 w-2.5 shrink-0" />
      )}
    </button>
  )
}

function CopyIconButton({ text, label }: { text: string; label: string }) {
  const { copied, copy } = useCopy()
  return (
    <button
      type="button"
      title={label}
      aria-label={copied ? 'Copied' : label}
      onClick={() => copy(text)}
      className="rounded p-1 text-text-subtle transition-colors hover:bg-panel-hover hover:text-text"
    >
      {copied ? (
        <Check className="h-3.5 w-3.5 text-status-indexed" />
      ) : (
        <Copy className="h-3.5 w-3.5" />
      )}
    </button>
  )
}

function CopyCommandButton({ text, label }: { text: string; label: string }) {
  const { copied, copy } = useCopy()
  return (
    <Button variant="outline" size="sm" onClick={() => copy(text)} title={text}>
      {copied ? (
        <Check className="h-3.5 w-3.5 text-status-indexed" />
      ) : (
        <Terminal className="h-3.5 w-3.5" />
      )}
      {copied ? 'Copied' : label}
    </Button>
  )
}

/**
 * Detail view for one session — meta, lifecycle events, client attachments,
 * the launch plan, and the logical agent's checkpoints. Fed by the sysop
 * /api/sessions/detail composite endpoint.
 */
export function SessionDetailDialog({
  detail,
  loading,
  error,
  onClose,
  onAction,
  onStop,
  onStream,
  onPolicy,
}: {
  detail: SessionDetailInfo | null
  loading: boolean
  error: string | null
  onClose: () => void
  onAction?: (kind: SessionActionKind, detail: SessionDetailInfo) => void
  onStop?: (detail: SessionDetailInfo) => void
  onStream?: (detail: SessionDetailInfo) => void
  onPolicy?: (detail: SessionDetailInfo) => void
}) {
  const open = loading || error !== null || detail !== null
  const s = detail?.session
  const launchPlan = detail?.launch_plan ? parseJson(detail.launch_plan) : null
  const launchProfile = planField(launchPlan, 'launch_id') || s?.launch_id || ''
  const bootProfilePath = planField(launchPlan, 'boot_profile_file')
  const liveState = s ? isLiveSessionState(s.state) : false

  return (
    <DetailDialog
      open={open}
      onClose={onClose}
      title={s ? `Session ${s.id.slice(0, 16)}` : loading ? 'Loading session…' : 'Session'}
      badge={s ? <StatusBadge status={s.state} /> : null}
      meta={
        s ? (
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-text-subtle">
            <CopyableId id={s.id} label={s.id.slice(0, 16)} />
            <span>created {formatRelativeTime(s.created_at)}</span>
            <span>updated {formatRelativeTime(s.updated_at)}</span>
          </div>
        ) : null
      }
      footer={
        detail && s ? (
          <div className="flex w-full flex-wrap items-center justify-between gap-2">
            <div className="flex flex-wrap gap-2">
              <Button
                variant="outline"
                size="sm"
                onClick={() => onStream?.(detail)}
                disabled={!liveState}
                title={liveState ? 'Open live output stream' : 'Live attach only applies to launched runtime-backed sessions.'}
              >
                <Radio className="h-3.5 w-3.5" />
                Live
              </Button>
              <CopyCommandButton text={`mux sessions attach ${s.id}`} label="Attach" />
              <CopyCommandButton text={`mux sessions tail --follow ${s.id}`} label="Tail" />
            </div>
            <div className="flex flex-wrap justify-end gap-2">
              <Button
                variant="outline"
                size="sm"
                onClick={() => onStop?.(detail)}
                disabled={!liveState}
                title={liveState ? 'Stop live runtime' : 'Stop only applies while the runtime is live.'}
              >
                <Square className="h-3.5 w-3.5" />
                Stop Live
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() => onPolicy?.(detail)}
                disabled={!s.logical_agent_id}
                title={
                  s.logical_agent_id
                    ? 'Edit daemon-honored logical agent policy.'
                    : 'No logical agent is associated.'
                }
              >
                <RotateCw className="h-3.5 w-3.5" />
                Policy
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() => onAction?.('resume', detail)}
                disabled={!s.logical_agent_id}
                title={
                  s.logical_agent_id
                    ? 'Start a new session from the latest checkpoint for this logical agent.'
                    : 'No logical agent is associated.'
                }
              >
                <RotateCw className="h-3.5 w-3.5" />
                New From Checkpoint
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() => onAction?.('wait', detail)}
                disabled={!liveState}
                title={liveState ? 'Wait for session exit' : 'Wait only applies while the runtime is live.'}
              >
                <Hourglass className="h-3.5 w-3.5" />
                Wait
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() => onAction?.('resize', detail)}
                disabled={!liveState}
                title={liveState ? 'Resize session PTY' : 'Resize only applies while the runtime is live.'}
              >
                <Maximize2 className="h-3.5 w-3.5" />
                Resize
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() => onAction?.('checkpoint', detail)}
                disabled={!s.logical_agent_id}
                title={s.logical_agent_id ? 'Create checkpoint' : 'No logical agent is associated.'}
              >
                <Save className="h-3.5 w-3.5" />
                Checkpoint
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() => onAction?.('input', detail)}
                disabled={!liveState}
                title={liveState ? 'Send raw PTY input' : 'Raw input only applies while the runtime is live.'}
              >
                <Keyboard className="h-3.5 w-3.5" />
                Input
              </Button>
              <Button
                variant="default"
                size="sm"
                onClick={() => onAction?.('turn', detail)}
                disabled={!liveState}
                title={liveState ? 'Send agent turn' : 'Turns only apply while the runtime is live.'}
              >
                <Send className="h-3.5 w-3.5" />
                Turn
              </Button>
            </div>
          </div>
        ) : null
      }
    >
      {error && <p className="text-[13px] text-status-blocked">{error}</p>}
      {loading && !detail && <p className="text-[13px] text-text-subtle">Loading session detail…</p>}

      {detail && s && (
        <>
          <DetailSection title="Session">
            <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <Field label="Launch">
                {s.launch_id ? <CopyText text={s.launch_id} /> : '—'}
              </Field>
              <Field label="Project">{s.project_id || '—'}</Field>
              <Field label="Logical agent">{s.logical_agent_id || '—'}</Field>
              <Field label="Provider">
                {s.provider_id || '—'}
                {s.provider_kind ? ` (${s.provider_kind})` : ''}
              </Field>
              <Field label="Workspace">
                {s.workspace ? <CopyText text={s.workspace} /> : '—'}
              </Field>
              <Field label="PID">{s.pid ?? '—'}</Field>
              <Field label="Exit code">{s.exit_code ?? '—'}</Field>
              <Field label="Ended">{s.ended_at ? formatRelativeTime(s.ended_at) : '—'}</Field>
              <Field label="Lifecycle">{sessionLifecycleLabel(s.state)}</Field>
              {detail.group_id && <Field label="Group">{detail.group_id}</Field>}
            </dl>
            <p className="mt-3 text-[12px] text-text-soft">{sessionLifecycleHint(s.state)}</p>
            {s.logical_agent_id && (
              <p className="mt-2 text-[12px] text-text-subtle">
                Checkpoint resume does not reopen this row. It creates a new session for the same
                logical agent from its latest checkpoint and stored launch profile.
              </p>
            )}
            <p className="mt-2 text-[12px] text-text-subtle">
              Attach, retention, and checkpoint cleanup policies are still backend-defined. Sysop
              does not expose overrides until the daemon has a real policy surface.
            </p>
          </DetailSection>

          <DetailSection title={`Events (${detail.events.length})`}>
            {detail.events.length === 0 ? (
              <p className="text-[12px] text-text-subtle">No events recorded for this session.</p>
            ) : (
              <div className="max-h-64 space-y-1 overflow-auto">
                {detail.events.map((ev) => (
                  <div key={ev.seq} className="flex items-baseline gap-2 text-[11px]">
                    <span className="w-16 shrink-0 text-text-subtle">
                      {formatRelativeTime(ev.at)}
                    </span>
                    <span className="w-16 shrink-0 uppercase tracking-[.1em] text-text-subtle">
                      {ev.scope}
                    </span>
                    <span className="shrink-0 font-mono text-text">{ev.kind}</span>
                    {ev.payload && (
                      <span className="truncate font-mono text-text-subtle/80">{ev.payload}</span>
                    )}
                  </div>
                ))}
              </div>
            )}
          </DetailSection>

          <DetailSection title={`Client Attachments (${detail.attachments.length})`}>
            {detail.attachments.length === 0 ? (
              <p className="text-[12px] text-text-subtle">No clients have attached.</p>
            ) : (
              <div className="space-y-1">
                {detail.attachments.map((a) => (
                  <div key={a.id} className="flex items-center gap-2 text-[11px]">
                    <span className="font-mono text-text">{a.client_kind || 'client'}</span>
                    <span className="text-text-subtle">
                      attached {formatRelativeTime(a.attached_at)}
                    </span>
                    <span
                      className={cn(
                        'rounded border px-1.5 py-0.5 text-[10px] uppercase tracking-wider',
                        a.detached_at
                          ? 'border-border bg-panel-2/50 text-text-subtle'
                          : 'border-status-done/40 bg-status-done/10 text-status-done',
                      )}
                    >
                      {a.detached_at ? `detached ${formatRelativeTime(a.detached_at)}` : 'active'}
                    </span>
                  </div>
                ))}
              </div>
            )}
          </DetailSection>

          <section className="border-t border-border-strong">
            <div className="flex min-w-0 items-center justify-between gap-3 border-b border-border-strong bg-panel px-4 py-2">
              <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1">
                <span className="text-[10px] font-semibold uppercase tracking-[.18em] text-text-subtle">
                  Launch Plan
                </span>
                {launchProfile && (
                  <span className="flex min-w-0 items-center gap-1">
                    <span className="text-[10px] uppercase tracking-[.12em] text-text-subtle/70">
                      profile
                    </span>
                    <CopyText text={launchProfile} />
                  </span>
                )}
                {bootProfilePath && (
                  <span className="flex min-w-0 flex-1 items-center gap-1">
                    <span className="shrink-0 text-[10px] uppercase tracking-[.12em] text-text-subtle/70">
                      path
                    </span>
                    <CopyText text={bootProfilePath} />
                  </span>
                )}
              </div>
              {detail.launch_plan && (
                <CopyIconButton text={detail.launch_plan} label="Copy launch plan" />
              )}
            </div>
            {detail.launch_plan ? (
              <JsonViewer
                value={launchPlan}
                className="rounded-none border-0 bg-transparent px-3 py-2"
              />
            ) : (
              <p className="px-4 py-3 text-[12px] text-text-subtle">
                No launch plan stored for this session.
              </p>
            )}
          </section>

          <DetailSection title={`Checkpoints (${detail.checkpoints.length})`}>
            {detail.checkpoints.length === 0 ? (
              <p className="text-[12px] text-text-subtle">
                No checkpoints for this session's logical agent.
              </p>
            ) : (
              <div className="space-y-2">
                {detail.checkpoints.map((cp) => (
                  <div key={cp.id} className="rounded-md border border-border bg-panel/40 p-3">
                    <div className="mb-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-text-subtle">
                      {cp.status && <StatusBadge status={cp.status} />}
                      <CopyableId id={cp.id} label={cp.id.slice(0, 12)} />
                      <span>{formatRelativeTime(cp.created_at)}</span>
                      {cp.task_id && <span>task {cp.task_id}</span>}
                    </div>
                    {cp.summary && <p className="text-[12px] text-text">{cp.summary}</p>}
                    <dl className="mt-1.5 grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-1 text-[11px]">
                      {cp.completed_work && <Field label="Completed">{cp.completed_work}</Field>}
                      {cp.pending_work && <Field label="Pending">{cp.pending_work}</Field>}
                      {cp.key_decisions && <Field label="Key decisions">{cp.key_decisions}</Field>}
                      {cp.next_recommendation && (
                        <Field label="Next">{cp.next_recommendation}</Field>
                      )}
                    </dl>
                  </div>
                ))}
              </div>
            )}
          </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}
