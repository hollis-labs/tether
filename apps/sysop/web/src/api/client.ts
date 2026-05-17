import { createApiClient, type JsonObject } from '@hollis-labs/sysop-ui'

// Same-origin: the Go binary serves both this SPA and the API, so an empty
// baseUrl resolves every request against the current origin.
const http = createApiClient({ baseUrl: '' })

export interface HealthInfo {
  status: string
  catalog_root: string
  error?: string
}

export interface CatalogInfo {
  projects: ProjectInfo[]
  agents: AgentInfo[]
  providers: ProviderInfo[]
  launches: LaunchInfo[]
}

export interface ProjectInfo {
  id: string
  name: string
  repo_root: string
  mode: string
}

export interface AgentInfo {
  id: string
  name: string
  roles?: string[]
  skills?: string[]
}

export interface ProviderInfo {
  id: string
  type: string
  provider: string
  runtime_kind: string
  command: string
}

export interface LaunchInfo {
  id: string
  project: string
  agent: string
  provider: string
  workspace_mode: string
  native_files: number
  boot_overlay: number
}

export interface SessionsInfo {
  sessions: SessionInfo[]
  error?: string
}

export interface SessionInfo {
  id: string
  launch_id: string
  project_id: string
  logical_agent_id: string
  provider_id: string
  provider_kind: string
  workspace: string
  state: string
  pid?: number
  exit_code?: number
  created_at: string
  updated_at: string
  ended_at?: string
}

// MessageInfo mirrors the messageDTO served by GET /api/messages. `scope`
// is derived from the recipient URN: 'user', 'agent', or 'other'.
// `subject`/`body` are the projected display fields; `payload` is the raw
// (possibly structured-JSON) message payload.
export interface MessageInfo {
  id: string
  kind: string
  channel?: string
  from: string
  to: string
  thread_id?: string
  in_reply_to?: string
  subject?: string
  body: string
  payload?: string
  content_type?: string
  scope: string
  created_at: string
  delivered_at?: string
  consumed_at?: string
  canceled_at?: string
  read_at?: string
  archived_at?: string
}

export interface MessagesInfo {
  messages: MessageInfo[]
  error?: string
}

// ReplyRequest is the POST /api/messages body. `from`/`to` are messaging
// URNs (msg://<kind>/<authority>/<id>).
export interface ReplyRequest {
  from: string
  to: string
  kind?: string
  body: string
  in_reply_to?: string
  thread_id?: string
  content_type?: string
}

export interface EventInfo {
  seq: number
  at: string
  scope: string
  session_id?: string
  kind: string
  payload?: string
}

export interface EventsInfo {
  events: EventInfo[]
  error?: string
}

export interface ToolCallInfo {
  id: number
  session_id?: string
  server?: string
  tool_name: string
  duration_ms: number
  ok: boolean
  error?: string
  timestamp: string
}

export interface ToolCallsInfo {
  tool_calls: ToolCallInfo[]
  error?: string
}

export interface NameCount {
  name: string
  count: number
}

// OverviewInfo mirrors the /api/overview aggregate payload. `trend` arrays
// are time-bucketed counts, oldest → newest.
export interface OverviewInfo {
  sessions: {
    total: number
    running: number
    ended: number
    success_pct: number
    avg_seconds: number
    trend: number[]
  }
  tool_calls: {
    total: number
    ok: number
    errors: number
    success_pct: number
    p50_ms: number
    p95_ms: number
    top_tools: NameCount[]
    top_errors: NameCount[]
    trend: number[]
  }
  messages: {
    total: number
    unread: number
    archived: number
    by_kind: NameCount[]
    trend: number[]
  }
  events: {
    total: number
    by_scope: NameCount[]
    trend: number[]
  }
  catalog: {
    projects: number
    agents: number
    providers: number
    launches: number
  }
  health: HealthInfo
  error?: string
}

export interface AttachmentInfo {
  id: string
  client_kind: string
  attached_at: string
  detached_at?: string
}

export interface CheckpointInfo {
  id: string
  status?: string
  task_id?: string
  workflow_id?: string
  summary?: string
  completed_work?: string
  pending_work?: string
  key_decisions?: string
  next_recommendation?: string
  created_at: string
  source_session_id?: string
}

export interface SessionDetailInfo {
  session: SessionInfo
  group_id?: string
  events: EventInfo[]
  attachments: AttachmentInfo[]
  launch_plan?: string
  checkpoints: CheckpointInfo[]
  error?: string
}

export const apiClient = {
  getHealth: () => http.get<HealthInfo>('/api/health'),
  getOverview: () => http.get<OverviewInfo>('/api/overview'),
  getCatalog: () => http.get<CatalogInfo>('/api/catalog'),
  getSessions: () => http.get<SessionsInfo>('/api/sessions'),
  getSessionDetail: (id: string) =>
    http.get<SessionDetailInfo>('/api/sessions/detail', { query: { id } }),
  getMessages: () => http.get<MessagesInfo>('/api/messages'),
  sendReply: (body: ReplyRequest) =>
    http.post<MessageInfo>('/api/messages', body as unknown as JsonObject),
  // Recipient-scoped, idempotent message actions. `as` is the recipient URN.
  archiveMessage: (id: string, as: string) =>
    http.post<{ status: string }>('/api/messages/archive', { id, as }),
  markRead: (id: string, as: string) =>
    http.post<{ status: string }>('/api/messages/read', { id, as }),
  getEvents: () => http.get<EventsInfo>('/api/activity/events'),
  getToolCalls: () => http.get<ToolCallsInfo>('/api/activity/tool-calls'),
}

export type AppApiClient = typeof apiClient
