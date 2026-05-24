'use client';

import { useEffect, useMemo, useState } from 'react';
import { z } from 'zod';
import { format } from 'date-fns';
import { useFieldArray, useForm, type Resolver, type UseFormReturn } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import {
  IconArchive,
  IconAlertTriangle,
  IconChartLine,
  IconChevronDown,
  IconChevronUp,
  IconCircleCheck,
  IconCircleX,
  IconDatabase,
  IconEye,
  IconKey,
  IconKeyOff,
  IconLoader2,
  IconPlus,
  IconPlayerPlay,
  IconRefresh,
  IconRestore,
  IconRoute,
  IconSettingsAutomation,
  IconTrash,
} from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { Area, AreaChart, Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis, type TooltipProps } from 'recharts';
import { toast } from 'sonner';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel, FormMessage } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { ScrollArea } from '@/components/ui/scroll-area';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Separator } from '@/components/ui/separator';
import { Switch } from '@/components/ui/switch';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Textarea } from '@/components/ui/textarea';
import {
  useAddChannelAPIKey,
  useArchiveChannelAPIKey,
  useChannelAPIKeyInventory,
  useDeleteChannelAPIKey,
  useDisableChannelAPIKey,
  useEnableChannelAPIKey,
  useRestoreChannelAPIKey,
  useRunChannelAPIKeyHealthCheck,
  useUpdateChannel,
} from '../data/channels';
import {
  Channel,
  ChannelAPIKeyInventoryItem,
  ChannelKeyHealthCheck,
  ChannelKeyHealthCheckHistoryEntry,
  ChannelKeyHealthCheckPolicyActionType,
  channelKeyHealthCheckFailureActionSchema,
  channelKeySelectionStrategySchema,
} from '../data/schema';
import { mergeChannelSettingsForUpdate } from '../utils/merge';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentRow: Channel;
}

type KeyInventoryStatus = 'active' | 'disabled' | 'archived';

interface KeyInventoryRow {
  id: string;
  maskedKey: string;
  status: KeyInventoryStatus;
  lastCheckedAt?: string | null;
  success?: boolean | null;
  failureCount?: number | null;
  balance?: unknown;
  currency?: string | null;
  available?: boolean | null;
  reason?: string | null;
  statusCode?: number | null;
  matchedPolicy?: string | null;
  action?: string | null;
  nextCheckAt?: string | null;
  backoffAttempt?: number | null;
  history?: ChannelKeyHealthCheckHistoryEntry[] | null;
}

type BatchKeyAction = 'health' | 'disable' | 'enable' | 'archive' | 'restore' | 'delete';
type PolicyActionType = ChannelKeyHealthCheckPolicyActionType;
type AvailabilityConditionMode = 'any' | 'available' | 'unavailable';
type KeyHistoryTooltipProps = TooltipProps<number, string> & {
  label?: string | number;
  payload?: Array<{
    dataKey?: string | number;
    color?: string;
    name?: string;
    value?: number | string;
  }>;
};

const keysFormSchema = z.object({
  strategy: channelKeySelectionStrategySchema,
  newKey: z.string().optional(),
  healthCheck: z.object({
    enabled: z.boolean(),
    intervalMinutes: z.coerce.number().int().min(5).max(10080),
    historyLimit: z.coerce.number().int().min(1).max(100),
    failureThreshold: z.coerce.number().int().min(1).max(20),
    failureAction: channelKeyHealthCheckFailureActionSchema,
    includeDisabled: z.boolean(),
    builtinRuleEnabled: z.boolean(),
    deepseekRuleEnabled: z.boolean(),
    deepseekPath: z.string().min(1),
    deepseekUseAbsoluteURL: z.boolean(),
    deepseekExpectedStatuses: z.string(),
    deepseekPassWhen: z.string(),
    policies: z.array(
      z.object({
        id: z.string().min(1),
        name: z.string().min(1),
        enabled: z.boolean(),
        minFailureCount: z.coerce.number().int().min(1).max(100).nullable(),
        statusCodes: z.string().optional(),
        availability: z.enum(['any', 'available', 'unavailable']),
        balanceLTE: z.coerce.number().nullable(),
        reasonContains: z.string().optional(),
        allCheckedKeysFailed: z.boolean(),
        expr: z.string().optional(),
        actions: z.array(
          z.object({
            type: z.enum(['report_only', 'disable_key', 'archive_key', 'delete_key', 'disable_channel', 'backoff']),
            backoffMode: z.enum(['fixed', 'exponential']),
            intervalMinutes: z.coerce.number().int().min(1).max(10080),
            maxIntervalMinutes: z.coerce.number().int().min(1).max(10080),
            multiplier: z.coerce.number().min(1).max(20),
          })
        ),
      })
    ),
  }),
});

type KeysFormValues = z.output<typeof keysFormSchema>;
const keysFormResolver = zodResolver(keysFormSchema) as unknown as Resolver<KeysFormValues, unknown, KeysFormValues>;

const DEFAULT_STRATEGY: KeysFormValues['strategy'] = 'trace_sticky';
const STRATEGIES: KeysFormValues['strategy'][] = ['trace_sticky', 'cache_affinity', 'random', 'round_robin'];
const FAILURE_ACTIONS: KeysFormValues['healthCheck']['failureAction'][] = ['report_only', 'disable', 'archive', 'delete'];
const POLICY_ACTIONS: PolicyActionType[] = ['report_only', 'disable_key', 'archive_key', 'delete_key', 'disable_channel', 'backoff'];

const DEFAULT_HEALTH_CHECK: KeysFormValues['healthCheck'] = {
  enabled: false,
  intervalMinutes: 60,
  historyLimit: 20,
  failureThreshold: 3,
  failureAction: 'report_only',
  includeDisabled: false,
  builtinRuleEnabled: true,
  deepseekRuleEnabled: false,
  deepseekPath: 'https://api.deepseek.com/user/balance',
  deepseekUseAbsoluteURL: true,
  deepseekExpectedStatuses: '200',
  deepseekPassWhen: 'json.is_available == true',
  policies: [],
};

function positiveOrDefault(value: number | null | undefined, fallback: number): number {
  return typeof value === 'number' && value > 0 ? value : fallback;
}

function createDefaultPolicy(index: number): KeysFormValues['healthCheck']['policies'][number] {
  return {
    id: `policy-${Date.now()}-${index + 1}`,
    name: `Policy ${index + 1}`,
    enabled: true,
    minFailureCount: 3,
    statusCodes: '',
    availability: 'any',
    balanceLTE: null,
    reasonContains: '',
    allCheckedKeysFailed: false,
    expr: '',
    actions: [
      {
        type: 'report_only',
        backoffMode: 'fixed',
        intervalMinutes: 30,
        maxIntervalMinutes: 240,
        multiplier: 2,
      },
    ],
  };
}

function parseStatusList(input: string): number[] {
  return input
    .split(',')
    .map((item) => Number(item.trim()))
    .filter((item) => Number.isInteger(item) && item >= 100 && item <= 599);
}

function policyValuesFromHealth(health?: ChannelKeyHealthCheck | null): KeysFormValues['healthCheck']['policies'] {
  return (health?.policies ?? []).map((policy, index) => ({
    id: policy.id || `policy-${index + 1}`,
    name: policy.name || `Policy ${index + 1}`,
    enabled: policy.enabled ?? true,
    minFailureCount: policy.conditions?.minFailureCount ?? null,
    statusCodes: (policy.conditions?.statusCodes ?? []).join(', '),
    availability: policy.conditions?.available == null ? 'any' : policy.conditions.available ? 'available' : 'unavailable',
    balanceLTE: policy.conditions?.balanceLTE ?? null,
    reasonContains: policy.conditions?.reasonContains ?? '',
    allCheckedKeysFailed: policy.conditions?.allCheckedKeysFailed ?? false,
    expr: policy.conditions?.expr ?? '',
    actions:
      policy.actions && policy.actions.length > 0
        ? policy.actions.map((action) => ({
            type: action.type,
            backoffMode: action.backoff?.mode ?? 'fixed',
            intervalMinutes: positiveOrDefault(action.backoff?.intervalMinutes, 30),
            maxIntervalMinutes: positiveOrDefault(action.backoff?.maxIntervalMinutes, 240),
            multiplier: positiveOrDefault(action.backoff?.multiplier, 2),
          }))
        : createDefaultPolicy(index).actions,
  }));
}

