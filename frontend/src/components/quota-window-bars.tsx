import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import {
  formatExactTimestamp,
  formatQuotaAmount,
  formatRelativeReset,
  hasRealWalletMeter,
  hasRealWindowMeter,
  isWindowExhausted,
  resolveWalletAmount,
  shortWindowLabel,
  windowUsedRatio,
  type QuotaDisplayMode,
} from '@/lib/quota-format';
import { Badge } from '@/components/ui/badge';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import type {
  BalanceAmount,
  ChannelQuotaState,
  ChannelQuotaWindow,
} from '@/features/channels/data/schema';

export interface QuotaWindowBarsProps {
  windows?: ChannelQuotaWindow[] | null;
  walletBalance?: BalanceAmount | null;
  walletUsed?: number | null;
  walletLimit?: number | null;
  walletRemaining?: number | null;
  walletUnit?: string | null;
  state?: ChannelQuotaState | null;
  unlimited?: boolean | null;
  freeTier?: boolean | null;
  sharedWallet?: boolean | null;
  planName?: string | null;
  source?: string | null;
  mode?: QuotaDisplayMode;
  compact?: boolean;
  maxWindows?: number;
  showReset?: boolean;
  nowMs?: number;
  className?: string;
}

function barColorClass(usedRatio: number, exhausted: boolean): string {
  if (exhausted || usedRatio >= 1) {
    return 'bg-destructive';
  }
  if (usedRatio >= 0.8) {
    return 'bg-amber-500';
  }
  return 'bg-emerald-500';
}

function valueToneClass(usedRatio: number, exhausted: boolean): string {
  if (exhausted || usedRatio >= 1) {
    return 'text-red-700 dark:text-red-400 font-semibold';
  }
  if (usedRatio >= 0.8) {
    return 'text-amber-800 dark:text-amber-300 font-medium';
  }
  return 'text-foreground';
}

function formatPercentValue(ratio: number): string {
  const pct = Math.round(ratio * 100);
  if (pct > 999) return '>999%';
  return `${Math.max(0, pct)}%`;
}

