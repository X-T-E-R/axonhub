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
  IconTrash,
} from '@tabler/icons-react';
import type { TFunction } from 'i18next';
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
  ChannelBalanceProbe,
  ChannelFailurePolicy,
  ChannelKeyBalanceSnapshot,
  ChannelFailurePolicyMode,
  FailurePolicyActionType,
  FailurePolicyEventSource,
  ChannelKeyHealthCheck,
  ChannelKeyHealthCheckHistoryEntry,
  channelKeySelectionStrategySchema,
  channelKeyStatusSchema,
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
  balanceSnapshot?: ChannelKeyBalanceSnapshot | null;
  reason?: string | null;
  statusCode?: number | null;
  matchedPolicy?: string | null;
  action?: string | null;
  nextCheckAt?: string | null;
  backoffAttempt?: number | null;
  history?: ChannelKeyHealthCheckHistoryEntry[] | null;
}

type BatchKeyAction = 'health' | 'disable' | 'enable' | 'archive' | 'restore' | 'delete';
const KEY_STATUS_FILTERS: KeyInventoryStatus[] = ['active', 'disabled', 'archived'];
const DEFAULT_KEY_STATUS_FILTERS: KeyInventoryStatus[] = ['active', 'disabled'];
type FailurePolicyTarget = 'key' | 'channel';
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
type BalanceCandidate = {
  amount: number;
  currency?: string | null;
};
type BalanceSummary = {
  display: string;
  keyCount: number;
};

const nullableIntField = (min: number, max: number) =>
  z.preprocess((value) => (value === '' || value == null ? null : value), z.coerce.number().int().min(min).max(max).nullable());
const nullableNumberField = z.preprocess((value) => (value === '' || value == null ? null : value), z.coerce.number().nullable());

const failureProfileFormSchema = z.object({
  id: z.string().min(1),
  name: z.string().min(1),
  enabled: z.boolean(),
  sources: z
    .array(
      z.enum([
        'request_failure',
        'scheduled_health_check',
        'manual_health_check',
        'scheduled_health_check_failure',
        'manual_health_check_failure',
        'scheduled_balance_probe',
        'scheduled_balance_probe_failure',
        'manual_balance_probe',
        'manual_balance_probe_failure',
      ])
    )
    .min(1),
  minFailureCount: nullableIntField(1, 100),
  statusCodes: z.string().optional(),
  availability: z.enum(['any', 'available', 'unavailable']),
  balanceLTE: nullableNumberField,
  reasonContains: z.string().optional(),
  allCheckedKeysFailed: z.boolean(),
  expr: z.string().optional(),
  actions: z.array(
    z.object({
      type: z.enum([
        'report_only',
        'backoff_key',
        'disable_key',
        'archive_key',
        'delete_key',
        'disable_channel',
        'enable_key',
        'restore_key',
      ]),
      backoffMode: z.enum(['fixed', 'exponential']),
      intervalMinutes: z.coerce.number().int().min(1).max(10080),
      maxIntervalMinutes: z.coerce.number().int().min(1).max(10080),
      multiplier: z.coerce.number().min(1).max(20),
    })
  ),
});

const keysFormSchema = z.object({
  strategy: channelKeySelectionStrategySchema,
  likelyAffinityTTLMinutes: z.coerce.number().int().min(1).max(1440),
  exactAffinityTTLMinutes: z.coerce.number().int().min(1).max(10080),
  newKey: z.string().optional(),
  healthCheck: z.object({
    enabled: z.boolean(),
    intervalMinutes: z.coerce.number().int().min(5).max(10080),
    historyLimit: z.coerce.number().int().min(1).max(100),
    includeDisabled: z.boolean(),
    builtinRuleEnabled: z.boolean(),
    deepseekRuleEnabled: z.boolean(),
    deepseekPath: z.string().min(1),
    deepseekUseAbsoluteURL: z.boolean(),
    deepseekExpectedStatuses: z.string(),
    deepseekPassWhen: z.string(),
  }),
  balanceProbe: z.object({
    enabled: z.boolean(),
    preset: z.string().min(1),
    experimental: z.boolean(),
    preferredCurrency: z.string().optional(),
    primarySelection: z.enum(['highest_amount', 'preferred_currency']),
    includeStatuses: z.array(channelKeyStatusSchema),
    timeoutMs: z.coerce.number().int().min(100).max(30000),
  }),
  failurePolicy: z.object({
    mode: z.enum(['inherit', 'override', 'merge', 'disabled']),
    keyProfiles: z.array(failureProfileFormSchema),
    channelProfiles: z.array(failureProfileFormSchema),
  }),
});

type KeysFormValues = z.output<typeof keysFormSchema>;
type FailureProfileFormValue = KeysFormValues['failurePolicy']['keyProfiles'][number];
type FailureActionFormValue = FailureProfileFormValue['actions'][number];
const keysFormResolver = zodResolver(keysFormSchema) as unknown as Resolver<KeysFormValues, unknown, KeysFormValues>;

const DEFAULT_STRATEGY: KeysFormValues['strategy'] = 'trace_sticky';
const DEFAULT_LIKELY_AFFINITY_TTL_MINUTES = 30;
const DEFAULT_EXACT_AFFINITY_TTL_MINUTES = 1440;
const STRATEGIES: KeysFormValues['strategy'][] = ['trace_sticky', 'cache_affinity', 'random', 'round_robin'];
const FAILURE_POLICY_MODES: ChannelFailurePolicyMode[] = ['inherit', 'override', 'merge', 'disabled'];
const FAILURE_EVENT_SOURCES: FailurePolicyEventSource[] = [
  'request_failure',
  'scheduled_health_check',
  'manual_health_check',
  'scheduled_health_check_failure',
  'manual_health_check_failure',
  'scheduled_balance_probe',
  'scheduled_balance_probe_failure',
  'manual_balance_probe',
  'manual_balance_probe_failure',
];
const KEY_FAILURE_ACTIONS: FailurePolicyActionType[] = [
  'report_only',
  'backoff_key',
  'disable_key',
  'archive_key',
  'delete_key',
  'enable_key',
  'restore_key',
];
const CHANNEL_FAILURE_ACTIONS: FailurePolicyActionType[] = ['report_only', 'disable_channel'];
const BALANCE_PROBE_PRESETS = [
  'deepseek_balance',
  'siliconflow_user_info',
  'moonshot_balance',
  'openrouter_credits',
  'nanogpt_check_balance',
] as const;
const BALANCE_PROBE_PRESET_BY_CHANNEL_TYPE: Partial<Record<Channel['type'], (typeof BALANCE_PROBE_PRESETS)[number]>> = {
  deepseek: 'deepseek_balance',
  deepseek_anthropic: 'deepseek_balance',
  siliconflow: 'siliconflow_user_info',
  moonshot: 'moonshot_balance',
  moonshot_anthropic: 'moonshot_balance',
  moonshot_coding: 'moonshot_balance',
  openrouter: 'openrouter_credits',
  nanogpt: 'nanogpt_check_balance',
  nanogpt_responses: 'nanogpt_check_balance',
};
const BALANCE_VALUE_KEYS = [
  'total_balance',
  'totalBalance',
  'balance',
  'available_balance',
  'availableBalance',
  'granted_balance',
  'topped_up_balance',
];
const BALANCE_COLLECTION_KEYS = ['balance_infos', 'balanceInfos', 'balances'];

const DEFAULT_HEALTH_CHECK: KeysFormValues['healthCheck'] = {
  enabled: false,
  intervalMinutes: 60,
  historyLimit: 20,
  includeDisabled: false,
  builtinRuleEnabled: true,
  deepseekRuleEnabled: false,
  deepseekPath: 'https://api.deepseek.com/user/balance',
  deepseekUseAbsoluteURL: true,
  deepseekExpectedStatuses: '200',
  deepseekPassWhen: 'json.is_available == true',
};