function valuesFromChannel(currentRow: Channel): KeysFormValues {
  const health = currentRow.settings?.keyHealthCheck;
  const rules = health?.rules ?? [];
  const builtinRule = rules.find((rule) => rule.type === 'builtin_test');
  const deepseekRule = rules.find(
    (rule) => rule.type === 'http' && (rule.name.toLowerCase().includes('deepseek') || rule.http?.path === '/user/balance')
  );
  const deepseekUseAbsoluteURL = deepseekRule ? deepseekRule.http?.urlMode === 'absolute_url' : DEFAULT_HEALTH_CHECK.deepseekUseAbsoluteURL;

  return {
    strategy: currentRow.settings?.keySelection?.strategy ?? DEFAULT_STRATEGY,
    newKey: '',
    healthCheck: {
      enabled: health?.enabled ?? DEFAULT_HEALTH_CHECK.enabled,
      intervalMinutes: health?.intervalMinutes ?? DEFAULT_HEALTH_CHECK.intervalMinutes,
      historyLimit: positiveOrDefault(health?.historyLimit, DEFAULT_HEALTH_CHECK.historyLimit),
      failureThreshold: health?.failureThreshold ?? DEFAULT_HEALTH_CHECK.failureThreshold,
      failureAction: health?.failureAction ?? DEFAULT_HEALTH_CHECK.failureAction,
      includeDisabled: health?.includeDisabled ?? DEFAULT_HEALTH_CHECK.includeDisabled,
      builtinRuleEnabled: builtinRule ? (builtinRule.enabled ?? true) : DEFAULT_HEALTH_CHECK.builtinRuleEnabled,
      deepseekRuleEnabled: deepseekRule ? (deepseekRule.enabled ?? true) : DEFAULT_HEALTH_CHECK.deepseekRuleEnabled,
      deepseekPath: deepseekRule
        ? (deepseekUseAbsoluteURL ? deepseekRule.http?.url : deepseekRule.http?.path) || DEFAULT_HEALTH_CHECK.deepseekPath
        : DEFAULT_HEALTH_CHECK.deepseekPath,
      deepseekUseAbsoluteURL,
      deepseekExpectedStatuses: (deepseekRule?.http?.expectedStatuses ?? [200]).join(', '),
      deepseekPassWhen: deepseekRule?.http?.passWhen || DEFAULT_HEALTH_CHECK.deepseekPassWhen,
      policies: policyValuesFromHealth(health),
    },
  };
}

function healthCheckFromValues(values: KeysFormValues, existing?: ChannelKeyHealthCheck | null): ChannelKeyHealthCheck {
  const rules: ChannelKeyHealthCheck['rules'] = [];

  if (values.healthCheck.builtinRuleEnabled) {
    rules.push({
      id: 'builtin-key-test',
      name: 'Built-in key test',
      type: 'builtin_test',
      enabled: true,
      builtin: {
        kind: 'channel_api_key_test',
      },
    });
  }

  if (values.healthCheck.deepseekRuleEnabled) {
    rules.push({
      id: 'deepseek-balance',
      name: 'DeepSeek balance',
      type: 'http',
      enabled: true,
      http: {
        method: 'GET',
        urlMode: values.healthCheck.deepseekUseAbsoluteURL ? 'absolute_url' : 'provider_base_url',
        path: values.healthCheck.deepseekUseAbsoluteURL ? null : values.healthCheck.deepseekPath,
        url: values.healthCheck.deepseekUseAbsoluteURL ? values.healthCheck.deepseekPath : null,
        timeoutMs: 10000,
        headers: [],
        keyInjection: {
          location: 'authorization_bearer',
        },
        expectedStatuses: parseStatusList(values.healthCheck.deepseekExpectedStatuses),
        passWhen: values.healthCheck.deepseekPassWhen,
      },
    });
  }

  return {
    enabled: values.healthCheck.enabled,
    intervalMinutes: values.healthCheck.intervalMinutes,
    historyLimit: values.healthCheck.historyLimit,
    failureThreshold: values.healthCheck.failureThreshold,
    failureAction: values.healthCheck.failureAction,
    includeDisabled: values.healthCheck.includeDisabled,
    rules,
    policies: values.healthCheck.policies.map((policy) => ({
      id: policy.id,
      name: policy.name,
      enabled: policy.enabled,
      conditions: {
        minFailureCount: policy.minFailureCount ?? null,
        statusCodes: parseStatusList(policy.statusCodes ?? ''),
        available: policy.availability === 'any' ? null : policy.availability === 'available',
        balanceLTE: policy.balanceLTE ?? null,
        reasonContains: policy.reasonContains?.trim() || null,
        allCheckedKeysFailed: policy.allCheckedKeysFailed || null,
        expr: policy.expr?.trim() || null,
      },
      actions: policy.actions.map((action) => ({
        type: action.type,
        backoff:
          action.type === 'backoff'
            ? {
                mode: action.backoffMode,
                intervalMinutes: action.intervalMinutes,
                maxIntervalMinutes: action.maxIntervalMinutes,
                multiplier: action.multiplier,
              }
            : null,
      })),
    })),
    keyMetadata: existing?.keyMetadata ?? null,
    archivedKeys: existing?.archivedKeys ?? null,
  };
}

function formatDateTime(value?: string | null): string {
  if (!value) {
    return '-';
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return format(date, 'yyyy-MM-dd HH:mm:ss');
}

function inventoryFromBackend(items: ChannelAPIKeyInventoryItem[] = []): KeyInventoryRow[] {
  return items.map((item) => ({
    id: item.id,
    maskedKey: item.maskedKey,
    status: item.status,
    lastCheckedAt: item.lastCheckedAt,
    success: item.success,
    failureCount: item.failureCount,
    balance: item.balance,
    currency: item.currency,
    available: item.available,
    reason: item.reason,
    statusCode: item.statusCode,
    matchedPolicy: item.matchedPolicy,
    action: item.action,
    nextCheckAt: item.nextCheckAt,
    backoffAttempt: item.backoffAttempt,
    history: item.history,
  }));
}

function numericBalance(value: unknown): number | null {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value;
  }
  if (typeof value === 'string') {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : null;
  }
  if (value && typeof value === 'object' && !Array.isArray(value)) {
    const record = value as Record<string, unknown>;
    const candidates = ['total_balance', 'totalBalance', 'balance', 'available_balance', 'availableBalance'];
    for (const key of candidates) {
      const parsed = numericBalance(record[key]);
      if (parsed != null) {
        return parsed;
      }
    }
  }
  return null;
}

function formatBalance(value: unknown, currency?: string | null): string {
  if (value == null) {
    return '-';
  }
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
    return `${value} ${currency ?? ''}`.trim();
  }
  if (Array.isArray(value)) {
    return `${value.length} items`;
  }
  if (typeof value === 'object') {
    const record = value as Record<string, unknown>;
    const candidates = ['total_balance', 'totalBalance', 'balance', 'available_balance', 'availableBalance'];
    for (const key of candidates) {
      const candidate = record[key];
      if (typeof candidate === 'string' || typeof candidate === 'number') {
        return `${candidate} ${currency ?? ''}`.trim();
      }
    }
  }

  return JSON.stringify(value);
}

function healthTone(success?: boolean | null): { textClass: string; badgeVariant: 'default' | 'secondary' | 'destructive' | 'outline' } {
  if (success === true) {
    return { textClass: 'text-green-600', badgeVariant: 'default' };
  }
  if (success === false) {
    return { textClass: 'text-destructive', badgeVariant: 'destructive' };
  }
  return { textClass: 'text-muted-foreground', badgeVariant: 'outline' };
}

function formatPolicyAction(action?: string | null): string {
  return action?.replaceAll('_', ' ') || '-';
}

