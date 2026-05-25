import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import {
  Bot,
  BrainCircuit,
  CircleDollarSign,
  Pencil,
  Plus,
  RefreshCw,
  Route,
  Shield,
  Trash2,
} from 'lucide-react'
import {
  Button,
  Combobox,
  CopyableId,
  DetailDialog,
  DetailSection,
  EmptyState,
  Pill,
  SummaryCards,
  TransferList,
  cn,
  formatRelativeTime,
} from '@hollis-labs/sysop-ui/ui'
import { DataTable, type ColumnDef } from '@hollis-labs/sysop-ui/data'
import { ListPageLayout, TabStrip, type TabStripItem } from '@hollis-labs/sysop-ui/layout'
import { useApi } from '../api/context'
import type {
  AIAuditEventInfo,
  AIAuditInfo,
  AIBudgetInfo,
  AIBudgetsInfo,
  AIConfigInfo,
  AIPolicyInfo,
  AIProviderCatalogInfo,
  AIProviderCatalogModelInfo,
  AIProviderSettingsInfo,
  AIRouteSettingsInfo,
  AIRuntimeInfo,
  AISettingsInfo,
  AIUsageBreakdownInfo,
  AIUsageBudgetPolicyInfo,
  AIUsageInfo,
} from '../api/client'

type TabKey = 'config' | 'providers' | 'routes' | 'runtime' | 'usage' | 'audit' | 'budgets'
type BoolSelect = '' | 'true' | 'false'
type DetailView =
  | { kind: 'provider'; item: AIProviderSettingsInfo }
  | { kind: 'route'; item: AIRouteSettingsInfo }
  | { kind: 'audit'; item: AIAuditEventInfo }
  | { kind: 'budget'; item: AIBudgetInfo }

interface PolicyFormState {
  allowReasoning: BoolSelect
  allowTools: BoolSelect
  allowAttachments: BoolSelect
  maxOutputTokens: string
  maxCostUSD: string
  usageBudgetMaxCostUSD: string
  usageBudgetWindow: string
  usageBudgetScope: string
}

interface ProviderFormState {
  originalID?: string
  id: string
  type: string
  model: string
  models: string[]
  defaultModel: string
  secretRef: string
  baseURL: string
  enabled: boolean
  policy: PolicyFormState
}

interface RouteFormState {
  originalKey?: string
  provider: string
  model: string
  mode: string
  intent: string
  requiresReasoning: BoolSelect
  requiresTools: BoolSelect
  policy: PolicyFormState
}

function splitList(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean)
}

function boolToSelect(value?: boolean): BoolSelect {
  if (value === true) return 'true'
  if (value === false) return 'false'
  return ''
}

function selectToBool(value: BoolSelect): boolean | undefined {
  if (value === 'true') return true
  if (value === 'false') return false
  return undefined
}

function parseOptionalNumber(raw: string): number | undefined {
  const trimmed = raw.trim()
  if (!trimmed) return undefined
  const parsed = Number(trimmed)
  return Number.isFinite(parsed) ? parsed : undefined
}

function emptyPolicyForm(): PolicyFormState {
  return {
    allowReasoning: '',
    allowTools: '',
    allowAttachments: '',
    maxOutputTokens: '',
    maxCostUSD: '',
    usageBudgetMaxCostUSD: '',
    usageBudgetWindow: '',
    usageBudgetScope: '',
  }
}

function policyToForm(policy?: AIPolicyInfo): PolicyFormState {
  return {
    allowReasoning: boolToSelect(policy?.allow_reasoning),
    allowTools: boolToSelect(policy?.allow_tools),
    allowAttachments: boolToSelect(policy?.allow_attachments),
    maxOutputTokens: policy?.max_output_tokens ? String(policy.max_output_tokens) : '',
    maxCostUSD: policy?.max_cost_usd ? String(policy.max_cost_usd) : '',
    usageBudgetMaxCostUSD: policy?.usage_budget?.max_cost_usd
      ? String(policy.usage_budget.max_cost_usd)
      : '',
    usageBudgetWindow: policy?.usage_budget?.window ?? '',
    usageBudgetScope: policy?.usage_budget?.scope ?? '',
  }
}

function formToPolicy(form: PolicyFormState): AIPolicyInfo | undefined {
  const usageBudget: AIUsageBudgetPolicyInfo | undefined =
    form.usageBudgetMaxCostUSD.trim() || form.usageBudgetWindow.trim() || form.usageBudgetScope.trim()
      ? {
          max_cost_usd: parseOptionalNumber(form.usageBudgetMaxCostUSD),
          window: form.usageBudgetWindow.trim() || undefined,
          scope: form.usageBudgetScope.trim() || undefined,
        }
      : undefined
  const policy: AIPolicyInfo = {
    allow_reasoning: selectToBool(form.allowReasoning),
    allow_tools: selectToBool(form.allowTools),
    allow_attachments: selectToBool(form.allowAttachments),
    max_output_tokens: parseOptionalNumber(form.maxOutputTokens),
    max_cost_usd: parseOptionalNumber(form.maxCostUSD),
    usage_budget: usageBudget,
  }
  const hasValue = Object.values(policy).some((value) => value !== undefined)
  return hasValue ? policy : undefined
}

function emptyProviderForm(): ProviderFormState {
  return {
    id: '',
    type: 'anthropic',
    model: '',
    models: [],
    defaultModel: '',
    secretRef: '',
    baseURL: '',
    enabled: true,
    policy: emptyPolicyForm(),
  }
}

function providerToForm(provider: AIProviderSettingsInfo): ProviderFormState {
  return {
    originalID: provider.id,
    id: provider.id,
    type: provider.type,
    model: provider.model ?? '',
    models: provider.models ?? [],
    defaultModel: provider.default_model ?? '',
    secretRef: provider.secret_ref ?? '',
    baseURL: provider.base_url ?? '',
    enabled: provider.enabled,
    policy: policyToForm(provider.policy),
  }
}

function formToProvider(form: ProviderFormState): AIProviderSettingsInfo {
  const models = form.models.map((item) => item.trim()).filter(Boolean)
  return {
    id: form.id.trim(),
    type: form.type.trim(),
    model: form.model.trim() || undefined,
    models: models.length > 0 ? models : undefined,
    default_model: form.defaultModel.trim() || undefined,
    secret_ref: form.secretRef.trim() || undefined,
    base_url: form.baseURL.trim() || undefined,
    enabled: form.enabled,
    policy: formToPolicy(form.policy),
  }
}