const DEFAULT_BALANCE_PROBE: KeysFormValues['balanceProbe'] = {
  enabled: false,
  preset: 'deepseek_balance',
  experimental: false,
  preferredCurrency: '',
  primarySelection: 'highest_amount',
  includeStatuses: ['active', 'disabled'],
  timeoutMs: 10000,
};

function positiveOrDefault(value: number | null | undefined, fallback: number): number {
  return typeof value === 'number' && value > 0 ? value : fallback;
}

function defaultBalanceProbeForChannel(type: Channel['type']): KeysFormValues['balanceProbe'] {
  return {
    ...DEFAULT_BALANCE_PROBE,
    preset: BALANCE_PROBE_PRESET_BY_CHANNEL_TYPE[type] ?? DEFAULT_BALANCE_PROBE.preset,
  };
}

function createDefaultProfile(index: number, target: FailurePolicyTarget): FailureProfileFormValue {
  return {
    id: `${target}-policy-${Date.now()}-${index + 1}`,
    name: `${target === 'key' ? 'Key' : 'Channel'} policy ${index + 1}`,
    enabled: true,
    sources:
      target === 'key' ? ['request_failure', 'scheduled_health_check_failure', 'scheduled_balance_probe_failure'] : ['request_failure'],
    minFailureCount: 3,
    statusCodes: '',
    availability: 'any',
    balanceLTE: null,
    reasonContains: '',
    allCheckedKeysFailed: false,
    expr: '',
    actions: [
      {
        type: 'report_only' as const,
        backoffMode: 'fixed',
        intervalMinutes: 30,
        maxIntervalMinutes: 240,
        multiplier: 2,
      },
    ],
  };
}

function createDefaultAction(target: FailurePolicyTarget): FailureActionFormValue {
  return {
    type: target === 'key' ? 'report_only' : 'report_only',
    backoffMode: 'fixed',
    intervalMinutes: 30,
    maxIntervalMinutes: 240,
    multiplier: 2,
  };
}

function parseStatusList(input: string): number[] {
  return input
    .split(',')
    .map((item) => Number(item.trim()))
    .filter((item) => Number.isInteger(item) && item >= 100 && item <= 599);
}

function toProfileFormValue(
  profile: NonNullable<ChannelFailurePolicy['keyProfiles']>[number],
  index: number,
  target: FailurePolicyTarget,
  sourcesFallback: FailurePolicyEventSource[]
): FailureProfileFormValue {
  const allowedActions = target === 'key' ? KEY_FAILURE_ACTIONS : CHANNEL_FAILURE_ACTIONS;
  const actions =
    profile.actions
      ?.filter((action) => allowedActions.includes(action.type))
      .map((action) => ({
        type: action.type,
        backoffMode: action.backoff?.mode ?? 'fixed',
        intervalMinutes: positiveOrDefault(action.backoff?.intervalMinutes, 30),
        maxIntervalMinutes: positiveOrDefault(action.backoff?.maxIntervalMinutes, 240),
        multiplier: positiveOrDefault(action.backoff?.multiplier, 2),
      })) ?? [];

  return {
    id: profile.id || `${target}-policy-${index + 1}`,
    name: profile.name || `${target === 'key' ? 'Key' : 'Channel'} policy ${index + 1}`,
    enabled: profile.enabled ?? true,
    sources: profile.sources && profile.sources.length > 0 ? profile.sources : sourcesFallback,
    minFailureCount: profile.conditions?.minFailureCount ?? null,
    statusCodes: (profile.conditions?.statusCodes ?? []).join(', '),
    availability: profile.conditions?.available == null ? 'any' : profile.conditions.available ? 'available' : 'unavailable',
    balanceLTE: profile.conditions?.balanceLTE ?? null,
    reasonContains: profile.conditions?.reasonContains ?? '',
    allCheckedKeysFailed: profile.conditions?.allCheckedKeysFailed ?? false,
    expr: profile.conditions?.expr ?? '',
    actions: actions.length > 0 ? actions : [createDefaultAction(target)],
  };
}

function failurePolicyValuesFromStored(policy?: ChannelFailurePolicy | null): KeysFormValues['failurePolicy'] {
  return {
    mode: policy?.mode ?? 'inherit',
    keyProfiles: (policy?.keyProfiles ?? []).map((profile, index) =>
      toProfileFormValue(profile, index, 'key', ['request_failure', 'scheduled_health_check_failure'])
    ),
    channelProfiles: (policy?.channelProfiles ?? []).map((profile, index) =>
      toProfileFormValue(profile, index, 'channel', ['request_failure'])
    ),
  };
}

function mapLegacyHealthActionType(type: string): FailurePolicyActionType | null {
  if (type === 'backoff') return 'backoff_key';
  if (type === 'report_only') return 'report_only';
  if (type === 'disable_key' || type === 'archive_key' || type === 'delete_key' || type === 'disable_channel') return type;
  return null;
}

function failurePolicyValuesFromLegacyHealth(health?: ChannelKeyHealthCheck | null): KeysFormValues['failurePolicy'] {
  const keyProfiles: FailureProfileFormValue[] = [];
  const channelProfiles: FailureProfileFormValue[] = [];
  const legacySources: FailurePolicyEventSource[] = ['scheduled_health_check_failure', 'manual_health_check_failure'];

  (health?.policies ?? []).forEach((policy, index) => {
    const legacyProfile = {
      id: policy.id,
      name: policy.name,
      enabled: policy.enabled,
      sources: legacySources,
      conditions: policy.conditions,
      actions: (policy.actions ?? [])
        .map((action) => {
          const type = mapLegacyHealthActionType(action.type);
          return type
            ? {
                type,
                backoff: type === 'backoff_key' ? action.backoff : null,
              }
            : null;
        })
        .filter((action): action is NonNullable<typeof action> => action != null),
    };

    const keyProfile = toProfileFormValue(legacyProfile, index, 'key', legacySources);

    if (keyProfile.actions.some((action) => KEY_FAILURE_ACTIONS.includes(action.type))) {
      keyProfiles.push({
        ...keyProfile,
        actions: keyProfile.actions.filter((action) => KEY_FAILURE_ACTIONS.includes(action.type)),
      });
    }

    if ((policy.actions ?? []).some((action) => action.type === 'disable_channel') || policy.conditions?.allCheckedKeysFailed) {
      const channelProfile = toProfileFormValue(legacyProfile, index, 'channel', legacySources);
      channelProfiles.push({
        ...channelProfile,
        id: `${channelProfile.id}-channel`,
        name: `${channelProfile.name} (channel)`,
        actions: channelProfile.actions.filter((action) => CHANNEL_FAILURE_ACTIONS.includes(action.type)),
      });
    }
  });

  if (health?.failureAction && health.failureAction !== 'report_only') {
    keyProfiles.push({
      ...createDefaultProfile(keyProfiles.length, 'key'),
      id: 'legacy-health-threshold',
      name: 'Legacy health-check threshold',
      sources: legacySources,
      minFailureCount: health.failureThreshold ?? 3,
      actions: [
        {
          ...createDefaultAction('key'),
          type: health.failureAction === 'disable' ? 'disable_key' : health.failureAction === 'archive' ? 'archive_key' : 'delete_key',
        },
      ],
    });
  }

  return {
    mode: 'merge',
    keyProfiles,
    channelProfiles: channelProfiles.filter((profile) => profile.actions.length > 0),
  };
}

