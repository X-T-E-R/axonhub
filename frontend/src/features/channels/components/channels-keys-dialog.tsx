'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import { z } from 'zod';
import { useForm, type Resolver, type UseFormReturn } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { useQueryClient } from '@tanstack/react-query';
import {
  IconArchive,
  IconAlertTriangle,
  IconChartLine,
  IconCircleCheck,
  IconCircleX,
  IconCopy,
  IconDotsVertical,
  IconEye,
  IconEyeOff,
  IconInfoCircle,
  IconKey,
  IconKeyOff,
  IconLoader2,
  IconPlayerPlay,
  IconRefresh,
  IconRestore,
  IconRoute,
  IconSearch,
  IconTrash,
} from '@tabler/icons-react';
import type { TFunction } from 'i18next';
import { useTranslation } from 'react-i18next';
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip as RechartsTooltip,
  XAxis,
  YAxis,
  type TooltipProps,
} from 'recharts';
import { toast } from 'sonner';
import { Alert, AlertDescription } from '@/components/ui/alert';
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { Form, FormControl, FormDescription, FormField, FormItem, FormLabel } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { useChannelSetting } from '@/features/system/data/system';
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
  ChannelKeyBalanceSnapshot,
  ChannelKeyHealthCheckHTTPRule,
  ChannelKeyHealthCheckHistoryEntry,
  ChannelAPIKeyHealthCheckMode,
  channelKeySelectionStrategySchema,
  type ChannelKeySelectionStrategy,
} from '../data/schema';
import { mergeChannelSettingsForUpdate } from '../utils/merge';
import {
  FailurePolicyEditor,
  failurePolicyFormSchema,
  failurePolicyFromValues,
  failurePolicyValuesFromLegacyHealth,
  failurePolicyValuesFromStored,
} from './channels-key-failure-policy-panel';
import { DEFAULT_EXACT_AFFINITY_TTL_MINUTES, DEFAULT_LIKELY_AFFINITY_TTL_MINUTES, KeyRoutingPanel } from './channels-key-routing-panel';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentRow: Channel;
}

type KeyInventoryStatus = 'active' | 'disabled' | 'archived';

