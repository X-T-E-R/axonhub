import type { TFunction } from 'i18next';
import type {
  BalanceAmount,
  ChannelAPIKeyInventoryItem,
  ChannelQuotaCounts,
  ChannelQuotaOverview,
  ChannelQuotaState,
  ChannelQuotaWindow,
} from '@/features/channels/data/schema';

export type QuotaDisplayMode = 'remaining' | 'used';

export function normalizeQuotaUnit(unit?: string | null): string {
  const trimmed = unit?.trim().toUpperCase() ?? '';
  if (trimmed === 'RMB') return 'CNY';
  return trimmed;
}

export function isIsoCurrencyUnit(unit?: string | null): boolean {
  const normalized = normalizeQuotaUnit(unit);
  return /^[A-Z]{3}$/.test(normalized) && normalized !== 'REQ';
}

export function localeForQuotaLanguage(language: string): string {
  return language.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en-US';
}

export function formatQuotaAmount(
  amount: number,
  unit: string | null | undefined,
  t: TFunction,
  language: string
): string {
  if (!Number.isFinite(amount)) {
    return '-';
  }
  const normalized = normalizeQuotaUnit(unit);
  const locale = localeForQuotaLanguage(language);
  if (isIsoCurrencyUnit(normalized)) {
    const abs = Math.abs(amount);
    const maxDigits = abs > 0 && abs < 0.01 ? 4 : 2;
    return t('currencies.format', {
      val: amount,
      currency: normalized,
      locale,
      minimumFractionDigits: 2,
      maximumFractionDigits: maxDigits,
    });
  }

  const formatted = new Intl.NumberFormat(locale, {
    maximumFractionDigits: Number.isInteger(amount) ? 0 : 2,
  }).format(amount);

  if (!normalized || normalized === 'CREDITS' || normalized === 'UNITS') {
    return formatted;
  }
  if (normalized === '%') {
    return `${formatted}%`;
  }
  return `${formatted} ${normalized}`;
}

/**
 * True ONLY when a window has a real denominator (limit > 0 or finite usageRatio).
 * Prevents drawing fabricated 100% green bars for unknown/unlimited/free-tier entries.
 */
export function hasRealWindowMeter(window: ChannelQuotaWindow): boolean {
  if (window.limit != null && Number.isFinite(window.limit) && window.limit > 0) {
    return true;
  }
  if (window.usageRatio != null && Number.isFinite(window.usageRatio) && window.usageRatio >= 0) {
    return true;
  }
  return false;
}

export function windowUsedRatio(window: ChannelQuotaWindow, nowMs?: number): number | null {
  if (nowMs != null && isResetExpired(window.resetAt, nowMs)) {
    return 0;
  }
  if (window.limit != null && Number.isFinite(window.limit) && window.limit > 0) {
    if (window.used != null && Number.isFinite(window.used)) {
      return Math.max(0, window.used / window.limit);
    }
    if (window.remaining != null && Number.isFinite(window.remaining)) {
      return Math.max(0, (window.limit - window.remaining) / window.limit);
    }
  }
  if (window.usageRatio != null && Number.isFinite(window.usageRatio) && window.usageRatio >= 0) {
    return window.usageRatio;
  }
  return null;
}

export function isResetExpired(resetAt?: string | null, nowMs?: number): boolean {
  if (!resetAt) {
    return false;
  }
  const resetMs = Date.parse(resetAt);
  if (Number.isNaN(resetMs)) {
    return false;
  }
  const effectiveNow = nowMs ?? Date.now();
  return resetMs <= effectiveNow;
}

export function isWindowExhausted(window: ChannelQuotaWindow, nowMs?: number): boolean {
  if (isResetExpired(window.resetAt, nowMs)) {
    return false;
  }
  if (window.limit != null && window.limit > 0) {
    if (window.remaining != null && window.remaining <= 0) {
      return true;
    }
    if (window.used != null && window.used >= window.limit) {
      return true;
    }
  }
  const ratio = windowUsedRatio(window);
  return ratio != null && ratio >= 1;
}