function KeyHistoryTooltip({ active, payload, label }: KeyHistoryTooltipProps) {
  if (!active || !payload?.length) {
    return null;
  }

  return (
    <div className='bg-background rounded-md border px-3 py-2 text-xs shadow-sm'>
      <div className='font-medium'>{label}</div>
      <div className='mt-1 space-y-1'>
        {payload.map((item) => (
          <div key={String(item.dataKey)} className='flex items-center gap-2'>
            <span className='h-2 w-2 rounded-full' style={{ backgroundColor: item.color }} />
            <span className='text-muted-foreground'>{item.name}</span>
            <span className='font-medium'>{item.value}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function KeyHistoryCharts({ history }: { history: ChannelKeyHealthCheckHistoryEntry[] }) {
  const { t } = useTranslation();
  const chartData = useMemo(
    () =>
      [...history].reverse().map((entry, index) => ({
        name: format(new Date(entry.checkedAt), 'MM-dd HH:mm'),
        success: entry.success ? 1 : 0,
        failure: entry.success ? 0 : 1,
        balance: numericBalance(entry.balance),
        index: index + 1,
      })),
    [history]
  );
  const balanceData = chartData.filter((item) => item.balance != null);

  if (chartData.length === 0) {
    return null;
  }

  return (
    <div className='grid gap-3 md:grid-cols-2'>
      <div className='rounded-lg border p-3'>
        <div className='mb-2 text-sm font-medium'>{t('channels.dialogs.keys.details.charts.health')}</div>
        <ResponsiveContainer width='100%' height={160}>
          <BarChart data={chartData}>
            <CartesianGrid strokeDasharray='3 3' stroke='var(--border)' vertical={false} />
            <XAxis dataKey='index' tickLine={false} axisLine={false} tick={{ fontSize: 11, fill: 'var(--muted-foreground)' }} />
            <YAxis hide domain={[0, 1]} />
            <Tooltip content={<KeyHistoryTooltip />} />
            <Bar dataKey='success' name={t('channels.dialogs.keys.healthState.success')} fill='var(--chart-2)' radius={[4, 4, 0, 0]} />
            <Bar dataKey='failure' name={t('channels.dialogs.keys.healthState.failed')} fill='var(--destructive)' radius={[4, 4, 0, 0]} />
          </BarChart>
        </ResponsiveContainer>
      </div>
      <div className='rounded-lg border p-3'>
        <div className='mb-2 text-sm font-medium'>{t('channels.dialogs.keys.details.charts.balance')}</div>
        {balanceData.length === 0 ? (
          <div className='text-muted-foreground flex h-40 items-center justify-center text-sm'>
            {t('channels.dialogs.keys.details.charts.noBalance')}
          </div>
        ) : (
          <ResponsiveContainer width='100%' height={160}>
            <AreaChart data={balanceData}>
              <CartesianGrid strokeDasharray='3 3' stroke='var(--border)' vertical={false} />
              <XAxis dataKey='index' tickLine={false} axisLine={false} tick={{ fontSize: 11, fill: 'var(--muted-foreground)' }} />
              <YAxis tickLine={false} axisLine={false} width={48} tick={{ fontSize: 11, fill: 'var(--muted-foreground)' }} />
              <Tooltip content={<KeyHistoryTooltip />} />
              <Area
                type='monotone'
                dataKey='balance'
                name={t('channels.dialogs.keys.details.balance')}
                stroke='var(--chart-1)'
                fill='var(--chart-1)'
                fillOpacity={0.18}
                dot={false}
                activeDot={{ r: 3 }}
              />
            </AreaChart>
          </ResponsiveContainer>
        )}
      </div>
    </div>
  );
}

function FailurePolicyEditor({ form, disabled }: { form: UseFormReturn<KeysFormValues, unknown, KeysFormValues>; disabled: boolean }) {
  const { t } = useTranslation();
  const [expandedPolicies, setExpandedPolicies] = useState<Set<string>>(new Set());
  const {
    fields: policyFields,
    append: appendPolicy,
    remove: removePolicy,
  } = useFieldArray({
    control: form.control,
    name: 'healthCheck.policies',
  });

  const addPolicy = () => {
    const policy = createDefaultPolicy(policyFields.length);
    appendPolicy(policy);
    setExpandedPolicies((prev) => new Set(prev).add(policy.id));
  };

  const togglePolicy = (id: string) => {
    setExpandedPolicies((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  return (
    <div className='space-y-3'>
      <div className='flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between'>
        <div>
          <h4 className='text-sm font-medium'>{t('channels.dialogs.keys.health.policies.title')}</h4>
          <p className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.health.policies.description')}</p>
        </div>
        <Button type='button' variant='outline' size='sm' onClick={addPolicy} disabled={disabled}>
          <IconPlus className='mr-2 h-4 w-4' />
          {t('channels.dialogs.keys.health.policies.add')}
        </Button>
      </div>

      {policyFields.length === 0 ? (
        <div className='text-muted-foreground rounded-lg border border-dashed p-4 text-sm'>
          {t('channels.dialogs.keys.health.policies.empty')}
        </div>
      ) : (
        policyFields.map((policyField, policyIndex) => {
          const policyID = form.watch(`healthCheck.policies.${policyIndex}.id`) || policyField.id;
          const policyName = form.watch(`healthCheck.policies.${policyIndex}.name`) || t('channels.dialogs.keys.health.policies.unnamed');
          const policyEnabled = form.watch(`healthCheck.policies.${policyIndex}.enabled`);
          const actions = form.watch(`healthCheck.policies.${policyIndex}.actions`) ?? [];
          const isExpanded = expandedPolicies.has(policyID);

          return (
            <div key={policyField.id} className='rounded-lg border'>
              <div className='flex flex-col gap-2 p-3 sm:flex-row sm:items-center sm:justify-between'>
                <button type='button' className='flex min-w-0 items-center gap-2 text-left' onClick={() => togglePolicy(policyID)}>
                  {isExpanded ? <IconChevronUp className='h-4 w-4' /> : <IconChevronDown className='h-4 w-4' />}
                  <div className='min-w-0'>
                    <div className='truncate text-sm font-medium'>{policyName}</div>
                    <div className='text-muted-foreground text-xs'>
                      {t('channels.dialogs.keys.health.policies.actionCount', { count: actions.length })}
                    </div>
                  </div>
                </button>
                <div className='flex items-center gap-2'>
                  <Badge variant={policyEnabled ? 'default' : 'outline'}>
                    {t(`channels.dialogs.keys.health.policies.${policyEnabled ? 'enabled' : 'disabled'}`)}
                  </Badge>
                  <FormField
                    control={form.control}
                    name={`healthCheck.policies.${policyIndex}.enabled`}
                    render={({ field }) => (
                      <FormItem>
                        <FormControl>
                          <Switch checked={field.value} onCheckedChange={field.onChange} disabled={disabled} />
                        </FormControl>
                      </FormItem>
                    )}
                  />
                  <Button type='button' variant='ghost' size='sm' onClick={() => removePolicy(policyIndex)} disabled={disabled}>
                    <IconTrash className='h-4 w-4' />
                  </Button>
                </div>
              </div>

              {isExpanded ? (
                <div className='space-y-4 border-t p-3'>
                  <FormField
                    control={form.control}
                    name={`healthCheck.policies.${policyIndex}.name`}
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>{t('channels.dialogs.keys.health.policies.fields.name')}</FormLabel>
                        <FormControl>
                          <Input {...field} disabled={disabled} />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />

                  <div className='grid gap-4 md:grid-cols-3'>
                    <FormField
                      control={form.control}
                      name={`healthCheck.policies.${policyIndex}.minFailureCount`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.health.policies.fields.minFailureCount')}</FormLabel>
                          <FormControl>
                            <Input
                              type='number'
                              min={1}
                              max={100}
                              value={typeof field.value === 'number' || typeof field.value === 'string' ? field.value : ''}
                              onBlur={field.onBlur}
                              onChange={(event) => field.onChange(event.target.value)}
                              disabled={disabled}
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`healthCheck.policies.${policyIndex}.statusCodes`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.health.policies.fields.statusCodes')}</FormLabel>
                          <FormControl>
                            <Input placeholder='429, 500' {...field} disabled={disabled} />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`healthCheck.policies.${policyIndex}.availability`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.health.policies.fields.availability')}</FormLabel>
                          <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                            <FormControl>
                              <SelectTrigger>
                                <SelectValue />
                              </SelectTrigger>
                            </FormControl>
                            <SelectContent>
                              {(['any', 'available', 'unavailable'] satisfies AvailabilityConditionMode[]).map((mode) => (
                                <SelectItem key={mode} value={mode}>
                                  {t(`channels.dialogs.keys.health.policies.availability.${mode}`)}
                                </SelectItem>
                              ))}
                            </SelectContent>
                          </Select>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`healthCheck.policies.${policyIndex}.balanceLTE`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.health.policies.fields.balanceLTE')}</FormLabel>
                          <FormControl>
                            <Input
                              type='number'
                              value={typeof field.value === 'number' || typeof field.value === 'string' ? field.value : ''}
                              onBlur={field.onBlur}
                              onChange={(event) => field.onChange(event.target.value)}
                              disabled={disabled}
                            />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`healthCheck.policies.${policyIndex}.reasonContains`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.health.policies.fields.reasonContains')}</FormLabel>
                          <FormControl>
                            <Input {...field} disabled={disabled} />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`healthCheck.policies.${policyIndex}.allCheckedKeysFailed`}
                      render={({ field }) => (
                        <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                          <div className='space-y-0.5'>
                            <FormLabel>{t('channels.dialogs.keys.health.policies.fields.allCheckedKeysFailed')}</FormLabel>
                          </div>
                          <FormControl>
                            <Switch checked={field.value} onCheckedChange={field.onChange} disabled={disabled} />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`healthCheck.policies.${policyIndex}.expr`}
                      render={({ field }) => (
                        <FormItem className='md:col-span-3'>
                          <FormLabel>{t('channels.dialogs.keys.health.policies.fields.expr')}</FormLabel>
                          <FormControl>
                            <Textarea rows={2} {...field} disabled={disabled} />
                          </FormControl>
                          <FormDescription>{t('channels.dialogs.keys.health.policies.fields.exprDescription')}</FormDescription>
                        </FormItem>
                      )}
                    />
                  </div>

                  <PolicyActionsEditor form={form} policyIndex={policyIndex} disabled={disabled} />
                </div>
              ) : null}
            </div>
          );
        })
      )}
    </div>
  );
}

function PolicyActionsEditor({
  form,
  policyIndex,
  disabled,
}: {
  form: UseFormReturn<KeysFormValues, unknown, KeysFormValues>;
  policyIndex: number;
  disabled: boolean;
}) {
  const { t } = useTranslation();
  const {
    fields: actionFields,
    append: appendAction,
    remove: removeAction,
  } = useFieldArray({
    control: form.control,
    name: `healthCheck.policies.${policyIndex}.actions`,
  });

  return (
    <div className='space-y-3'>
      <div className='flex items-center justify-between'>
        <h5 className='text-sm font-medium'>{t('channels.dialogs.keys.health.policies.actions.title')}</h5>
        <Button
          type='button'
          variant='outline'
          size='sm'
          onClick={() =>
            appendAction({
              type: 'report_only',
              backoffMode: 'fixed',
              intervalMinutes: 30,
              maxIntervalMinutes: 240,
              multiplier: 2,
            })
          }
          disabled={disabled}
        >
          <IconPlus className='mr-2 h-4 w-4' />
          {t('channels.dialogs.keys.health.policies.actions.add')}
        </Button>
      </div>
      {actionFields.map((actionField, actionIndex) => {
        const actionType = form.watch(`healthCheck.policies.${policyIndex}.actions.${actionIndex}.type`);
        return (
          <div key={actionField.id} className='grid gap-3 rounded-lg border p-3 md:grid-cols-4'>
            <FormField
              control={form.control}
              name={`healthCheck.policies.${policyIndex}.actions.${actionIndex}.type`}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('channels.dialogs.keys.health.policies.actions.type')}</FormLabel>
                  <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                    <FormControl>
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                    </FormControl>
                    <SelectContent>
                      {POLICY_ACTIONS.map((action) => (
                        <SelectItem key={action} value={action}>
                          {t(`channels.dialogs.keys.health.policies.actions.${action}`)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </FormItem>
              )}
            />
            {actionType === 'backoff' ? (
              <>
                <FormField
                  control={form.control}
                  name={`healthCheck.policies.${policyIndex}.actions.${actionIndex}.backoffMode`}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keys.health.policies.actions.backoffMode')}</FormLabel>
                      <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                        <FormControl>
                          <SelectTrigger>
                            <SelectValue />
                          </SelectTrigger>
                        </FormControl>
                        <SelectContent>
                          <SelectItem value='fixed'>{t('channels.dialogs.keys.health.policies.actions.fixed')}</SelectItem>
                          <SelectItem value='exponential'>{t('channels.dialogs.keys.health.policies.actions.exponential')}</SelectItem>
                        </SelectContent>
                      </Select>
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name={`healthCheck.policies.${policyIndex}.actions.${actionIndex}.intervalMinutes`}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keys.health.policies.actions.intervalMinutes')}</FormLabel>
                      <FormControl>
                        <Input
                          ref={field.ref}
                          name={field.name}
                          type='number'
                          min={1}
                          max={10080}
                          value={field.value}
                          onBlur={field.onBlur}
                          onChange={(event) => field.onChange(event.target.value)}
                          disabled={disabled}
                        />
                      </FormControl>
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name={`healthCheck.policies.${policyIndex}.actions.${actionIndex}.maxIntervalMinutes`}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keys.health.policies.actions.maxIntervalMinutes')}</FormLabel>
                      <FormControl>
                        <Input
                          ref={field.ref}
                          name={field.name}
                          type='number'
                          min={1}
                          max={10080}
                          value={field.value}
                          onBlur={field.onBlur}
                          onChange={(event) => field.onChange(event.target.value)}
                          disabled={disabled}
                        />
                      </FormControl>
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name={`healthCheck.policies.${policyIndex}.actions.${actionIndex}.multiplier`}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keys.health.policies.actions.multiplier')}</FormLabel>
                      <FormControl>
                        <Input
                          ref={field.ref}
                          name={field.name}
                          type='number'
                          min={1}
                          max={20}
                          step='0.1'
                          value={field.value}
                          onBlur={field.onBlur}
                          onChange={(event) => field.onChange(event.target.value)}
                          disabled={disabled}
                        />
                      </FormControl>
                    </FormItem>
                  )}
                />
              </>
            ) : null}
            <div className='flex items-end justify-end md:col-start-4'>
              <Button
                type='button'
                variant='ghost'
                size='sm'
                onClick={() => removeAction(actionIndex)}
                disabled={disabled || actionFields.length <= 1}
              >
                <IconTrash className='h-4 w-4' />
              </Button>
            </div>
          </div>
        );
      })}
    </div>
  );
}

function KeyDetailsDialog({
  row,
  open,
  onOpenChange,
}: {
  row: KeyInventoryRow | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();

  if (!row) {
    return null;
  }

  const latestTone = healthTone(row.success);
  const history = row.history ?? [];

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='sm:max-w-2xl'>
        <DialogHeader className='text-left'>
          <DialogTitle className='flex items-center gap-2'>
            <IconKey className='h-5 w-5' />
            {t('channels.dialogs.keys.details.title')}
          </DialogTitle>
          <DialogDescription>{t('channels.dialogs.keys.details.description')}</DialogDescription>
        </DialogHeader>

        <div className='space-y-4'>
          <div className='grid gap-3 md:grid-cols-2'>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.maskedKey')}</div>
              <code className='bg-muted mt-1 block w-fit rounded px-2 py-0.5 font-mono text-sm'>{row.maskedKey}</code>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.status')}</div>
              <Badge className='mt-1' variant={row.status === 'active' ? 'default' : row.status === 'disabled' ? 'secondary' : 'outline'}>
                {t(`channels.dialogs.keys.status.${row.status}`)}
              </Badge>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.latestHealth')}</div>
              <div className={`mt-1 flex items-center gap-2 text-sm font-medium ${latestTone.textClass}`}>
                {row.success === true ? (
                  <IconCircleCheck className='h-4 w-4' />
                ) : row.success === false ? (
                  <IconCircleX className='h-4 w-4' />
                ) : null}
                {row.success == null
                  ? t('channels.dialogs.keys.healthState.unknown')
                  : t(`channels.dialogs.keys.healthState.${row.success ? 'success' : 'failed'}`)}
              </div>
              <div className='text-muted-foreground mt-1 text-xs'>{formatDateTime(row.lastCheckedAt)}</div>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.balance')}</div>
              <div className='mt-1 text-sm font-medium'>{formatBalance(row.balance, row.currency)}</div>
              {row.available != null ? (
                <Badge variant='outline' className='mt-2'>
                  {t(`channels.dialogs.keys.availability.${row.available ? 'available' : 'unavailable'}`)}
                </Badge>
              ) : null}
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.failureCount')}</div>
              <div className='mt-1 text-sm font-medium'>{row.failureCount ?? 0}</div>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.statusCode')}</div>
              <div className='mt-1 text-sm font-medium'>{row.statusCode ?? '-'}</div>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.matchedPolicy')}</div>
              <div className='mt-1 text-sm font-medium'>{row.matchedPolicy || '-'}</div>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.action')}</div>
              <div className='mt-1 text-sm font-medium'>{formatPolicyAction(row.action)}</div>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.nextCheckAt')}</div>
              <div className='mt-1 text-sm font-medium'>{formatDateTime(row.nextCheckAt)}</div>
              {row.backoffAttempt != null && row.backoffAttempt > 0 ? (
                <div className='text-muted-foreground mt-1 text-xs'>
                  {t('channels.dialogs.keys.details.backoffAttempt', { count: row.backoffAttempt })}
                </div>
              ) : null}
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.reason')}</div>
              <div className='mt-1 text-sm'>{row.reason || '-'}</div>
            </div>
          </div>

          <KeyHistoryCharts history={history} />

          <div className='rounded-lg border'>
            <div className='border-b px-3 py-2 text-sm font-medium'>{t('channels.dialogs.keys.details.history')}</div>
            <div className='max-h-72 divide-y overflow-auto'>
              {history.length === 0 ? (
                <div className='text-muted-foreground p-4 text-sm'>{t('channels.dialogs.keys.details.historyEmpty')}</div>
              ) : (
                history.map((entry) => {
                  const tone = healthTone(entry.success);
                  return (
                    <div key={entry.id} className='flex gap-3 p-3'>
                      <div className={tone.textClass}>
                        {entry.success ? <IconCircleCheck className='h-4 w-4' /> : <IconCircleX className='h-4 w-4' />}
                      </div>
                      <div className='min-w-0 flex-1 space-y-1'>
                        <div className='flex flex-wrap items-center gap-2'>
                          <Badge variant={tone.badgeVariant}>
                            {t(`channels.dialogs.keys.healthState.${entry.success ? 'success' : 'failed'}`)}
                          </Badge>
                          {entry.trigger ? <Badge variant='outline'>{t(`channels.dialogs.keys.trigger.${entry.trigger}`)}</Badge> : null}
                          {entry.rule ? <span className='text-muted-foreground text-xs'>{entry.rule}</span> : null}
                          {entry.statusCode ? (
                            <Badge variant='outline'>
                              {t('channels.dialogs.keys.details.statusCode')}: {entry.statusCode}
                            </Badge>
                          ) : null}
                          {entry.matchedPolicy ? <Badge variant='outline'>{entry.matchedPolicy}</Badge> : null}
                        </div>
                        <div className='text-muted-foreground text-xs'>{formatDateTime(entry.checkedAt)}</div>
                        <div className='text-sm'>{entry.reason || '-'}</div>
                        <div className='text-muted-foreground text-xs'>
                          {formatBalance(entry.balance, entry.currency)}
                          {entry.available != null
                            ? ` · ${t(`channels.dialogs.keys.availability.${entry.available ? 'available' : 'unavailable'}`)}`
                            : ''}
                          {entry.action ? ` · ${formatPolicyAction(entry.action)}` : ''}
                          {entry.nextCheckAt
                            ? ` · ${t('channels.dialogs.keys.details.nextCheckAt')}: ${formatDateTime(entry.nextCheckAt)}`
                            : ''}
                        </div>
                      </div>
                    </div>
                  );
                })
              )}
            </div>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  );
}

export function ChannelsKeysDialog({ open, onOpenChange, currentRow }: Props) {
  const { t } = useTranslation();
  const [selectedKeys, setSelectedKeys] = useState<Set<string>>(new Set());
  const [showArchived, setShowArchived] = useState(false);
  const [detailsKeyID, setDetailsKeyID] = useState<string | null>(null);
  const [confirmDeleteKey, setConfirmDeleteKey] = useState<string | null>(null);
  const [confirmBatchDelete, setConfirmBatchDelete] = useState(false);

  const keyInventory = useChannelAPIKeyInventory(currentRow.id, { enabled: open });
  const addAPIKey = useAddChannelAPIKey();
  const deleteAPIKey = useDeleteChannelAPIKey();
  const archiveAPIKey = useArchiveChannelAPIKey();
  const restoreAPIKey = useRestoreChannelAPIKey();
  const updateChannel = useUpdateChannel();
  const runHealthCheck = useRunChannelAPIKeyHealthCheck();
  const disableAPIKey = useDisableChannelAPIKey();
  const enableAPIKey = useEnableChannelAPIKey();

  const form = useForm<KeysFormValues, unknown, KeysFormValues>({
    resolver: keysFormResolver,
    defaultValues: valuesFromChannel(currentRow),
    mode: 'onChange',
  });

  useEffect(() => {
    if (open) {
      form.reset(valuesFromChannel(currentRow));
      setSelectedKeys(new Set());
      setShowArchived(false);
      setDetailsKeyID(null);
      setConfirmDeleteKey(null);
      setConfirmBatchDelete(false);
    }
  }, [open, currentRow, form]);

  const inventory = useMemo(() => inventoryFromBackend(keyInventory.data), [keyInventory.data]);
  const activeKeys = useMemo(() => inventory.filter((item) => item.status === 'active'), [inventory]);
  const disabledKeys = useMemo(() => inventory.filter((item) => item.status === 'disabled'), [inventory]);
  const archivedKeys = useMemo(() => inventory.filter((item) => item.status === 'archived'), [inventory]);
  const visibleInventory = useMemo(() => inventory.filter((item) => showArchived || item.status !== 'archived'), [inventory, showArchived]);
  const selectedRows = useMemo(() => visibleInventory.filter((item) => selectedKeys.has(item.id)), [visibleInventory, selectedKeys]);
  const selectedHealthCheckKeyIDs = useMemo(
    () => selectedRows.filter((item) => item.status !== 'archived').map((item) => item.id),
    [selectedRows]
  );
  const selectedActiveKeyIDs = useMemo(
    () => selectedRows.filter((item) => item.status === 'active').map((item) => item.id),
    [selectedRows]
  );
  const selectedDisabledKeyIDs = useMemo(
    () => selectedRows.filter((item) => item.status === 'disabled').map((item) => item.id),
    [selectedRows]
  );
  const selectedArchivedKeyIDs = useMemo(
    () => selectedRows.filter((item) => item.status === 'archived').map((item) => item.id),
    [selectedRows]
  );
  const detailsRow = useMemo(() => inventory.find((item) => item.id === detailsKeyID) ?? null, [detailsKeyID, inventory]);
  const selectedStrategy = form.watch('strategy');
  const deepseekRuleEnabled = form.watch('healthCheck.deepseekRuleEnabled');
  const deepseekUseAbsoluteURL = form.watch('healthCheck.deepseekUseAbsoluteURL');
  const policyCount = form.watch('healthCheck.policies')?.length ?? 0;
  const isPending =
    keyInventory.isFetching ||
    addAPIKey.isPending ||
    deleteAPIKey.isPending ||
    archiveAPIKey.isPending ||
    restoreAPIKey.isPending ||
    updateChannel.isPending ||
    runHealthCheck.isPending ||
    disableAPIKey.isPending ||
    enableAPIKey.isPending;

  useEffect(() => {
    const visibleIDs = new Set(visibleInventory.map((item) => item.id));
    setSelectedKeys((prev) => {
      const next = new Set([...prev].filter((id) => visibleIDs.has(id)));
      return next.size === prev.size ? prev : next;
    });
  }, [visibleInventory]);

  const toggleSelected = (id: string, checked: boolean) => {
    setSelectedKeys((prev) => {
      const next = new Set(prev);
      if (checked) {
        next.add(id);
      } else {
        next.delete(id);
      }
      return next;
    });
  };

  const saveSettings = async (values: KeysFormValues) => {
    const nextSettings = mergeChannelSettingsForUpdate(currentRow.settings, {
      keySelection: {
        strategy: values.strategy,
      },
      keyHealthCheck: healthCheckFromValues(values, currentRow.settings?.keyHealthCheck),
    });

    await updateChannel.mutateAsync({
      id: currentRow.id,
      input: {
        settings: nextSettings,
      },
    });
  };

  const handleSaveSettings = async (values: KeysFormValues) => {
    try {
      await saveSettings(values);
      toast.success(t('channels.messages.updateSuccess'));
      onOpenChange(false);
    } catch {
      // Error handled by hook.
    }
  };

  const handleAddKey = async () => {
    const key = form.getValues('newKey')?.trim();
    if (!key) {
      return;
    }

    try {
      await addAPIKey.mutateAsync({ channelID: currentRow.id, key });
      form.setValue('newKey', '');
    } catch {
      // Error handled by hook.
    }
  };

  const handleDeleteKey = async (keyID: string) => {
    try {
      await deleteAPIKey.mutateAsync({ channelID: currentRow.id, keyID });
      setConfirmDeleteKey(null);
    } catch {
      // Error handled by hook.
    }
  };

  const handleDisableKey = async (keyID: string) => {
    try {
      await disableAPIKey.mutateAsync({ channelID: currentRow.id, key: keyID });
    } catch {
      // Error handled by hook.
    }
  };

  const handleEnableKey = async (keyID: string) => {
    try {
      await enableAPIKey.mutateAsync({ channelID: currentRow.id, key: keyID });
    } catch {
      // Error handled by hook.
    }
  };

  const handleArchiveKey = async (keyID: string) => {
    try {
      await archiveAPIKey.mutateAsync({ channelID: currentRow.id, keyID, reason: 'Manually archived by user' });
    } catch {
      // Error handled by hook.
    }
  };

  const handleRestoreKey = async (keyID: string) => {
    try {
      await restoreAPIKey.mutateAsync({ channelID: currentRow.id, keyID });
    } catch {
      // Error handled by hook.
    }
  };

  const handleRunChecks = async (keyIDs?: string[]) => {
    try {
      await runHealthCheck.mutateAsync({
        channelID: currentRow.id,
        keyIDs,
      });
    } catch {
      // Error handled by hook.
    }
  };

  const handleBatchAction = async (action: BatchKeyAction) => {
    const actionMap = {
      health: selectedHealthCheckKeyIDs,
      disable: selectedActiveKeyIDs,
      enable: selectedDisabledKeyIDs,
      archive: [...selectedActiveKeyIDs, ...selectedDisabledKeyIDs],
      restore: selectedArchivedKeyIDs,
      delete: selectedRows.map((item) => item.id),
    };
    const keyIDs = actionMap[action];
    if (keyIDs.length === 0) {
      return;
    }

    if (action === 'health') {
      await handleRunChecks(keyIDs);
    } else if (action === 'disable') {
      await Promise.all(keyIDs.map((keyID) => disableAPIKey.mutateAsync({ channelID: currentRow.id, key: keyID })));
    } else if (action === 'enable') {
      await Promise.all(keyIDs.map((keyID) => enableAPIKey.mutateAsync({ channelID: currentRow.id, key: keyID })));
    } else if (action === 'archive') {
      await Promise.all(
        keyIDs.map((keyID) => archiveAPIKey.mutateAsync({ channelID: currentRow.id, keyID, reason: 'Manually archived by user' }))
      );
    } else if (action === 'restore') {
      await Promise.all(keyIDs.map((keyID) => restoreAPIKey.mutateAsync({ channelID: currentRow.id, keyID })));
    } else {
      await Promise.all(keyIDs.map((keyID) => deleteAPIKey.mutateAsync({ channelID: currentRow.id, keyID })));
    }

    setSelectedKeys(new Set());
    setConfirmBatchDelete(false);
  };

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className='flex max-h-[92vh] flex-col sm:max-w-5xl'>
          <DialogHeader className='text-left'>
            <DialogTitle className='flex items-center gap-2'>
              <IconKey className='h-5 w-5' />
              {t('channels.dialogs.keys.title')}
            </DialogTitle>
            <DialogDescription>{t('channels.dialogs.keys.description', { name: currentRow.name })}</DialogDescription>
          </DialogHeader>

          <Form {...form}>
            <Tabs defaultValue='inventory' className='min-h-0 flex-1'>
              <TabsList className='grid w-full grid-cols-3'>
                <TabsTrigger value='inventory'>{t('channels.dialogs.keys.tabs.inventory')}</TabsTrigger>
                <TabsTrigger value='routing'>{t('channels.dialogs.keys.tabs.routing')}</TabsTrigger>
                <TabsTrigger value='health'>{t('channels.dialogs.keys.tabs.health')}</TabsTrigger>
              </TabsList>

              <ScrollArea className='mt-4 h-[58vh] pr-3'>
                <TabsContent value='inventory' className='mt-0 space-y-4'>
                  <div className='grid gap-3 md:grid-cols-3'>
                    <Card>
                      <CardHeader className='pb-2'>
                        <CardTitle className='text-sm'>{t('channels.dialogs.keys.summary.active')}</CardTitle>
                      </CardHeader>
                      <CardContent className='text-2xl font-semibold'>{activeKeys.length}</CardContent>
                    </Card>
                    <Card>
                      <CardHeader className='pb-2'>
                        <CardTitle className='text-sm'>{t('channels.dialogs.keys.summary.disabled')}</CardTitle>
                      </CardHeader>
                      <CardContent className='text-2xl font-semibold'>{disabledKeys.length}</CardContent>
                    </Card>
                    <Card>
                      <CardHeader className='pb-2'>
                        <CardTitle className='text-sm'>{t('channels.dialogs.keys.summary.archived')}</CardTitle>
                      </CardHeader>
                      <CardContent className='text-2xl font-semibold'>{archivedKeys.length}</CardContent>
                    </Card>
                  </div>

                  <Card>
                    <CardHeader>
                      <CardTitle className='flex items-center gap-2'>
                        <IconKey className='h-5 w-5' />
                        {t('channels.dialogs.keys.inventory.title')}
                      </CardTitle>
                      <CardDescription>{t('channels.dialogs.keys.inventory.description')}</CardDescription>
                    </CardHeader>
                    <CardContent className='space-y-4'>
                      <div className='flex flex-col gap-2 sm:flex-row'>
                        <FormField
                          control={form.control}
                          name='newKey'
                          render={({ field }) => (
                            <FormItem className='flex-1'>
                              <FormControl>
                                <Input
                                  type='password'
                                  autoComplete='off'
                                  placeholder={t('channels.dialogs.keys.fields.newKey.placeholder')}
                                  {...field}
                                />
                              </FormControl>
                              <FormDescription>{t('channels.dialogs.keys.fields.newKey.description')}</FormDescription>
                            </FormItem>
                          )}
                        />
                        <Button type='button' className='sm:mt-0' onClick={handleAddKey} disabled={isPending}>
                          {t('channels.dialogs.keys.actions.add')}
                        </Button>
                      </div>

                      <Alert>
                        <IconAlertTriangle className='h-4 w-4' />
                        <AlertDescription>{t('channels.dialogs.keys.inventory.statusCopy')}</AlertDescription>
                      </Alert>

                      <button
                        type='button'
                        className='from-primary/10 via-background to-muted/50 hover:border-primary/50 flex w-full flex-col gap-3 rounded-xl border bg-gradient-to-r p-4 text-left transition sm:flex-row sm:items-center sm:justify-between'
                        onClick={() => {
                          const firstHistoryRow = visibleInventory.find((item) => (item.history?.length ?? 0) > 0) ?? visibleInventory[0];
                          if (firstHistoryRow) {
                            setDetailsKeyID(firstHistoryRow.id);
                          }
                        }}
                        disabled={visibleInventory.length === 0}
                      >
                        <div className='flex items-start gap-3'>
                          <div className='bg-primary/10 text-primary rounded-lg p-2'>
                            <IconChartLine className='h-5 w-5' />
                          </div>
                          <div>
                            <div className='font-medium'>{t('channels.dialogs.keys.analytics.title')}</div>
                            <div className='text-muted-foreground mt-1 text-sm'>{t('channels.dialogs.keys.analytics.description')}</div>
                          </div>
                        </div>
                        <Badge variant='outline'>
                          {t('channels.dialogs.keys.analytics.historyCount', {
                            count: visibleInventory.reduce((sum, item) => sum + (item.history?.length ?? 0), 0),
                          })}
                        </Badge>
                      </button>

                      <div className='bg-muted/30 flex flex-col gap-3 rounded-md border px-3 py-2 sm:flex-row sm:items-center sm:justify-between'>
                        <div className='text-muted-foreground text-sm'>
                          {showArchived
                            ? t('channels.dialogs.keys.inventory.archivedVisible', { count: archivedKeys.length })
                            : t('channels.dialogs.keys.inventory.archivedHidden', { count: archivedKeys.length })}
                        </div>
                        <div className='flex items-center gap-2'>
                          <span className='text-sm'>{t('channels.dialogs.keys.inventory.showArchived')}</span>
                          <Switch checked={showArchived} onCheckedChange={setShowArchived} />
                        </div>
                      </div>

                      {selectedRows.length > 0 && (
                        <div className='bg-muted/40 flex flex-col gap-2 rounded-md border px-3 py-2'>
                          <span className='text-sm'>{t('channels.dialogs.keys.selectedCount', { count: selectedRows.length })}</span>
                          <div className='flex flex-wrap gap-2'>
                            <Button
                              type='button'
                              variant='outline'
                              size='sm'
                              onClick={() => handleBatchAction('health')}
                              disabled={isPending || selectedHealthCheckKeyIDs.length === 0}
                            >
                              <IconPlayerPlay className='mr-2 h-4 w-4' />
                              {t('channels.dialogs.keys.actions.healthSelected', { count: selectedHealthCheckKeyIDs.length })}
                            </Button>
                            <Button
                              type='button'
                              variant='outline'
                              size='sm'
                              onClick={() => handleBatchAction('disable')}
                              disabled={isPending || selectedActiveKeyIDs.length === 0}
                            >
                              <IconKeyOff className='mr-2 h-4 w-4' />
                              {t('channels.dialogs.keys.actions.disableSelected', { count: selectedActiveKeyIDs.length })}
                            </Button>
                            <Button
                              type='button'
                              variant='outline'
                              size='sm'
                              onClick={() => handleBatchAction('enable')}
                              disabled={isPending || selectedDisabledKeyIDs.length === 0}
                            >
                              <IconRefresh className='mr-2 h-4 w-4' />
                              {t('channels.dialogs.keys.actions.enableSelected', { count: selectedDisabledKeyIDs.length })}
                            </Button>
                            <Button
                              type='button'
                              variant='outline'
                              size='sm'
                              onClick={() => handleBatchAction('archive')}
                              disabled={isPending || selectedActiveKeyIDs.length + selectedDisabledKeyIDs.length === 0}
                            >
                              <IconArchive className='mr-2 h-4 w-4' />
                              {t('channels.dialogs.keys.actions.archiveSelected', {
                                count: selectedActiveKeyIDs.length + selectedDisabledKeyIDs.length,
                              })}
                            </Button>
                            <Button
                              type='button'
                              variant='outline'
                              size='sm'
                              onClick={() => handleBatchAction('restore')}
                              disabled={isPending || selectedArchivedKeyIDs.length === 0}
                            >
                              <IconRestore className='mr-2 h-4 w-4' />
                              {t('channels.dialogs.keys.actions.restoreSelected', { count: selectedArchivedKeyIDs.length })}
                            </Button>
                            <Popover open={confirmBatchDelete} onOpenChange={setConfirmBatchDelete}>
                              <PopoverTrigger asChild>
                                <Button type='button' variant='destructive' size='sm' disabled={isPending}>
                                  <IconTrash className='mr-2 h-4 w-4' />
                                  {t('channels.dialogs.keys.actions.deleteSelected', { count: selectedRows.length })}
                                </Button>
                              </PopoverTrigger>
                              <PopoverContent className='w-80'>
                                <div className='space-y-3'>
                                  <p className='text-sm'>
                                    {t('channels.dialogs.keys.confirmDeleteSelected', { count: selectedRows.length })}
                                  </p>
                                  <div className='flex justify-end gap-2'>
                                    <Button type='button' size='sm' variant='outline' onClick={() => setConfirmBatchDelete(false)}>
                                      {t('common.buttons.cancel')}
                                    </Button>
                                    <Button
                                      type='button'
                                      size='sm'
                                      variant='destructive'
                                      onClick={() => handleBatchAction('delete')}
                                      disabled={isPending}
                                    >
                                      {t('common.buttons.confirm')}
                                    </Button>
                                  </div>
                                </div>
                              </PopoverContent>
                            </Popover>
                            <Button type='button' variant='ghost' size='sm' onClick={() => setSelectedKeys(new Set())} disabled={isPending}>
                              {t('channels.dialogs.keys.actions.clearSelection')}
                            </Button>
                          </div>
                        </div>
                      )}

                      <div className='rounded-lg border'>
                        <Table>
                          <TableHeader>
                            <TableRow>
                              <TableHead className='w-12'></TableHead>
                              <TableHead>{t('channels.dialogs.keys.columns.key')}</TableHead>
                              <TableHead>{t('channels.dialogs.keys.columns.status')}</TableHead>
                              <TableHead>{t('channels.dialogs.keys.columns.lastCheck')}</TableHead>
                              <TableHead>{t('channels.dialogs.keys.columns.balance')}</TableHead>
                              <TableHead className='text-right'>{t('common.columns.actions')}</TableHead>
                            </TableRow>
                          </TableHeader>
                          <TableBody>
                            {visibleInventory.length === 0 ? (
                              <TableRow>
                                <TableCell colSpan={6} className='text-muted-foreground h-28 text-center text-sm'>
                                  {inventory.length === 0
                                    ? t('channels.dialogs.keys.inventory.empty')
                                    : t('channels.dialogs.keys.inventory.archivedOnlyEmpty')}
                                </TableCell>
                              </TableRow>
                            ) : (
                              visibleInventory.map((item) => {
                                const tone = healthTone(item.success);
                                return (
                                  <TableRow key={item.id}>
                                    <TableCell>
                                      <Checkbox
                                        checked={selectedKeys.has(item.id)}
                                        onCheckedChange={(checked) => toggleSelected(item.id, checked === true)}
                                      />
                                    </TableCell>
                                    <TableCell>
                                      <div className='flex flex-col gap-1'>
                                        <code className='bg-muted w-fit rounded px-2 py-0.5 font-mono text-sm'>{item.maskedKey}</code>
                                        <div className='text-muted-foreground flex flex-wrap items-center gap-2 text-xs'>
                                          {item.success != null ? (
                                            <span className={`inline-flex items-center gap-1 ${tone.textClass}`}>
                                              {item.success ? (
                                                <IconCircleCheck className='h-3.5 w-3.5' />
                                              ) : (
                                                <IconCircleX className='h-3.5 w-3.5' />
                                              )}
                                              {t(`channels.dialogs.keys.healthState.${item.success ? 'success' : 'failed'}`)}
                                            </span>
                                          ) : null}
                                          {item.failureCount != null && item.failureCount > 0 ? (
                                            <span>{t('channels.dialogs.keys.failureCount', { count: item.failureCount })}</span>
                                          ) : null}
                                          {item.reason ? <span className='max-w-64 truncate'>{item.reason}</span> : null}
                                        </div>
                                      </div>
                                    </TableCell>
                                    <TableCell>
                                      <Badge
                                        variant={
                                          item.status === 'active' ? 'default' : item.status === 'disabled' ? 'secondary' : 'outline'
                                        }
                                      >
                                        {t(`channels.dialogs.keys.status.${item.status}`)}
                                      </Badge>
                                    </TableCell>
                                    <TableCell className='text-muted-foreground text-sm'>{formatDateTime(item.lastCheckedAt)}</TableCell>
                                    <TableCell>
                                      <div className='text-sm'>
                                        {formatBalance(item.balance, item.currency)}
                                        {item.available != null ? (
                                          <Badge variant='outline' className='ml-2'>
                                            {t(`channels.dialogs.keys.availability.${item.available ? 'available' : 'unavailable'}`)}
                                          </Badge>
                                        ) : null}
                                      </div>
                                    </TableCell>
                                    <TableCell>
                                      <div className='flex justify-end gap-1'>
                                        {item.status !== 'archived' ? (
                                          <Button
                                            type='button'
                                            size='sm'
                                            variant='ghost'
                                            onClick={() => handleRunChecks([item.id])}
                                            disabled={isPending}
                                          >
                                            <IconPlayerPlay className='h-4 w-4' />
                                          </Button>
                                        ) : null}
                                        <Button
                                          type='button'
                                          size='sm'
                                          variant='ghost'
                                          onClick={() => setDetailsKeyID(item.id)}
                                          disabled={isPending}
                                        >
                                          <IconEye className='h-4 w-4' />
                                        </Button>
                                        {item.status === 'active' ? (
                                          <Button
                                            type='button'
                                            size='sm'
                                            variant='ghost'
                                            onClick={() => handleDisableKey(item.id)}
                                            disabled={isPending}
                                          >
                                            <IconKeyOff className='h-4 w-4' />
                                          </Button>
                                        ) : null}
                                        {item.status === 'disabled' ? (
                                          <Button
                                            type='button'
                                            size='sm'
                                            variant='ghost'
                                            onClick={() => handleEnableKey(item.id)}
                                            disabled={isPending}
                                          >
                                            <IconRefresh className='h-4 w-4' />
                                          </Button>
                                        ) : null}
                                        <Popover
                                          open={confirmDeleteKey === item.id}
                                          onOpenChange={(state) => setConfirmDeleteKey(state ? item.id : null)}
                                        >
                                          <PopoverTrigger asChild>
                                            <Button
                                              type='button'
                                              size='sm'
                                              variant='ghost'
                                              className='text-destructive'
                                              disabled={isPending}
                                            >
                                              <IconTrash className='h-4 w-4' />
                                            </Button>
                                          </PopoverTrigger>
                                          <PopoverContent className='w-72'>
                                            <div className='space-y-3'>
                                              <p className='text-sm'>{t('channels.dialogs.keys.confirmDelete')}</p>
                                              <div className='flex justify-end gap-2'>
                                                <Button type='button' size='sm' variant='outline' onClick={() => setConfirmDeleteKey(null)}>
                                                  {t('common.buttons.cancel')}
                                                </Button>
                                                <Button
                                                  type='button'
                                                  size='sm'
                                                  variant='destructive'
                                                  onClick={() => handleDeleteKey(item.id)}
                                                  disabled={isPending}
                                                >
                                                  {t('common.buttons.confirm')}
                                                </Button>
                                              </div>
                                            </div>
                                          </PopoverContent>
                                        </Popover>
                                        {item.status === 'archived' ? (
                                          <Button
                                            type='button'
                                            size='sm'
                                            variant='ghost'
                                            onClick={() => handleRestoreKey(item.id)}
                                            disabled={isPending}
                                          >
                                            <IconRestore className='h-4 w-4' />
                                          </Button>
                                        ) : (
                                          <Button
                                            type='button'
                                            size='sm'
                                            variant='ghost'
                                            onClick={() => handleArchiveKey(item.id)}
                                            disabled={isPending}
                                          >
                                            <IconArchive className='h-4 w-4' />
                                          </Button>
                                        )}
                                      </div>
                                    </TableCell>
                                  </TableRow>
                                );
                              })
                            )}
                          </TableBody>
                        </Table>
                      </div>
                    </CardContent>
                  </Card>
                </TabsContent>

                <TabsContent value='routing' className='mt-0 space-y-4'>
                  <Card>
                    <CardHeader>
                      <CardTitle className='flex items-center gap-2'>
                        <IconRoute className='h-5 w-5' />
                        {t('channels.dialogs.keys.routing.title')}
                      </CardTitle>
                      <CardDescription>{t('channels.dialogs.keys.routing.description')}</CardDescription>
                    </CardHeader>
                    <CardContent className='space-y-4'>
                      <FormField
                        control={form.control}
                        name='strategy'
                        render={({ field }) => (
                          <FormItem>
                            <FormLabel>{t('channels.dialogs.keyRouting.fields.strategy.label')}</FormLabel>
                            <Select value={field.value} onValueChange={field.onChange}>
                              <FormControl>
                                <SelectTrigger>
                                  <SelectValue placeholder={t('channels.dialogs.keyRouting.fields.strategy.placeholder')} />
                                </SelectTrigger>
                              </FormControl>
                              <SelectContent>
                                {STRATEGIES.map((strategy) => (
                                  <SelectItem key={strategy} value={strategy}>
                                    {t(`channels.dialogs.keyRouting.strategies.${strategy}.label`)}
                                  </SelectItem>
                                ))}
                              </SelectContent>
                            </Select>
                            <FormDescription>{t('channels.dialogs.keyRouting.fields.strategy.description')}</FormDescription>
                            <FormMessage />
                          </FormItem>
                        )}
                      />

                      <Alert>
                        <IconDatabase className='h-4 w-4' />
                        <AlertDescription>
                          <div className='font-medium'>{t(`channels.dialogs.keyRouting.strategies.${selectedStrategy}.label`)}</div>
                          <div className='mt-1 text-sm'>{t(`channels.dialogs.keyRouting.strategies.${selectedStrategy}.description`)}</div>
                        </AlertDescription>
                      </Alert>
                    </CardContent>
                  </Card>
                </TabsContent>

                <TabsContent value='health' className='mt-0 space-y-4'>
                  <Card>
                    <CardHeader>
                      <CardTitle className='flex items-center gap-2'>
                        <IconSettingsAutomation className='h-5 w-5' />
                        {t('channels.dialogs.keys.health.title')}
                      </CardTitle>
                      <CardDescription>{t('channels.dialogs.keys.health.description')}</CardDescription>
                    </CardHeader>
                    <CardContent className='space-y-5'>
                      <div className='grid gap-4 md:grid-cols-2'>
                        <FormField
                          control={form.control}
                          name='healthCheck.enabled'
                          render={({ field }) => (
                            <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                              <div className='space-y-0.5'>
                                <FormLabel>{t('channels.dialogs.keys.health.enabled.label')}</FormLabel>
                                <FormDescription>{t('channels.dialogs.keys.health.enabled.description')}</FormDescription>
                              </div>
                              <FormControl>
                                <Switch checked={field.value} onCheckedChange={field.onChange} />
                              </FormControl>
                            </FormItem>
                          )}
                        />
                        <FormField
                          control={form.control}
                          name='healthCheck.includeDisabled'
                          render={({ field }) => (
                            <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                              <div className='space-y-0.5'>
                                <FormLabel>{t('channels.dialogs.keys.health.includeDisabled.label')}</FormLabel>
                                <FormDescription>{t('channels.dialogs.keys.health.includeDisabled.description')}</FormDescription>
                              </div>
                              <FormControl>
                                <Switch checked={field.value} onCheckedChange={field.onChange} />
                              </FormControl>
                            </FormItem>
                          )}
                        />
                      </div>

                      <div className='grid gap-4 md:grid-cols-3'>
                        <FormField
                          control={form.control}
                          name='healthCheck.intervalMinutes'
                          render={({ field }) => (
                            <FormItem>
                              <FormLabel>{t('channels.dialogs.keys.health.interval.label')}</FormLabel>
                              <FormControl>
                                <Input
                                  ref={field.ref}
                                  name={field.name}
                                  type='number'
                                  min={5}
                                  max={10080}
                                  value={typeof field.value === 'number' || typeof field.value === 'string' ? field.value : ''}
                                  onBlur={field.onBlur}
                                  onChange={(event) => field.onChange(event.target.value)}
                                />
                              </FormControl>
                              <FormDescription>{t('channels.dialogs.keys.health.interval.description')}</FormDescription>
                              <FormMessage />
                            </FormItem>
                          )}
                        />
                        <FormField
                          control={form.control}
                          name='healthCheck.historyLimit'
                          render={({ field }) => (
                            <FormItem>
                              <FormLabel>{t('channels.dialogs.keys.health.historyLimit.label')}</FormLabel>
                              <FormControl>
                                <Input
                                  ref={field.ref}
                                  name={field.name}
                                  type='number'
                                  min={1}
                                  max={100}
                                  value={typeof field.value === 'number' || typeof field.value === 'string' ? field.value : ''}
                                  onBlur={field.onBlur}
                                  onChange={(event) => field.onChange(event.target.value)}
                                />
                              </FormControl>
                              <FormDescription>{t('channels.dialogs.keys.health.historyLimit.description')}</FormDescription>
                              <FormMessage />
                            </FormItem>
                          )}
                        />
                      </div>

                      <FailurePolicyEditor form={form} disabled={isPending} />

                      <Separator />

                      <div className='space-y-3 rounded-lg border p-3'>
                        <div>
                          <h4 className='text-sm font-medium'>{t('channels.dialogs.keys.health.legacy.title')}</h4>
                          <p className='text-muted-foreground text-xs'>
                            {t(
                              policyCount > 0
                                ? 'channels.dialogs.keys.health.legacy.descriptionWithPolicies'
                                : 'channels.dialogs.keys.health.legacy.description'
                            )}
                          </p>
                        </div>
                        <div className='grid gap-4 md:grid-cols-2'>
                          <FormField
                            control={form.control}
                            name='healthCheck.failureThreshold'
                            render={({ field }) => (
                              <FormItem>
                                <FormLabel>{t('channels.dialogs.keys.health.failureThreshold.label')}</FormLabel>
                                <FormControl>
                                  <Input
                                    ref={field.ref}
                                    name={field.name}
                                    type='number'
                                    min={1}
                                    max={20}
                                    value={typeof field.value === 'number' || typeof field.value === 'string' ? field.value : ''}
                                    onBlur={field.onBlur}
                                    onChange={(event) => field.onChange(event.target.value)}
                                  />
                                </FormControl>
                                <FormDescription>{t('channels.dialogs.keys.health.failureThreshold.description')}</FormDescription>
                                <FormMessage />
                              </FormItem>
                            )}
                          />
                          <FormField
                            control={form.control}
                            name='healthCheck.failureAction'
                            render={({ field }) => (
                              <FormItem>
                                <FormLabel>{t('channels.dialogs.keys.health.failureAction.label')}</FormLabel>
                                <Select value={field.value} onValueChange={field.onChange}>
                                  <FormControl>
                                    <SelectTrigger>
                                      <SelectValue />
                                    </SelectTrigger>
                                  </FormControl>
                                  <SelectContent>
                                    {FAILURE_ACTIONS.map((action) => (
                                      <SelectItem key={action} value={action}>
                                        {t(`channels.dialogs.keys.health.failureActions.${action}`)}
                                      </SelectItem>
                                    ))}
                                  </SelectContent>
                                </Select>
                                <FormDescription>{t('channels.dialogs.keys.health.failureAction.description')}</FormDescription>
                              </FormItem>
                            )}
                          />
                        </div>
                      </div>

                      <Separator />

                      <div className='space-y-4'>
                        <FormField
                          control={form.control}
                          name='healthCheck.builtinRuleEnabled'
                          render={({ field }) => (
                            <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                              <div className='space-y-0.5'>
                                <FormLabel>{t('channels.dialogs.keys.health.rules.builtin.label')}</FormLabel>
                                <FormDescription>{t('channels.dialogs.keys.health.rules.builtin.description')}</FormDescription>
                              </div>
                              <FormControl>
                                <Switch checked={field.value} onCheckedChange={field.onChange} />
                              </FormControl>
                            </FormItem>
                          )}
                        />

                        <FormField
                          control={form.control}
                          name='healthCheck.deepseekRuleEnabled'
                          render={({ field }) => (
                            <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                              <div className='space-y-0.5'>
                                <FormLabel>{t('channels.dialogs.keys.health.rules.deepseek.label')}</FormLabel>
                                <FormDescription>{t('channels.dialogs.keys.health.rules.deepseek.description')}</FormDescription>
                              </div>
                              <FormControl>
                                <Switch checked={field.value} onCheckedChange={field.onChange} />
                              </FormControl>
                            </FormItem>
                          )}
                        />

                        {deepseekRuleEnabled ? (
                          <div className='grid gap-4 rounded-lg border p-3 md:grid-cols-3'>
                            <FormField
                              control={form.control}
                              name='healthCheck.deepseekUseAbsoluteURL'
                              render={({ field }) => (
                                <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3 md:col-span-3'>
                                  <div className='space-y-0.5'>
                                    <FormLabel>{t('channels.dialogs.keys.health.rules.deepseek.absoluteUrl.label')}</FormLabel>
                                    <FormDescription>
                                      {t('channels.dialogs.keys.health.rules.deepseek.absoluteUrl.description')}
                                    </FormDescription>
                                  </div>
                                  <FormControl>
                                    <Switch checked={field.value} onCheckedChange={field.onChange} />
                                  </FormControl>
                                </FormItem>
                              )}
                            />
                            <FormField
                              control={form.control}
                              name='healthCheck.deepseekPath'
                              render={({ field }) => (
                                <FormItem>
                                  <FormLabel>
                                    {t(
                                      deepseekUseAbsoluteURL
                                        ? 'channels.dialogs.keys.health.rules.deepseek.url'
                                        : 'channels.dialogs.keys.health.rules.deepseek.path'
                                    )}
                                  </FormLabel>
                                  <FormControl>
                                    <Input {...field} />
                                  </FormControl>
                                  <FormMessage />
                                </FormItem>
                              )}
                            />
                            <FormField
                              control={form.control}
                              name='healthCheck.deepseekExpectedStatuses'
                              render={({ field }) => (
                                <FormItem>
                                  <FormLabel>{t('channels.dialogs.keys.health.rules.deepseek.statuses')}</FormLabel>
                                  <FormControl>
                                    <Input {...field} />
                                  </FormControl>
                                </FormItem>
                              )}
                            />
                            <FormField
                              control={form.control}
                              name='healthCheck.deepseekPassWhen'
                              render={({ field }) => (
                                <FormItem className='md:col-span-3'>
                                  <FormLabel>{t('channels.dialogs.keys.health.rules.deepseek.passWhen')}</FormLabel>
                                  <FormControl>
                                    <Textarea rows={3} {...field} />
                                  </FormControl>
                                  <FormDescription>{t('channels.dialogs.keys.health.rules.deepseek.passWhenDescription')}</FormDescription>
                                </FormItem>
                              )}
                            />
                          </div>
                        ) : null}
                      </div>
                    </CardContent>
                  </Card>
                </TabsContent>
              </ScrollArea>
            </Tabs>
          </Form>

          <DialogFooter className='gap-2 sm:justify-between'>
            <div className='flex items-center gap-2'>
              <Button type='button' variant='outline' onClick={() => handleRunChecks()} disabled={isPending || activeKeys.length === 0}>
                {runHealthCheck.isPending ? (
                  <IconLoader2 className='mr-2 h-4 w-4 animate-spin' />
                ) : (
                  <IconPlayerPlay className='mr-2 h-4 w-4' />
                )}
                {t('channels.dialogs.keys.actions.runChecks')}
              </Button>
              <div className='text-muted-foreground hidden items-center gap-1 text-xs sm:flex'>
                <IconAlertTriangle className='h-3.5 w-3.5' />
                {t('channels.dialogs.keys.rawKeySafety')}
              </div>
            </div>
            <div className='flex gap-2'>
              <Button type='button' variant='outline' onClick={() => onOpenChange(false)}>
                {t('common.buttons.cancel')}
              </Button>
              <Button type='button' onClick={form.handleSubmit(handleSaveSettings)} disabled={isPending || !form.formState.isValid}>
                {updateChannel.isPending ? t('common.buttons.saving') : t('common.buttons.save')}
              </Button>
            </div>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <KeyDetailsDialog row={detailsRow} open={!!detailsRow} onOpenChange={(state) => !state && setDetailsKeyID(null)} />
    </>
  );
}
