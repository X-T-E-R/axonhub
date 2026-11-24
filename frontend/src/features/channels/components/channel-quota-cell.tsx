import { memo, useCallback } from 'react';
import { IconAlertTriangle } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import { formatExactTimestamp, formatRelativeReset, getQuotaKeyAvailability } from '@/lib/quota-format';
import { useSharedMinuteNow } from '@/lib/quota-clock';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { QuotaWindowBars } from '@/components/quota-window-bars';
import { useChannels } from '../context/channels-context';
import type { Channel } from '../data/schema';

interface ChannelQuotaCellProps {
  channel: Channel;
  canWrite: boolean;
}

export const ChannelQuotaCell = memo(({ channel, canWrite }: ChannelQuotaCellProps) => {
  const { t, i18n } = useTranslation();
  const { setOpen, setCurrentRow } = useChannels();
  const overview = channel.quotaOverview;
  const nowMs = useSharedMinuteNow(Boolean(overview?.nextResetAt));

  const handleOpenKeys = useCallback(
    (e: React.MouseEvent) => {
      e.stopPropagation();
      if (!canWrite) return;
      setCurrentRow(channel);
      setOpen('keys');
    },
    [canWrite, channel, setCurrentRow, setOpen]
  );

  if (!overview) {
    return (
      <div className='flex justify-center'>
        <span className='text-muted-foreground text-xs'>—</span>
      </div>
    );
  }

  const diag = overview.probeDiagnostic;
  const hasDiagnostic = Boolean(diag && diag.status !== 'ok' && diag.status !== 'unsupported');
  const nextReset = formatRelativeReset(overview.nextResetAt, nowMs, t);
  const avail = getQuotaKeyAvailability(overview.counts);
  const showMetaLine =
    avail.total > 0 || avail.exhausted > 0 || avail.disabled > 0 || Boolean(nextReset) || hasDiagnostic;

  const content = (
    <div
      className={cn(
        'inline-flex max-w-52 flex-col items-start gap-1 rounded-md px-1.5 py-1 text-left transition-colors',
        canWrite && 'hover:bg-muted/60 cursor-pointer'
      )}
      onClick={handleOpenKeys}
      role={canWrite ? 'button' : undefined}
      tabIndex={canWrite ? 0 : undefined}
      onKeyDown={
        canWrite
          ? (e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                e.stopPropagation();
                setCurrentRow(channel);
                setOpen('keys');
              }
            }
          : undefined
      }
      data-testid='channel-quota-cell'
    >
      <QuotaWindowBars
        windows={overview.windows}
        walletBalance={overview.walletBalance}
        walletUsed={overview.walletUsed}
        walletLimit={overview.walletLimit}
        walletRemaining={overview.walletRemaining}
        walletUnit={overview.walletUnit}
        state={overview.state}
        unlimited={overview.unlimited}
        freeTier={overview.freeTier}
        sharedWallet={overview.sharedWallet}
        planName={overview.planName}
        source={overview.source}
        mode='remaining'
        compact
        maxWindows={2}
        nowMs={nowMs}
      />

      {showMetaLine ? (
        <div className='text-muted-foreground flex flex-wrap items-center gap-1.5 text-[10px] leading-none'>
          {avail.total > 0 ? (
            <span className={cn('font-mono tabular-nums', avail.schedulable === 0 && 'font-medium text-red-700 dark:text-red-400')}>
              {avail.schedulable}/{avail.total}
            </span>
          ) : null}
          {avail.exhausted > 0 ? (
            <span className='font-medium text-red-700 dark:text-red-400'>
              {t('quotas.counts.exhausted', { count: avail.exhausted })}
            </span>
          ) : null}
          {avail.disabled > 0 ? (
            <span>{t('quotas.counts.disabled', { count: avail.disabled })}</span>
          ) : null}
          {nextReset ? (
            <span className='font-mono tabular-nums'>{nextReset}</span>
          ) : null}
          {hasDiagnostic ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <span className='inline-flex items-center text-amber-600 dark:text-amber-400'>
                  <IconAlertTriangle className='h-3 w-3' />
                </span>
              </TooltipTrigger>
              <TooltipContent className='max-w-xs break-words text-xs'>
                <div>{diag?.error || diag?.status}</div>
                {overview.checkedAt ? (
                  <div className='text-muted-foreground mt-0.5'>
                    {formatExactTimestamp(overview.checkedAt, i18n.language)}
                  </div>
                ) : null}
              </TooltipContent>
            </Tooltip>
          ) : null}
        </div>
      ) : null}
    </div>
  );

  return <div className='flex justify-center'>{content}</div>;
});

ChannelQuotaCell.displayName = 'ChannelQuotaCell';
