import { createApiClient, type JsonObject } from '@hollis-labs/sysop-ui/api'

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
  mcp_servers?: string[]
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
  profile?: string
  launch_plan?: string
  plan_error?: string
}

export interface ActionInfo {
  status: string
  session_id?: string
  workspace?: string
  log?: string
  provider_id?: string
  provider_kind?: string
  logical_agent_id?: string
  exit_code?: number
  count?: number
  output?: string
  backup_path?: string
  error?: string
}

export interface LaunchSaveRequest {
  id: string
  project: string
  agent: string
  provider: string
  workspace_mode: string
  worktree_name?: string
  include_project_boot: boolean
  include_agent_boot: boolean
  include_knowledge_base: boolean
  mcp_servers?: string[]
  env_overrides?: Record<string, string>
  native_files?: InjectedFileInfo[]
  boot_dir_overlay?: InjectedFileInfo[]
}

export interface InjectedFileInfo {
  kind?: string
  id?: string
  rel_path?: string
  content?: string
  source?: string
  mode?: number
}

export interface LaunchPreviewInfo {
  existing: boolean
  current_profile?: string
  next_profile?: string
  current_plan?: string
  next_plan?: string
  current_profile_yaml?: string
  next_profile_yaml?: string
  profile_diff?: string
  plan_diff?: string
  profile_changed: boolean
  plan_changed: boolean
  target_path?: string
  will_create_backup: boolean
  comment_loss_risk: boolean
  current_plan_error?: string
  next_plan_error?: string
  warnings?: string[]
  error?: string
}

export interface SessionsInfo {
  sessions: SessionInfo[]
  total: number
  running: number
  ended: number
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
  totals: MessageTotals
  error?: string
}

export interface MessageTotals {
  total: number
  user: MessageScopeTotals
  agent: MessageScopeTotals
  other: MessageScopeTotals
  groups: MessageScopeTotals
}

export interface MessageScopeTotals {
  total: number
  unread: number
  archived: number
}

export interface BrokerEnvelopeInfo {
  id: string
  sender?: string
  recipient?: string
  workflow_id?: string
  correlation_id?: string
  message_type?: string
  priority: number
  payload?: string
  created_at: string
  delivered_at?: string
  consumed_at?: string
  audit_json?: string
}

export interface BrokerEnvelopesInfo {
  envelopes: BrokerEnvelopeInfo[]
  error?: string
}