interface KeyInventoryRow {
  id: string;
  rawKey: string;
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

const keysFormSchema = z.object({
  strategy: z.union([z.literal('inherit'), channelKeySelectionStrategySchema]),
  likelyAffinityTTLMinutes: z.coerce.number().int().min(1).max(1440),
  exactAffinityTTLMinutes: z.coerce.number().int().min(1).max(10080),
  balanceProbe: z.object({
    enabled: z.boolean(),
    preset: z.string().min(1),
    experimental: z.boolean(),
    preferredCurrency: z.string().optional(),
    primarySelection: z.enum(['auto_highest', 'preferred_currency']),
    timeoutMs: z.coerce.number().int().min(100).max(30000),
    sameAsChannelBaseURL: z.boolean(),
    baseURL: z.string().optional(),
    method: z.enum(['GET', 'POST', 'PUT', 'PATCH', 'DELETE']),
    path: z.string().optional(),
    expectedStatuses: z.string().optional(),
    keyInjectionLocation: z.enum(['authorization_bearer', 'header']),
    keyInjectionHeaderName: z.string().optional(),
  }),
  failurePolicy: failurePolicyFormSchema,
});

type KeysFormValues = z.output<typeof keysFormSchema>;
const keysFormResolver = zodResolver(keysFormSchema) as unknown as Resolver<KeysFormValues, unknown, KeysFormValues>;

const BALANCE_PROBE_PRESETS = [
  'deepseek_balance',
  'siliconflow_user_info',
  'moonshot_balance',
  'openrouter_credits',
  'nanogpt_check_balance',
] as const;
const CUSTOM_BALANCE_PROBE_PRESET = 'custom';
const BALANCE_PROBE_PRESET_OPTIONS = [...BALANCE_PROBE_PRESETS, CUSTOM_BALANCE_PROBE_PRESET] as const;
type BalanceProbePresetOption = (typeof BALANCE_PROBE_PRESET_OPTIONS)[number];
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
const BALANCE_PROBE_PRESET_HTTP_DEFAULTS: Record<
  BalanceProbePresetOption,
  Required<Pick<ChannelKeyHealthCheckHTTPRule, 'method' | 'urlMode'>> &
    Pick<ChannelKeyHealthCheckHTTPRule, 'path' | 'url' | 'expectedStatuses' | 'keyInjection'>
> = {
  deepseek_balance: {
    method: 'GET',
    urlMode: 'absolute_url',
    path: '/user/balance',
    url: 'https://api.deepseek.com/user/balance',
    expectedStatuses: [200],
    keyInjection: { location: 'authorization_bearer' },
  },
  siliconflow_user_info: {
    method: 'GET',
    urlMode: 'provider_base_url',
    path: '/user/info',
    url: null,
    expectedStatuses: [200],
    keyInjection: { location: 'authorization_bearer' },
  },
  moonshot_balance: {
    method: 'GET',
    urlMode: 'provider_base_url',
    path: '/users/me/balance',
    url: null,
    expectedStatuses: [200],
    keyInjection: { location: 'authorization_bearer' },
  },
  openrouter_credits: {
    method: 'GET',
    urlMode: 'provider_base_url',
    path: '/credits',
    url: null,
    expectedStatuses: [200],
    keyInjection: { location: 'authorization_bearer' },
  },
  nanogpt_check_balance: {
    method: 'POST',
    urlMode: 'absolute_url',
    path: '/api/check-balance',
    url: 'https://nano-gpt.com/api/check-balance',
    expectedStatuses: [200],
    keyInjection: { location: 'header', headerName: 'x-api-key' },
  },
  custom: {
    method: 'GET',
    urlMode: 'provider_base_url',
    path: '/balance',
    url: null,
    expectedStatuses: [200],
    keyInjection: { location: 'authorization_bearer' },
  },
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

const DEFAULT_BALANCE_PROBE: KeysFormValues['balanceProbe'] = {
  enabled: false,
  preset: 'deepseek_balance',
  experimental: false,
  preferredCurrency: '',
  primarySelection: 'auto_highest',
  timeoutMs: 10000,
  sameAsChannelBaseURL: true,
  baseURL: '',
  method: 'GET',
  path: '/user/balance',
  expectedStatuses: '200',
  keyInjectionLocation: 'authorization_bearer',
  keyInjectionHeaderName: '',
};

function positiveOrDefault(value: number | null | undefined, fallback: number): number {
  return typeof value === 'number' && value > 0 ? value : fallback;
}

function isBuiltInBalanceProbePreset(value: string | null | undefined): value is (typeof BALANCE_PROBE_PRESETS)[number] {
  return BALANCE_PROBE_PRESETS.includes(value as (typeof BALANCE_PROBE_PRESETS)[number]);
}

function balanceProbePresetHTTPDefaults(
  preset: string | null | undefined
): (typeof BALANCE_PROBE_PRESET_HTTP_DEFAULTS)[keyof typeof BALANCE_PROBE_PRESET_HTTP_DEFAULTS] {
  return preset === CUSTOM_BALANCE_PROBE_PRESET || isBuiltInBalanceProbePreset(preset)
    ? BALANCE_PROBE_PRESET_HTTP_DEFAULTS[preset]
    : BALANCE_PROBE_PRESET_HTTP_DEFAULTS.custom;
}

function parseURLOrNull(value: string | null | undefined): URL | null {
  if (!value) {
    return null;
  }

  try {
    return new URL(value);
  } catch {
    return null;
  }
}

function baseURLFromAbsoluteURL(url: string | null | undefined, fallback: string): string {
  const parsed = parseURLOrNull(url);
  if (!parsed) {
    return fallback;
  }

  parsed.pathname = parsed.pathname.replace(/\/[^/]*$/, '') || '/';
  parsed.search = '';
  parsed.hash = '';
  return parsed.toString().replace(/\/$/, '');
}

function baseURLFromAbsoluteURLAndPath(url: string | null | undefined, path: string | null | undefined, fallback: string): string {
  const parsed = parseURLOrNull(url);
  const trimmedPath = path?.trim();
  if (!parsed || !trimmedPath) {
    return baseURLFromAbsoluteURL(url, fallback);
  }

  const normalizedPath = `/${trimmedPath.replace(/^\//, '')}`;
  if (parsed.pathname.endsWith(normalizedPath)) {
    parsed.pathname = parsed.pathname.slice(0, -normalizedPath.length) || '/';
    parsed.search = '';
    parsed.hash = '';
    return parsed.toString().replace(/\/$/, '');
  }

  return baseURLFromAbsoluteURL(url, fallback);
}

function pathFromAbsoluteURL(url: string | null | undefined, fallback: string): string {
  const parsed = parseURLOrNull(url);
  if (!parsed) {
    return fallback;
  }

  return `${parsed.pathname || '/'}${parsed.search}`;
}

function composeAbsoluteProbeURL(baseURL: string | null | undefined, path: string | null | undefined): string | null {
  const base = parseURLOrNull(baseURL);
  const trimmedPath = path?.trim();
  if (!base || !trimmedPath) {
    return null;
  }

  base.pathname = `${base.pathname.replace(/\/$/, '')}/${trimmedPath.replace(/^\//, '')}`;
  base.search = '';
  base.hash = '';
  return base.toString();
}

function balanceProbeHTTPValuesFromStored(
  probe: ChannelBalanceProbe | null | undefined,
  preset: BalanceProbePresetOption,
  channelBaseURL: string
): Pick<
  KeysFormValues['balanceProbe'],
  'sameAsChannelBaseURL' | 'baseURL' | 'method' | 'path' | 'expectedStatuses' | 'keyInjectionLocation' | 'keyInjectionHeaderName'
> {
  const presetDefaults = balanceProbePresetHTTPDefaults(preset);
  const http = probe?.http;
  const method = http?.method ?? presetDefaults.method;
  const sameAsChannelBaseURL = http?.urlMode ? http.urlMode !== 'absolute_url' : presetDefaults.urlMode !== 'absolute_url';
  const fallbackPath = presetDefaults.path ?? pathFromAbsoluteURL(presetDefaults.url, '');
  const path = http?.urlMode === 'absolute_url' ? pathFromAbsoluteURL(http.url, fallbackPath) : (http?.path ?? fallbackPath);
  const baseURL =
    http?.urlMode === 'absolute_url'
      ? baseURLFromAbsoluteURLAndPath(http.url, path, channelBaseURL || baseURLFromAbsoluteURLAndPath(presetDefaults.url, fallbackPath, ''))
      : channelBaseURL || baseURLFromAbsoluteURLAndPath(presetDefaults.url, fallbackPath, '');
  const expectedStatuses = (http?.expectedStatuses ?? presetDefaults.expectedStatuses ?? [200]).join(', ');
  const keyInjection = http?.keyInjection ?? presetDefaults.keyInjection ?? { location: 'authorization_bearer' as const };

  return {
    sameAsChannelBaseURL,
    baseURL,
    method,
    path,
    expectedStatuses,
    keyInjectionLocation: keyInjection.location ?? 'authorization_bearer',
    keyInjectionHeaderName: keyInjection.headerName ?? '',
  };
}

function defaultBalanceProbeForChannel(type: Channel['type']): KeysFormValues['balanceProbe'] {
  const preset = BALANCE_PROBE_PRESET_BY_CHANNEL_TYPE[type] ?? DEFAULT_BALANCE_PROBE.preset;
  return {
    ...DEFAULT_BALANCE_PROBE,
    preset,
    ...balanceProbeHTTPValuesFromStored(null, preset, ''),
  };
}

function channelSupportsManualBalanceProbe(channel: Channel): boolean {
  const probe = channel.settings?.balanceProbe;
  if (probe?.preset === CUSTOM_BALANCE_PROBE_PRESET) {
    return !!probe.http;
  }
  if (probe?.preset && isBuiltInBalanceProbePreset(probe.preset)) {
    return true;
  }
  if (probe?.preset) {
    return false;
  }
  if (probe?.http && !probe?.preset) {
    return true;
  }

  return BALANCE_PROBE_PRESET_BY_CHANNEL_TYPE[channel.type] != null;
}

function parseStatusList(input: string): number[] {
  return input
    .split(',')
    .map((item) => Number(item.trim()))
    .filter((item) => Number.isInteger(item) && item >= 100 && item <= 599);
}

function valuesFromChannel(currentRow: Channel): KeysFormValues {
  const health = currentRow.settings?.keyHealthCheck;
  const balanceProbe = currentRow.settings?.balanceProbe;
  const defaultBalanceProbe = defaultBalanceProbeForChannel(currentRow.type);
  const balanceProbePreset: BalanceProbePresetOption =
    balanceProbe?.preset === CUSTOM_BALANCE_PROBE_PRESET
      ? CUSTOM_BALANCE_PROBE_PRESET
      : balanceProbe?.preset && isBuiltInBalanceProbePreset(balanceProbe.preset)
        ? balanceProbe.preset
        : balanceProbe?.http
          ? CUSTOM_BALANCE_PROBE_PRESET
          : defaultBalanceProbe.preset;
  return {
    strategy: currentRow.settings?.keySelection?.strategy ?? 'inherit',
    likelyAffinityTTLMinutes: currentRow.settings?.keySelection?.likelyAffinityTTLMinutes ?? DEFAULT_LIKELY_AFFINITY_TTL_MINUTES,
    exactAffinityTTLMinutes: currentRow.settings?.keySelection?.exactAffinityTTLMinutes ?? DEFAULT_EXACT_AFFINITY_TTL_MINUTES,
    balanceProbe: {
      enabled: balanceProbe?.enabled ?? defaultBalanceProbe.enabled,
      preset: balanceProbePreset,
      experimental: balanceProbe?.experimental ?? defaultBalanceProbe.experimental,
      preferredCurrency: balanceProbe?.preferredCurrency ?? defaultBalanceProbe.preferredCurrency,
      primarySelection:
        balanceProbe?.primarySelection === 'preferred_currency' ? 'preferred_currency' : defaultBalanceProbe.primarySelection,
      timeoutMs: positiveOrDefault(balanceProbe?.timeoutMs, defaultBalanceProbe.timeoutMs),
      ...balanceProbeHTTPValuesFromStored(balanceProbe, balanceProbePreset, currentRow.baseURL ?? ''),
    },
    failurePolicy: currentRow.settings?.failurePolicy
      ? failurePolicyValuesFromStored(currentRow.settings.failurePolicy)
      : failurePolicyValuesFromLegacyHealth(health),
  };
}

function balanceProbeFromValues(values: KeysFormValues, existing?: ChannelBalanceProbe | null): ChannelBalanceProbe {
  const keyInjection =
    values.balanceProbe.keyInjectionLocation === 'header'
      ? {
          location: 'header' as const,
          headerName: values.balanceProbe.keyInjectionHeaderName?.trim() || 'Authorization',
        }
      : {
          location: 'authorization_bearer' as const,
          headerName: null,
        };
  const path = values.balanceProbe.path?.trim() || balanceProbePresetHTTPDefaults(values.balanceProbe.preset).path || '';
  const http: ChannelKeyHealthCheckHTTPRule | null = values.balanceProbe.sameAsChannelBaseURL
    ? {
        method: values.balanceProbe.method,
        urlMode: 'provider_base_url',
        path,
        url: null,
        timeoutMs: values.balanceProbe.timeoutMs,
        headers: [],
        keyInjection,
        expectedStatuses: parseStatusList(values.balanceProbe.expectedStatuses || '200'),
        passWhen: null,
      }
    : {
        method: values.balanceProbe.method,
        urlMode: 'absolute_url',
        path: null,
        url: composeAbsoluteProbeURL(values.balanceProbe.baseURL, path),
        timeoutMs: values.balanceProbe.timeoutMs,
        headers: [],
        keyInjection,
        expectedStatuses: parseStatusList(values.balanceProbe.expectedStatuses || '200'),
        passWhen: null,
      };

  return {
    enabled: values.balanceProbe.enabled,
    preset: values.balanceProbe.preset,
    experimental: values.balanceProbe.experimental,
    preferredCurrency: values.balanceProbe.preferredCurrency?.trim() || null,
    primarySelection: values.balanceProbe.primarySelection,
    timeoutMs: values.balanceProbe.timeoutMs,
    http: http.urlMode === 'absolute_url' && !http.url ? (existing?.http ?? null) : http,
  };
}

function formatDateTime(value?: string | null, language?: string): string {
  if (!value) {
    return '-';
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return new Intl.DateTimeFormat(language || undefined, {
    dateStyle: 'medium',
    timeStyle: 'medium',
  }).format(date);
}

function inventoryFromBackend(items: ChannelAPIKeyInventoryItem[] = []): KeyInventoryRow[] {
  return items.map((item) => ({
    id: item.id,
    rawKey: item.rawKey ?? '',
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

type KeyHistoryListEntry = {
  key: KeyInventoryRow;
  entry: ChannelKeyHealthCheckHistoryEntry;
};

function isRequestFailureHistory(entry: ChannelKeyHealthCheckHistoryEntry): boolean {
  return entry.trigger === 'request' || !!entry.matchedPolicy || !!entry.action;
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
            <RechartsTooltip content={<KeyHistoryTooltip />} />
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
              <RechartsTooltip content={<KeyHistoryTooltip />} />
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

function BalanceProbeEditor({
  form,
  disabled,
  channelBaseURL,
}: {
  form: UseFormReturn<KeysFormValues, unknown, KeysFormValues>;
  disabled: boolean;
  channelBaseURL: string;
}) {
  const { t } = useTranslation();
  const selectedPrimarySelection = form.watch('balanceProbe.primarySelection');
  const sameAsChannelBaseURL = form.watch('balanceProbe.sameAsChannelBaseURL');
  const keyInjectionLocation = form.watch('balanceProbe.keyInjectionLocation');

  const applyPresetDefaults = (preset: string) => {
    form.setValue('balanceProbe.preset', preset, { shouldDirty: true, shouldValidate: true });
    const defaults = balanceProbePresetHTTPDefaults(preset);
    const fallbackPath = defaults.path ?? pathFromAbsoluteURL(defaults.url, '');
    const defaultSameAsChannelBaseURL = defaults.urlMode !== 'absolute_url';

    form.setValue('balanceProbe.sameAsChannelBaseURL', defaultSameAsChannelBaseURL, { shouldDirty: true, shouldValidate: true });
    form.setValue('balanceProbe.method', defaults.method, { shouldDirty: true, shouldValidate: true });
    form.setValue('balanceProbe.path', fallbackPath, { shouldDirty: true, shouldValidate: true });
    form.setValue('balanceProbe.expectedStatuses', (defaults.expectedStatuses ?? [200]).join(', '), {
      shouldDirty: true,
      shouldValidate: true,
    });
    form.setValue('balanceProbe.keyInjectionLocation', defaults.keyInjection?.location ?? 'authorization_bearer', {
      shouldDirty: true,
      shouldValidate: true,
    });
    form.setValue('balanceProbe.keyInjectionHeaderName', defaults.keyInjection?.headerName ?? '', {
      shouldDirty: true,
      shouldValidate: true,
    });

    form.setValue(
      'balanceProbe.baseURL',
      defaultSameAsChannelBaseURL ? channelBaseURL : baseURLFromAbsoluteURLAndPath(defaults.url, fallbackPath, channelBaseURL),
      {
        shouldDirty: true,
        shouldValidate: true,
      }
    );
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('channels.dialogs.keys.balanceProbe.title')}</CardTitle>
        <CardDescription>{t('channels.dialogs.keys.balanceProbe.description')}</CardDescription>
      </CardHeader>
      <CardContent className='space-y-5'>
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
                  <Switch checked={field.value} onCheckedChange={field.onChange} disabled={disabled} />
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
                  <Switch checked={field.value} onCheckedChange={field.onChange} disabled={disabled} />
                </FormControl>
              </FormItem>
            )}
          />
        </div>

        <div className='grid gap-4 md:grid-cols-2'>
          <FormField
            control={form.control}
            name='balanceProbe.preset'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('channels.dialogs.keys.balanceProbe.preset.label')}</FormLabel>
                <Select value={field.value} onValueChange={applyPresetDefaults} disabled={disabled}>
                  <FormControl>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                  </FormControl>
                  <SelectContent>
                    {BALANCE_PROBE_PRESET_OPTIONS.map((preset) => (
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
            name='balanceProbe.sameAsChannelBaseURL'
            render={({ field }) => (
              <FormItem className='flex items-center justify-between gap-4 rounded-lg border p-3'>
                <div>
                  <FormLabel>{t('channels.dialogs.keys.balanceProbe.sameAsChannelBaseURL.label')}</FormLabel>
                  <FormDescription>
                    {t('channels.dialogs.keys.balanceProbe.sameAsChannelBaseURL.description', { baseURL: channelBaseURL || '-' })}
                  </FormDescription>
                </div>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={(checked) => {
                      field.onChange(checked);
                      if (checked) {
                        form.setValue('balanceProbe.baseURL', channelBaseURL, { shouldDirty: true, shouldValidate: true });
                      }
                    }}
                    disabled={disabled}
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='balanceProbe.baseURL'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('channels.dialogs.keys.balanceProbe.baseURL.label')}</FormLabel>
                <FormControl>
                  <Input
                    value={field.value ?? ''}
                    onChange={field.onChange}
                    placeholder='https://api.provider.example'
                    disabled={disabled || sameAsChannelBaseURL}
                  />
                </FormControl>
                <FormDescription>{t('channels.dialogs.keys.balanceProbe.baseURL.description')}</FormDescription>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='balanceProbe.path'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('channels.dialogs.keys.balanceProbe.path.label')}</FormLabel>
                <FormControl>
                  <Input value={field.value ?? ''} onChange={field.onChange} placeholder='/credits' disabled={disabled} />
                </FormControl>
                <FormDescription>{t('channels.dialogs.keys.balanceProbe.path.description')}</FormDescription>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='balanceProbe.method'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('channels.dialogs.keys.balanceProbe.method.label')}</FormLabel>
                <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                  <FormControl>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                  </FormControl>
                  <SelectContent>
                    {(['GET', 'POST', 'PUT', 'PATCH', 'DELETE'] as const).map((method) => (
                      <SelectItem key={method} value={method}>
                        {method}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='balanceProbe.expectedStatuses'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('channels.dialogs.keys.balanceProbe.expectedStatuses.label')}</FormLabel>
                <FormControl>
                  <Input value={field.value ?? ''} onChange={field.onChange} placeholder='200' disabled={disabled} />
                </FormControl>
                <FormDescription>{t('channels.dialogs.keys.balanceProbe.expectedStatuses.description')}</FormDescription>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='balanceProbe.keyInjectionLocation'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('channels.dialogs.keys.balanceProbe.keyInjection.label')}</FormLabel>
                <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                  <FormControl>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                  </FormControl>
                  <SelectContent>
                    <SelectItem value='authorization_bearer'>
                      {t('channels.dialogs.keys.balanceProbe.keyInjection.authorization_bearer')}
                    </SelectItem>
                    <SelectItem value='header'>{t('channels.dialogs.keys.balanceProbe.keyInjection.header')}</SelectItem>
                  </SelectContent>
                </Select>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='balanceProbe.keyInjectionHeaderName'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('channels.dialogs.keys.balanceProbe.keyInjection.headerName')}</FormLabel>
                <FormControl>
                  <Input
                    value={field.value ?? ''}
                    onChange={field.onChange}
                    placeholder='x-api-key'
                    disabled={disabled || keyInjectionLocation !== 'header'}
                  />
                </FormControl>
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='balanceProbe.primarySelection'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('channels.dialogs.keys.balanceProbe.primarySelection.label')}</FormLabel>
                <Select value={field.value} onValueChange={field.onChange} disabled={disabled}>
                  <FormControl>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                  </FormControl>
                  <SelectContent>
                    <SelectItem value='auto_highest'>{t('channels.dialogs.keys.balanceProbe.primarySelection.auto_highest')}</SelectItem>
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
                    disabled={disabled || selectedPrimarySelection !== 'preferred_currency'}
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
                    disabled={disabled}
                  />
                </FormControl>
              </FormItem>
            )}
          />
        </div>

        <Alert>
          <IconAlertTriangle className='h-4 w-4' />
          <AlertDescription>{t('channels.dialogs.keys.balanceProbe.monitoringOwnership')}</AlertDescription>
        </Alert>
      </CardContent>
    </Card>
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
  const copyRawKey = async () => {
    if (!row.rawKey) {
      return;
    }
    try {
      await navigator.clipboard.writeText(row.rawKey);
      toast.success(t('channels.dialogs.keys.messages.copied', { count: 1 }));
    } catch {
      toast.error(t('common.errors.copyFailed'));
    }
  };

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
            <div className='rounded-lg border p-3 md:col-span-2'>
              <div className='text-muted-foreground text-xs'>
                {row.rawKey ? t('channels.dialogs.keys.details.rawKey') : t('channels.dialogs.keys.details.maskedKey')}
              </div>
              <div className='mt-1 flex min-w-0 items-start gap-2'>
                <code className='bg-muted min-w-0 flex-1 overflow-x-auto rounded px-2 py-1 font-mono text-sm whitespace-nowrap'>
                  {row.rawKey || row.maskedKey}
                </code>
                {row.rawKey ? (
                  <Button
                    type='button'
                    size='icon'
                    variant='outline'
                    className='shrink-0'
                    onClick={copyRawKey}
                    aria-label={t('channels.dialogs.keys.actions.copyKey')}
                  >
                    <IconCopy className='h-4 w-4' aria-hidden='true' />
                  </Button>
                ) : null}
              </div>
              {row.rawKey ? <div className='text-muted-foreground mt-2 font-mono text-xs'>{row.maskedKey}</div> : null}
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
              <div className='text-muted-foreground mt-1 text-xs'>{formatDateTime(row.lastCheckedAt, i18n.language)}</div>
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
              <div className='mt-1 text-sm font-medium'>{formatDateTime(row.nextCheckAt, i18n.language)}</div>
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
                        <div className='text-muted-foreground text-xs'>{formatDateTime(entry.checkedAt, i18n.language)}</div>
                        <div className='text-sm'>{entry.reason || '-'}</div>
                        <div className='text-muted-foreground text-xs'>
                          {formatRowBalance(entry, t, i18n.language)}
                          {(entry.balanceSnapshot?.available ?? entry.available) != null
                            ? ` · ${t(`channels.dialogs.keys.availability.${(entry.balanceSnapshot?.available ?? entry.available) ? 'available' : 'unavailable'}`)}`
                            : ''}
                          {entry.action ? ` · ${formatPolicyAction(entry.action)}` : ''}
                          {entry.nextCheckAt
                            ? ` · ${t('channels.dialogs.keys.details.nextCheckAt')}: ${formatDateTime(entry.nextCheckAt, i18n.language)}`
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
  const { t, i18n } = useTranslation();
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
              <div className='text-muted-foreground mt-1 text-xs'>{formatDateTime(latest?.checkedAt, i18n.language)}</div>
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
                        <div className='text-muted-foreground text-xs'>{formatDateTime(entry.checkedAt, i18n.language)}</div>
                        <div className='text-sm'>{entry.reason || '-'}</div>
                        <div className='text-muted-foreground text-xs'>
                          {entry.action ? formatPolicyAction(entry.action) : '-'}
                          {entry.nextCheckAt
                            ? ` · ${t('channels.dialogs.keys.details.nextCheckAt')}: ${formatDateTime(entry.nextCheckAt, i18n.language)}`
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
  const queryClient = useQueryClient();
  const resetContext = useRef({ open: false, channelID: currentRow.id });
  const [newKey, setNewKey] = useState('');
  const [searchQuery, setSearchQuery] = useState('');
  const [selectedKeys, setSelectedKeys] = useState<Set<string>>(new Set());
  const [revealedKeys, setRevealedKeys] = useState<Set<string>>(new Set());
  const [statusFilter, setStatusFilter] = useState<Set<KeyInventoryStatus>>(new Set(DEFAULT_KEY_STATUS_FILTERS));
  const [detailsKeyID, setDetailsKeyID] = useState<string | null>(null);
  const [channelHistoryOpen, setChannelHistoryOpen] = useState(false);
  const [confirmDeleteKey, setConfirmDeleteKey] = useState<string | null>(null);
  const [confirmBatchDelete, setConfirmBatchDelete] = useState(false);
  const [confirmDiscardSettings, setConfirmDiscardSettings] = useState(false);

  const keyInventory = useChannelAPIKeyInventory(currentRow.id, { enabled: open });
  const { data: channelSetting } = useChannelSetting();
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
    const shouldReset = open && (!resetContext.current.open || resetContext.current.channelID !== currentRow.id);
    resetContext.current = { open, channelID: currentRow.id };
    if (!shouldReset) {
      return;
    }

    form.reset(valuesFromChannel(currentRow));
    setNewKey('');
    setSearchQuery('');
    setSelectedKeys(new Set());
    setRevealedKeys(new Set());
    setStatusFilter(new Set(DEFAULT_KEY_STATUS_FILTERS));
    setDetailsKeyID(null);
    setChannelHistoryOpen(false);
    setConfirmDeleteKey(null);
    setConfirmBatchDelete(false);
    setConfirmDiscardSettings(false);
  }, [open, currentRow, form]);

  const inventory = useMemo(() => inventoryFromBackend(keyInventory.data), [keyInventory.data]);
  const activeKeys = useMemo(() => inventory.filter((item) => item.status === 'active'), [inventory]);
  const disabledKeys = useMemo(() => inventory.filter((item) => item.status === 'disabled'), [inventory]);
  const archivedKeys = useMemo(() => inventory.filter((item) => item.status === 'archived'), [inventory]);
  const channelHistory = useMemo(() => currentRow.settings?.keyHealthCheck?.history ?? [], [currentRow.settings?.keyHealthCheck?.history]);
  const activeBalanceSummary = useMemo(() => summarizeActiveBalances(activeKeys, t, i18n.language), [activeKeys, i18n.language, t]);
  const visibleInventory = useMemo(() => {
    const query = searchQuery.trim().toLocaleLowerCase();
    return inventory.filter(
      (item) =>
        statusFilter.has(item.status) &&
        (!query ||
          item.id.toLocaleLowerCase().includes(query) ||
          item.maskedKey.toLocaleLowerCase().includes(query) ||
          item.rawKey.toLocaleLowerCase().includes(query))
    );
  }, [inventory, searchQuery, statusFilter]);
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
  const selectedRawKeys = useMemo(() => selectedRows.map((item) => item.rawKey).filter(Boolean), [selectedRows]);
  const detailsRow = useMemo(() => inventory.find((item) => item.id === detailsKeyID) ?? null, [detailsKeyID, inventory]);
  const keyHealthHistory = useMemo<KeyHistoryListEntry[]>(
    () =>
      visibleInventory.flatMap((item) =>
        (item.history ?? [])
          .filter((entry) => !isRequestFailureHistory(entry))
          .map((entry) => ({
            key: item,
            entry,
          }))
      ),
    [visibleInventory]
  );
  const requestFailureHistory = useMemo<KeyHistoryListEntry[]>(
    () =>
      inventory.flatMap((item) =>
        (item.history ?? []).filter(isRequestFailureHistory).map((entry) => ({
          key: item,
          entry,
        }))
      ),
    [inventory]
  );
  const channelFailureHistory = useMemo(() => channelHistory.filter(isRequestFailureHistory), [channelHistory]);
  const canRunBalanceProbe = useMemo(() => channelSupportsManualBalanceProbe(currentRow), [currentRow]);
  const globalRoutingStrategy = channelSetting?.routing?.strategy ?? 'trace_sticky';
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
  const hasUnsavedChanges = form.formState.isDirty || newKey.trim().length > 0;

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

  const toggleKeyReveal = (id: string) => {
    setRevealedKeys((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  const copyKeys = async (keys: string[]) => {
    if (keys.length === 0) {
      return;
    }
    try {
      await navigator.clipboard.writeText(keys.join('\n'));
      toast.success(t('channels.dialogs.keys.messages.copied', { count: keys.length }));
    } catch {
      toast.error(t('common.errors.copyFailed'));
    }
  };

  const requestOpenChange = (nextOpen: boolean) => {
    if (!nextOpen && hasUnsavedChanges) {
      setConfirmDiscardSettings(true);
      return;
    }
    onOpenChange(nextOpen);
  };

  const saveSettings = async (values: KeysFormValues) => {
    const nextSettings = mergeChannelSettingsForUpdate(currentRow.settings, {
      keySelection:
        values.strategy === 'inherit'
          ? null
          : {
              strategy: values.strategy,
              likelyAffinityTTLMinutes: values.likelyAffinityTTLMinutes,
              exactAffinityTTLMinutes: values.exactAffinityTTLMinutes,
            },
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
      form.reset(values);
      onOpenChange(false);
    } catch {
      // Error handled by hook.
    }
  };

  const handleAddKey = async () => {
    const key = newKey.trim();
    if (!key) {
      return;
    }

    try {
      await addAPIKey.mutateAsync({ channelID: currentRow.id, key });
      setNewKey('');
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

  const handleRunChecks = async (keyIDs?: string[], mode: ChannelAPIKeyHealthCheckMode = 'real_request') => {
    try {
      await runHealthCheck.mutateAsync({
        channelID: currentRow.id,
        keyIDs,
        mode,
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
      await handleRunChecks(keyIDs, 'real_request');
      return;
    }

    const succeeded = new Set<string>();
    const failed = new Set<string>();
    for (const keyID of keyIDs) {
      try {
        if (action === 'disable') {
          await disableAPIKey.mutateAsync({ channelID: currentRow.id, key: keyID, silent: true, deferRefresh: true });
        } else if (action === 'enable') {
          await enableAPIKey.mutateAsync({ channelID: currentRow.id, key: keyID, silent: true, deferRefresh: true });
        } else if (action === 'archive') {
          await archiveAPIKey.mutateAsync({
            channelID: currentRow.id,
            keyID,
            reason: 'Manually archived by user',
            silent: true,
            deferRefresh: true,
          });
        } else if (action === 'restore') {
          await restoreAPIKey.mutateAsync({ channelID: currentRow.id, keyID, silent: true, deferRefresh: true });
        } else {
          const result = await deleteAPIKey.mutateAsync({
            channelID: currentRow.id,
            keyID,
            silent: true,
            deferRefresh: true,
          });
          if (result.message === 'ONE_KEY_PRESERVED') {
            failed.add(keyID);
            continue;
          }
        }
        succeeded.add(keyID);
      } catch {
        failed.add(keyID);
      }
    }

    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ['channelAPIKeyInventory', currentRow.id] }),
      queryClient.invalidateQueries({ queryKey: ['channelDisabledAPIKeys', currentRow.id] }),
      queryClient.invalidateQueries({ queryKey: ['channels'] }),
    ]);
    setSelectedKeys((previous) => new Set([...previous].filter((id) => !succeeded.has(id))));
    setConfirmBatchDelete(false);
    if (failed.size === 0) {
      toast.success(t('channels.dialogs.keys.messages.batchComplete', { count: succeeded.size }));
    } else {
      toast.error(
        t('channels.dialogs.keys.messages.batchPartial', {
          success: succeeded.size,
          failed: failed.size,
        })
      );
    }
  };

  return (
    <>
      <Dialog open={open} onOpenChange={requestOpenChange}>
        <DialogContent className='flex max-h-[92dvh] w-[calc(100%-1rem)] max-w-[calc(100%-1rem)] flex-col overflow-hidden sm:max-w-5xl'>
          <DialogHeader className='text-left'>
            <DialogTitle className='flex items-center gap-2'>
              <IconKey className='h-5 w-5' />
              {t('channels.dialogs.keys.title')}
            </DialogTitle>
            <DialogDescription>{t('channels.dialogs.keys.description', { name: currentRow.name })}</DialogDescription>
          </DialogHeader>

          <Form {...form}>
            <Tabs defaultValue='inventory' className='min-h-0 min-w-0 flex-1 overflow-hidden'>
              <div className='w-full max-w-full overflow-x-auto overflow-y-hidden py-1'>
                <TabsList className='inline-flex h-10 min-w-max justify-start'>
                  <TabsTrigger className='shrink-0' value='inventory'>
                    {t('channels.dialogs.keys.tabs.inventory')}
                  </TabsTrigger>
                  <TabsTrigger className='shrink-0' value='keyHistory'>
                    {t('channels.dialogs.keys.tabs.keyHistory')}
                  </TabsTrigger>
                  <TabsTrigger className='shrink-0' value='failureHistory'>
                    {t('channels.dialogs.keys.tabs.failureHistory')}
                  </TabsTrigger>
                  <TabsTrigger className='shrink-0' value='routing'>
                    {t('channels.dialogs.keys.tabs.routing')}
                  </TabsTrigger>
                  <TabsTrigger className='shrink-0' value='balanceProbe'>
                    {t('channels.dialogs.keys.tabs.balanceProbe')}
                  </TabsTrigger>
                  <TabsTrigger className='shrink-0' value='failurePolicy'>
                    {t('channels.dialogs.keys.tabs.failurePolicy')}
                  </TabsTrigger>
                </TabsList>
              </div>

              <div className='mt-3 h-[min(64dvh,42rem)] w-full max-w-full min-w-0 overflow-x-hidden overflow-y-auto pr-3'>
                <TabsContent value='inventory' className='mt-0 min-w-0 space-y-4'>
                  {keyInventory.isPending ? (
                    <div className='text-muted-foreground flex h-40 items-center justify-center gap-2 text-sm' aria-live='polite'>
                      <IconLoader2 className='h-4 w-4 animate-spin' />
                      {t('channels.dialogs.keys.inventory.loading')}
                    </div>
                  ) : keyInventory.isError ? (
                    <Alert variant='destructive'>
                      <IconAlertTriangle className='h-4 w-4' />
                      <AlertDescription className='flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between'>
                        <span>{t('channels.dialogs.keys.inventory.loadError')}</span>
                        <Button
                          type='button'
                          size='sm'
                          variant='outline'
                          onClick={() => keyInventory.refetch()}
                          disabled={keyInventory.isFetching}
                        >
                          {keyInventory.isFetching ? <IconLoader2 className='mr-2 h-4 w-4 animate-spin' /> : null}
                          {t('channels.dialogs.keys.actions.retry')}
                        </Button>
                      </AlertDescription>
                    </Alert>
                  ) : (
                    <>
                      <div className='grid grid-cols-2 overflow-hidden rounded-lg border lg:grid-cols-4 lg:divide-x'>
                        <div className='border-r border-b px-3 py-3 lg:border-b-0 lg:px-4'>
                          <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.summary.active')}</div>
                          <div className='mt-1 font-mono text-xl font-semibold tabular-nums'>{activeKeys.length}</div>
                        </div>
                        <div className='border-b px-3 py-3 lg:border-b-0 lg:px-4'>
                          <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.summary.disabled')}</div>
                          <div className='mt-1 font-mono text-xl font-semibold tabular-nums'>{disabledKeys.length}</div>
                        </div>
                        <div className='border-r px-3 py-3 lg:px-4'>
                          <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.summary.archived')}</div>
                          <div className='mt-1 font-mono text-xl font-semibold tabular-nums'>{archivedKeys.length}</div>
                        </div>
                        <div className='px-3 py-3 lg:px-4'>
                          <div className='text-muted-foreground text-xs'>{t('channels.dialogs.keys.summary.activeBalance')}</div>
                          <div className='mt-1 font-mono text-xl font-semibold tabular-nums'>{activeBalanceSummary?.display ?? '-'}</div>
                          <div className='text-muted-foreground mt-1 text-xs'>
                            {activeBalanceSummary
                              ? t('channels.dialogs.keys.summary.activeBalanceKeys', { count: activeBalanceSummary.keyCount })
                              : t('channels.dialogs.keys.summary.activeBalanceEmpty')}
                          </div>
                        </div>
                      </div>

                      <Card className='min-w-0 overflow-hidden'>
                        <CardHeader className='min-w-0'>
                          <CardTitle className='flex items-center gap-2'>
                            <IconKey className='h-5 w-5' />
                            {t('channels.dialogs.keys.inventory.title')}
                          </CardTitle>
                          <CardDescription className='break-words'>{t('channels.dialogs.keys.inventory.description')}</CardDescription>
                        </CardHeader>
                        <CardContent className='min-w-0 space-y-4'>
                          <div className='flex flex-col gap-2 sm:flex-row sm:items-start'>
                            <div className='flex-1 space-y-2'>
                              <label className='text-sm font-medium' htmlFor='channel-new-api-key'>
                                {t('channels.dialogs.keys.fields.newKey.label')}
                              </label>
                              <Input
                                id='channel-new-api-key'
                                name='channel-new-api-key'
                                type='text'
                                autoComplete='off'
                                autoCorrect='off'
                                autoCapitalize='none'
                                spellCheck={false}
                                data-lpignore='true'
                                data-1p-ignore='true'
                                data-form-type='other'
                                placeholder={t('channels.dialogs.keys.fields.newKey.placeholder')}
                                value={newKey}
                                onChange={(event) => setNewKey(event.target.value)}
                                onKeyDown={(event) => {
                                  if (event.key === 'Enter') {
                                    event.preventDefault();
                                    handleAddKey();
                                  }
                                }}
                              />
                              <p className='text-muted-foreground text-sm'>{t('channels.dialogs.keys.fields.newKey.description')}</p>
                            </div>
                            <Button type='button' className='sm:mt-7' onClick={handleAddKey} disabled={isPending || !newKey.trim()}>
                              {t('channels.dialogs.keys.actions.add')}
                            </Button>
                          </div>

                          <Alert>
                            <IconAlertTriangle className='h-4 w-4' />
                            <AlertDescription>{t('channels.dialogs.keys.inventory.statusCopy')}</AlertDescription>
                          </Alert>

                          <div className='bg-muted/30 flex flex-col gap-3 rounded-md border px-3 py-3 lg:flex-row lg:items-center lg:justify-between'>
                            <div className='space-y-1'>
                              <div className='text-sm font-medium'>{t('channels.dialogs.keys.manualTest.title')}</div>
                              <div className='text-muted-foreground text-sm'>{t('channels.dialogs.keys.manualTest.description')}</div>
                            </div>
                            <div className='flex flex-wrap gap-2'>
                              <Button
                                type='button'
                                variant='outline'
                                onClick={() => handleRunChecks(undefined, 'real_request')}
                                disabled={isPending || inventory.length === 0}
                              >
                                <IconPlayerPlay className='mr-2 h-4 w-4' />
                                {t('channels.dialogs.keys.manualTest.modes.real_request.label')}
                              </Button>
                              {canRunBalanceProbe ? (
                                <Button
                                  type='button'
                                  variant='outline'
                                  onClick={() => handleRunChecks(undefined, 'balance_probe')}
                                  disabled={isPending || inventory.length === 0}
                                >
                                  <IconChartLine className='mr-2 h-4 w-4' />
                                  {t('channels.dialogs.keys.manualTest.modes.balance_probe.label')}
                                </Button>
                              ) : null}
                            </div>
                          </div>

                          <div className='bg-muted/30 grid gap-3 rounded-md border px-3 py-3 lg:grid-cols-[minmax(14rem,1fr)_auto] lg:items-end'>
                            <div className='space-y-2'>
                              <label className='text-sm font-medium' htmlFor='channel-key-search'>
                                {t('channels.dialogs.keys.inventory.search.label')}
                              </label>
                              <div className='relative'>
                                <IconSearch className='text-muted-foreground pointer-events-none absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2' />
                                <Input
                                  id='channel-key-search'
                                  name='channel-key-search'
                                  className='pl-9'
                                  type='search'
                                  autoComplete='off'
                                  spellCheck={false}
                                  value={searchQuery}
                                  onChange={(event) => setSearchQuery(event.target.value)}
                                  placeholder={t('channels.dialogs.keys.inventory.search.placeholder')}
                                />
                              </div>
                            </div>
                            <div className='space-y-2'>
                              <div className='text-sm font-medium'>{t('channels.dialogs.keys.inventory.statusFilter.label')}</div>
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
                            <div className='text-muted-foreground text-xs lg:col-span-2'>
                              {statusFilter.size > 0
                                ? t('channels.dialogs.keys.inventory.statusFilter.summary', {
                                    count: visibleInventory.length,
                                    statuses: statusFilterSummary,
                                  })
                                : t('channels.dialogs.keys.inventory.statusFilter.none')}
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
                                  onClick={() => handleRunChecks(selectedHealthCheckKeyIDs, 'real_request')}
                                  disabled={isPending || selectedHealthCheckKeyIDs.length === 0}
                                >
                                  <IconPlayerPlay className='mr-2 h-4 w-4' />
                                  {t('channels.dialogs.keys.actions.testSelected', { count: selectedHealthCheckKeyIDs.length })}
                                </Button>
                                {canRunBalanceProbe ? (
                                  <Button
                                    type='button'
                                    variant='outline'
                                    size='sm'
                                    onClick={() => handleRunChecks(selectedHealthCheckKeyIDs, 'balance_probe')}
                                    disabled={isPending || selectedHealthCheckKeyIDs.length === 0}
                                  >
                                    <IconChartLine className='mr-2 h-4 w-4' />
                                    {t('channels.dialogs.keys.actions.balanceSelected', { count: selectedHealthCheckKeyIDs.length })}
                                  </Button>
                                ) : null}
                                <Button
                                  type='button'
                                  variant='outline'
                                  size='sm'
                                  onClick={() => copyKeys(selectedRawKeys)}
                                  disabled={isPending || selectedRawKeys.length === 0}
                                >
                                  <IconCopy className='mr-2 h-4 w-4' />
                                  {t('channels.dialogs.keys.actions.copySelected', { count: selectedRawKeys.length })}
                                </Button>
                                <DropdownMenu>
                                  <DropdownMenuTrigger asChild>
                                    <Button type='button' variant='outline' size='sm' disabled={isPending}>
                                      <IconDotsVertical className='mr-2 h-4 w-4' />
                                      {t('channels.dialogs.keys.actions.more')}
                                    </Button>
                                  </DropdownMenuTrigger>
                                  <DropdownMenuContent align='start'>
                                    <DropdownMenuItem
                                      onSelect={() => handleBatchAction('disable')}
                                      disabled={selectedActiveKeyIDs.length === 0}
                                    >
                                      <IconKeyOff className='mr-2 h-4 w-4' />
                                      {t('channels.dialogs.keys.actions.disableSelected', { count: selectedActiveKeyIDs.length })}
                                    </DropdownMenuItem>
                                    <DropdownMenuItem
                                      onSelect={() => handleBatchAction('enable')}
                                      disabled={selectedDisabledKeyIDs.length === 0}
                                    >
                                      <IconRefresh className='mr-2 h-4 w-4' />
                                      {t('channels.dialogs.keys.actions.enableSelected', { count: selectedDisabledKeyIDs.length })}
                                    </DropdownMenuItem>
                                    <DropdownMenuItem
                                      onSelect={() => handleBatchAction('archive')}
                                      disabled={selectedActiveKeyIDs.length + selectedDisabledKeyIDs.length === 0}
                                    >
                                      <IconArchive className='mr-2 h-4 w-4' />
                                      {t('channels.dialogs.keys.actions.archiveSelected', {
                                        count: selectedActiveKeyIDs.length + selectedDisabledKeyIDs.length,
                                      })}
                                    </DropdownMenuItem>
                                    <DropdownMenuItem
                                      onSelect={() => handleBatchAction('restore')}
                                      disabled={selectedArchivedKeyIDs.length === 0}
                                    >
                                      <IconRestore className='mr-2 h-4 w-4' />
                                      {t('channels.dialogs.keys.actions.restoreSelected', { count: selectedArchivedKeyIDs.length })}
                                    </DropdownMenuItem>
                                  </DropdownMenuContent>
                                </DropdownMenu>
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
                                <Button
                                  type='button'
                                  variant='ghost'
                                  size='sm'
                                  onClick={() => setSelectedKeys(new Set())}
                                  disabled={isPending}
                                >
                                  {t('channels.dialogs.keys.actions.clearSelection')}
                                </Button>
                              </div>
                            </div>
                          )}

                          <div className='overflow-x-auto rounded-lg border'>
                            <Table className='min-w-[54rem]'>
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
                                            aria-label={t('channels.dialogs.keys.inventory.selectKey', { key: item.maskedKey })}
                                            disabled={isPending}
                                          />
                                        </TableCell>
                                        <TableCell>
                                          <div className='flex flex-col gap-1'>
                                            <div className='flex min-w-0 items-center gap-1'>
                                              <code className='bg-muted max-w-80 overflow-x-auto rounded px-2 py-0.5 font-mono text-sm whitespace-nowrap'>
                                                {revealedKeys.has(item.id) && item.rawKey ? item.rawKey : item.maskedKey}
                                              </code>
                                              {item.rawKey ? (
                                                <>
                                                  <Tooltip>
                                                    <TooltipTrigger asChild>
                                                      <Button
                                                        type='button'
                                                        size='icon'
                                                        variant='ghost'
                                                        className='h-7 w-7 shrink-0'
                                                        onClick={() => toggleKeyReveal(item.id)}
                                                        aria-label={t(
                                                          revealedKeys.has(item.id)
                                                            ? 'channels.dialogs.keys.actions.hideKey'
                                                            : 'channels.dialogs.keys.actions.revealKey'
                                                        )}
                                                      >
                                                        {revealedKeys.has(item.id) ? (
                                                          <IconEyeOff className='h-4 w-4' aria-hidden='true' />
                                                        ) : (
                                                          <IconEye className='h-4 w-4' aria-hidden='true' />
                                                        )}
                                                      </Button>
                                                    </TooltipTrigger>
                                                    <TooltipContent>
                                                      {t(
                                                        revealedKeys.has(item.id)
                                                          ? 'channels.dialogs.keys.actions.hideKey'
                                                          : 'channels.dialogs.keys.actions.revealKey'
                                                      )}
                                                    </TooltipContent>
                                                  </Tooltip>
                                                  <Tooltip>
                                                    <TooltipTrigger asChild>
                                                      <Button
                                                        type='button'
                                                        size='icon'
                                                        variant='ghost'
                                                        className='h-7 w-7 shrink-0'
                                                        onClick={() => copyKeys([item.rawKey])}
                                                        aria-label={t('channels.dialogs.keys.actions.copyKey')}
                                                      >
                                                        <IconCopy className='h-4 w-4' aria-hidden='true' />
                                                      </Button>
                                                    </TooltipTrigger>
                                                    <TooltipContent>{t('channels.dialogs.keys.actions.copyKey')}</TooltipContent>
                                                  </Tooltip>
                                                </>
                                              ) : null}
                                            </div>
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
                                        <TableCell className='text-muted-foreground text-sm'>
                                          {formatDateTime(item.lastCheckedAt, i18n.language)}
                                        </TableCell>
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
                                            <Tooltip>
                                              <TooltipTrigger asChild>
                                                <Button
                                                  type='button'
                                                  size='icon'
                                                  variant='ghost'
                                                  onClick={() => handleRunChecks([item.id], 'real_request')}
                                                  disabled={isPending || item.status === 'archived'}
                                                  aria-label={t('channels.dialogs.keys.manualTest.modes.real_request.label')}
                                                >
                                                  <IconPlayerPlay className='h-4 w-4' aria-hidden='true' />
                                                </Button>
                                              </TooltipTrigger>
                                              <TooltipContent>
                                                {t('channels.dialogs.keys.manualTest.modes.real_request.label')}
                                              </TooltipContent>
                                            </Tooltip>
                                            {canRunBalanceProbe ? (
                                              <Tooltip>
                                                <TooltipTrigger asChild>
                                                  <Button
                                                    type='button'
                                                    size='icon'
                                                    variant='ghost'
                                                    onClick={() => handleRunChecks([item.id], 'balance_probe')}
                                                    disabled={isPending || item.status === 'archived'}
                                                    aria-label={t('channels.dialogs.keys.manualTest.modes.balance_probe.label')}
                                                  >
                                                    <IconChartLine className='h-4 w-4' aria-hidden='true' />
                                                  </Button>
                                                </TooltipTrigger>
                                                <TooltipContent>
                                                  {t('channels.dialogs.keys.manualTest.modes.balance_probe.label')}
                                                </TooltipContent>
                                              </Tooltip>
                                            ) : null}
                                            <Tooltip>
                                              <TooltipTrigger asChild>
                                                <Button
                                                  type='button'
                                                  size='icon'
                                                  variant='ghost'
                                                  onClick={() => setDetailsKeyID(item.id)}
                                                  aria-label={t('channels.dialogs.keys.actions.viewDetails')}
                                                >
                                                  <IconInfoCircle className='h-4 w-4' aria-hidden='true' />
                                                </Button>
                                              </TooltipTrigger>
                                              <TooltipContent>{t('channels.dialogs.keys.actions.viewDetails')}</TooltipContent>
                                            </Tooltip>
                                            <DropdownMenu>
                                              <DropdownMenuTrigger asChild>
                                                <Button
                                                  type='button'
                                                  size='icon'
                                                  variant='ghost'
                                                  disabled={isPending}
                                                  aria-label={t('channels.dialogs.keys.actions.moreForKey', { key: item.maskedKey })}
                                                >
                                                  <IconDotsVertical className='h-4 w-4' aria-hidden='true' />
                                                </Button>
                                              </DropdownMenuTrigger>
                                              <DropdownMenuContent align='end'>
                                                {item.status === 'active' ? (
                                                  <DropdownMenuItem onSelect={() => handleDisableKey(item.id)}>
                                                    <IconKeyOff className='mr-2 h-4 w-4' aria-hidden='true' />
                                                    {t('channels.dialogs.keys.actions.disable')}
                                                  </DropdownMenuItem>
                                                ) : null}
                                                {item.status === 'disabled' ? (
                                                  <DropdownMenuItem onSelect={() => handleEnableKey(item.id)}>
                                                    <IconRefresh className='mr-2 h-4 w-4' aria-hidden='true' />
                                                    {t('channels.dialogs.keys.actions.enable')}
                                                  </DropdownMenuItem>
                                                ) : null}
                                                {item.status === 'archived' ? (
                                                  <DropdownMenuItem onSelect={() => handleRestoreKey(item.id)}>
                                                    <IconRestore className='mr-2 h-4 w-4' aria-hidden='true' />
                                                    {t('channels.dialogs.keys.actions.restore')}
                                                  </DropdownMenuItem>
                                                ) : (
                                                  <DropdownMenuItem onSelect={() => handleArchiveKey(item.id)}>
                                                    <IconArchive className='mr-2 h-4 w-4' aria-hidden='true' />
                                                    {t('channels.dialogs.keys.actions.archive')}
                                                  </DropdownMenuItem>
                                                )}
                                                <DropdownMenuSeparator />
                                                <DropdownMenuItem
                                                  className='text-destructive'
                                                  onSelect={() => setConfirmDeleteKey(item.id)}
                                                >
                                                  <IconTrash className='mr-2 h-4 w-4' aria-hidden='true' />
                                                  {t('channels.dialogs.keys.actions.delete')}
                                                </DropdownMenuItem>
                                              </DropdownMenuContent>
                                            </DropdownMenu>
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
                    </>
                  )}
                </TabsContent>

                <TabsContent value='keyHistory' className='mt-0 space-y-4'>
                  <Card>
                    <CardHeader>
                      <CardTitle className='flex items-center gap-2'>
                        <IconChartLine className='h-5 w-5' />
                        {t('channels.dialogs.keys.keyHistory.title')}
                      </CardTitle>
                      <CardDescription>{t('channels.dialogs.keys.keyHistory.description')}</CardDescription>
                    </CardHeader>
                    <CardContent>
                      <div className='overflow-x-auto rounded-lg border'>
                        <Table className='min-w-[44rem]'>
                          <TableHeader>
                            <TableRow>
                              <TableHead>{t('channels.dialogs.keys.columns.key')}</TableHead>
                              <TableHead>{t('channels.dialogs.keys.columns.lastCheck')}</TableHead>
                              <TableHead>{t('channels.dialogs.keys.details.statusCode')}</TableHead>
                              <TableHead>{t('channels.dialogs.keys.details.reason')}</TableHead>
                              <TableHead className='text-right'>{t('common.columns.actions')}</TableHead>
                            </TableRow>
                          </TableHeader>
                          <TableBody>
                            {keyHealthHistory.length === 0 ? (
                              <TableRow>
                                <TableCell colSpan={5} className='text-muted-foreground h-28 text-center text-sm'>
                                  {t('channels.dialogs.keys.keyHistory.empty')}
                                </TableCell>
                              </TableRow>
                            ) : (
                              keyHealthHistory.map(({ key, entry }) => {
                                const tone = healthTone(entry.success);
                                return (
                                  <TableRow key={`${key.id}-${entry.id}`}>
                                    <TableCell>
                                      <code className='bg-muted w-fit rounded px-2 py-0.5 font-mono text-sm'>{key.maskedKey}</code>
                                    </TableCell>
                                    <TableCell className='text-muted-foreground text-sm'>
                                      {formatDateTime(entry.checkedAt, i18n.language)}
                                    </TableCell>
                                    <TableCell>{entry.statusCode ?? '-'}</TableCell>
                                    <TableCell>
                                      <div className='flex flex-col gap-1'>
                                        <Badge className='w-fit' variant={tone.badgeVariant}>
                                          {t(`channels.dialogs.keys.healthState.${entry.success ? 'success' : 'failed'}`)}
                                        </Badge>
                                        <span className='text-sm'>{entry.reason || '-'}</span>
                                      </div>
                                    </TableCell>
                                    <TableCell className='text-right'>
                                      <Button
                                        type='button'
                                        variant='ghost'
                                        size='sm'
                                        onClick={() => setDetailsKeyID(key.id)}
                                        aria-label={t('channels.dialogs.keys.actions.viewDetails')}
                                      >
                                        <IconEye className='h-4 w-4' aria-hidden='true' />
                                      </Button>
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

                <TabsContent value='failureHistory' className='mt-0 space-y-4'>
                  <button
                    type='button'
                    className='from-primary/5 via-background to-muted/40 hover:border-primary/40 flex w-full flex-col gap-3 rounded-xl border bg-gradient-to-r p-4 text-left transition sm:flex-row sm:items-center sm:justify-between'
                    onClick={() => setChannelHistoryOpen(true)}
                    disabled={channelHistory.length === 0}
                  >
                    <div className='flex items-start gap-3'>
                      <div className='bg-primary/10 text-primary rounded-lg p-2'>
                        <IconRoute className='h-5 w-5' />
                      </div>
                      <div>
                        <div className='font-medium'>{t('channels.dialogs.keys.channelHistory.title')}</div>
                        <div className='text-muted-foreground mt-1 text-sm'>{t('channels.dialogs.keys.channelHistory.description')}</div>
                      </div>
                    </div>
                    <Badge variant='outline'>{t('channels.dialogs.keys.analytics.historyCount', { count: channelHistory.length })}</Badge>
                  </button>

                  <Card>
                    <CardHeader>
                      <CardTitle>{t('channels.dialogs.keys.failureHistory.title')}</CardTitle>
                      <CardDescription>{t('channels.dialogs.keys.failureHistory.description')}</CardDescription>
                    </CardHeader>
                    <CardContent>
                      <div className='overflow-x-auto rounded-lg border'>
                        <Table className='min-w-[52rem]'>
                          <TableHeader>
                            <TableRow>
                              <TableHead>{t('channels.dialogs.keys.columns.key')}</TableHead>
                              <TableHead>{t('channels.dialogs.keys.columns.lastCheck')}</TableHead>
                              <TableHead>{t('channels.dialogs.keys.details.matchedPolicy')}</TableHead>
                              <TableHead>{t('channels.dialogs.keys.details.action')}</TableHead>
                              <TableHead>{t('channels.dialogs.keys.details.reason')}</TableHead>
                            </TableRow>
                          </TableHeader>
                          <TableBody>
                            {requestFailureHistory.length === 0 && channelFailureHistory.length === 0 ? (
                              <TableRow>
                                <TableCell colSpan={5} className='text-muted-foreground h-28 text-center text-sm'>
                                  {t('channels.dialogs.keys.failureHistory.empty')}
                                </TableCell>
                              </TableRow>
                            ) : (
                              <>
                                {channelFailureHistory.map((entry) => (
                                  <TableRow key={`channel-${entry.id}`}>
                                    <TableCell>
                                      <Badge variant='outline'>{t('channels.dialogs.keys.channelHistory.scopeValue')}</Badge>
                                    </TableCell>
                                    <TableCell className='text-muted-foreground text-sm'>
                                      {formatDateTime(entry.checkedAt, i18n.language)}
                                    </TableCell>
                                    <TableCell>{entry.matchedPolicy || '-'}</TableCell>
                                    <TableCell>{formatPolicyAction(entry.action)}</TableCell>
                                    <TableCell>{entry.reason || '-'}</TableCell>
                                  </TableRow>
                                ))}
                                {requestFailureHistory.map(({ key, entry }) => (
                                  <TableRow key={`${key.id}-${entry.id}`}>
                                    <TableCell>
                                      <code className='bg-muted w-fit rounded px-2 py-0.5 font-mono text-sm'>{key.maskedKey}</code>
                                    </TableCell>
                                    <TableCell className='text-muted-foreground text-sm'>
                                      {formatDateTime(entry.checkedAt, i18n.language)}
                                    </TableCell>
                                    <TableCell>{entry.matchedPolicy || '-'}</TableCell>
                                    <TableCell>{formatPolicyAction(entry.action)}</TableCell>
                                    <TableCell>{entry.reason || '-'}</TableCell>
                                  </TableRow>
                                ))}
                              </>
                            )}
                          </TableBody>
                        </Table>
                      </div>
                    </CardContent>
                  </Card>
                </TabsContent>

                <TabsContent value='routing' className='mt-0 space-y-4'>
                  <KeyRoutingPanel form={form} globalStrategy={globalRoutingStrategy as ChannelKeySelectionStrategy} />
                </TabsContent>

                <TabsContent value='balanceProbe' className='mt-0 space-y-4'>
                  <BalanceProbeEditor form={form} disabled={isPending} channelBaseURL={currentRow.baseURL ?? ''} />
                </TabsContent>

                <TabsContent value='failurePolicy' className='mt-0 space-y-4'>
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
              </div>
            </Tabs>
          </Form>

          <DialogFooter className='flex flex-col gap-3 border-t pt-3 sm:flex-row sm:items-center sm:justify-between'>
            <div className='text-muted-foreground flex items-start gap-1.5 text-left text-xs'>
              <IconInfoCircle className='mt-0.5 h-3.5 w-3.5 shrink-0' aria-hidden='true' />
              <span>{t('channels.dialogs.keys.persistenceNote')}</span>
            </div>
            <div className='flex w-full shrink-0 gap-2 sm:w-auto'>
              <Button className='flex-1 sm:flex-none' type='button' variant='outline' onClick={() => requestOpenChange(false)}>
                {hasUnsavedChanges ? t('channels.dialogs.keys.actions.discardChanges') : t('common.buttons.close')}
              </Button>
              <Button
                className='flex-1 sm:flex-none'
                type='button'
                onClick={form.handleSubmit(handleSaveSettings)}
                disabled={isPending || !form.formState.isValid || !form.formState.isDirty}
              >
                {updateChannel.isPending ? t('common.buttons.saving') : t('channels.dialogs.keys.actions.saveSettings')}
              </Button>
            </div>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <KeyDetailsDialog row={detailsRow} open={!!detailsRow} onOpenChange={(state) => !state && setDetailsKeyID(null)} />
      <ChannelHistoryDialog history={channelHistory} open={channelHistoryOpen} onOpenChange={setChannelHistoryOpen} />
      <AlertDialog open={confirmDeleteKey != null} onOpenChange={(state) => !state && setConfirmDeleteKey(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t('channels.dialogs.keys.deleteDialog.title')}</AlertDialogTitle>
            <AlertDialogDescription>{t('channels.dialogs.keys.confirmDelete')}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t('common.buttons.cancel')}</AlertDialogCancel>
            <AlertDialogAction
              className='bg-destructive text-destructive-foreground hover:bg-destructive/90'
              onClick={(event) => {
                event.preventDefault();
                if (confirmDeleteKey) {
                  handleDeleteKey(confirmDeleteKey);
                }
              }}
              disabled={deleteAPIKey.isPending}
            >
              {deleteAPIKey.isPending ? <IconLoader2 className='mr-2 h-4 w-4 animate-spin' /> : null}
              {t('channels.dialogs.keys.actions.delete')}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      <AlertDialog open={confirmDiscardSettings} onOpenChange={setConfirmDiscardSettings}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t('channels.dialogs.keys.discardDialog.title')}</AlertDialogTitle>
            <AlertDialogDescription>{t('channels.dialogs.keys.discardDialog.description')}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t('common.buttons.cancel')}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                form.reset(valuesFromChannel(currentRow));
                setNewKey('');
                setConfirmDiscardSettings(false);
                onOpenChange(false);
              }}
            >
              {t('channels.dialogs.keys.actions.discardChanges')}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
