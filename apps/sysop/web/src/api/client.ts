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
export interface MessageInfo {
  id: string
  kind: string
  channel?: string
  from: string
  to: string
  thread_id?: string
  in_reply_to?: string
  body: string
  content_type?: string
  scope: string
  created_at: string
  delivered_at?: string
  consumed_at?: string
  canceled_at?: string
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

export const apiClient = {
  getHealth: () => http.get<HealthInfo>('/api/health'),
  getCatalog: () => http.get<CatalogInfo>('/api/catalog'),
  getSessions: () => http.get<SessionsInfo>('/api/sessions'),
  getMessages: () => http.get<MessagesInfo>('/api/messages'),
  sendReply: (body: ReplyRequest) =>
    http.post<MessageInfo>('/api/messages', body as unknown as JsonObject),
  getEvents: () => http.get<EventsInfo>('/api/activity/events'),
  getToolCalls: () => http.get<ToolCallsInfo>('/api/activity/tool-calls'),
}

export type AppApiClient = typeof apiClient
