import type { ReactNode } from 'react'
import {
  CopyableId,
  DetailDialog,
  DetailSection,
  JsonViewer,
  StatusBadge,
  cn,
  formatRelativeTime,
} from '@hollis-labs/sysop-ui'
import type { SessionDetailInfo } from '../api/client'

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
}: {
  detail: SessionDetailInfo | null
  loading: boolean
  error: string | null
  onClose: () => void
}) {
  const open = loading || error !== null || detail !== null
  const s = detail?.session

  return (
    <DetailDialog
      open={open}
      onClose={onClose}
      widthClassName="max-w-4xl"
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
    >
      {error && <p className="text-[13px] text-status-blocked">{error}</p>}
      {loading && !detail && <p className="text-[13px] text-text-subtle">Loading session detail…</p>}

      {detail && s && (
        <>
          <DetailSection title="Session">
            <dl className="grid grid-cols-[minmax(7rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <Field label="Launch">{s.launch_id || '—'}</Field>
              <Field label="Project">{s.project_id || '—'}</Field>
              <Field label="Logical agent">{s.logical_agent_id || '—'}</Field>
              <Field label="Provider">
                {s.provider_id || '—'}
                {s.provider_kind ? ` (${s.provider_kind})` : ''}
              </Field>
              <Field label="Workspace">{s.workspace || '—'}</Field>
              <Field label="PID">{s.pid ?? '—'}</Field>
              <Field label="Exit code">{s.exit_code ?? '—'}</Field>
              <Field label="Ended">{s.ended_at ? formatRelativeTime(s.ended_at) : '—'}</Field>
              {detail.group_id && <Field label="Group">{detail.group_id}</Field>}
            </dl>
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

          <DetailSection title="Launch Plan">
            {detail.launch_plan ? (
              <JsonViewer value={parseJson(detail.launch_plan)} />
            ) : (
              <p className="text-[12px] text-text-subtle">No launch plan stored for this session.</p>
            )}
          </DetailSection>

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