export interface BrokerEnvelopeQuery {
  workflow_id?: string
  recipient?: string
  correlation_id?: string
  order?: 'asc' | 'desc'
  limit?: number
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

export interface MessageAgentInfo {
  urn: string
  display_name: string
  title?: string
  status: string
}

export interface MessageAgentsInfo {
  agents: MessageAgentInfo[]
  error?: string
}

export interface GroupMemberInfo {
  member_urn: string
  display_name?: string
  role: string
  joined_at: string
  last_read_seq: number
}

export interface GroupMessageInfo {
  id: string
  group_urn: string
  group_seq: number
  from_urn: string
  kind: string
  thread_id?: string
  subject?: string
  body: string
  payload?: string
  content_type?: string
  created_at: string
}

export interface GroupInfo {
  urn: string
  display_name: string
  title?: string
  description?: string
  status: string
  created_at: string
  updated_at: string
  members: GroupMemberInfo[]
  messages: GroupMessageInfo[]
}

export interface GroupsInfo {
  groups: GroupInfo[]
  error?: string
}

export interface GroupReplyRequest {
  group_urn: string
  from: string
  kind?: string
  body: string
  thread_id?: string
  content_type?: string
}

export interface GroupCreateRequest {
  display_name: string
  description?: string
  creator_urn: string
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
  total: number
  error?: string
}

export interface ToolCallInfo {
  id: number
  session_id?: string
  server?: string
  tool_name: string
  args_schema_fp?: string
  duration_ms: number
  ok: boolean
  error?: string
  timestamp: string
  payload?: string
}

export interface ToolCallsInfo {
  tool_calls: ToolCallInfo[]
  total: number
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
    failure_pct: number
    avg_seconds: number
    recent_24h: number
    trend: number[]
    by_state: NameCount[]
    by_provider: NameCount[]
    by_project: NameCount[]
  }
  tool_calls: {
    total: number
    ok: number
    errors: number
    success_pct: number
    p50_ms: number
    p95_ms: number
    avg_ms: number
    recent_1h: number
    slow_calls: number
    sessions: number
    top_tools: NameCount[]
    top_errors: NameCount[]
    by_server: NameCount[]
    latency: NameCount[]
    trend: number[]
  }
  messages: {
    total: number
    unread: number
    archived: number
    recent_24h: number
    by_kind: NameCount[]
    by_scope: NameCount[]
    trend: number[]
  }
  events: {
    total: number
    recent_1h: number
    latest_seq: number
    by_scope: NameCount[]
    by_kind: NameCount[]
    trend: number[]
  }
  ai: {
    configured_providers: number
    enabled_providers: number
    routes: number
    requests: number
    successes: number
    errors: number
    budget_rejections: number
    input_tokens: number
    output_tokens: number
    estimated_cost_usd: number
    by_provider: NameCount[]
    by_model: NameCount[]
    by_event_type: NameCount[]
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

export interface LogicalAgentPolicyInfo {
  logical_agent_id: string
  name?: string
  launch_id?: string
  checkpoint_policy: string
  checkpoint_status?: string
  updated_at?: string
  error?: string
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

// MCPServerInfo mirrors the catalog mcp-servers/*.yaml entry. `token` and
// env values are not surfaced (secret-bearing); only `has_token` + env keys.
export interface MCPServerInfo {
  id: string
  transport: string
  command?: string
  args?: string[]
  url?: string
  env_keys?: string[]
  has_token: boolean
  scopes?: string[]
  tags?: string[]
  enabled: boolean
  visibility: string
  project_refs?: string[]
  launch_refs?: string[]
  server_status?: string
  server_error?: string
}

export interface MCPServersInfo {
  servers: MCPServerInfo[]
  error?: string
}

export interface MCPServerSaveRequest {
  id: string
  transport: string
  command?: string
  args?: string[]
  url?: string
  token?: string
  env?: Record<string, string>
  scopes?: string[]
  tags?: string[]
  enabled?: boolean
}

// MCPToolInfo is a usage aggregate over the proxy_events ring buffer.
export interface MCPToolInfo {
  name: string
  server: string
  calls: number
  errors: number
  success_pct: number
  avg_ms: number
  p95_ms: number
  last_seen: string
  live: boolean
  source: string
  server_status?: string
  server_error?: string
}

export interface MCPToolsInfo {
  tools: MCPToolInfo[]
  total_calls: number
  error?: string
}

export interface RegistryCallbackInfo {
  scheme: string
  target: string
}

export interface RegistrySkillInfo {
  name: string
  learned_at: string
  via?: string
  level?: string
}

export interface RegistryLinkInfo {
  kind: string
  target: string
}

export interface RegistryProfileInfo {
  urn: string
  kind: 'agent' | 'project' | string
  mux_instance_id: string
  display_name: string
  title?: string
  role?: string
  description?: string
  avatar?: string
  project?: string
  status: string
  callback?: RegistryCallbackInfo
  cached_at?: string
  health_status?: string
  last_seen_at?: string
  host_address?: string
  last_updated_by?: string
  capabilities?: string[]
  skills?: RegistrySkillInfo[]
  links?: RegistryLinkInfo[]
  created_at: string
  updated_at: string
}

export interface RegistryInfo {
  rows: RegistryProfileInfo[]
  error?: string
}

export interface RegistrySaveRequest {
  kind: 'agent' | 'project'
  urn?: string
  display_name: string
  title?: string
  role?: string
  description?: string
  avatar?: string
  project?: string
  status?: string
  health_status?: string
  host_address?: string
  last_updated_by?: string
  callback?: RegistryCallbackInfo
  capabilities?: string[]
  skills?: RegistrySkillInfo[]
  links?: RegistryLinkInfo[]
}

export interface RegistryBootstrapErrorInfo {
  path: string
  reason: string
}

export interface RegistryBootstrapReportInfo {
  Imported: number
  Skipped: number
  Refreshed: number
  Errors?: RegistryBootstrapErrorInfo[]
}

export interface SettingsInfo {
  server: SettingsServerInfo
  paths: SettingsPathsInfo
  daemon: SettingsDaemonInfo
  catalog: SettingsCatalogInfo
  mcp: SettingsMCPInfo
  providers: SettingsProviderInfo[]
  launches: SettingsLaunchesInfo
  sessions: SettingsSessionsInfo
  roadmap: SettingsRoadmapInfo[]
  error?: string
}

export interface SettingsServerInfo {
  http_addr: string
  catalog_root: string
  pid: number
  started_at: string
  uptime_sec: number
}

export interface SettingsPathInfo {
  path: string
  exists: boolean
}

export interface SettingsPathsInfo {
  catalog_root: SettingsPathInfo
  state_db: SettingsPathInfo
  workspace_root: SettingsPathInfo
  temp_root: SettingsPathInfo
  launch_specs_root: SettingsPathInfo
  projects_root: SettingsPathInfo
  agents_root: SettingsPathInfo
  providers_root: SettingsPathInfo
  launches_root: SettingsPathInfo
  boot_root: SettingsPathInfo
  mcp_servers_root: SettingsPathInfo
}

export interface SettingsDaemonInfo {
  listen_addr: string
  listen_kind: string
  listen_endpoint: string
  socket_exists: boolean
  pid_file: string
  pid_file_exists: boolean
  pid?: number
  pid_running: boolean
  shutdown_timeout: string
  permission_mode: string
  launch_engine: string
  launch_specs_root: string
  config_modified_at?: string
  restart_required: boolean
}

export interface SettingsCatalogInfo {
  version: string
  projects: number
  agents: number
  providers: number
  launches: number
}

export interface SettingsMCPInfo {
  servers: number
  enabled: number
  with_tokens: number
  transports: string[]
  root: string
  root_exists: boolean
  config_surface: string
  config_modified_at?: string
  restart_required: boolean
}

export interface SettingsProviderInfo {
  id: string
  type: string
  provider: string
  runtime_kind: string
  command: string
  args?: string[]
  adapter?: string
  bootstrap_mode: string
  bootstrap_prefix?: string
  env_mode: string
  env_passthrough?: string[]
  env_redact?: string[]
  referenced_launches: number
}

export interface SettingsLaunchesInfo {
  total: number
  with_mcp: number
  with_injection: number
  with_env: number
  with_worktree: number
}

export interface SettingsSessionsInfo {
  total: number
  running: number
  ended: number
  error?: string
}

export interface SettingsRoadmapInfo {
  area: string
  status: string
  next: string
}

export interface AIUsageBudgetPolicyInfo {
  max_cost_usd?: number
  window?: string
  scope?: string
}

export interface AIPolicyInfo {
  allow_reasoning?: boolean
  allow_tools?: boolean
  allow_attachments?: boolean
  max_output_tokens?: number
  max_cost_usd?: number
  usage_budget?: AIUsageBudgetPolicyInfo
}

export interface AIProviderSettingsInfo {
  id: string
  type: string
  model?: string
  models?: string[]
  default_model?: string
  secret_ref?: string
  base_url?: string
  enabled: boolean
  policy?: AIPolicyInfo
}

export interface AIRouteSettingsInfo {
  provider: string
  model: string
  mode?: string
  intent?: string
  requires_reasoning?: boolean
  requires_tools?: boolean
  policy?: AIPolicyInfo
}

export interface AIConfigInfo {
  policy?: AIPolicyInfo
  default_provider_order?: string[]
  providers: AIProviderSettingsInfo[]
  routes: AIRouteSettingsInfo[]
}

export interface AISettingsInfo {
  config: AIConfigInfo
  runtime: {
    daemon_reachable: boolean
    providers: number
    models: number
    routes: number
    last_error?: string
  }
  error?: string
}

export interface AIRuntimeProviderInfo {
  id: string
  type: string
  default_model?: string
  models?: string[]
  base_url?: string
}

export interface AIRuntimeModelInfo {
  configured_provider_id: string
  vendor_provider_id: string
  id: string
  name?: string
  family?: string
  context_window?: number
  max_output_tokens?: number
  input_modalities?: string[]
  output_modalities?: string[]
}

export interface AIRuntimeRouteInfo {
  provider: string
  model: string
  mode?: string
  intent?: string
  requires_reasoning?: boolean
  requires_tools?: boolean
  allow_reasoning?: boolean
  allow_tools?: boolean
  allow_attachments?: boolean
  max_output_tokens?: number
  max_cost_usd?: number
  usage_budget?: {
    level?: string
    max_cost_usd?: number
    window?: string
    scope?: string
  }
}

export interface AIRuntimeInfo {
  providers: AIRuntimeProviderInfo[]
  models: AIRuntimeModelInfo[]
  routes: AIRuntimeRouteInfo[]
  error?: string
}

export interface AIUsageBreakdownInfo {
  key: string
  requests: number
  successes: number
  errors: number
  latency_ms: number
  input_tokens?: number
  output_tokens?: number
  cache_read_tokens?: number
  cache_write_tokens?: number
  reasoning_tokens?: number
  estimated_cost_usd?: number
}

export interface AIUsageInfo {
  summary: {
    requests: number
    successes: number
    errors: number
    latency_ms: number
    input_tokens?: number
    output_tokens?: number
    cache_read_tokens?: number
    cache_write_tokens?: number
    reasoning_tokens?: number
    estimated_cost_usd?: number
    by_provider?: AIUsageBreakdownInfo[]
    by_model?: AIUsageBreakdownInfo[]
    by_operation?: AIUsageBreakdownInfo[]
  }
  error?: string
}

export interface AIAuditEventInfo {
  id: number
  event_type: string
  request_id?: string
  session_id?: string
  caller_id?: string
  operation: string
  provider?: string
  model?: string
  policy_version?: string
  latency_ms: number
  success: boolean
  refusal?: string
  error?: string
  input_tokens?: number
  output_tokens?: number
  cache_read_tokens?: number
  cache_write_tokens?: number
  reasoning_tokens?: number
  estimated_cost_usd?: number
  request_summary?: string
  response_summary?: string
  timestamp: string
}

export interface AIAuditInfo {
  events: AIAuditEventInfo[]
  count: number
  error?: string
}

export interface AIBudgetInfo {
  provider: string
  model: string
  mode?: string
  intent?: string
  usage_budget: {
    level?: string
    max_cost_usd?: number
    window?: string
    scope?: string
  }
  window_start: string
  spent_cost_usd?: number
  remaining_cost_usd?: number
  exhausted: boolean
  filter: {
    provider?: string
    model?: string
    session_id?: string
    caller_id?: string
    operation?: string
  }
  error?: string
}

export interface AIBudgetsInfo {
  budgets: AIBudgetInfo[]
  count: number
  error?: string
}

export interface AIProviderCatalogModelInfo {
  id: string
  name?: string
  family?: string
  context_window?: number
  max_output_tokens?: number
  input_modalities?: string[]
  output_modalities?: string[]
  supports_tools?: boolean
  supports_reasoning?: boolean
  supports_attachments?: boolean
  input_cost_usd_per_mtok?: number
  output_cost_usd_per_mtok?: number
}

export interface AIProviderCatalogInfo {
  provider_type: string
  vendor_provider_id?: string
  vendor_provider_name?: string
  models: AIProviderCatalogModelInfo[]
  last_fetched_at?: string
  from_cache_only?: boolean
  error?: string
}

export interface GlobalSettingsSaveRequest {
  shutdown_timeout: string
  permission_mode: string
  launch_engine: string
  launch_specs_root: string
  workspace_root: string
  state_db: string
  temp_root: string
}

export interface ProviderSaveRequest {
  id: string
  type: string
  provider?: string
  runtime_kind?: string
  command?: string
  args?: string[]
  adapter?: string
  bootstrap_mode?: string
  bootstrap_prefix?: string
  env_mode?: string
  env_passthrough?: string[]
  env_redact?: string[]
}

export interface DaemonLogsInfo {
  lines: string[]
  total: number
  clamped?: boolean
}

export interface FSValidateResponse {
  exists: boolean
  executable?: boolean
  resolved?: string
  note?: string
}

export interface FSDetectResponse {
  brand: string
  found: boolean
  path?: string
  source: string
}

export const apiClient = {
  getHealth: () => http.get<HealthInfo>('/api/health'),
  getSettings: () => http.get<SettingsInfo>('/api/settings'),
  saveGlobalSettings: (body: GlobalSettingsSaveRequest) =>
    http.post<ActionInfo>('/api/settings/global/save', body as unknown as JsonObject),
  saveProvider: (body: ProviderSaveRequest) =>
    http.post<ActionInfo>('/api/settings/providers/save', body as unknown as JsonObject),
  deleteProvider: (id: string) =>
    http.post<ActionInfo>('/api/settings/providers/delete', { id }),
  getAISettings: () => http.get<AISettingsInfo>('/api/ai/settings'),
  saveAISettings: (body: { config: AIConfigInfo }) =>
    http.post<ActionInfo>('/api/ai/settings/save', body as unknown as JsonObject),
  getAIProviderCatalog: (providerType: string) =>
    http.get<AIProviderCatalogInfo>('/api/ai/catalog/models', { query: { provider_type: providerType } }),
  getAIRuntime: () => http.get<AIRuntimeInfo>('/api/ai/runtime'),
  getAIUsage: () => http.get<AIUsageInfo>('/api/ai/usage'),
  getAIAudit: () => http.get<AIAuditInfo>('/api/ai/audit'),
  getAIBudgets: () => http.get<AIBudgetsInfo>('/api/ai/budgets'),
  runSystemResourceAction: (resource: string, action: string) =>
    http.post<ActionInfo>('/api/system/resource/action', { resource, action }),
  getOverview: () => http.get<OverviewInfo>('/api/overview'),
  getMCPServers: () => http.get<MCPServersInfo>('/api/mcp/servers'),
  saveMCPServer: (body: MCPServerSaveRequest) =>
    http.post<ActionInfo>('/api/mcp/servers/save', body as unknown as JsonObject),
  deleteMCPServer: (id: string) =>
    http.post<ActionInfo>('/api/mcp/servers/delete', { id }),
  toggleMCPServer: (id: string, enabled: boolean) =>
    http.post<ActionInfo>('/api/mcp/servers/toggle', { id, enabled }),
  getMCPTools: () => http.get<MCPToolsInfo>('/api/mcp/tools'),
  getRegistry: (
    kind: 'agent' | 'project',
    query?: {
      status?: string
      role?: string
      title?: string
      project?: string
      capability?: string
      skill_name?: string
    },
  ) => http.get<RegistryInfo>('/api/registry', { query: { kind, ...(query ?? {}) } }),
  saveRegistry: (body: RegistrySaveRequest) =>
    http.post<ActionInfo>('/api/registry/save', body as unknown as JsonObject),
  deregisterRegistry: (urn: string) =>
    http.post<ActionInfo>('/api/registry/deregister', { urn }),
  syncRegistry: (urn: string) =>
    http.post<ActionInfo>('/api/registry/sync', { urn }),
  bootstrapRegistry: (force: boolean) =>
    http.post<RegistryBootstrapReportInfo>('/api/registry/bootstrap', { force }),
  getCatalog: () => http.get<CatalogInfo>('/api/catalog'),
  saveLaunch: (body: LaunchSaveRequest) =>
    http.post<ActionInfo>('/api/launches/save', body as unknown as JsonObject),
  previewLaunch: (body: LaunchSaveRequest) =>
    http.post<LaunchPreviewInfo>('/api/launches/preview', body as unknown as JsonObject),
  deleteLaunch: (launchId: string) =>
    http.post<ActionInfo>('/api/launches/delete', { launch_id: launchId }),
  launchProfile: (launchId: string) =>
    http.post<ActionInfo>('/api/launches/launch', { launch_id: launchId }),
  getSessions: () => http.get<SessionsInfo>('/api/sessions'),
  stopSession: (id: string) => http.post<ActionInfo>('/api/sessions/stop', { id }),
  sendSessionTurn: (id: string, text: string) =>
    http.post<ActionInfo>('/api/sessions/turn', { id, text }),
  sendSessionInput: (id: string, text: string) =>
    http.post<ActionInfo>('/api/sessions/input', { id, text }),
  resizeSession: (id: string, rows: number, cols: number) =>
    http.post<ActionInfo>('/api/sessions/resize', { id, rows, cols }),
  waitSession: (id: string) => http.post<ActionInfo>('/api/sessions/wait', { id }),
  cleanupSessions: (olderThanDays: number, limit: number, dryRun: boolean) =>
    http.post<ActionInfo>('/api/sessions/cleanup', {
      older_than_days: olderThanDays,
      limit,
      dry_run: dryRun,
    }),
  checkpointSession: (id: string, status?: string, summary?: string) =>
    http.post<ActionInfo>('/api/sessions/checkpoint', { id, status: status ?? '', summary: summary ?? '' }),
  resumeLogicalAgent: (logicalAgentId: string) =>
    http.post<ActionInfo>('/api/logical-agents/resume', { logical_agent_id: logicalAgentId }),
  getLogicalAgentPolicy: (logicalAgentId: string) =>
    http.get<LogicalAgentPolicyInfo>('/api/logical-agents/policy', { query: { id: logicalAgentId } }),
  saveLogicalAgentPolicy: (body: LogicalAgentPolicyInfo) =>
    http.post<LogicalAgentPolicyInfo>('/api/logical-agents/policy', body as unknown as JsonObject),
  getSessionDetail: (id: string) =>
    http.get<SessionDetailInfo>('/api/sessions/detail', { query: { id } }),
  getMessages: () => http.get<MessagesInfo>('/api/messages'),
  sendReply: (body: ReplyRequest) =>
    http.post<MessageInfo>('/api/messages', body as unknown as JsonObject),
  getGroups: () => http.get<GroupsInfo>('/api/messages/groups'),
  sendGroupReply: (body: GroupReplyRequest) =>
    http.post<GroupMessageInfo>('/api/messages/groups', body as unknown as JsonObject),
  createGroup: (body: GroupCreateRequest) =>
    http.post<GroupInfo>('/api/messages/groups/create', body as unknown as JsonObject),
  getMessageAgents: () => http.get<MessageAgentsInfo>('/api/messages/agents'),
  getBrokerEnvelopes: (query?: BrokerEnvelopeQuery) =>
    http.get<BrokerEnvelopesInfo>('/api/broker/envelopes', { query: { ...(query ?? {}) } }),
  // Recipient-scoped, idempotent message actions. `as` is the recipient URN.
  archiveMessage: (id: string, as: string) =>
    http.post<{ status: string }>('/api/messages/archive', { id, as }),
  markRead: (id: string, as: string) =>
    http.post<{ status: string }>('/api/messages/read', { id, as }),
  getEvents: () => http.get<EventsInfo>('/api/activity/events'),
  getToolCalls: () => http.get<ToolCallsInfo>('/api/activity/tool-calls'),
  getDaemonLogs: (tail = 100) =>
    http.get<DaemonLogsInfo>('/api/logs/daemon', { query: { tail: String(tail) } }),
  validatePath: (path: string, kind: 'file' | 'dir' | 'executable') =>
    http.post<FSValidateResponse>('/api/fs/validate', { path, kind } as unknown as JsonObject),
  detectBrand: (brand: string) =>
    http.get<FSDetectResponse>('/api/fs/detect', { query: { brand } }),
}

export type AppApiClient = typeof apiClient