export function isQuotaKeyExhausted(
  item: Pick<ChannelAPIKeyInventoryItem, 'status' | 'available' | 'quotaRemainingRatio' | 'quotaResetAt' | 'balanceSnapshot'>,
  nowMs?: number
): boolean {
  const snap = item.balanceSnapshot;
  if (snap?.unlimited || snap?.freeTier) {
    return false;
  }
  const windows = snap?.windows ?? [];
  if (windows.some((w) => isWindowExhausted(w, nowMs))) {
    return true;
  }
  // Independent account/wallet cap check (e.g. Sub2 key with 0 total quota while rolling window is 50%).
  const walletExpired = isResetExpired(snap?.resetAt ?? item.quotaResetAt, nowMs);
  if (!walletExpired) {
    if (snap?.limit != null && snap.limit > 0) {
      if (snap.remaining != null && snap.remaining <= 0) {
        return true;
      }
      if (snap.used != null && snap.used >= snap.limit) {
        return true;
      }
    }
    if (snap?.primaryBalance && Number.isFinite(snap.primaryBalance.amount) && snap.primaryBalance.amount <= 0) {
      if (snap.primaryBalance.kind === 'remaining' || snap.primaryBalance.kind === 'available') {
        return true;
      }
    }
    if (windows.length === 0) {
      if (item.available === false || snap?.available === false) {
        return true;
      }
      if (snap?.usageRatio != null && Number.isFinite(snap.usageRatio) && snap.usageRatio >= 1) {
        return true;
      }
    }
  }
  return false;
}

export function hasRealWalletMeter(params: {
  unlimited?: boolean | null;
  freeTier?: boolean | null;
  walletLimit?: number | null;
  walletUsed?: number | null;
  walletRemaining?: number | null;
}): boolean {
  if (params.unlimited || params.freeTier) {
    return false;
  }
  if (params.walletLimit == null || !Number.isFinite(params.walletLimit) || params.walletLimit <= 0) {
    return false;
  }
  return (
    (params.walletUsed != null && Number.isFinite(params.walletUsed)) ||
    (params.walletRemaining != null && Number.isFinite(params.walletRemaining))
  );
}

export function shortWindowLabel(window: Pick<ChannelQuotaWindow, 'key' | 'label'>): string {
  const raw = (window.label || window.key || '').trim();
  const lower = raw.toLowerCase();
  if (lower === '5h' || lower === '5-hour' || lower === '5 hour' || lower === 'five_hour' || lower === 'rolling') {
    return '5h';
  }
  if (lower === '7d' || lower === 'weekly' || lower === 'seven_day' || lower === 'week' || lower === '7-day') {
    return '7d';
  }
  if (lower === '30d' || lower === 'monthly' || lower === 'month') {
    return '30d';
  }
  if (lower === '24h' || lower === 'daily' || lower === '1d') {
    return '24h';
  }
  return raw.length > 10 ? raw.slice(0, 10) : raw || 'Win';
}

export function formatRelativeReset(
  resetAt: string | null | undefined,
  nowMs: number,
  t: TFunction
): string | null {
  if (!resetAt) {
    return null;
  }
  const targetMs = Date.parse(resetAt);
  if (Number.isNaN(targetMs)) {
    return null;
  }
  const diffMs = targetMs - nowMs;
  if (diffMs <= 0) {
    return t('quotas.time.dueNow');
  }

  const totalMins = Math.max(1, Math.floor(diffMs / 60_000));
  const days = Math.floor(totalMins / (60 * 24));
  const hours = Math.floor((totalMins % (60 * 24)) / 60);
  const mins = totalMins % 60;

  if (days > 0) {
    return hours > 0
      ? t('quotas.time.daysHours', { days, hours })
      : t('quotas.time.daysOnly', { days });
  }
  if (hours > 0) {
    return mins > 0
      ? t('quotas.time.hoursMins', { hours, mins })
      : t('quotas.time.hoursOnly', { hours });
  }
  return t('quotas.time.minsOnly', { mins });
}

