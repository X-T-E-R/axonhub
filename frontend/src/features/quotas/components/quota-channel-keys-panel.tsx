import { useMemo, useState } from 'react';
import { IconAlertTriangle, IconChevronDown, IconChevronUp, IconLoader2, IconRefresh } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import {
  formatExactTimestamp,
  formatRelativeAge,
  formatRelativeReset,
  isQuotaKeyExhausted,
  windowUsedRatio,
  type QuotaDisplayMode,
} from '@/lib/quota-format';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { QuotaWindowBars } from '@/components/quota-window-bars';
import { useChannelQuotaKeyInventory } from '@/features/channels/data/channels';
import type { ChannelAPIKeyInventoryItem } from '@/features/channels/data/schema';

export const INITIAL_VISIBLE_QUOTA_KEYS = 6;

interface QuotaChannelKeysPanelProps {
  channelID: string;
  sharedWallet: boolean;
  canRefresh: boolean;
  canWrite: boolean;
  mode: QuotaDisplayMode;
  nowMs: number;
  refreshingKeyIDs: Set<string>;
  onRefreshKey: (channelID: string, keyID: string) => void;
}

function keyAttentionRank(item: ChannelAPIKeyInventoryItem, nowMs: number): number {
  const snap = item.balanceSnapshot;
  if (isQuotaKeyExhausted(item, nowMs)) return 0;
  if (item.lastProbeStatus && item.lastProbeStatus !== 'ok' && item.lastProbeStatus !== 'unsupported') return 1;
  if (item.status === 'disabled') return 2;

  const ratios = (snap?.windows ?? [])
    .map((w) => windowUsedRatio(w, nowMs))
    .filter((v): v is number => v != null);
  const worstRatio =
    ratios.length > 0
      ? Math.max(...ratios)
      : snap?.usageRatio != null
        ? snap.usageRatio
        : item.quotaRemainingRatio != null
          ? 1 - item.quotaRemainingRatio
          : null;

  if (worstRatio != null && worstRatio >= 0.8) return 3;
  if (snap?.success) return 4;
  return 5;
}

export function sortQuotaInventoryKeys(
  items: ChannelAPIKeyInventoryItem[],
  nowMs: number = Date.now()
): ChannelAPIKeyInventoryItem[] {
  return [...items]
    .filter((item) => item.status !== 'archived')
    .sort((a, b) => {
      const rankDiff = keyAttentionRank(a, nowMs) - keyAttentionRank(b, nowMs);
      if (rankDiff !== 0) return rankDiff;
      const remA = a.quotaRemainingRatio ?? 1;
      const remB = b.quotaRemainingRatio ?? 1;
      if (remA !== remB) return remA - remB;
      return a.maskedKey.localeCompare(b.maskedKey);
    });
}