export function QuotaWindowBars({
  windows,
  walletBalance,
  walletUsed,
  walletLimit,
  walletRemaining,
  walletUnit,
  state,
  unlimited,
  freeTier,
  sharedWallet,
  planName,
  source,
  mode = 'remaining',
  compact = false,
  maxWindows,
  showReset = false,
  nowMs,
  className,
}: QuotaWindowBarsProps) {
  const { t, i18n } = useTranslation();
  const effectiveNow = nowMs ?? Date.now();
  const visibleWindows = (windows ?? []).slice(0, maxWindows ?? (compact ? 2 : 6));
  const extraWindowCount = Math.max(0, (windows?.length ?? 0) - visibleWindows.length);

  const resolvedWallet = resolveWalletAmount(walletBalance, walletRemaining, walletUnit);
  const effectiveUnit = walletUnit || walletBalance?.currency || '';
  const hasWalletMeter = hasRealWalletMeter({
    unlimited,
    freeTier,
    walletLimit,
    walletUsed,
    walletRemaining,
  });

  // 1. Unlimited — never draw a fabricated 100% bar.
  if (unlimited || state === 'unlimited') {
    return (
      <div className={cn('flex flex-wrap items-center gap-1.5', className)}>
        <Badge
          variant='outline'
          className='border-emerald-500/30 bg-emerald-500/10 px-1.5 py-0 text-[11px] font-medium text-emerald-700 dark:text-emerald-400'
        >
          {t('quotas.state.unlimited')}
        </Badge>
        {resolvedWallet && resolvedWallet.amount > 0 ? (
          <span className='text-muted-foreground font-mono text-xs tabular-nums'>
            {formatQuotaAmount(resolvedWallet.amount, resolvedWallet.unit, t, i18n.language)}
          </span>
        ) : null}
        {sharedWallet ? (
          <Badge variant='secondary' className='px-1.5 py-0 text-[10px] font-normal'>
            {t('quotas.badges.shared')}
          </Badge>
        ) : null}
      </div>
    );
  }

  // 2. Free tier — never misreport usage as remaining balance, never draw a fake bar.
  if (freeTier || state === 'free_tier') {
    return (
      <div className={cn('flex flex-wrap items-center gap-1.5', className)}>
        <Badge variant='secondary' className='px-1.5 py-0 text-[11px] font-medium'>
          {t('quotas.state.free_tier')}
        </Badge>
        {walletUsed != null && Number.isFinite(walletUsed) && walletUsed > 0 ? (
          <span className='text-muted-foreground font-mono text-xs tabular-nums'>
            {t('quotas.labels.usedInline', {
              amount: formatQuotaAmount(walletUsed, effectiveUnit || 'USD', t, i18n.language),
            })}
          </span>
        ) : null}
      </div>
    );
  }

  // 3. Multi-window quotas (e.g. Kimi 5h + 7d, CommandCode, StepFun, Codex/Claude)
  if (visibleWindows.length > 0) {
    return (
      <div className={cn('flex flex-col gap-1', className)}>
        {visibleWindows.map((win) => {
          const hasMeter = hasRealWindowMeter(win);
          const usedRatio = windowUsedRatio(win, effectiveNow);
          const exhausted = isWindowExhausted(win, effectiveNow);
          const displayRatio =
            usedRatio != null ? (mode === 'remaining' ? Math.max(0, 1 - usedRatio) : usedRatio) : null;
          const fillWidth =
            usedRatio != null
              ? Math.max(0, Math.min(100, (mode === 'remaining' ? Math.max(0, 1 - usedRatio) : usedRatio) * 100))
              : 0;
          const resetLabel = showReset ? formatRelativeReset(win.resetAt, effectiveNow, t) : null;
          const tag = shortWindowLabel(win);

          let figureText = '-';
          if (win.limit != null && win.limit > 0 && win.unit && win.unit !== '%') {
            const primaryNum =
              mode === 'remaining'
                ? Math.max(0, win.remaining ?? Math.max(0, win.limit - (win.used ?? 0)))
                : (win.used ?? Math.max(0, win.limit - (win.remaining ?? 0)));
            figureText = compact
              ? displayRatio != null
                ? formatPercentValue(displayRatio)
                : formatQuotaAmount(primaryNum, win.unit, t, i18n.language)
              : `${formatQuotaAmount(primaryNum, win.unit, t, i18n.language)} / ${formatQuotaAmount(win.limit, win.unit, t, i18n.language)}`;
          } else if (displayRatio != null) {
            figureText = formatPercentValue(displayRatio);
          } else if (win.remaining != null) {
            figureText = formatQuotaAmount(win.remaining, win.unit, t, i18n.language);
          } else if (win.used != null) {
            figureText = formatQuotaAmount(win.used, win.unit, t, i18n.language);
          }

          const row = (
            <div className='flex items-center gap-1.5 text-xs leading-tight'>
              <span
                className={cn(
                  'bg-muted text-muted-foreground shrink-0 rounded px-1.5 py-0.5 text-center font-mono text-[10px] font-medium',
                  compact ? 'min-w-[2rem]' : 'min-w-[2.4rem]'
                )}
              >
                {tag}
              </span>
              {hasMeter && usedRatio != null ? (
                <div
                  className={cn(
                    'bg-muted/80 h-1.5 shrink-0 overflow-hidden rounded-full',
                    compact ? 'w-12' : 'w-20'
                  )}
                >
                  <div
                    className={cn('h-full rounded-full transition-all duration-300', barColorClass(usedRatio, exhausted))}
                    style={{ width: `${fillWidth}%` }}
                  />
                </div>
              ) : null}
              <span
                className={cn(
                  'shrink-0 font-mono text-[11px] tabular-nums',
                  usedRatio != null ? valueToneClass(usedRatio, exhausted) : 'text-foreground'
                )}
              >
                {figureText}
              </span>
              {resetLabel ? (
                <span className='text-muted-foreground shrink-0 font-mono text-[10px] tabular-nums'>
                  {resetLabel}
                </span>
              ) : null}
            </div>
          );

          return (
            <Tooltip key={`${win.key}-${win.label}`}>
              <TooltipTrigger asChild>{row}</TooltipTrigger>
              <TooltipContent side='top' className='max-w-xs space-y-0.5 text-xs'>
                <div className='font-medium'>{win.label || win.key}</div>
                {win.used != null && win.limit != null && win.limit > 0 ? (
                  <div>
                    {t('quotas.tooltip.usedOfLimit', {
                      used: formatQuotaAmount(win.used, win.unit, t, i18n.language),
                      limit: formatQuotaAmount(win.limit, win.unit, t, i18n.language),
                    })}
                  </div>
                ) : null}
                {usedRatio != null ? (
                  <div>
                    {t('quotas.tooltip.percentPair', {
                      remaining: formatPercentValue(Math.max(0, 1 - usedRatio)),
                      used: formatPercentValue(usedRatio),
                    })}
                  </div>
                ) : null}
                {win.resetAt ? (
                  <div>
                    {t('quotas.tooltip.resetsAt', {
                      time: formatExactTimestamp(win.resetAt, i18n.language),
                    })}
                  </div>
                ) : null}
              </TooltipContent>
            </Tooltip>
          );
        })}

        {(extraWindowCount > 0 || (resolvedWallet && !compact)) ? (
          <div className='text-muted-foreground flex flex-wrap items-center gap-1.5 text-[11px]'>
            {extraWindowCount > 0 ? (
              <span className='font-mono'>+{extraWindowCount}</span>
            ) : null}
            {resolvedWallet && !compact ? (
              <span className='font-mono tabular-nums'>
                {formatQuotaAmount(resolvedWallet.amount, resolvedWallet.unit, t, i18n.language)}
              </span>
            ) : null}
            {planName && !compact ? <span className='truncate'>{planName}</span> : null}
          </div>
        ) : null}
      </div>
    );
  }

  // 4. Metered wallet with real denominator (e.g. Tavily 1001/1000, manual $100 ledger, prepaid cap)
  if (hasWalletMeter && walletLimit != null && walletLimit > 0) {
    const used = walletUsed ?? Math.max(0, walletLimit - (walletRemaining ?? 0));
    const remaining = walletRemaining ?? Math.max(0, walletLimit - used);
    const usedRatio = Math.max(0, used / walletLimit);
    const exhausted = remaining <= 0 || used >= walletLimit;
    const fillWidth = Math.max(0, Math.min(100, (mode === 'remaining' ? Math.max(0, 1 - usedRatio) : usedRatio) * 100));

    const primaryStr =
      mode === 'remaining'
        ? `${formatQuotaAmount(Math.max(0, remaining), effectiveUnit, t, i18n.language)} / ${formatQuotaAmount(walletLimit, effectiveUnit, t, i18n.language)}`
        : `${formatQuotaAmount(used, effectiveUnit, t, i18n.language)} / ${formatQuotaAmount(walletLimit, effectiveUnit, t, i18n.language)}`;

    return (
      <div className={cn('flex flex-col gap-1', className)}>
        <div className='flex items-center gap-2'>
          <div className={cn('bg-muted/80 h-1.5 shrink-0 overflow-hidden rounded-full', compact ? 'w-12' : 'w-20')}>
            <div
              className={cn('h-full rounded-full transition-all duration-300', barColorClass(usedRatio, exhausted))}
              style={{ width: `${fillWidth}%` }}
            />
          </div>
          <span className={cn('font-mono text-xs tabular-nums', valueToneClass(usedRatio, exhausted))}>
            {primaryStr}
          </span>
        </div>
        {!compact && (sharedWallet || source === 'manual') ? (
          <div className='flex items-center gap-1.5'>
            {sharedWallet ? (
              <Badge variant='secondary' className='px-1.5 py-0 text-[10px] font-normal'>
                {t('quotas.badges.shared')}
              </Badge>
            ) : null}
            {source === 'manual' ? (
              <Badge variant='outline' className='px-1.5 py-0 text-[10px] font-normal'>
                {t('quotas.badges.manual')}
              </Badge>
            ) : null}
          </div>
        ) : null}
      </div>
    );
  }

  // 5. Unbounded balance figure (no denominator -> clean number ONLY, NO fake 100% bar!)
  if (resolvedWallet) {
    const isZero = resolvedWallet.amount <= 0;
    return (
      <div className={cn('flex flex-wrap items-center gap-1.5', className)}>
        <span
          className={cn(
            'font-mono text-xs tabular-nums',
            isZero || state === 'exhausted' ? 'text-destructive font-semibold' : 'text-foreground font-medium'
          )}
        >
          {formatQuotaAmount(resolvedWallet.amount, resolvedWallet.unit, t, i18n.language)}
        </span>
        {sharedWallet ? (
          <Badge variant='secondary' className='px-1.5 py-0 text-[10px] font-normal'>
            {t('quotas.badges.shared')}
          </Badge>
        ) : null}
        {source === 'manual' ? (
          <Badge variant='outline' className='px-1.5 py-0 text-[10px] font-normal'>
            {t('quotas.badges.manual')}
          </Badge>
        ) : null}
      </div>
    );
  }

  // 6. Unknown / Unsupported / Error without valid fact
  if (state === 'unsupported') {
    return <span className={cn('text-muted-foreground text-xs', className)}>{compact ? '—' : t('quotas.state.unsupported')}</span>;
  }
  if (state === 'error') {
    return <span className={cn('text-amber-600 dark:text-amber-400 text-xs', className)}>{t('quotas.state.error')}</span>;
  }

  return <span className={cn('text-muted-foreground text-xs', className)}>—</span>;
}