export function formatRelativeAge(
  timestamp: string | null | undefined,
  nowMs: number,
  t: TFunction
): string | null {
  if (!timestamp) {
    return null;
  }
  const tsMs = Date.parse(timestamp);
  if (Number.isNaN(tsMs)) {
    return null;
  }
  const diffMs = Math.max(0, nowMs - tsMs);
  const mins = Math.floor(diffMs / 60_000);
  if (mins < 1) {
    return t('quotas.time.justNow');
  }
  if (mins < 60) {
    return t('quotas.time.minsAgo', { mins });
  }
  const hours = Math.floor(mins / 60);
  if (hours < 24) {
    return t('quotas.time.hoursAgo', { hours });
  }
  const days = Math.floor(hours / 24);
  return t('quotas.time.daysAgo', { days });
}

export function formatExactTimestamp(timestamp: string | null | undefined, language: string): string {
  if (!timestamp) {
    return '-';
  }
  const date = new Date(timestamp);
  if (Number.isNaN(date.getTime())) {
    return timestamp;
  }
  return new Intl.DateTimeFormat(localeForQuotaLanguage(language), {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(date);
}

export function isAttentionQuotaOverview(overview?: ChannelQuotaOverview | null): boolean {
  if (!overview) {
    return false;
  }
  if (overview.state === 'exhausted' || overview.state === 'warning' || overview.state === 'error') {
    return true;
  }
  if (overview.counts.exhausted > 0) {
    return true;
  }
  if (overview.probeDiagnostic && overview.probeDiagnostic.status !== 'ok' && overview.probeDiagnostic.status !== 'unsupported') {
    return true;
  }
  return false;
}

export function isResettingSoon(nextResetAt: string | null | undefined, nowMs: number, horizonHours = 24): boolean {
  if (!nextResetAt) {
    return false;
  }
  const targetMs = Date.parse(nextResetAt);
  if (Number.isNaN(targetMs)) {
    return false;
  }
  const diff = targetMs - nowMs;
  return diff >= -60_000 && diff <= horizonHours * 3600 * 1000;
}

export function resolveWalletAmount(
  walletBalance?: BalanceAmount | null,
  walletRemaining?: number | null,
  walletUnit?: string | null
): { amount: number; unit: string } | null {
  if (walletRemaining != null && Number.isFinite(walletRemaining)) {
    return {
      amount: walletRemaining,
      unit: walletUnit || walletBalance?.currency || '',
    };
  }
  if (walletBalance && Number.isFinite(walletBalance.amount)) {
    return {
      amount: walletBalance.amount,
      unit: walletBalance.currency || walletUnit || '',
    };
  }
  return null;
}

export function getQuotaKeyAvailability(counts?: ChannelQuotaCounts | null): {
  schedulable: number;
  total: number;
  disabled: number;
  exhausted: number;
  unknown: number;
} {
  const total = Math.max(0, counts?.total ?? 0);
  const active = Math.max(0, counts?.active ?? 0);
  const exhausted = Math.max(0, counts?.exhausted ?? 0);
  const unknown = Math.max(0, counts?.unknown ?? 0);
  return {
    schedulable: Math.max(0, active - exhausted),
    total,
    disabled: Math.max(0, total - active),
    exhausted,
    unknown,
  };
}

export function quotaStateSortRank(state?: ChannelQuotaState | null, hasDiagnostic?: boolean): number {
  switch (state) {
    case 'exhausted':
      return 0;
    case 'warning':
      return 1;
    case 'error':
      return 2;
    case 'available':
      return hasDiagnostic ? 3 : 4;
    case 'free_tier':
      return 5;
    case 'unlimited':
      return 6;
    case 'unknown':
      return 7;
    case 'unsupported':
      return 8;
    default:
      return 9;
  }
}