function valuesFromChannel(currentRow: Channel): KeysFormValues {
  const health = currentRow.settings?.keyHealthCheck;
  const balanceProbe = currentRow.settings?.balanceProbe;
  const defaultBalanceProbe = defaultBalanceProbeForChannel(currentRow.type);
  const rules = health?.rules ?? [];
  const builtinRule = rules.find((rule) => rule.type === 'builtin_test');
  const deepseekRule = rules.find(
    (rule) => rule.type === 'http' && (rule.name.toLowerCase().includes('deepseek') || rule.http?.path === '/user/balance')
  );
  const deepseekUseAbsoluteURL = deepseekRule ? deepseekRule.http?.urlMode === 'absolute_url' : DEFAULT_HEALTH_CHECK.deepseekUseAbsoluteURL;

  return {
    strategy: currentRow.settings?.keySelection?.strategy ?? DEFAULT_STRATEGY,
    likelyAffinityTTLMinutes: currentRow.settings?.keySelection?.likelyAffinityTTLMinutes ?? DEFAULT_LIKELY_AFFINITY_TTL_MINUTES,
    exactAffinityTTLMinutes: currentRow.settings?.keySelection?.exactAffinityTTLMinutes ?? DEFAULT_EXACT_AFFINITY_TTL_MINUTES,
    newKey: '',
    healthCheck: {
      enabled: health?.enabled ?? DEFAULT_HEALTH_CHECK.enabled,
      intervalMinutes: health?.intervalMinutes ?? DEFAULT_HEALTH_CHECK.intervalMinutes,
      historyLimit: positiveOrDefault(health?.historyLimit, DEFAULT_HEALTH_CHECK.historyLimit),
      includeDisabled: health?.includeDisabled ?? DEFAULT_HEALTH_CHECK.includeDisabled,
      builtinRuleEnabled: builtinRule ? (builtinRule.enabled ?? true) : DEFAULT_HEALTH_CHECK.builtinRuleEnabled,
      deepseekRuleEnabled: deepseekRule ? (deepseekRule.enabled ?? true) : DEFAULT_HEALTH_CHECK.deepseekRuleEnabled,
      deepseekPath: deepseekRule
        ? (deepseekUseAbsoluteURL ? deepseekRule.http?.url : deepseekRule.http?.path) || DEFAULT_HEALTH_CHECK.deepseekPath
        : DEFAULT_HEALTH_CHECK.deepseekPath,
      deepseekUseAbsoluteURL,
      deepseekExpectedStatuses: (deepseekRule?.http?.expectedStatuses ?? [200]).join(', '),
      deepseekPassWhen: deepseekRule?.http?.passWhen || DEFAULT_HEALTH_CHECK.deepseekPassWhen,
    },
    balanceProbe: {
      enabled: balanceProbe?.enabled ?? defaultBalanceProbe.enabled,
      preset: balanceProbe?.preset || defaultBalanceProbe.preset,
      experimental: balanceProbe?.experimental ?? defaultBalanceProbe.experimental,
      preferredCurrency: balanceProbe?.preferredCurrency ?? defaultBalanceProbe.preferredCurrency,
      primarySelection: balanceProbe?.primarySelection ?? defaultBalanceProbe.primarySelection,
      includeStatuses: balanceProbe?.includeStatuses?.length ? balanceProbe.includeStatuses : defaultBalanceProbe.includeStatuses,
      timeoutMs: positiveOrDefault(balanceProbe?.timeoutMs, defaultBalanceProbe.timeoutMs),
    },
    failurePolicy: currentRow.settings?.failurePolicy
      ? failurePolicyValuesFromStored(currentRow.settings.failurePolicy)
      : failurePolicyValuesFromLegacyHealth(health),
  };
}

function balanceProbeFromValues(values: KeysFormValues, existing?: ChannelBalanceProbe | null): ChannelBalanceProbe {
  return {
    enabled: values.balanceProbe.enabled,
    preset: values.balanceProbe.preset,
    experimental: values.balanceProbe.experimental,
    preferredCurrency: values.balanceProbe.preferredCurrency?.trim() || null,
    primarySelection: values.balanceProbe.primarySelection,
    includeStatuses: values.balanceProbe.includeStatuses,
    timeoutMs: values.balanceProbe.timeoutMs,
    http: existing?.http ?? null,
  };
}

function healthCheckFromValues(values: KeysFormValues): ChannelKeyHealthCheck {
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
    failureThreshold: 3,
    failureAction: 'report_only',
    includeDisabled: values.healthCheck.includeDisabled,
    rules,
    policies: [],
  };
}

function failurePolicyFromValues(values: KeysFormValues): ChannelFailurePolicy {
  const toStoredProfile = (policy: FailureProfileFormValue, target: FailurePolicyTarget) => ({
    id: policy.id,
    name: policy.name,
    enabled: policy.enabled,
    sources: policy.sources,
    conditions: {
      minFailureCount: policy.minFailureCount ?? null,
      statusCodes: parseStatusList(policy.statusCodes ?? ''),
      available: policy.availability === 'any' ? null : policy.availability === 'available',
      balanceLTE: policy.balanceLTE ?? null,
      reasonContains: policy.reasonContains?.trim() || null,
      allCheckedKeysFailed: policy.allCheckedKeysFailed || null,
      expr: policy.expr?.trim() || null,
    },
    actions: policy.actions
      .filter((action) => (target === 'key' ? KEY_FAILURE_ACTIONS : CHANNEL_FAILURE_ACTIONS).includes(action.type))
      .map((action) => ({
        type: action.type,
        backoff:
          action.type === 'backoff_key'
            ? {
                mode: action.backoffMode,
                intervalMinutes: action.intervalMinutes,
                maxIntervalMinutes: action.maxIntervalMinutes,
                multiplier: action.multiplier,
              }
            : null,
      })),
  });

  return {
    mode: values.failurePolicy.mode,
    keyProfiles: values.failurePolicy.keyProfiles.map((profile) => toStoredProfile(profile, 'key')),
    channelProfiles: values.failurePolicy.channelProfiles.map((profile) => toStoredProfile(profile, 'channel')),
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
    balanceSnapshot: item.balanceSnapshot,
    reason: item.reason,
    statusCode: item.statusCode,
    matchedPolicy: item.matchedPolicy,
    action: item.action,
    nextCheckAt: item.nextCheckAt,
    backoffAttempt: item.backoffAttempt,
    history: item.history,
  }));
}

function numericBalanceValue(value: unknown): number | null {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value;
  }
  if (typeof value === 'string') {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : null;
  }
  return null;
}

function normalizeBalanceCurrency(value: unknown): string | null {
  return typeof value === 'string' && value.trim().length > 0 ? value.trim().toUpperCase() : null;
}

function isCnyBalanceCurrency(currency?: string | null): boolean {
  const normalized = normalizeBalanceCurrency(currency);
  return normalized === 'CNY' || normalized === 'RMB';
}

function getBalanceCandidates(value: unknown, inheritedCurrency?: string | null): BalanceCandidate[] {
  const directAmount = numericBalanceValue(value);
  if (directAmount != null) {
    return [{ amount: directAmount, currency: normalizeBalanceCurrency(inheritedCurrency) }];
  }

  if (Array.isArray(value)) {
    return value.flatMap((item) => getBalanceCandidates(item, inheritedCurrency));
  }

  if (value && typeof value === 'object' && !Array.isArray(value)) {
    const record = value as Record<string, unknown>;
    const currency =
      normalizeBalanceCurrency(record.currency) ??
      normalizeBalanceCurrency(record.currency_code) ??
      normalizeBalanceCurrency(record.currencyCode) ??
      normalizeBalanceCurrency(inheritedCurrency);
    const candidates: BalanceCandidate[] = [];

    for (const key of BALANCE_VALUE_KEYS) {
      const candidate = record[key];
      const parsed = numericBalanceValue(candidate);
      if (parsed != null) {
        candidates.push({ amount: parsed, currency });
        continue;
      }
      candidates.push(...getBalanceCandidates(candidate, currency));
    }

    for (const key of BALANCE_COLLECTION_KEYS) {
      candidates.push(...getBalanceCandidates(record[key], currency));
    }

    return candidates;
  }

  return [];
}