export function QuotaChannelKeysPanel({
  channelID,
  sharedWallet,
  canRefresh,
  canWrite,
  mode,
  nowMs,
  refreshingKeyIDs,
  onRefreshKey,
}: QuotaChannelKeysPanelProps) {
  const { t, i18n } = useTranslation();
  const [showAll, setShowAll] = useState(false);
  const { data, isLoading, isError, refetch } = useChannelQuotaKeyInventory(channelID, { enabled: true });

  const sortedKeys = useMemo(() => sortQuotaInventoryKeys(data ?? [], nowMs), [data, nowMs]);
  const visibleKeys = showAll ? sortedKeys : sortedKeys.slice(0, INITIAL_VISIBLE_QUOTA_KEYS);
  const hiddenCount = Math.max(0, sortedKeys.length - visibleKeys.length);

  if (isLoading) {
    return (
      <div className='bg-muted/25 text-muted-foreground flex items-center gap-2 border-t px-6 py-4 text-xs'>
        <IconLoader2 className='h-3.5 w-3.5 animate-spin' />
        <span>{t('quotas.expanded.loading')}</span>
      </div>
    );
  }

  if (isError) {
    return (
      <div className='bg-muted/25 flex items-center justify-between border-t px-6 py-3 text-xs'>
        <span className='text-destructive'>{t('quotas.expanded.loadError')}</span>
        <Button type='button' size='sm' variant='outline' className='h-7 text-xs' onClick={() => refetch()}>
          {t('common.buttons.retry')}
        </Button>
      </div>
    );
  }

  if (sortedKeys.length === 0) {
    return (
      <div className='bg-muted/25 text-muted-foreground border-t px-6 py-3 text-xs'>
        {t('quotas.expanded.empty')}
      </div>
    );
  }

  return (
    <div className='bg-muted/25 border-t px-6 py-3'>
      {sharedWallet ? (
        <div className='text-muted-foreground mb-2.5 flex items-center gap-2 text-xs'>
          <Badge variant='secondary' className='px-1.5 py-0 text-[10px] font-medium'>
            {t('quotas.badges.shared')}
          </Badge>
          <span>{t('quotas.expanded.sharedWalletNote', { count: sortedKeys.length })}</span>
        </div>
      ) : null}

      <div className='divide-border/60 divide-y rounded-lg border bg-[var(--table-background)]'>
        {visibleKeys.map((item) => {
          const snap = item.balanceSnapshot;
          const isKeyRefreshing = refreshingKeyIDs.has(`${channelID}:${item.id}`);
          const probeErr =
            item.lastProbeStatus && item.lastProbeStatus !== 'ok' && item.lastProbeStatus !== 'unsupported'
              ? item.lastProbeError || item.lastProbeStatus
              : null;
          const resetAt = snap?.windows?.[0]?.resetAt ?? snap?.resetAt ?? item.quotaResetAt;
          const resetLabel = formatRelativeReset(resetAt, nowMs, t);
          const syncedAt = snap?.checkedAt ?? item.lastProbeAt ?? item.lastCheckedAt;
          const syncedAge = formatRelativeAge(syncedAt, nowMs, t);
          const attention = keyAttentionRank(item, nowMs) <= 1;
          const isExhausted = item.status === 'active' && isQuotaKeyExhausted(item, nowMs);

          return (
            <div
              key={item.id}
              data-testid='quota-expanded-key-row'
              className={cn(
                'grid grid-cols-1 items-center gap-2 px-3.5 py-2.5 text-xs md:grid-cols-[minmax(12rem,1.2fr)_5.5rem_minmax(14rem,1.6fr)_7rem_8.5rem_auto]',
                attention && 'bg-destructive/[0.03]'
              )}
            >
              <div className='flex min-w-0 items-center gap-2'>
                <code className='bg-muted truncate rounded px-1.5 py-0.5 font-mono text-[11px]'>
                  {item.maskedKey}
                </code>
                {item.policy?.name ? (
                  <span className='text-muted-foreground truncate text-[11px]'>{item.policy.name}</span>
                ) : null}
              </div>

              <div>
                <Badge
                  variant={isExhausted ? 'destructive' : item.status === 'active' ? 'outline' : 'secondary'}
                  className={cn(
                    'px-1.5 py-0 text-[10px] font-normal',
                    !isExhausted && item.status === 'active' && 'border-emerald-500/30 text-emerald-700 dark:text-emerald-400'
                  )}
                >
                  {isExhausted ? t('quotas.state.exhausted') : t(`channels.dialogs.keys.status.${item.status}`)}
                </Badge>
              </div>

              <div className='min-w-0'>
                {sharedWallet ? (
                  <span className='text-muted-foreground text-[11px]'>
                    {t('quotas.expanded.inheritsSharedWallet')}
                  </span>
                ) : (
                  <QuotaWindowBars
                    windows={snap?.windows}
                    walletBalance={snap?.primaryBalance}
                    walletUsed={snap?.used}
                    walletLimit={snap?.limit}
                    walletRemaining={snap?.remaining}
                    walletUnit={snap?.primaryBalance?.currency}
                    unlimited={snap?.unlimited}
                    freeTier={snap?.freeTier}
                    source={snap?.source}
                    planName={snap?.planName}
                    mode={mode}
                    showReset
                    nowMs={nowMs}
                  />
                )}
              </div>

              <div className='text-muted-foreground font-mono text-[11px] tabular-nums'>
                {resetLabel ? (
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <span className='cursor-default'>{resetLabel}</span>
                    </TooltipTrigger>
                    <TooltipContent>{formatExactTimestamp(resetAt, i18n.language)}</TooltipContent>
                  </Tooltip>
                ) : (
                  <span>—</span>
                )}
              </div>

              <div className='flex min-w-0 items-center gap-1.5'>
                {syncedAge ? (
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <span className='text-muted-foreground truncate text-[11px]'>{syncedAge}</span>
                    </TooltipTrigger>
                    <TooltipContent>{formatExactTimestamp(syncedAt, i18n.language)}</TooltipContent>
                  </Tooltip>
                ) : (
                  <span className='text-muted-foreground text-[11px]'>—</span>
                )}
                {probeErr ? (
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <span className='inline-flex shrink-0 items-center text-amber-600 dark:text-amber-400'>
                        <IconAlertTriangle className='h-3.5 w-3.5' />
                      </span>
                    </TooltipTrigger>
                    <TooltipContent className='max-w-xs break-words text-xs'>{probeErr}</TooltipContent>
                  </Tooltip>
                ) : null}
              </div>

              <div className='flex justify-end'>
                {canWrite && canRefresh && !sharedWallet ? (
                  <Button
                    type='button'
                    size='icon'
                    variant='ghost'
                    className='h-7 w-7'
                    disabled={isKeyRefreshing}
                    onClick={() => onRefreshKey(channelID, item.id)}
                    aria-label={t('quotas.actions.refreshKey')}
                  >
                    {isKeyRefreshing ? (
                      <IconLoader2 className='h-3.5 w-3.5 animate-spin' />
                    ) : (
                      <IconRefresh className='h-3.5 w-3.5' />
                    )}
                  </Button>
                ) : null}
              </div>
            </div>
          );
        })}
      </div>

      {sortedKeys.length > INITIAL_VISIBLE_QUOTA_KEYS ? (
        <div className='mt-2 flex items-center justify-between'>
          <Button
            type='button'
            size='sm'
            variant='ghost'
            className='text-muted-foreground hover:text-foreground h-7 px-2 text-xs'
            onClick={() => setShowAll((prev) => !prev)}
            data-testid='quota-toggle-more-keys'
          >
            {showAll ? (
              <>
                <IconChevronUp className='mr-1 h-3.5 w-3.5' />
                {t('quotas.expanded.collapseKeys')}
              </>
            ) : (
              <>
                <IconChevronDown className='mr-1 h-3.5 w-3.5' />
                {t('quotas.expanded.showMoreKeys', { count: hiddenCount })}
              </>
            )}
          </Button>
          <span className='text-muted-foreground font-mono text-[11px] tabular-nums'>
            {visibleKeys.length} / {sortedKeys.length}
          </span>
        </div>
      ) : null}
    </div>
  );
}
