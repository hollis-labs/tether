import { useCallback, useEffect, useRef, useState } from 'react'
import { RefreshCw } from 'lucide-react'
import { Button, EmptyState } from '@hollis-labs/sysop-ui/ui'
import { useApi } from '../api/context'
import type { DaemonLogsInfo } from '../api/client'

const DEFAULT_TAIL = 100

export function LogsPage() {
  const api = useApi()
  const [logs, setLogs] = useState<DaemonLogsInfo | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [tail, setTail] = useState(DEFAULT_TAIL)
  const bottomRef = useRef<HTMLDivElement>(null)

  const fetchLogs = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const result = await api.getDaemonLogs(tail)
      setLogs(result)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }, [api, tail])

  useEffect(() => {
    void fetchLogs()
  }, [fetchLogs])

  // Scroll to bottom when new log content arrives.
  useEffect(() => {
    if (logs && bottomRef.current) {
      bottomRef.current.scrollIntoView({ behavior: 'instant' })
    }
  }, [logs])

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4 p-4">
      <div className="flex items-center gap-3">
        <Button variant="ghost" size="sm" onClick={fetchLogs} disabled={loading}>
          <RefreshCw className={`h-3.5 w-3.5 ${loading ? 'animate-spin' : ''}`} />
          Refresh
        </Button>
        <label className="flex items-center gap-2 text-sm text-text-subtle">
          Lines
          <select
            className="rounded border border-border bg-bg px-2 py-0.5 text-sm text-text"
            value={tail}
            onChange={(e) => setTail(Number(e.target.value))}
          >
            {[50, 100, 200, 500].map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        {logs?.clamped && (
          <span className="text-xs text-text-subtle">
            (output capped at {logs.total} lines)
          </span>
        )}
      </div>

      {error && (
        <div className="rounded border border-red-500/30 bg-red-500/10 px-3 py-2 text-sm text-red-400">
          {error}
        </div>
      )}

      {!error && logs && logs.lines.length === 0 && (
        <EmptyState
          variant="empty"
          title="No daemon logs yet"
          description="Start the daemon with 'mux daemon start'. Logs appear here once it writes to ~/.tether/logs/muxd.log."
        />
      )}

      {logs && logs.lines.length > 0 && (
        <div className="min-h-0 flex-1 overflow-y-auto rounded border border-border bg-bg-subtle p-3 font-mono text-xs leading-relaxed text-text-subtle">
          {logs.lines.map((line, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: log lines have no stable id
            <div key={i} className="whitespace-pre-wrap break-all">
              {line}
            </div>
          ))}
          <div ref={bottomRef} />
        </div>
      )}
    </div>
  )
}
