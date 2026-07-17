import type { TFunction } from 'i18next';

export type PolicyQuota = {
  requests?: number | null;
  totalTokens?: number | null;
  cost?: number | string | null;
  period?:
    | string
    | {
        type: string;
        pastDuration?: { value: number; unit: string } | null;
        calendarDuration?: { unit: string } | null;
      }
    | null;
};

export function formatPolicyList(values?: Array<string | number> | null) {
  return values?.length ? values.join(', ') : '-';
}

function formatQuotaUnit(unit: string, count: number, t: TFunction) {
  const key = `apikeys.classification.preview.quotaUnit.${unit}`;
  const translated = t(key, { count });
  return translated === key ? `${count} ${unit}` : translated;
}

function formatQuotaPeriod(period: PolicyQuota['period'], t: TFunction) {
  if (!period) return '';
  const type = typeof period === 'string' ? period : period.type;
  if (type === 'all_time') return t('apikeys.classification.preview.quotaPeriod.allTime');
  if (type === 'past_duration' && typeof period !== 'string' && period.pastDuration) {
    return t('apikeys.classification.preview.quotaPeriod.pastDuration', {
      duration: formatQuotaUnit(period.pastDuration.unit, period.pastDuration.value, t),
    });
  }
  if (type === 'calendar_duration' && typeof period !== 'string' && period.calendarDuration) {
    return t('apikeys.classification.preview.quotaPeriod.calendarDuration', {
      unit: formatQuotaUnit(period.calendarDuration.unit, 1, t),
    });
  }
  const knownKey = `apikeys.classification.preview.quotaPeriod.${type}`;
  const translated = t(knownKey);
  return translated === knownKey ? type : translated;
}

export function formatPolicyQuota(quota: PolicyQuota | null | undefined, t: TFunction) {
  if (!quota) return '-';
  return [
    quota.requests
      ? t('apikeys.classification.preview.quotaRequests', { count: quota.requests, formattedCount: quota.requests.toLocaleString() })
      : '',
    quota.totalTokens
      ? t('apikeys.classification.preview.quotaTokens', {
          count: quota.totalTokens,
          formattedCount: quota.totalTokens.toLocaleString(),
        })
      : '',
    quota.cost ? t('apikeys.classification.preview.quotaCost', { cost: quota.cost }) : '',
    formatQuotaPeriod(quota.period, t),
  ]
    .filter(Boolean)
    .join(' · ');
}