function preferredBalance(value: unknown, currency?: string | null): BalanceCandidate | null {
  const candidates = getBalanceCandidates(value, currency);
  if (candidates.length === 0) {
    return null;
  }

  return candidates.reduce((best, candidate) => (candidate.amount > best.amount ? candidate : best));
}

function preferredSnapshotBalance(snapshot?: ChannelKeyBalanceSnapshot | null, preferredCurrency?: string | null): BalanceCandidate | null {
  if (!snapshot) {
    return null;
  }

  const candidates = [snapshot.primaryBalance, ...(snapshot.components ?? [])]
    .filter((item): item is NonNullable<typeof item> => item != null && Number.isFinite(item.amount))
    .map((item) => ({ amount: item.amount, currency: normalizeBalanceCurrency(item.currency) }));

  if (candidates.length === 0) {
    return null;
  }

  const preferred = normalizeBalanceCurrency(preferredCurrency);
  const preferredCandidates = preferred ? candidates.filter((candidate) => normalizeBalanceCurrency(candidate.currency) === preferred) : [];
  if (preferredCandidates.length > 0) {
    return preferredCandidates.reduce((best, candidate) => (candidate.amount > best.amount ? candidate : best));
  }

  if (snapshot.primaryBalance && Number.isFinite(snapshot.primaryBalance.amount)) {
    return {
      amount: snapshot.primaryBalance.amount,
      currency: normalizeBalanceCurrency(snapshot.primaryBalance.currency),
    };
  }

  return candidates.reduce((best, candidate) => (candidate.amount > best.amount ? candidate : best));
}

function preferredRowBalance(row: {
  balance?: unknown;
  currency?: string | null;
  balanceSnapshot?: ChannelKeyBalanceSnapshot | null;
}): BalanceCandidate | null {
  return preferredSnapshotBalance(row.balanceSnapshot) ?? preferredBalance(row.balance, row.currency);
}

function numericBalance(value: unknown, snapshot?: ChannelKeyBalanceSnapshot | null): number | null {
  return (snapshot ? preferredSnapshotBalance(snapshot)?.amount : null) ?? preferredBalance(value)?.amount ?? null;
}

function currencyForIntl(currency?: string | null): string | null {
  const normalized = normalizeBalanceCurrency(currency);
  if (!normalized) {
    return null;
  }
  if (normalized === 'RMB') {
    return 'CNY';
  }
  return /^[A-Z]{3}$/.test(normalized) ? normalized : null;
}

function localeForLanguage(language: string): string {
  return language.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en-US';
}

function formatNumber(value: number, language: string): string {
  return new Intl.NumberFormat(localeForLanguage(language), {
    maximumFractionDigits: 6,
  }).format(value);
}

function formatBalanceAmount(amount: number, currency: string | null | undefined, t: TFunction, language: string): string {
  const intlCurrency = currencyForIntl(currency);
  if (intlCurrency) {
    return t('currencies.format', {
      val: amount,
      currency: intlCurrency,
      locale: localeForLanguage(language),
      minimumFractionDigits: 2,
      maximumFractionDigits: 6,
    });
  }

  const normalizedCurrency = normalizeBalanceCurrency(currency);
  return `${formatNumber(amount, language)} ${normalizedCurrency ?? ''}`.trim();
}

function formatBalance(value: unknown, currency: string | null | undefined, t: TFunction, language: string): string {
  const selected = preferredBalance(value, currency);
  if (selected) {
    return formatBalanceAmount(selected.amount, selected.currency, t, language);
  }

  if (value == null) {
    return '-';
  }
  if (typeof value === 'string' || typeof value === 'boolean') {
    return `${value} ${currency ?? ''}`.trim();
  }
  if (Array.isArray(value)) {
    return `${value.length} items`;
  }

  return JSON.stringify(value);
}

function formatBalanceSnapshot(snapshot: ChannelKeyBalanceSnapshot, t: TFunction, language: string): string {
  const selected = preferredSnapshotBalance(snapshot);
  if (selected) {
    return formatBalanceAmount(selected.amount, selected.currency, t, language);
  }
  if (snapshot.available != null) {
    return t(`channels.dialogs.keys.availability.${snapshot.available ? 'available' : 'unavailable'}`);
  }
  return snapshot.accountStatus || '-';
}

function formatRowBalance(
  row: { balance?: unknown; currency?: string | null; balanceSnapshot?: ChannelKeyBalanceSnapshot | null },
  t: TFunction,
  language: string
): string {
  return row.balanceSnapshot
    ? formatBalanceSnapshot(row.balanceSnapshot, t, language)
    : formatBalance(row.balance, row.currency, t, language);
}