function compactTokenCount(value?: number): string | null {
  if (!value) return null
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(value % 1_000_000 === 0 ? 0 : 1)}M ctx`
  if (value >= 1_000) return `${(value / 1_000).toFixed(value % 1_000 === 0 ? 0 : 1)}K ctx`
  return `${value} ctx`
}

function modelCatalogDescription(model: AIProviderCatalogModelInfo): string {
  const parts = [model.name, model.family].filter(Boolean)
  return parts.join(' · ')
}

function modelCatalogMeta(model: AIProviderCatalogModelInfo): string {
  const parts: string[] = []
  const context = compactTokenCount(model.context_window)
  if (context) parts.push(context)
  if (model.supports_tools) parts.push('tools')
  if (model.supports_reasoning) parts.push('reasoning')
  if (model.supports_attachments) parts.push('attachments')
  if (model.input_modalities?.length) parts.push(`in ${model.input_modalities.join('/')}`)
  return parts.join(' · ')
}

function fallbackModelItem(id: string) {
  return {
    value: id,
    label: id,
    description: 'Custom model ID',
    meta: 'manual',
    keywords: [id],
  }
}

function routeKey(route: AIRouteSettingsInfo): string {
  return [route.provider, route.model, route.mode ?? '', route.intent ?? ''].join('::')
}

function emptyRouteForm(): RouteFormState {
  return {
    provider: '',
    model: '',
    mode: '',
    intent: '',
    requiresReasoning: '',
    requiresTools: '',
    policy: emptyPolicyForm(),
  }
}

function routeToForm(route: AIRouteSettingsInfo): RouteFormState {
  return {
    originalKey: routeKey(route),
    provider: route.provider,
    model: route.model,
    mode: route.mode ?? '',
    intent: route.intent ?? '',
    requiresReasoning: boolToSelect(route.requires_reasoning),
    requiresTools: boolToSelect(route.requires_tools),
    policy: policyToForm(route.policy),
  }
}

function formToRoute(form: RouteFormState): AIRouteSettingsInfo {
  return {
    provider: form.provider.trim(),
    model: form.model.trim(),
    mode: form.mode.trim() || undefined,
    intent: form.intent.trim() || undefined,
    requires_reasoning: selectToBool(form.requiresReasoning),
    requires_tools: selectToBool(form.requiresTools),
    policy: formToPolicy(form.policy),
  }
}

function compactNumber(value: number): string {
  return new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 }).format(value)
}

function formatUSD(value?: number): string {
  if (!value) return '$0.00'
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: value < 1 ? 3 : 2,
    maximumFractionDigits: value < 1 ? 3 : 2,
  }).format(value)
}

function toneForOptionalBool(value?: boolean): 'success' | 'warning' | 'neutral' {
  if (value === true) return 'success'
  if (value === false) return 'warning'
  return 'neutral'
}

function boolLabel(value?: boolean, trueLabel = 'allow', falseLabel = 'deny'): string {
  if (value === true) return trueLabel
  if (value === false) return falseLabel
  return 'inherit'
}

function policySummary(policy?: AIPolicyInfo): string {
  if (!policy) return 'inherit'
  const parts: string[] = []
  if (policy.allow_tools !== undefined) parts.push(`tools ${policy.allow_tools ? 'on' : 'off'}`)
  if (policy.allow_reasoning !== undefined) parts.push(`reasoning ${policy.allow_reasoning ? 'on' : 'off'}`)
  if (policy.allow_attachments !== undefined) parts.push(`attachments ${policy.allow_attachments ? 'on' : 'off'}`)
  if (policy.max_output_tokens) parts.push(`max out ${policy.max_output_tokens}`)
  if (policy.max_cost_usd) parts.push(`max cost ${formatUSD(policy.max_cost_usd)}`)
  if (policy.usage_budget?.max_cost_usd) {
    parts.push(
      `budget ${formatUSD(policy.usage_budget.max_cost_usd)}/${policy.usage_budget.window ?? 'window'}`,
    )
  }
  return parts.length > 0 ? parts.join(' · ') : 'inherit'
}

function DetailField({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="contents">
      <dt className="truncate text-text-subtle">{label}</dt>
      <dd className="break-words text-text-soft">{children}</dd>
    </div>
  )
}

function FormField({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="block">
      <span className="mb-1 block text-[11px] uppercase tracking-[.14em] text-text-subtle">{label}</span>
      {children}
    </label>
  )
}

function MetricCard({ label, value, sub }: { label: string; value: ReactNode; sub?: ReactNode }) {
  return (
    <div className="rounded border border-border bg-panel/70 p-3">
      <div className="text-[10px] uppercase tracking-[.16em] text-text-subtle">{label}</div>
      <div className="mt-1 font-mono text-[16px] font-semibold tabular-nums text-text">{value}</div>
      {sub ? <div className="mt-1 text-[11px] text-text-soft">{sub}</div> : null}
    </div>
  )
}

function PolicyFields({
  value,
  onChange,
}: {
  value: PolicyFormState
  onChange: (next: PolicyFormState) => void
}) {
  function patch(patchValue: Partial<PolicyFormState>) {
    onChange({ ...value, ...patchValue })
  }

  return (
    <div className="grid gap-3 sm:grid-cols-2">
      <FormField label="Allow tools">
        <select
          className={inputClass}
          value={value.allowTools}
          onChange={(event) => patch({ allowTools: event.target.value as BoolSelect })}
        >
          <option value="">inherit</option>
          <option value="true">allow</option>
          <option value="false">deny</option>
        </select>
      </FormField>
      <FormField label="Allow reasoning">
        <select
          className={inputClass}
          value={value.allowReasoning}
          onChange={(event) => patch({ allowReasoning: event.target.value as BoolSelect })}
        >
          <option value="">inherit</option>
          <option value="true">allow</option>
          <option value="false">deny</option>
        </select>
      </FormField>
      <FormField label="Allow attachments">
        <select
          className={inputClass}
          value={value.allowAttachments}
          onChange={(event) => patch({ allowAttachments: event.target.value as BoolSelect })}
        >
          <option value="">inherit</option>
          <option value="true">allow</option>
          <option value="false">deny</option>
        </select>
      </FormField>
      <FormField label="Max output tokens">
        <input
          className={inputClass}
          value={value.maxOutputTokens}
          onChange={(event) => patch({ maxOutputTokens: event.target.value })}
          placeholder="4096"
        />
      </FormField>
      <FormField label="Max request cost USD">
        <input
          className={inputClass}
          value={value.maxCostUSD}
          onChange={(event) => patch({ maxCostUSD: event.target.value })}
          placeholder="0.500"
        />
      </FormField>
      <div className="rounded border border-border bg-panel/40 p-3 sm:col-span-2">
        <div className="mb-3 text-[10px] uppercase tracking-[.16em] text-text-subtle">Durable usage budget</div>
        <div className="grid gap-3 sm:grid-cols-3">
          <FormField label="Max spend USD">
            <input
              className={inputClass}
              value={value.usageBudgetMaxCostUSD}
              onChange={(event) => patch({ usageBudgetMaxCostUSD: event.target.value })}
              placeholder="25.00"
            />
          </FormField>
          <FormField label="Window">
            <select
              className={inputClass}
              value={value.usageBudgetWindow}
              onChange={(event) => patch({ usageBudgetWindow: event.target.value })}
            >
              <option value="">unset</option>
              <option value="day">day</option>
              <option value="month">month</option>
            </select>
          </FormField>
          <FormField label="Scope">
            <select
              className={inputClass}
              value={value.usageBudgetScope}
              onChange={(event) => patch({ usageBudgetScope: event.target.value })}
            >
              <option value="">unset</option>
              <option value="total">total</option>
              <option value="caller">caller</option>
              <option value="session">session</option>
            </select>
          </FormField>
        </div>
      </div>
    </div>
  )
}

function BreakdownTable({
  title,
  rows,
}: {
  title: string
  rows?: AIUsageBreakdownInfo[]
}) {
  const data = rows ?? []
  return (
    <div className="rounded border border-border bg-panel/40">
      <div className="border-b border-border px-3 py-2 text-[10px] uppercase tracking-[.16em] text-text-subtle">
        {title}
      </div>
      {data.length === 0 ? (
        <div className="px-3 py-4 text-[12px] text-text-subtle">No data yet.</div>
      ) : (
        <div className="overflow-auto">
          <table className="min-w-full text-left text-[12px]">
            <thead className="border-b border-border bg-panel-2/60 text-text-subtle">
              <tr>
                <th className="px-3 py-2 font-medium">Key</th>
                <th className="px-3 py-2 font-medium">Requests</th>
                <th className="px-3 py-2 font-medium">Success</th>
                <th className="px-3 py-2 font-medium">Errors</th>
                <th className="px-3 py-2 font-medium">Latency</th>
                <th className="px-3 py-2 font-medium">Input</th>
                <th className="px-3 py-2 font-medium">Output</th>
                <th className="px-3 py-2 font-medium">Cost</th>
              </tr>
            </thead>
            <tbody>
              {data.map((row) => (
                <tr key={row.key} className="border-b border-border/80 last:border-b-0">
                  <td className="px-3 py-2 font-mono text-text">{row.key}</td>
                  <td className="px-3 py-2 font-mono tabular-nums text-text">{row.requests}</td>
                  <td className="px-3 py-2 font-mono tabular-nums text-text">{row.successes}</td>
                  <td className="px-3 py-2 font-mono tabular-nums text-text">{row.errors}</td>
                  <td className="px-3 py-2 font-mono tabular-nums text-text-soft">{row.latency_ms}ms</td>
                  <td className="px-3 py-2 font-mono tabular-nums text-text-soft">{row.input_tokens ?? 0}</td>
                  <td className="px-3 py-2 font-mono tabular-nums text-text-soft">{row.output_tokens ?? 0}</td>
                  <td className="px-3 py-2 font-mono tabular-nums text-text-soft">{formatUSD(row.estimated_cost_usd)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

const providerColumns: ColumnDef<AIProviderSettingsInfo>[] = [
  {
    key: 'id',
    header: 'Provider',
    width: 'fill',
    cell: (provider) => <CopyableId id={provider.id} />,
    sortValue: (provider) => provider.id,
  },
  {
    key: 'type',
    header: 'Type',
    cell: (provider) => provider.type,
    sortValue: (provider) => provider.type,
  },
  {
    key: 'default_model',
    header: 'Default model',
    width: 'fill',
    cell: (provider) => (
      <span className="block truncate font-mono text-[11px] text-text-soft">
        {provider.default_model || provider.model || provider.models?.[0] || '—'}
      </span>
    ),
    sortValue: (provider) => provider.default_model || provider.model || provider.models?.[0] || '',
  },
  {
    key: 'models',
    header: 'Models',
    align: 'right',
    cell: (provider) => provider.models?.length ?? (provider.model ? 1 : 0),
    sortValue: (provider) => provider.models?.length ?? (provider.model ? 1 : 0),
  },
  {
    key: 'secret_ref',
    header: 'Secret',
    cell: (provider) =>
      provider.secret_ref ? <Pill tone="success">configured</Pill> : <span className="text-text-subtle">—</span>,
    sortValue: (provider) => (provider.secret_ref ? 1 : 0),
  },
  {
    key: 'enabled',
    header: 'State',
    cell: (provider) => (
      <Pill tone={provider.enabled ? 'success' : 'neutral'}>{provider.enabled ? 'enabled' : 'disabled'}</Pill>
    ),
    sortValue: (provider) => (provider.enabled ? 1 : 0),
  },
]

const routeColumns: ColumnDef<AIRouteSettingsInfo>[] = [
  {
    key: 'provider',
    header: 'Provider',
    cell: (route) => <CopyableId id={route.provider} />,
    sortValue: (route) => route.provider,
  },
  {
    key: 'model',
    header: 'Model',
    width: 'fill',
    cell: (route) => <span className="font-mono text-[12px] text-text">{route.model}</span>,
    sortValue: (route) => route.model,
  },
  {
    key: 'mode',
    header: 'Mode',
    cell: (route) => route.mode || '—',
    sortValue: (route) => route.mode || '',
  },
  {
    key: 'intent',
    header: 'Intent',
    cell: (route) => route.intent || '—',
    sortValue: (route) => route.intent || '',
  },
  {
    key: 'requires_tools',
    header: 'Tools',
    cell: (route) => <Pill tone={toneForOptionalBool(route.requires_tools)}>{boolLabel(route.requires_tools, 'need', 'avoid')}</Pill>,
    sortValue: (route) => `${route.requires_tools}`,
  },
  {
    key: 'requires_reasoning',
    header: 'Reasoning',
    cell: (route) => <Pill tone={toneForOptionalBool(route.requires_reasoning)}>{boolLabel(route.requires_reasoning, 'need', 'avoid')}</Pill>,
    sortValue: (route) => `${route.requires_reasoning}`,
  },
]

const runtimeProviderColumns: ColumnDef<AIRuntimeInfo['providers'][number]>[] = [
  {
    key: 'id',
    header: 'Provider',
    cell: (provider) => <CopyableId id={provider.id} />,
    sortValue: (provider) => provider.id,
  },
  {
    key: 'type',
    header: 'Type',
    cell: (provider) => provider.type,
    sortValue: (provider) => provider.type,
  },
  {
    key: 'default_model',
    header: 'Default model',
    width: 'fill',
    cell: (provider) => provider.default_model || provider.models?.[0] || '—',
    sortValue: (provider) => provider.default_model || provider.models?.[0] || '',
  },
  {
    key: 'models',
    header: 'Models',
    align: 'right',
    cell: (provider) => provider.models?.length ?? 0,
    sortValue: (provider) => provider.models?.length ?? 0,
  },
]

const runtimeModelColumns: ColumnDef<AIRuntimeInfo['models'][number]>[] = [
  {
    key: 'id',
    header: 'Model',
    width: 'fill',
    cell: (model) => <span className="font-mono text-[12px] text-text">{model.id}</span>,
    sortValue: (model) => model.id,
  },
  {
    key: 'configured_provider_id',
    header: 'Provider',
    cell: (model) => model.configured_provider_id,
    sortValue: (model) => model.configured_provider_id,
  },
  {
    key: 'family',
    header: 'Family',
    cell: (model) => model.family || '—',
    sortValue: (model) => model.family || '',
  },
  {
    key: 'context_window',
    header: 'Context',
    align: 'right',
    cell: (model) => model.context_window ?? '—',
    sortValue: (model) => model.context_window ?? 0,
  },
  {
    key: 'max_output_tokens',
    header: 'Max out',
    align: 'right',
    cell: (model) => model.max_output_tokens ?? '—',
    sortValue: (model) => model.max_output_tokens ?? 0,
  },
]

const runtimeRouteColumns: ColumnDef<AIRuntimeInfo['routes'][number]>[] = [
  {
    key: 'provider',
    header: 'Provider',
    cell: (route) => route.provider,
    sortValue: (route) => route.provider,
  },
  {
    key: 'model',
    header: 'Model',
    width: 'fill',
    cell: (route) => <span className="font-mono text-[12px] text-text">{route.model}</span>,
    sortValue: (route) => route.model,
  },
  {
    key: 'mode',
    header: 'Mode',
    cell: (route) => route.mode || '—',
    sortValue: (route) => route.mode || '',
  },
  {
    key: 'policy',
    header: 'Policy',
    width: 'fill',
    cell: (route) => <span className="text-[11px] text-text-soft">{policySummary(route)}</span>,
    sortValue: (route) => policySummary(route),
  },
]

const auditColumns: ColumnDef<AIAuditEventInfo>[] = [
  {
    key: 'event_type',
    header: 'Event',
    cell: (event) => (
      <Pill tone={event.success ? 'success' : event.event_type === 'budget_rejection' ? 'warning' : 'neutral'}>
        {event.event_type}
      </Pill>
    ),
    sortValue: (event) => event.event_type,
  },
  {
    key: 'provider',
    header: 'Provider',
    cell: (event) => event.provider || '—',
    sortValue: (event) => event.provider || '',
  },
  {
    key: 'model',
    header: 'Model',
    width: 'fill',
    cell: (event) => <span className="font-mono text-[11px] text-text-soft">{event.model || '—'}</span>,
    sortValue: (event) => event.model || '',
  },
  {
    key: 'operation',
    header: 'Op',
    cell: (event) => event.operation,
    sortValue: (event) => event.operation,
  },
  {
    key: 'cost',
    header: 'Cost',
    align: 'right',
    cell: (event) => <span className="font-mono tabular-nums">{formatUSD(event.estimated_cost_usd)}</span>,
    sortValue: (event) => event.estimated_cost_usd ?? 0,
  },
  {
    key: 'timestamp',
    header: 'When',
    align: 'right',
    cell: (event) => <span className="text-[11px] text-text-soft">{formatRelativeTime(event.timestamp)}</span>,
    sortValue: (event) => event.timestamp,
  },
]

const budgetColumns: ColumnDef<AIBudgetInfo>[] = [
  {
    key: 'provider',
    header: 'Provider',
    cell: (budget) => budget.provider,
    sortValue: (budget) => budget.provider,
  },
  {
    key: 'model',
    header: 'Model',
    width: 'fill',
    cell: (budget) => <span className="font-mono text-[11px] text-text-soft">{budget.model}</span>,
    sortValue: (budget) => budget.model,
  },
  {
    key: 'window',
    header: 'Window',
    cell: (budget) => budget.usage_budget.window || '—',
    sortValue: (budget) => budget.usage_budget.window || '',
  },
  {
    key: 'scope',
    header: 'Scope',
    cell: (budget) => budget.usage_budget.scope || '—',
    sortValue: (budget) => budget.usage_budget.scope || '',
  },
  {
    key: 'spent',
    header: 'Spent',
    align: 'right',
    cell: (budget) => <span className="font-mono tabular-nums">{formatUSD(budget.spent_cost_usd)}</span>,
    sortValue: (budget) => budget.spent_cost_usd ?? 0,
  },
  {
    key: 'remaining',
    header: 'Remaining',
    align: 'right',
    cell: (budget) => <span className="font-mono tabular-nums">{formatUSD(budget.remaining_cost_usd)}</span>,
    sortValue: (budget) => budget.remaining_cost_usd ?? 0,
  },
  {
    key: 'status',
    header: 'Status',
    cell: (budget) => (
      <Pill tone={budget.error ? 'warning' : budget.exhausted ? 'warning' : 'success'}>
        {budget.error ? 'needs input' : budget.exhausted ? 'exhausted' : 'available'}
      </Pill>
    ),
    sortValue: (budget) => `${budget.error}:${budget.exhausted}`,
  },
]

const inputClass =
  'h-8 w-full border border-border bg-bg px-2 font-mono text-[12px] text-text outline-none focus:border-border-strong'
const textAreaClass =
  'min-h-20 w-full resize-y border border-border bg-bg px-2 py-1.5 font-mono text-[12px] text-text outline-none focus:border-border-strong'

export function AIPage() {
  const api = useApi()
  const [tab, setTab] = useState<TabKey>('config')
  const [settings, setSettings] = useState<AISettingsInfo | null>(null)
  const [draftConfig, setDraftConfig] = useState<AIConfigInfo | null>(null)
  const [runtime, setRuntime] = useState<AIRuntimeInfo | null>(null)
  const [usage, setUsage] = useState<AIUsageInfo | null>(null)
  const [audit, setAudit] = useState<AIAuditInfo | null>(null)
  const [budgets, setBudgets] = useState<AIBudgetsInfo | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [message, setMessage] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [reloadingDaemon, setReloadingDaemon] = useState(false)
  const [providerForm, setProviderForm] = useState<ProviderFormState | null>(null)
  const [routeForm, setRouteForm] = useState<RouteFormState | null>(null)
  const [providerCatalogs, setProviderCatalogs] = useState<Record<string, AIProviderCatalogInfo | undefined>>({})
  const [detailView, setDetailView] = useState<DetailView | null>(null)
  const scrollRef = useRef<HTMLDivElement | null>(null)

  const load = useCallback(() => {
    let cancelled = false
    setLoading(true)
    Promise.all([
      api.getAISettings(),
      api.getAIRuntime(),
      api.getAIUsage(),
      api.getAIAudit(),
      api.getAIBudgets(),
    ])
      .then(([settingsInfo, runtimeInfo, usageInfo, auditInfo, budgetsInfo]) => {
        if (cancelled) return
        setSettings(settingsInfo)
        setDraftConfig(settingsInfo.config)
        setRuntime(runtimeInfo)
        setUsage(usageInfo)
        setAudit(auditInfo)
        setBudgets(budgetsInfo)
        setError(
          settingsInfo.error ??
            runtimeInfo.error ??
            usageInfo.error ??
            auditInfo.error ??
            budgetsInfo.error ??
            null,
        )
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

  useEffect(() => {
    const providerType = providerForm?.type.trim()
    if (!providerType || providerCatalogs[providerType]) return
    let cancelled = false
    api
      .getAIProviderCatalog(providerType)
      .then((info) => {
        if (!cancelled) {
          setProviderCatalogs((current) => ({ ...current, [providerType]: info }))
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setProviderCatalogs((current) => ({
            ...current,
            [providerType]: {
              provider_type: providerType,
              models: [],
              error: err instanceof Error ? err.message : String(err),
            },
          }))
        }
      })
    return () => {
      cancelled = true
    }
  }, [api, providerCatalogs, providerForm?.type])

  const providerList = draftConfig?.providers ?? []
  const routeList = draftConfig?.routes ?? []
  const runtimeProviderList = runtime?.providers ?? []
  const runtimeModelList = runtime?.models ?? []
  const runtimeRouteList = runtime?.routes ?? []
  const auditList = audit?.events ?? []
  const budgetList = budgets?.budgets ?? []
  const dirty = useMemo(
    () => JSON.stringify(draftConfig ?? null) !== JSON.stringify(settings?.config ?? null),
    [draftConfig, settings?.config],
  )

  const tabs: TabStripItem<TabKey>[] = [
    { key: 'config', label: 'Config', icon: <Shield className="h-3.5 w-3.5" /> },
    { key: 'providers', label: 'Providers', icon: <Bot className="h-3.5 w-3.5" />, count: providerList.length },
    { key: 'routes', label: 'Routes', icon: <Route className="h-3.5 w-3.5" />, count: routeList.length },
    { key: 'runtime', label: 'Runtime', icon: <BrainCircuit className="h-3.5 w-3.5" />, count: runtimeRouteList.length },
    { key: 'usage', label: 'Usage', icon: <CircleDollarSign className="h-3.5 w-3.5" /> },
    { key: 'audit', label: 'Audit', icon: <Shield className="h-3.5 w-3.5" />, count: audit?.count ?? 0 },
    { key: 'budgets', label: 'Budgets', icon: <CircleDollarSign className="h-3.5 w-3.5" />, count: budgets?.count ?? 0 },
  ]

  const summaryCards = [
    { label: 'Configured', value: draftConfig ? providerList.length : '...' },
    {
      label: 'Enabled',
      value: draftConfig ? providerList.filter((provider) => provider.enabled).length : '...',
      accentColor: 'var(--color-status-done)',
    },
    { label: 'Routes', value: draftConfig ? routeList.length : '...' },
    { label: 'Runtime providers', value: runtime ? runtimeProviderList.length : '...' },
    { label: 'Requests', value: usage ? compactNumber(usage.summary.requests) : '...' },
    {
      label: 'Spend',
      value: usage ? formatUSD(usage.summary.estimated_cost_usd) : '...',
      accentColor: 'var(--color-status-indexed)',
    },
    {
      label: 'Config state',
      value: dirty ? 'unsaved' : 'current',
      accentColor: dirty ? 'var(--color-status-blocked)' : 'var(--color-status-done)',
    },
  ]

  function updateDraft(next: AIConfigInfo) {
    setDraftConfig(next)
  }

  function saveConfig() {
    if (!draftConfig) return
    setSaving(true)
    setMessage(null)
    setError(null)
    api
      .saveAISettings({ config: draftConfig })
      .then((info) => {
        setMessage(
          info.backup_path ? `Saved AI config. Backup: ${info.backup_path}.` : 'Saved AI config.',
        )
        return Promise.all([api.getAISettings(), api.getAIRuntime(), api.getAIBudgets()])
      })
      .then(([settingsInfo, runtimeInfo, budgetsInfo]) => {
        setSettings(settingsInfo)
        setDraftConfig(settingsInfo.config)
        setRuntime(runtimeInfo)
        setBudgets(budgetsInfo)
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSaving(false))
  }

  function reloadDaemon() {
    setReloadingDaemon(true)
    setMessage(null)
    setError(null)
    api
      .runSystemResourceAction('tether-daemon-service', 'reload')
      .then(() => {
        setMessage('Reloaded tether-daemon-service.')
        load()
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setReloadingDaemon(false))
  }

  function saveProvider() {
    if (!draftConfig || !providerForm) return
    const next = formToProvider(providerForm)
    const providers = [...draftConfig.providers]
    const existingIndex = providers.findIndex((provider) => provider.id === providerForm.originalID)
    if (existingIndex >= 0) {
      providers[existingIndex] = next
    } else {
      providers.push(next)
    }
    updateDraft({ ...draftConfig, providers })
    setProviderForm(null)
  }

  function deleteProvider(provider: AIProviderSettingsInfo) {
    if (!draftConfig) return
    updateDraft({
      ...draftConfig,
      providers: draftConfig.providers.filter((item) => item.id !== provider.id),
      routes: draftConfig.routes.filter((route) => route.provider !== provider.id),
    })
    setDetailView(null)
  }

  function saveRoute() {
    if (!draftConfig || !routeForm) return
    const next = formToRoute(routeForm)
    const routes = [...draftConfig.routes]
    const existingIndex = routes.findIndex((route) => routeKey(route) === routeForm.originalKey)
    if (existingIndex >= 0) {
      routes[existingIndex] = next
    } else {
      routes.push(next)
    }
    updateDraft({ ...draftConfig, routes })
    setRouteForm(null)
  }

  function deleteRoute(route: AIRouteSettingsInfo) {
    if (!draftConfig) return
    updateDraft({
      ...draftConfig,
      routes: draftConfig.routes.filter((item) => routeKey(item) !== routeKey(route)),
    })
    setDetailView(null)
  }

  if (error && !settings && !runtime && !usage && !audit && !budgets) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center bg-bg p-6">
        <EmptyState variant="error" title="Could not load AI gateway data" description={error} />
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
            value={tab}
            onChange={setTab}
            actions={
              <div className="flex items-center gap-2">
                {tab === 'providers' && (
                  <Button variant="outline" size="sm" onClick={() => setProviderForm(emptyProviderForm())}>
                    <Plus className="h-3.5 w-3.5" />
                    Add provider
                  </Button>
                )}
                {tab === 'routes' && (
                  <Button variant="outline" size="sm" onClick={() => setRouteForm(emptyRouteForm())}>
                    <Plus className="h-3.5 w-3.5" />
                    Add route
                  </Button>
                )}
                <Button
                  variant={dirty ? 'default' : 'outline'}
                  size="sm"
                  onClick={saveConfig}
                  disabled={saving || !draftConfig}
                >
                  {saving ? 'Saving' : 'Save config'}
                </Button>
                <Button
                  variant={settings?.runtime.daemon_reachable ? 'outline' : 'default'}
                  size="sm"
                  onClick={reloadDaemon}
                  disabled={reloadingDaemon}
                  title="cerberus resource reload tether-daemon-service"
                >
                  <RefreshCw className={cn('h-3.5 w-3.5', reloadingDaemon && 'animate-spin')} />
                  {reloadingDaemon ? 'Reloading' : 'Reload daemon'}
                </Button>
                <Button variant="outline" size="sm" onClick={() => load()} disabled={loading}>
                  <RefreshCw className={cn('h-3.5 w-3.5', loading && 'animate-spin')} />
                  Refresh
                </Button>
              </div>
            }
          />
        }
        summary={<SummaryCards cards={summaryCards} />}
        filters={
          <div className="shrink-0 border-b border-border-strong bg-bg px-4 py-1.5 text-[11px]">
            <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
              <span className="text-text-subtle">
                Manage the `global.yaml` AI gateway config, then reload the daemon so provider and routing changes take effect at runtime.
              </span>
              {settings?.runtime.last_error && <span className="text-status-blocked">{settings.runtime.last_error}</span>}
              {message && <span className="text-status-done">{message}</span>}
              {error && <span className="text-status-blocked">{error}</span>}
            </div>
          </div>
        }
      >
        {tab === 'config' && draftConfig && (
          <div className="grid gap-3 p-3 lg:grid-cols-[minmax(0,1.1fr)_minmax(0,0.9fr)]">
            <section className="rounded border border-border bg-panel/40 p-4">
              <div className="mb-3 flex items-center gap-2">
                <Shield className="h-4 w-4 text-text-soft" />
                <h2 className="text-sm font-semibold text-text">Routing defaults</h2>
              </div>
              <div className="grid gap-3">
                <FormField label="Default provider order">
                  <textarea
                    className={textAreaClass}
                    value={(draftConfig.default_provider_order ?? []).join('\n')}
                    onChange={(event) =>
                      updateDraft({
                        ...draftConfig,
                        default_provider_order: splitList(event.target.value),
                      })
                    }
                    placeholder={'anthropic\nopenai'}
                  />
                </FormField>
              </div>
            </section>

            <section className="rounded border border-border bg-panel/40 p-4">
              <div className="mb-3 flex items-center gap-2">
                <Shield className="h-4 w-4 text-text-soft" />
                <h2 className="text-sm font-semibold text-text">Global policy</h2>
              </div>
              <PolicyFields
                value={policyToForm(draftConfig.policy)}
                onChange={(next) => updateDraft({ ...draftConfig, policy: formToPolicy(next) })}
              />
            </section>
          </div>
        )}

        {tab === 'providers' && (
          <DataTable
            items={providerList}
            columns={providerColumns}
            getRowId={(provider) => provider.id}
            initialSort={{ key: 'id', dir: 'asc' }}
            onRowOpen={(_, item) => setDetailView({ kind: 'provider', item })}
            rowAriaLabel={(provider) => `Open AI provider ${provider.id}`}
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading providers...' : 'No AI providers'}
                description={loading ? 'Reading global.yaml.' : 'Add an AI provider to begin routing requests.'}
              />
            }
          />
        )}

        {tab === 'routes' && (
          <DataTable
            items={routeList}
            columns={routeColumns}
            getRowId={(route) => routeKey(route)}
            initialSort={{ key: 'provider', dir: 'asc' }}
            onRowOpen={(_, item) => setDetailView({ kind: 'route', item })}
            rowAriaLabel={(route) => `Open AI route ${route.provider} ${route.model}`}
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading routes...' : 'No explicit routes'}
                description={
                  loading
                    ? 'Reading global.yaml.'
                    : 'Routes are optional. The daemon falls back to provider/model order when none are configured.'
                }
              />
            }
          />
        )}

        {tab === 'runtime' && (
          <div className="grid gap-3 p-3 xl:grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)]">
            <div className="grid gap-3">
              <div className="grid gap-3 sm:grid-cols-3">
                <MetricCard label="Daemon" value={settings?.runtime.daemon_reachable ? 'reachable' : 'offline'} sub={settings?.runtime.last_error || 'AI read surfaces are available.'} />
                <MetricCard label="Providers" value={runtimeProviderList.length} />
                <MetricCard label="Models" value={runtimeModelList.length} />
              </div>
              <div className="rounded border border-border bg-panel/40">
                <div className="border-b border-border px-3 py-2 text-[10px] uppercase tracking-[.16em] text-text-subtle">Live providers</div>
                <DataTable
                  items={runtimeProviderList}
                  columns={runtimeProviderColumns}
                  getRowId={(provider) => provider.id}
                  initialSort={{ key: 'id', dir: 'asc' }}
                  scrollRootRef={scrollRef}
                />
              </div>
              <div className="rounded border border-border bg-panel/40">
                <div className="border-b border-border px-3 py-2 text-[10px] uppercase tracking-[.16em] text-text-subtle">Live routes</div>
                <DataTable
                  items={runtimeRouteList}
                  columns={runtimeRouteColumns}
                  getRowId={(route) => `${route.provider}:${route.model}:${route.mode ?? ''}:${route.intent ?? ''}`}
                  initialSort={{ key: 'provider', dir: 'asc' }}
                  scrollRootRef={scrollRef}
                />
              </div>
            </div>
            <div className="rounded border border-border bg-panel/40">
              <div className="border-b border-border px-3 py-2 text-[10px] uppercase tracking-[.16em] text-text-subtle">Configured model runtime view</div>
              <DataTable
                items={runtimeModelList}
                columns={runtimeModelColumns}
                getRowId={(model) => `${model.configured_provider_id}:${model.id}`}
                initialSort={{ key: 'configured_provider_id', dir: 'asc' }}
                scrollRootRef={scrollRef}
              />
            </div>
          </div>
        )}

        {tab === 'usage' && (
          <div className="grid gap-3 p-3">
            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-6">
              <MetricCard label="Requests" value={usage?.summary.requests ?? 0} />
              <MetricCard label="Successes" value={usage?.summary.successes ?? 0} />
              <MetricCard label="Errors" value={usage?.summary.errors ?? 0} />
              <MetricCard label="Input tokens" value={compactNumber(usage?.summary.input_tokens ?? 0)} />
              <MetricCard label="Output tokens" value={compactNumber(usage?.summary.output_tokens ?? 0)} />
              <MetricCard label="Estimated cost" value={formatUSD(usage?.summary.estimated_cost_usd)} />
            </div>
            <div className="grid gap-3 xl:grid-cols-3">
              <BreakdownTable title="By provider" rows={usage?.summary.by_provider} />
              <BreakdownTable title="By model" rows={usage?.summary.by_model} />
              <BreakdownTable title="By operation" rows={usage?.summary.by_operation} />
            </div>
          </div>
        )}

        {tab === 'audit' && (
          <DataTable
            items={auditList}
            columns={auditColumns}
            getRowId={(event) => `${event.id}`}
            initialSort={{ key: 'timestamp', dir: 'desc' }}
            onRowOpen={(_, item) => setDetailView({ kind: 'audit', item })}
            rowAriaLabel={(event) => `Open AI audit event ${event.id}`}
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading audit...' : 'No AI audit rows'}
                description={loading ? 'Reading daemon audit surfaces.' : 'Run AI traffic through the gateway to populate audit history.'}
              />
            }
          />
        )}

        {tab === 'budgets' && (
          <DataTable
            items={budgetList}
            columns={budgetColumns}
            getRowId={(budget) => `${budget.provider}:${budget.model}:${budget.mode ?? ''}:${budget.intent ?? ''}`}
            initialSort={{ key: 'provider', dir: 'asc' }}
            onRowOpen={(_, item) => setDetailView({ kind: 'budget', item })}
            rowAriaLabel={(budget) => `Open AI budget ${budget.provider} ${budget.model}`}
            scrollRootRef={scrollRef}
            emptyState={
              <EmptyState
                variant="empty"
                title={loading ? 'Loading budgets...' : 'No durable budgets'}
                description={loading ? 'Reading daemon budget surfaces.' : 'Add usage_budget policy to a global, provider, or route policy block.'}
              />
            }
          />
        )}
      </ListPageLayout>

      <ProviderDialog
        form={providerForm}
        catalog={providerForm ? providerCatalogs[providerForm.type] : undefined}
        onChange={setProviderForm}
        onClose={() => setProviderForm(null)}
        onSave={saveProvider}
      />
      <RouteDialog
        form={routeForm}
        onChange={setRouteForm}
        onClose={() => setRouteForm(null)}
        onSave={saveRoute}
      />
      <AIDetailDialog
        detail={detailView}
        onClose={() => setDetailView(null)}
        onEditProvider={(provider) => setProviderForm(providerToForm(provider))}
        onDeleteProvider={deleteProvider}
        onEditRoute={(route) => setRouteForm(routeToForm(route))}
        onDeleteRoute={deleteRoute}
      />
    </>
  )
}

function ProviderDialog({
  form,
  catalog,
  onChange,
  onClose,
  onSave,
}: {
  form: ProviderFormState | null
  catalog?: AIProviderCatalogInfo
  onChange: (next: ProviderFormState | null) => void
  onClose: () => void
  onSave: () => void
}) {
  const [customModel, setCustomModel] = useState('')

  useEffect(() => {
    setCustomModel('')
  }, [form?.id, form?.type])

  function update(patch: Partial<ProviderFormState>) {
    if (form) onChange({ ...form, ...patch })
  }

  const catalogItems = useMemo(() => {
    const mapped =
      catalog?.models.map((model) => ({
        value: model.id,
        label: model.id,
        description: modelCatalogDescription(model),
        meta: modelCatalogMeta(model),
        keywords: [
          model.id,
          model.name ?? '',
          model.family ?? '',
          ...(model.input_modalities ?? []),
          ...(model.output_modalities ?? []),
        ],
      })) ?? []
    if (!form) return mapped
    const present = new Set(mapped.map((item) => item.value))
    const manual = form.models.filter((id) => !present.has(id)).map(fallbackModelItem)
    return [...mapped, ...manual]
  }, [catalog?.models, form])

  const defaultModelItems = useMemo(() => {
    if (!form) return []
    const itemMap = new Map(catalogItems.map((item) => [item.value, item]))
    return form.models.map((id) => itemMap.get(id) ?? fallbackModelItem(id))
  }, [catalogItems, form])

  function addCustomModel() {
    if (!form) return
    const next = customModel.trim()
    if (!next || form.models.includes(next)) return
    const nextModels = [...form.models, next]
    update({ models: nextModels, defaultModel: form.defaultModel || next })
    setCustomModel('')
  }

  return (
    <DetailDialog
      open={form !== null}
      onClose={onClose}
      title={form?.id ? `Edit ${form.id}` : 'Add AI provider'}
      widthClassName="w-[760px] max-w-[calc(100vw-2rem)]"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="outline" size="sm" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="default" size="sm" onClick={onSave} disabled={!form}>
            Save
          </Button>
        </div>
      }
    >
      {form && (
        <>
          <DetailSection title="Provider">
            <div className="grid gap-3">
              <div className="grid gap-3 sm:grid-cols-[1fr_12rem_8rem]">
                <FormField label="ID">
                  <input className={inputClass} value={form.id} onChange={(event) => update({ id: event.target.value })} placeholder="anthropic-primary" />
                </FormField>
                <FormField label="Type">
                  <select className={inputClass} value={form.type} onChange={(event) => update({ type: event.target.value })}>
                    <option value="anthropic">anthropic</option>
                    <option value="openai">openai</option>
                    <option value="openai-compatible">openai-compatible</option>
                  </select>
                </FormField>
                <FormField label="Enabled">
                  <label className="flex h-8 items-center gap-2 border border-border bg-bg px-2 text-[12px] text-text-soft">
                    <input type="checkbox" checked={form.enabled} onChange={(event) => update({ enabled: event.target.checked })} />
                    Enabled
                  </label>
                </FormField>
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                <FormField label="Legacy single model">
                  <input className={inputClass} value={form.model} onChange={(event) => update({ model: event.target.value })} placeholder="claude-sonnet-4-20250514" />
                </FormField>
                <FormField label="Default model">
                  <div className="flex h-8 items-center">
                    <Combobox
                      items={defaultModelItems}
                      value={form.defaultModel || null}
                      onChange={(value) => update({ defaultModel: value ?? '' })}
                      ariaLabel="Select default model"
                      placeholder={form.models.length > 0 ? 'Select default model' : 'Add models first'}
                      searchPlaceholder="Search selected models"
                      emptyText="No selected models."
                      clearable
                    />
                  </div>
                </FormField>
              </div>
              <FormField label="Configured models">
                <div className="grid gap-3">
                  <TransferList
                    items={catalogItems}
                    selected={form.models}
                    onChange={(next) =>
                      update({
                        models: next,
                        defaultModel: next.includes(form.defaultModel) ? form.defaultModel : next[0] ?? '',
                      })
                    }
                    availableTitle={
                      catalog?.vendor_provider_name
                        ? `${catalog.vendor_provider_name} suggestions`
                        : 'Available models'
                    }
                    selectedTitle="Configured models"
                    emptyAvailableText={
                      catalog?.error
                        ? 'Model catalog unavailable right now.'
                        : 'No catalog models found for this provider type.'
                    }
                    emptySelectedText="No configured models yet."
                  />
                  <div className="flex flex-col gap-2 sm:flex-row">
                    <input
                      className={inputClass}
                      value={customModel}
                      onChange={(event) => setCustomModel(event.target.value)}
                      placeholder="Add custom model ID for local or vendor-specific variants"
                    />
                    <Button variant="outline" size="sm" onClick={addCustomModel} disabled={!customModel.trim()}>
                      Add custom model
                    </Button>
                  </div>
                  <div className="text-[11px] text-text-subtle">
                    {catalog?.error
                      ? `Catalog lookup error: ${catalog.error}`
                      : form.type === 'openai-compatible'
                        ? 'OpenAI-compatible uses the OpenAI catalog as a suggestion set. Add custom local model IDs when needed.'
                        : catalog?.last_fetched_at
                          ? `Catalog data fetched ${formatRelativeTime(catalog.last_fetched_at)}.`
                          : 'Catalog suggestions come from models.dev when available.'}
                  </div>
                </div>
              </FormField>
              <div className="grid gap-3 sm:grid-cols-2">
                <FormField label="Secret ref">
                  <input className={inputClass} value={form.secretRef} onChange={(event) => update({ secretRef: event.target.value })} placeholder="helper://anthropic-api-key" />
                </FormField>
                <FormField label="Base URL">
                  <input className={inputClass} value={form.baseURL} onChange={(event) => update({ baseURL: event.target.value })} placeholder="http://127.0.0.1:11434/v1" />
                </FormField>
              </div>
            </div>
          </DetailSection>
          <DetailSection title="Policy">
            <PolicyFields value={form.policy} onChange={(next) => update({ policy: next })} />
          </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}

function RouteDialog({
  form,
  onChange,
  onClose,
  onSave,
}: {
  form: RouteFormState | null
  onChange: (next: RouteFormState | null) => void
  onClose: () => void
  onSave: () => void
}) {
  function update(patch: Partial<RouteFormState>) {
    if (form) onChange({ ...form, ...patch })
  }

  return (
    <DetailDialog
      open={form !== null}
      onClose={onClose}
      title={form?.provider ? `Edit route ${form.provider}` : 'Add AI route'}
      widthClassName="w-[760px] max-w-[calc(100vw-2rem)]"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="outline" size="sm" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="default" size="sm" onClick={onSave} disabled={!form}>
            Save
          </Button>
        </div>
      }
    >
      {form && (
        <>
          <DetailSection title="Route">
            <div className="grid gap-3">
              <div className="grid gap-3 sm:grid-cols-2">
                <FormField label="Provider">
                  <input className={inputClass} value={form.provider} onChange={(event) => update({ provider: event.target.value })} placeholder="anthropic-primary" />
                </FormField>
                <FormField label="Model">
                  <input className={inputClass} value={form.model} onChange={(event) => update({ model: event.target.value })} placeholder="claude-sonnet-4-20250514" />
                </FormField>
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                <FormField label="Mode">
                  <input className={inputClass} value={form.mode} onChange={(event) => update({ mode: event.target.value })} placeholder="summarize" />
                </FormField>
                <FormField label="Intent">
                  <input className={inputClass} value={form.intent} onChange={(event) => update({ intent: event.target.value })} placeholder="support" />
                </FormField>
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                <FormField label="Requires tools">
                  <select className={inputClass} value={form.requiresTools} onChange={(event) => update({ requiresTools: event.target.value as BoolSelect })}>
                    <option value="">unset</option>
                    <option value="true">true</option>
                    <option value="false">false</option>
                  </select>
                </FormField>
                <FormField label="Requires reasoning">
                  <select className={inputClass} value={form.requiresReasoning} onChange={(event) => update({ requiresReasoning: event.target.value as BoolSelect })}>
                    <option value="">unset</option>
                    <option value="true">true</option>
                    <option value="false">false</option>
                  </select>
                </FormField>
              </div>
            </div>
          </DetailSection>
          <DetailSection title="Policy">
            <PolicyFields value={form.policy} onChange={(next) => update({ policy: next })} />
          </DetailSection>
        </>
      )}
    </DetailDialog>
  )
}

function AIDetailDialog({
  detail,
  onClose,
  onEditProvider,
  onDeleteProvider,
  onEditRoute,
  onDeleteRoute,
}: {
  detail: DetailView | null
  onClose: () => void
  onEditProvider: (provider: AIProviderSettingsInfo) => void
  onDeleteProvider: (provider: AIProviderSettingsInfo) => void
  onEditRoute: (route: AIRouteSettingsInfo) => void
  onDeleteRoute: (route: AIRouteSettingsInfo) => void
}) {
  const provider = detail?.kind === 'provider' ? detail.item : null
  const route = detail?.kind === 'route' ? detail.item : null
  const audit = detail?.kind === 'audit' ? detail.item : null
  const budget = detail?.kind === 'budget' ? detail.item : null

  return (
    <DetailDialog
      open={detail !== null}
      onClose={onClose}
      title={
        provider
          ? `Provider ${provider.id}`
          : route
            ? `Route ${route.provider}`
            : audit
              ? `Audit ${audit.id}`
              : budget
                ? `Budget ${budget.provider}`
                : ''
      }
      badge={
        provider ? (
          <Pill tone={provider.enabled ? 'success' : 'neutral'}>{provider.enabled ? 'enabled' : 'disabled'}</Pill>
        ) : route ? (
          <Pill tone="neutral">route</Pill>
        ) : audit ? (
          <Pill tone={audit.success ? 'success' : audit.event_type === 'budget_rejection' ? 'warning' : 'neutral'}>
            {audit.event_type}
          </Pill>
        ) : budget ? (
          <Pill tone={budget.error ? 'warning' : budget.exhausted ? 'warning' : 'success'}>
            {budget.error ? 'needs input' : budget.exhausted ? 'exhausted' : 'available'}
          </Pill>
        ) : null
      }
      footer={
        provider ? (
          <div className="flex justify-between gap-2">
            <Button variant="outline" size="sm" onClick={() => onDeleteProvider(provider)}>
              <Trash2 className="h-3.5 w-3.5" />
              Delete
            </Button>
            <Button variant="default" size="sm" onClick={() => onEditProvider(provider)}>
              <Pencil className="h-3.5 w-3.5" />
              Edit
            </Button>
          </div>
        ) : route ? (
          <div className="flex justify-between gap-2">
            <Button variant="outline" size="sm" onClick={() => onDeleteRoute(route)}>
              <Trash2 className="h-3.5 w-3.5" />
              Delete
            </Button>
            <Button variant="default" size="sm" onClick={() => onEditRoute(route)}>
              <Pencil className="h-3.5 w-3.5" />
              Edit
            </Button>
          </div>
        ) : null
      }
      widthClassName="w-[760px] max-w-[calc(100vw-2rem)]"
    >
      {provider && (
        <>
          <DetailSection title="Provider">
            <dl className="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <DetailField label="ID"><CopyableId id={provider.id} /></DetailField>
              <DetailField label="Type">{provider.type}</DetailField>
              <DetailField label="Default model">{provider.default_model || provider.model || '—'}</DetailField>
              <DetailField label="Models">{provider.models?.join(', ') || '—'}</DetailField>
              <DetailField label="Secret ref">{provider.secret_ref || '—'}</DetailField>
              <DetailField label="Base URL">{provider.base_url || '—'}</DetailField>
            </dl>
          </DetailSection>
          <DetailSection title="Policy">
            <p className="text-[12px] text-text-soft">{policySummary(provider.policy)}</p>
          </DetailSection>
        </>
      )}

      {route && (
        <>
          <DetailSection title="Route">
            <dl className="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <DetailField label="Provider">{route.provider}</DetailField>
              <DetailField label="Model">{route.model}</DetailField>
              <DetailField label="Mode">{route.mode || '—'}</DetailField>
              <DetailField label="Intent">{route.intent || '—'}</DetailField>
              <DetailField label="Requires tools">{String(route.requires_tools ?? 'unset')}</DetailField>
              <DetailField label="Requires reasoning">{String(route.requires_reasoning ?? 'unset')}</DetailField>
            </dl>
          </DetailSection>
          <DetailSection title="Policy">
            <p className="text-[12px] text-text-soft">{policySummary(route.policy)}</p>
          </DetailSection>
        </>
      )}

      {audit && (
        <>
          <DetailSection title="Event">
            <dl className="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <DetailField label="Type">{audit.event_type}</DetailField>
              <DetailField label="Request ID">{audit.request_id || '—'}</DetailField>
              <DetailField label="Provider">{audit.provider || '—'}</DetailField>
              <DetailField label="Model">{audit.model || '—'}</DetailField>
              <DetailField label="Success">{audit.success ? 'true' : 'false'}</DetailField>
              <DetailField label="Latency">{audit.latency_ms}ms</DetailField>
              <DetailField label="Input tokens">{audit.input_tokens ?? 0}</DetailField>
              <DetailField label="Output tokens">{audit.output_tokens ?? 0}</DetailField>
              <DetailField label="Cost">{formatUSD(audit.estimated_cost_usd)}</DetailField>
              <DetailField label="When">{audit.timestamp}</DetailField>
            </dl>
          </DetailSection>
          <DetailSection title="Summaries">
            <div className="space-y-3 text-[12px] text-text-soft">
              <div>
                <div className="mb-1 text-[10px] uppercase tracking-[.16em] text-text-subtle">Request</div>
                <p>{audit.request_summary || '—'}</p>
              </div>
              <div>
                <div className="mb-1 text-[10px] uppercase tracking-[.16em] text-text-subtle">Response</div>
                <p>{audit.response_summary || audit.error || audit.refusal || '—'}</p>
              </div>
            </div>
          </DetailSection>
        </>
      )}

      {budget && (
        <>
          <DetailSection title="Budget">
            <dl className="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-1.5 text-[12px]">
              <DetailField label="Provider">{budget.provider}</DetailField>
              <DetailField label="Model">{budget.model}</DetailField>
              <DetailField label="Window">{budget.usage_budget.window || '—'}</DetailField>
              <DetailField label="Scope">{budget.usage_budget.scope || '—'}</DetailField>
              <DetailField label="Max cost">{formatUSD(budget.usage_budget.max_cost_usd)}</DetailField>
              <DetailField label="Spent">{formatUSD(budget.spent_cost_usd)}</DetailField>
              <DetailField label="Remaining">{formatUSD(budget.remaining_cost_usd)}</DetailField>
              <DetailField label="Window start">{budget.window_start}</DetailField>
              <DetailField label="Filter">
                <span className="font-mono text-[11px]">
                  {JSON.stringify(budget.filter)}
                </span>
              </DetailField>
            </dl>
          </DetailSection>
          {budget.error && (
            <DetailSection title="Runtime note">
              <p className="text-[12px] text-status-blocked">{budget.error}</p>
            </DetailSection>
          )}
        </>
      )}
    </DetailDialog>
  )
}