function summarizeActiveBalances(rows: KeyInventoryRow[], t: TFunction, language: string): BalanceSummary | null {
  const balances = rows.map((row) => preferredRowBalance(row)).filter((balance): balance is BalanceCandidate => balance != null);

  if (balances.length === 0) {
    return null;
  }

  const totalsByCurrency = new Map<string, number>();

  for (const balance of balances) {
    const currency = normalizeBalanceCurrency(balance.currency) ?? '';
    totalsByCurrency.set(currency, (totalsByCurrency.get(currency) ?? 0) + balance.amount);
  }

  const parts = [...totalsByCurrency.entries()]
    .sort(([leftCurrency], [rightCurrency]) => {
      if (isCnyBalanceCurrency(leftCurrency)) {
        return -1;
      }
      if (isCnyBalanceCurrency(rightCurrency)) {
        return 1;
      }
      return leftCurrency.localeCompare(rightCurrency);
    })
    .map(([currency, amount]) => formatBalanceAmount(amount, currency || null, t, language));

  return {
    display: parts.join(' · '),
    keyCount: balances.length,
  };
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
        balance: numericBalance(entry.balance, entry.balanceSnapshot),
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

  return (
    <div className='space-y-5'>
      <FormField
        control={form.control}
        name='failurePolicy.mode'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('channels.dialogs.keys.failureStrategy.mode.label')}</FormLabel>
            <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
              <FormControl>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
              </FormControl>
              <SelectContent>
                {FAILURE_POLICY_MODES.map((mode) => (
                  <SelectItem key={mode} value={mode}>
                    {t(`channels.dialogs.keys.failureStrategy.modes.${mode}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <FormDescription>{t('channels.dialogs.keys.failureStrategy.mode.description')}</FormDescription>
          </FormItem>
        )}
      />

      <div className='grid gap-4 lg:grid-cols-2'>
        <FailurePolicyProfilesEditor
          form={form}
          disabled={disabled}
          target='key'
          name='failurePolicy.keyProfiles'
          title={t('channels.dialogs.keys.failureStrategy.keyProfiles.title')}
          description={t('channels.dialogs.keys.failureStrategy.keyProfiles.description')}
        />
        <FailurePolicyProfilesEditor
          form={form}
          disabled={disabled}
          target='channel'
          name='failurePolicy.channelProfiles'
          title={t('channels.dialogs.keys.failureStrategy.channelProfiles.title')}
          description={t('channels.dialogs.keys.failureStrategy.channelProfiles.description')}
        />
      </div>
    </div>
  );
}

function FailurePolicyProfilesEditor({
  form,
  disabled,
  target,
  name,
  title,
  description,
}: {
  form: UseFormReturn<KeysFormValues, unknown, KeysFormValues>;
  disabled: boolean;
  target: FailurePolicyTarget;
  name: 'failurePolicy.keyProfiles' | 'failurePolicy.channelProfiles';
  title: string;
  description: string;
}) {
  const { t } = useTranslation();
  const [expandedPolicies, setExpandedPolicies] = useState<Set<string>>(new Set());
  const {
    fields: policyFields,
    append: appendPolicy,
    remove: removePolicy,
  } = useFieldArray({
    control: form.control,
    name,
  });

  const addPolicy = () => {
    const policy = createDefaultProfile(policyFields.length, target);
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
    <div className='space-y-3 rounded-lg border p-3'>
      <div className='flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between'>
        <div>
          <h4 className='text-sm font-medium'>{title}</h4>
          <p className='text-muted-foreground text-xs'>{description}</p>
        </div>
        <Button type='button' variant='outline' size='sm' onClick={addPolicy} disabled={disabled}>
          <IconPlus className='mr-2 h-4 w-4' />
          {t('channels.dialogs.keys.failureStrategy.profiles.add')}
        </Button>
      </div>

      {policyFields.length === 0 ? (
        <div className='text-muted-foreground rounded-lg border border-dashed p-4 text-sm'>
          {t('channels.dialogs.keys.failureStrategy.profiles.empty')}
        </div>
      ) : (
        policyFields.map((policyField, policyIndex) => {
          const profilePath = `${name}.${policyIndex}` as const;
          const policyID = form.watch(`${profilePath}.id`) || policyField.id;
          const policyName = form.watch(`${profilePath}.name`) || t('channels.dialogs.keys.failureStrategy.profiles.unnamed');
          const policyEnabled = form.watch(`${profilePath}.enabled`);
          const actions = form.watch(`${profilePath}.actions`) ?? [];
          const isExpanded = expandedPolicies.has(policyID);

          return (
            <div key={policyField.id} className='rounded-lg border'>
              <div className='flex flex-col gap-2 p-3 sm:flex-row sm:items-center sm:justify-between'>
                <button type='button' className='flex min-w-0 items-center gap-2 text-left' onClick={() => togglePolicy(policyID)}>
                  {isExpanded ? <IconChevronUp className='h-4 w-4' /> : <IconChevronDown className='h-4 w-4' />}
                  <div className='min-w-0'>
                    <div className='truncate text-sm font-medium'>{policyName}</div>
                    <div className='text-muted-foreground text-xs'>
                      {t('channels.dialogs.keys.failureStrategy.profiles.actionCount', { count: actions.length })}
                    </div>
                  </div>
                </button>
                <div className='flex items-center gap-2'>
                  <Badge variant={policyEnabled ? 'default' : 'outline'}>
                    {t(`channels.dialogs.keys.failureStrategy.profiles.${policyEnabled ? 'enabled' : 'disabled'}`)}
                  </Badge>
                  <FormField
                    control={form.control}
                    name={`${profilePath}.enabled`}
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
                    name={`${profilePath}.name`}
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.name')}</FormLabel>
                        <FormControl>
                          <Input {...field} disabled={disabled} />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />

                  <div className='space-y-2 rounded-lg border p-3'>
                    <div className='text-sm font-medium'>{t('channels.dialogs.keys.failureStrategy.profiles.fields.sources')}</div>
                    <div className='grid gap-2'>
                      {FAILURE_EVENT_SOURCES.map((source) => (
                        <FormField
                          key={source}
                          control={form.control}
                          name={`${profilePath}.sources`}
                          render={({ field }) => {
                            const selected = field.value?.includes(source) ?? false;
                            return (
                              <FormItem className='flex items-center gap-2 space-y-0'>
                                <FormControl>
                                  <Checkbox
                                    checked={selected}
                                    onCheckedChange={(checked) => {
                                      const current = field.value ?? [];
                                      field.onChange(
                                        checked
                                          ? [...current.filter((item) => item !== source), source]
                                          : current.length <= 1
                                            ? current
                                            : current.filter((item) => item !== source)
                                      );
                                    }}
                                    disabled={disabled}
                                  />
                                </FormControl>
                                <FormLabel className='text-sm font-normal'>
                                  {t(`channels.dialogs.keys.failureStrategy.sources.${source}`)}
                                </FormLabel>
                              </FormItem>
                            );
                          }}
                        />
                      ))}
                    </div>
                  </div>

                  <div className='grid gap-4 md:grid-cols-3'>
                    <FormField
                      control={form.control}
                      name={`${profilePath}.minFailureCount`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.minFailureCount')}</FormLabel>
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
                      name={`${profilePath}.statusCodes`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.statusCodes')}</FormLabel>
                          <FormControl>
                            <Input placeholder='429, 500' {...field} disabled={disabled} />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`${profilePath}.availability`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.availability')}</FormLabel>
                          <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                            <FormControl>
                              <SelectTrigger>
                                <SelectValue />
                              </SelectTrigger>
                            </FormControl>
                            <SelectContent>
                              {(['any', 'available', 'unavailable'] satisfies AvailabilityConditionMode[]).map((mode) => (
                                <SelectItem key={mode} value={mode}>
                                  {t(`channels.dialogs.keys.failureStrategy.profiles.availability.${mode}`)}
                                </SelectItem>
                              ))}
                            </SelectContent>
                          </Select>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`${profilePath}.balanceLTE`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.balanceLTE')}</FormLabel>
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
                      name={`${profilePath}.reasonContains`}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.reasonContains')}</FormLabel>
                          <FormControl>
                            <Input {...field} disabled={disabled} />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`${profilePath}.allCheckedKeysFailed`}
                      render={({ field }) => (
                        <FormItem className='flex flex-row items-center justify-between rounded-lg border p-3'>
                          <div className='space-y-0.5'>
                            <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.allCheckedKeysFailed')}</FormLabel>
                          </div>
                          <FormControl>
                            <Switch checked={field.value} onCheckedChange={field.onChange} disabled={disabled} />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name={`${profilePath}.expr`}
                      render={({ field }) => (
                        <FormItem className='md:col-span-3'>
                          <FormLabel>{t('channels.dialogs.keys.failureStrategy.profiles.fields.expr')}</FormLabel>
                          <FormControl>
                            <Textarea rows={2} {...field} disabled={disabled} />
                          </FormControl>
                          <FormDescription>{t('channels.dialogs.keys.failureStrategy.profiles.fields.exprDescription')}</FormDescription>
                        </FormItem>
                      )}
                    />
                  </div>

                  <PolicyActionsEditor form={form} profileName={name} profileIndex={policyIndex} target={target} disabled={disabled} />
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
  profileName,
  profileIndex,
  target,
  disabled,
}: {
  form: UseFormReturn<KeysFormValues, unknown, KeysFormValues>;
  profileName: 'failurePolicy.keyProfiles' | 'failurePolicy.channelProfiles';
  profileIndex: number;
  target: FailurePolicyTarget;
  disabled: boolean;
}) {
  const { t } = useTranslation();
  const actionsName = `${profileName}.${profileIndex}.actions` as const;
  const actionChoices = target === 'key' ? KEY_FAILURE_ACTIONS : CHANNEL_FAILURE_ACTIONS;
  const {
    fields: actionFields,
    append: appendAction,
    remove: removeAction,
  } = useFieldArray({
    control: form.control,
    name: actionsName,
  });

  return (
    <div className='space-y-3'>
      <div className='flex items-center justify-between'>
        <h5 className='text-sm font-medium'>{t('channels.dialogs.keys.failureStrategy.actions.title')}</h5>
        <Button type='button' variant='outline' size='sm' onClick={() => appendAction(createDefaultAction(target))} disabled={disabled}>
          <IconPlus className='mr-2 h-4 w-4' />
          {t('channels.dialogs.keys.failureStrategy.actions.add')}
        </Button>
      </div>
      {actionFields.map((actionField, actionIndex) => {
        const actionPath = `${actionsName}.${actionIndex}` as const;
        const actionType = form.watch(`${actionPath}.type`);
        return (
          <div key={actionField.id} className='grid gap-3 rounded-lg border p-3 md:grid-cols-4'>
            <FormField
              control={form.control}
              name={`${actionPath}.type`}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('channels.dialogs.keys.failureStrategy.actions.type')}</FormLabel>
                  <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                    <FormControl>
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                    </FormControl>
                    <SelectContent>
                      {actionChoices.map((action) => (
                        <SelectItem key={action} value={action}>
                          {t(`channels.dialogs.keys.failureStrategy.actions.${action}`)}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </FormItem>
              )}
            />
            {actionType === 'backoff_key' ? (
              <>
                <FormField
                  control={form.control}
                  name={`${actionPath}.backoffMode`}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keys.failureStrategy.actions.backoffMode')}</FormLabel>
                      <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                        <FormControl>
                          <SelectTrigger>
                            <SelectValue />
                          </SelectTrigger>
                        </FormControl>
                        <SelectContent>
                          <SelectItem value='fixed'>{t('channels.dialogs.keys.failureStrategy.actions.fixed')}</SelectItem>
                          <SelectItem value='exponential'>{t('channels.dialogs.keys.failureStrategy.actions.exponential')}</SelectItem>
                        </SelectContent>
                      </Select>
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name={`${actionPath}.intervalMinutes`}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keys.failureStrategy.actions.intervalMinutes')}</FormLabel>
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
                  name={`${actionPath}.maxIntervalMinutes`}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keys.failureStrategy.actions.maxIntervalMinutes')}</FormLabel>
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
                  name={`${actionPath}.multiplier`}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.keys.failureStrategy.actions.multiplier')}</FormLabel>
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
  const { t, i18n } = useTranslation();

  if (!row) {
    return null;
  }

  const latestTone = healthTone(row.success);
  const history = row.history ?? [];

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='flex max-h-[90dvh] flex-col overflow-hidden sm:max-w-2xl'>
        <DialogHeader className='shrink-0 text-left'>
          <DialogTitle className='flex items-center gap-2'>
            <IconKey className='h-5 w-5' />
            {t('channels.dialogs.keys.details.title')}
          </DialogTitle>
          <DialogDescription>{t('channels.dialogs.keys.details.description')}</DialogDescription>
        </DialogHeader>

        <div className='min-h-0 flex-1 space-y-4 overflow-y-auto pr-1'>
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
              <div className='mt-1 text-sm font-medium'>{formatRowBalance(row, t, i18n.language)}</div>
              {row.balanceSnapshot?.components?.length ? (
                <div className='text-muted-foreground mt-2 space-y-1 text-xs'>
                  {row.balanceSnapshot.components.map((component, index) => (
                    <div key={`${component.kind}-${component.currency}-${index}`}>
                      {component.label || t(`channels.dialogs.keys.balanceKinds.${component.kind}`)}:{' '}
                      {formatBalanceAmount(component.amount, component.currency, t, i18n.language)}
                    </div>
                  ))}
                </div>
              ) : null}
              {(row.balanceSnapshot?.available ?? row.available) != null ? (
                <Badge variant='outline' className='mt-2'>
                  {t(
                    `channels.dialogs.keys.availability.${(row.balanceSnapshot?.available ?? row.available) ? 'available' : 'unavailable'}`
                  )}
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
                          {formatRowBalance(entry, t, i18n.language)}
                          {(entry.balanceSnapshot?.available ?? entry.available) != null
                            ? ` · ${t(`channels.dialogs.keys.availability.${(entry.balanceSnapshot?.available ?? entry.available) ? 'available' : 'unavailable'}`)}`
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

function ChannelHistoryDialog({
  history,
  open,
  onOpenChange,
}: {
  history: ChannelKeyHealthCheckHistoryEntry[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation();
  const latest = history[0] ?? null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='flex max-h-[90dvh] flex-col overflow-hidden sm:max-w-2xl'>
        <DialogHeader className='shrink-0 text-left'>
          <DialogTitle className='flex items-center gap-2'>
            <IconRoute className='h-5 w-5' />
            {t('channels.dialogs.keys.channelHistory.title')}
          </DialogTitle>
          <DialogDescription>{t('channels.dialogs.keys.channelHistory.description')}</DialogDescription>
        </DialogHeader>

        <div className='min-h-0 flex-1 space-y-4 overflow-y-auto pr-1'>
          <div className='grid gap-3 md:grid-cols-2'>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.channelHistory.scope')}</div>
              <div className='mt-1 text-sm font-medium'>{t('channels.dialogs.keys.channelHistory.scopeValue')}</div>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.channelHistory.eventCount')}</div>
              <div className='mt-1 text-sm font-medium'>{history.length}</div>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.latestHealth')}</div>
              <div className='mt-1 text-sm font-medium'>
                {latest ? t(`channels.dialogs.keys.healthState.${latest.success ? 'success' : 'failed'}`) : '-'}
              </div>
              <div className='text-muted-foreground mt-1 text-xs'>{formatDateTime(latest?.checkedAt)}</div>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.matchedPolicy')}</div>
              <div className='mt-1 text-sm font-medium'>{latest?.matchedPolicy || '-'}</div>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.action')}</div>
              <div className='mt-1 text-sm font-medium'>{formatPolicyAction(latest?.action)}</div>
            </div>
            <div className='rounded-lg border p-3'>
              <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.details.reason')}</div>
              <div className='mt-1 text-sm'>{latest?.reason || '-'}</div>
            </div>
          </div>

          <KeyHistoryCharts history={history} />

          <div className='rounded-lg border'>
            <div className='border-b px-3 py-2 text-sm font-medium'>{t('channels.dialogs.keys.details.history')}</div>
            <div className='max-h-72 divide-y overflow-auto'>
              {history.length === 0 ? (
                <div className='text-muted-foreground p-4 text-sm'>{t('channels.dialogs.keys.channelHistory.empty')}</div>
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
                          <Badge variant='outline'>
                            {entry.id?.startsWith('channel:')
                              ? t('channels.dialogs.keys.channelHistory.target.channel')
                              : t('channels.dialogs.keys.channelHistory.target.key')}
                          </Badge>
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
                          {entry.action ? formatPolicyAction(entry.action) : '-'}
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
  const { t, i18n } = useTranslation();
  const [selectedKeys, setSelectedKeys] = useState<Set<string>>(new Set());
  const [statusFilter, setStatusFilter] = useState<Set<KeyInventoryStatus>>(new Set(DEFAULT_KEY_STATUS_FILTERS));
  const [detailsKeyID, setDetailsKeyID] = useState<string | null>(null);
  const [channelHistoryOpen, setChannelHistoryOpen] = useState(false);
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
      setStatusFilter(new Set(DEFAULT_KEY_STATUS_FILTERS));
      setDetailsKeyID(null);
      setChannelHistoryOpen(false);
      setConfirmDeleteKey(null);
      setConfirmBatchDelete(false);
    }
  }, [open, currentRow, form]);

  const inventory = useMemo(() => inventoryFromBackend(keyInventory.data), [keyInventory.data]);
  const activeKeys = useMemo(() => inventory.filter((item) => item.status === 'active'), [inventory]);
  const disabledKeys = useMemo(() => inventory.filter((item) => item.status === 'disabled'), [inventory]);
  const archivedKeys = useMemo(() => inventory.filter((item) => item.status === 'archived'), [inventory]);
  const channelHistory = useMemo(() => currentRow.settings?.keyHealthCheck?.history ?? [], [currentRow.settings?.keyHealthCheck?.history]);
  const activeBalanceSummary = useMemo(() => summarizeActiveBalances(activeKeys, t, i18n.language), [activeKeys, i18n.language, t]);
  const visibleInventory = useMemo(() => inventory.filter((item) => statusFilter.has(item.status)), [inventory, statusFilter]);
  const selectedRows = useMemo(() => visibleInventory.filter((item) => selectedKeys.has(item.id)), [visibleInventory, selectedKeys]);
  const visibleSelectedCount = useMemo(
    () => visibleInventory.filter((item) => selectedKeys.has(item.id)).length,
    [selectedKeys, visibleInventory]
  );
  const allVisibleSelected = visibleInventory.length > 0 && visibleSelectedCount === visibleInventory.length;
  const someVisibleSelected = visibleSelectedCount > 0 && !allVisibleSelected;
  const statusFilterSummary = useMemo(
    () =>
      KEY_STATUS_FILTERS.filter((status) => statusFilter.has(status))
        .map((status) => t(`channels.dialogs.keys.status.${status}`))
        .join(', '),
    [statusFilter, t]
  );
  const selectedHealthCheckKeyIDs = useMemo(() => selectedRows.map((item) => item.id), [selectedRows]);
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
  const inventoryHistoryCount = useMemo(
    () => visibleInventory.reduce((sum, item) => sum + (item.history?.length ?? 0), 0),
    [visibleInventory]
  );
  const selectedStrategy = form.watch('strategy');
  const selectedBalanceProbePrimarySelection = form.watch('balanceProbe.primarySelection');
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

  const toggleStatusFilter = (status: KeyInventoryStatus, checked: boolean) => {
    setStatusFilter((prev) => {
      const next = new Set(prev);
      if (checked) {
        next.add(status);
      } else {
        next.delete(status);
      }
      return next;
    });
  };

  const toggleVisibleSelection = (checked: boolean) => {
    setSelectedKeys((prev) => {
      const next = new Set(prev);
      for (const item of visibleInventory) {
        if (checked) {
          next.add(item.id);
        } else {
          next.delete(item.id);
        }
      }
      return next;
    });
  };

  const saveSettings = async (values: KeysFormValues) => {
    const nextSettings = mergeChannelSettingsForUpdate(currentRow.settings, {
      keySelection: {
        strategy: values.strategy,
        likelyAffinityTTLMinutes: values.likelyAffinityTTLMinutes,
        exactAffinityTTLMinutes: values.exactAffinityTTLMinutes,
      },
      keyHealthCheck: healthCheckFromValues(values),
      balanceProbe: balanceProbeFromValues(values, currentRow.settings?.balanceProbe),
      failurePolicy: failurePolicyFromValues(values),
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
              <TabsList className='grid w-full grid-cols-2'>
                <TabsTrigger value='inventory'>{t('channels.dialogs.keys.tabs.inventory')}</TabsTrigger>
                <TabsTrigger value='routing'>{t('channels.dialogs.keys.tabs.routing')}</TabsTrigger>
              </TabsList>

              <ScrollArea className='mt-4 h-[58vh] pr-3'>
                <TabsContent value='inventory' className='mt-0 space-y-4'>
                  <div className='grid gap-3 md:grid-cols-4'>
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
                    <Card>
                      <CardHeader className='pb-2'>
                        <CardTitle className='text-sm'>{t('channels.dialogs.keys.summary.activeBalance')}</CardTitle>
                      </CardHeader>
                      <CardContent>
                        <div className='text-2xl font-semibold'>{activeBalanceSummary?.display ?? '-'}</div>
                        <div className='text-muted-foreground mt-1 text-xs'>
                          {activeBalanceSummary
                            ? t('channels.dialogs.keys.summary.activeBalanceKeys', {
                                count: activeBalanceSummary.keyCount,
                              })
                            : t('channels.dialogs.keys.summary.activeBalanceEmpty')}
                        </div>
                      </CardContent>
                    </Card>
                  </div>

                  <Card>
                    <CardHeader>
                      <CardTitle>{t('channels.dialogs.keys.balanceProbe.title')}</CardTitle>
                      <CardDescription>{t('channels.dialogs.keys.balanceProbe.description')}</CardDescription>
                    </CardHeader>
                    <CardContent className='space-y-4'>
                      <div className='grid gap-4 md:grid-cols-2'>
                        <FormField
                          control={form.control}
                          name='balanceProbe.enabled'
                          render={({ field }) => (
                            <FormItem className='flex items-center justify-between gap-4 rounded-lg border p-3'>
                              <div>
                                <FormLabel>{t('channels.dialogs.keys.balanceProbe.enabled.label')}</FormLabel>
                                <FormDescription>{t('channels.dialogs.keys.balanceProbe.enabled.description')}</FormDescription>
                              </div>
                              <FormControl>
                                <Switch checked={field.value} onCheckedChange={field.onChange} disabled={isPending} />
                              </FormControl>
                            </FormItem>
                          )}
                        />
                        <FormField
                          control={form.control}
                          name='balanceProbe.preset'
                          render={({ field }) => (
                            <FormItem>
                              <FormLabel>{t('channels.dialogs.keys.balanceProbe.preset.label')}</FormLabel>
                              <Select value={field.value} onValueChange={field.onChange} disabled={isPending}>
                                <FormControl>
                                  <SelectTrigger>
                                    <SelectValue />
                                  </SelectTrigger>
                                </FormControl>
                                <SelectContent>
                                  {BALANCE_PROBE_PRESETS.map((preset) => (
                                    <SelectItem key={preset} value={preset}>
                                      {t(`channels.dialogs.keys.balanceProbe.presets.${preset}`)}
                                    </SelectItem>
                                  ))}
                                </SelectContent>
                              </Select>
                              <FormDescription>{t('channels.dialogs.keys.balanceProbe.preset.description')}</FormDescription>
                            </FormItem>
                          )}
                        />
                        <FormField
                          control={form.control}
                          name='balanceProbe.primarySelection'
                          render={({ field }) => (
                            <FormItem>
                              <FormLabel>{t('channels.dialogs.keys.balanceProbe.primarySelection.label')}</FormLabel>
                              <Select value={field.value} onValueChange={field.onChange} disabled={isPending}>
                                <FormControl>
                                  <SelectTrigger>
                                    <SelectValue />
                                  </SelectTrigger>
                                </FormControl>
                                <SelectContent>
                                  <SelectItem value='highest_amount'>
                                    {t('channels.dialogs.keys.balanceProbe.primarySelection.highest_amount')}
                                  </SelectItem>
                                  <SelectItem value='preferred_currency'>
                                    {t('channels.dialogs.keys.balanceProbe.primarySelection.preferred_currency')}
                                  </SelectItem>
                                </SelectContent>
                              </Select>
                            </FormItem>
                          )}
                        />
                        <FormField
                          control={form.control}
                          name='balanceProbe.preferredCurrency'
                          render={({ field }) => (
                            <FormItem>
                              <FormLabel>{t('channels.dialogs.keys.balanceProbe.preferredCurrency.label')}</FormLabel>
                              <FormControl>
                                <Input
                                  value={field.value ?? ''}
                                  onChange={field.onChange}
                                  placeholder='USD'
                                  disabled={isPending || selectedBalanceProbePrimarySelection !== 'preferred_currency'}
                                />
                              </FormControl>
                              <FormDescription>{t('channels.dialogs.keys.balanceProbe.preferredCurrency.description')}</FormDescription>
                            </FormItem>
                          )}
                        />
                        <FormField
                          control={form.control}
                          name='balanceProbe.timeoutMs'
                          render={({ field }) => (
                            <FormItem>
                              <FormLabel>{t('channels.dialogs.keys.balanceProbe.timeoutMs.label')}</FormLabel>
                              <FormControl>
                                <Input
                                  ref={field.ref}
                                  name={field.name}
                                  type='number'
                                  min={100}
                                  max={30000}
                                  value={field.value}
                                  onBlur={field.onBlur}
                                  onChange={(event) => field.onChange(event.target.value)}
                                  disabled={isPending}
                                />
                              </FormControl>
                            </FormItem>
                          )}
                        />
                        <FormField
                          control={form.control}
                          name='balanceProbe.experimental'
                          render={({ field }) => (
                            <FormItem className='flex items-center justify-between gap-4 rounded-lg border p-3'>
                              <div>
                                <FormLabel>{t('channels.dialogs.keys.balanceProbe.experimental.label')}</FormLabel>
                                <FormDescription>{t('channels.dialogs.keys.balanceProbe.experimental.description')}</FormDescription>
                              </div>
                              <FormControl>
                                <Switch checked={field.value} onCheckedChange={field.onChange} disabled={isPending} />
                              </FormControl>
                            </FormItem>
                          )}
                        />
                      </div>
                      <FormField
                        control={form.control}
                        name='balanceProbe.includeStatuses'
                        render={({ field }) => (
                          <FormItem>
                            <FormLabel>{t('channels.dialogs.keys.balanceProbe.includeStatuses.label')}</FormLabel>
                            <div className='flex flex-wrap gap-3 rounded-lg border p-3'>
                              {KEY_STATUS_FILTERS.map((status) => (
                                <label key={status} className='flex items-center gap-2 text-sm'>
                                  <Checkbox
                                    checked={(field.value ?? []).includes(status)}
                                    onCheckedChange={(checked) =>
                                      field.onChange(
                                        checked ? [...(field.value ?? []), status] : (field.value ?? []).filter((item) => item !== status)
                                      )
                                    }
                                    disabled={isPending}
                                  />
                                  <span>{t(`channels.dialogs.keys.status.${status}`)}</span>
                                </label>
                              ))}
                            </div>
                            <FormDescription>{t('channels.dialogs.keys.balanceProbe.includeStatuses.description')}</FormDescription>
                          </FormItem>
                        )}
                      />
                    </CardContent>
                  </Card>

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
                                  type='text'
                                  autoComplete='off'
                                  autoCorrect='off'
                                  autoCapitalize='none'
                                  spellCheck={false}
                                  data-lpignore='true'
                                  data-1p-ignore='true'
                                  data-form-type='other'
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
                          } else if (channelHistory.length > 0) {
                            setChannelHistoryOpen(true);
                          }
                        }}
                        disabled={visibleInventory.length === 0 && channelHistory.length === 0}
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
                            count: inventoryHistoryCount + channelHistory.length,
                          })}
                        </Badge>
                      </button>

                      {channelHistory.length > 0 ? (
                        <button
                          type='button'
                          className='from-primary/5 via-background to-muted/40 hover:border-primary/40 flex w-full flex-col gap-3 rounded-xl border bg-gradient-to-r p-4 text-left transition sm:flex-row sm:items-center sm:justify-between'
                          onClick={() => setChannelHistoryOpen(true)}
                        >
                          <div className='flex items-start gap-3'>
                            <div className='bg-primary/10 text-primary rounded-lg p-2'>
                              <IconRoute className='h-5 w-5' />
                            </div>
                            <div>
                              <div className='font-medium'>{t('channels.dialogs.keys.channelHistory.title')}</div>
                              <div className='text-muted-foreground mt-1 text-sm'>
                                {t('channels.dialogs.keys.channelHistory.description')}
                              </div>
                            </div>
                          </div>
                          <Badge variant='outline'>
                            {t('channels.dialogs.keys.analytics.historyCount', { count: channelHistory.length })}
                          </Badge>
                        </button>
                      ) : null}

                      <div className='bg-muted/30 flex flex-col gap-3 rounded-md border px-3 py-2 lg:flex-row lg:items-center lg:justify-between'>
                        <div className='space-y-1'>
                          <div className='text-sm font-medium'>{t('channels.dialogs.keys.inventory.statusFilter.label')}</div>
                          <div className='text-muted-foreground text-sm'>
                            {statusFilter.size > 0
                              ? t('channels.dialogs.keys.inventory.statusFilter.summary', {
                                  count: visibleInventory.length,
                                  statuses: statusFilterSummary,
                                })
                              : t('channels.dialogs.keys.inventory.statusFilter.none')}
                          </div>
                        </div>
                        <div className='flex flex-wrap gap-3'>
                          {KEY_STATUS_FILTERS.map((status) => (
                            <label key={status} className='flex items-center gap-2 text-sm'>
                              <Checkbox
                                checked={statusFilter.has(status)}
                                onCheckedChange={(checked) => toggleStatusFilter(status, checked === true)}
                              />
                              <span>{t(`channels.dialogs.keys.status.${status}`)}</span>
                            </label>
                          ))}
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
                              <TableHead className='w-12'>
                                <Checkbox
                                  checked={allVisibleSelected ? true : someVisibleSelected ? 'indeterminate' : false}
                                  onCheckedChange={(checked) => toggleVisibleSelection(checked === true)}
                                  aria-label={t('channels.dialogs.keys.inventory.selectVisible')}
                                  disabled={visibleInventory.length === 0 || isPending}
                                />
                              </TableHead>
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
                                    : t('channels.dialogs.keys.inventory.statusFilter.empty')}
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
                                        {formatRowBalance(item, t, i18n.language)}
                                        {(item.balanceSnapshot?.available ?? item.available) != null ? (
                                          <Badge variant='outline' className='ml-2'>
                                            {t(
                                              `channels.dialogs.keys.availability.${(item.balanceSnapshot?.available ?? item.available) ? 'available' : 'unavailable'}`
                                            )}
                                          </Badge>
                                        ) : null}
                                      </div>
                                    </TableCell>
                                    <TableCell>
                                      <div className='flex justify-end gap-1'>
                                        <Button
                                          type='button'
                                          size='sm'
                                          variant='ghost'
                                          onClick={() => handleRunChecks([item.id])}
                                          disabled={isPending}
                                        >
                                          <IconPlayerPlay className='h-4 w-4' />
                                        </Button>
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

                  <Card>
                    <CardHeader>
                      <CardTitle>{t('channels.dialogs.keys.failureStrategy.title')}</CardTitle>
                      <CardDescription>{t('channels.dialogs.keys.failureStrategy.description')}</CardDescription>
                    </CardHeader>
                    <CardContent className='space-y-5'>
                      <FailurePolicyEditor form={form} disabled={isPending} />
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

                      {selectedStrategy === 'cache_affinity' ? (
                        <div className='grid gap-4 md:grid-cols-2'>
                          <FormField
                            control={form.control}
                            name='likelyAffinityTTLMinutes'
                            render={({ field }) => (
                              <FormItem>
                                <FormLabel>{t('channels.dialogs.keyRouting.fields.likelyAffinityTTLMinutes.label')}</FormLabel>
                                <FormControl>
                                  <Input
                                    type='number'
                                    min={1}
                                    max={1440}
                                    placeholder={String(DEFAULT_LIKELY_AFFINITY_TTL_MINUTES)}
                                    {...field}
                                  />
                                </FormControl>
                                <FormDescription>
                                  {t('channels.dialogs.keyRouting.fields.likelyAffinityTTLMinutes.description')}
                                </FormDescription>
                                <FormMessage />
                              </FormItem>
                            )}
                          />
                          <FormField
                            control={form.control}
                            name='exactAffinityTTLMinutes'
                            render={({ field }) => (
                              <FormItem>
                                <FormLabel>{t('channels.dialogs.keyRouting.fields.exactAffinityTTLMinutes.label')}</FormLabel>
                                <FormControl>
                                  <Input
                                    type='number'
                                    min={1}
                                    max={10080}
                                    placeholder={String(DEFAULT_EXACT_AFFINITY_TTL_MINUTES)}
                                    {...field}
                                  />
                                </FormControl>
                                <FormDescription>
                                  {t('channels.dialogs.keyRouting.fields.exactAffinityTTLMinutes.description')}
                                </FormDescription>
                                <FormMessage />
                              </FormItem>
                            )}
                          />
                        </div>
                      ) : null}

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
              </ScrollArea>
            </Tabs>
          </Form>

          <DialogFooter className='gap-2 sm:justify-between'>
            <div className='flex items-center gap-2'>
              <Button type='button' variant='outline' onClick={() => handleRunChecks()} disabled={isPending || inventory.length === 0}>
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
      <ChannelHistoryDialog history={channelHistory} open={channelHistoryOpen} onOpenChange={setChannelHistoryOpen} />
    </>
  );
}
