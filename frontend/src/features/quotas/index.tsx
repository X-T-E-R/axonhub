import React, { useCallback, useMemo, useState } from 'react';
import {
  IconAlertTriangle,
  IconChevronDown,
  IconChevronRight,
  IconKey,
  IconLoader2,
  IconRefresh,
  IconSearch,
} from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { cn } from '@/lib/utils';
import { useDebounce } from '@/hooks/use-debounce';
import { usePermissions } from '@/hooks/usePermissions';
import { useSharedMinuteNow } from '@/lib/quota-clock';
import {
  formatExactTimestamp,
  formatRelativeAge,
  formatRelativeReset,
  getQuotaKeyAvailability,
  isAttentionQuotaOverview,
  isResettingSoon,
  quotaStateSortRank,
  type QuotaDisplayMode,
} from '@/lib/quota-format';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { TableSkeleton } from '@/components/ui/table-skeleton';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { Header } from '@/components/layout/header';
import { Main } from '@/components/layout/main';
import { QuotaWindowBars } from '@/components/quota-window-bars';
import { useChannelDetail, useRefreshChannelQuota } from '@/features/channels/data/channels';
import { CHANNEL_CONFIGS, getProvider } from '@/features/channels/data/config_channels';
import { ChannelsKeysDialog, type ChannelsKeysDialogTab } from '@/features/channels/components/channels-keys-dialog';
import { QuotaChannelKeysPanel } from './components/quota-channel-keys-panel';
import { useQuotaChannels, type QuotaChannelItem } from './data/quotas';

type QuotaFilter = 'all' | 'attention' | 'resetting';

function sourceLabelKey(source: string): string {
  switch (source) {
    case 'manual':
      return 'quotas.source.manual';
    case 'balance_probe':
      return 'quotas.source.balance_probe';
    case 'provider_quota':
      return 'quotas.source.provider_quota';
    default:
      return 'quotas.source.none';
  }
}

function canExpandChannelRow(item: QuotaChannelItem): boolean {
  const counts = item.quotaOverview?.counts;
  if (!counts) return false;
  if (counts.total > 1) return true;
  if (counts.total === 1 && item.quotaOverview?.walletScope !== 'provider_account') {
    return true;
  }
  return false;
}

function QuotaKeysDialogBridge({
  channelID,
  initialTab,
  onClose,
}: {
  channelID: string;
  initialTab: ChannelsKeysDialogTab;
  onClose: () => void;
}) {
  const { data: detail, isLoading } = useChannelDetail(channelID, { enabled: true });

  if (isLoading || !detail) {
    return null;
  }

  return (
    <ChannelsKeysDialog
      key={`quota-keys-${detail.id}-${initialTab}`}
      open
      initialTab={initialTab}
      currentRow={detail}
      onOpenChange={(isOpen) => {
        if (!isOpen) {
          onClose();
        }
      }}
    />
  );
}

export default function QuotasPage() {
  const { t, i18n } = useTranslation();
  const { channelPermissions } = usePermissions();
  const canWrite = channelPermissions.canWrite;

  const [filter, setFilter] = useState<QuotaFilter>('all');
  const [searchInput, setSearchInput] = useState('');
  const [mode, setMode] = useState<QuotaDisplayMode>('remaining');
  const [expandedIDs, setExpandedIDs] = useState<Set<string>>(new Set());
  const [refreshingChannelIDs, setRefreshingChannelIDs] = useState<Set<string>>(new Set());
  const [refreshingKeyIDs, setRefreshingKeyIDs] = useState<Set<string>>(new Set());
  const [retryAtByChannel, setRetryAtByChannel] = useState<Record<string, number>>({});
  const [keysDialogTarget, setKeysDialogTarget] = useState<{
    channelID: string;
    tab: ChannelsKeysDialogTab;
  } | null>(null);

  const debouncedSearch = useDebounce(searchInput, 250);
  const { data, isLoading } = useQuotaChannels({ search: debouncedSearch, includeDisabled: true });
  const refreshQuota = useRefreshChannelQuota();

  const items = data?.items ?? [];
  const hasVisibleClockNeeds = useMemo(
    () => items.some((item) => Boolean(item.quotaOverview?.nextResetAt || item.quotaOverview?.checkedAt)),
    [items]
  );
  const nowMs = useSharedMinuteNow(hasVisibleClockNeeds);

  const sortedItems = useMemo(() => {
    return [...items].sort((a, b) => {
      const oa = a.quotaOverview;
      const ob = b.quotaOverview;
      const attnA = isAttentionQuotaOverview(oa) ? 0 : 1;
      const attnB = isAttentionQuotaOverview(ob) ? 0 : 1;
      if (attnA !== attnB) return attnA - attnB;

      const statusA = a.status === 'enabled' ? 0 : 1;
      const statusB = b.status === 'enabled' ? 0 : 1;
      if (statusA !== statusB) return statusA - statusB;

      const rankA = quotaStateSortRank(oa?.state, Boolean(oa?.probeDiagnostic));
      const rankB = quotaStateSortRank(ob?.state, Boolean(ob?.probeDiagnostic));
      if (rankA !== rankB) return rankA - rankB;

      return a.name.localeCompare(b.name);
    });
  }, [items]);

  const counts = useMemo(() => {
    let attention = 0;
    let resetting = 0;
    for (const item of sortedItems) {
      if (isAttentionQuotaOverview(item.quotaOverview)) {
        attention++;
      }
      if (isResettingSoon(item.quotaOverview?.nextResetAt, nowMs)) {
        resetting++;
      }
    }
    return {
      all: sortedItems.length,
      attention,
      resetting,
    };
  }, [sortedItems, nowMs]);

  const filteredItems = useMemo(() => {
    if (filter === 'attention') {
      return sortedItems.filter((item) => isAttentionQuotaOverview(item.quotaOverview));
    }
    if (filter === 'resetting') {
      return sortedItems.filter((item) => isResettingSoon(item.quotaOverview?.nextResetAt, nowMs));
    }
    return sortedItems;
  }, [sortedItems, filter, nowMs]);

  const toggleExpand = useCallback((id: string) => {
    setExpandedIDs((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  }, []);

  const reportRefreshResult = useCallback(
    (channelID: string, result: Awaited<ReturnType<typeof refreshQuota.mutateAsync>>) => {
      if (result.retryAt) {
        const retryMs = Date.parse(result.retryAt);
        if (!Number.isNaN(retryMs) && retryMs > Date.now()) {
          setRetryAtByChannel((prev) => ({ ...prev, [channelID]: retryMs }));
        }
      }

      if (result.status === 'updated') {
        if (result.deferredKeys > 0) {
          toast.success(
            t('quotas.refresh.updatedWithDeferred', {
              updated: result.updatedKeys,
              deferred: result.deferredKeys,
            })
          );
        } else {
          toast.success(t('quotas.refresh.updated', { count: Math.max(1, result.updatedKeys) }));
        }
      } else if (result.status === 'deferred') {
        const retryStr = result.retryAt ? formatExactTimestamp(result.retryAt, i18n.language) : '';
        toast.info(
          retryStr
            ? t('quotas.refresh.deferredUntil', { time: retryStr })
            : result.message || t('quotas.refresh.deferred')
        );
      } else if (result.status === 'unsupported') {
        toast.info(result.message || t('quotas.refresh.unsupported'));
      } else {
        toast.error(result.message || t('quotas.refresh.error'));
      }
    },
    [i18n.language, refreshQuota, t]
  );

  const handleRefreshChannel = useCallback(
    async (channelID: string) => {
      setRefreshingChannelIDs((prev) => new Set(prev).add(channelID));
      try {
        const res = await refreshQuota.mutateAsync({ channelID });
        reportRefreshResult(channelID, res);
      } catch {
        // Error surfaced by hook.
      } finally {
        setRefreshingChannelIDs((prev) => {
          const next = new Set(prev);
          next.delete(channelID);
          return next;
        });
      }
    },
    [refreshQuota, reportRefreshResult]
  );

  const handleRefreshKey = useCallback(
    async (channelID: string, keyID: string) => {
      const token = `${channelID}:${keyID}`;
      setRefreshingKeyIDs((prev) => new Set(prev).add(token));
      try {
        const res = await refreshQuota.mutateAsync({ channelID, keyIDs: [keyID] });
        reportRefreshResult(channelID, res);
      } catch {
        // Error surfaced by hook.
      } finally {
        setRefreshingKeyIDs((prev) => {
          const next = new Set(prev);
          next.delete(token);
          return next;
        });
      }
    },
    [refreshQuota, reportRefreshResult]
  );

  return (
    <>
      <Header fixed>
        <div className='flex w-full flex-1 flex-col gap-1 md:flex-row md:items-center md:justify-between'>
          <div className='min-w-0'>
            <h2 className='text-xl font-bold tracking-tight'>{t('quotas.title')}</h2>
            <p className='text-muted-foreground text-sm'>{t('quotas.description')}</p>
          </div>
        </div>
      </Header>

      <Main fixed>
        <div className='flex flex-1 flex-col overflow-hidden'>
          {/* Single compact control bar — no KPI card wall */}
          <div className='flex flex-wrap items-center justify-between gap-3'>
            <div className='flex flex-wrap items-center gap-1.5' role='tablist' aria-label={t('quotas.filters.ariaLabel')}>
              <Button
                type='button'
                size='sm'
                variant={filter === 'all' ? 'default' : 'outline'}
                className='h-8 rounded-full px-3 text-xs'
                onClick={() => setFilter('all')}
              >
                {t('quotas.filters.all')}
                <span className='ml-1.5 font-mono opacity-75'>{counts.all}</span>
              </Button>
              <Button
                type='button'
                size='sm'
                variant={filter === 'attention' ? 'default' : 'outline'}
                className={cn(
                  'h-8 rounded-full px-3 text-xs',
                  filter !== 'attention' && counts.attention > 0 && 'border-amber-500/40 text-amber-700 dark:text-amber-400'
                )}
                onClick={() => setFilter('attention')}
              >
                {counts.attention > 0 ? (
                  <span
                    className={cn(
                      'mr-1.5 inline-block h-1.5 w-1.5 rounded-full',
                      filter === 'attention' ? 'bg-primary-foreground' : 'bg-destructive'
                    )}
                    aria-hidden='true'
                  />
                ) : null}
                {t('quotas.filters.attention')}
                <span className='ml-1.5 font-mono opacity-80'>{counts.attention}</span>
              </Button>
              <Button
                type='button'
                size='sm'
                variant={filter === 'resetting' ? 'default' : 'outline'}
                className='h-8 rounded-full px-3 text-xs'
                onClick={() => setFilter('resetting')}
              >
                {t('quotas.filters.resettingSoon')}
                <span className='ml-1.5 font-mono opacity-75'>{counts.resetting}</span>
              </Button>
            </div>

            <div className='flex items-center gap-2'>
              <div className='relative w-56 sm:w-64'>
                <IconSearch className='text-muted-foreground pointer-events-none absolute top-1/2 left-2.5 h-3.5 w-3.5 -translate-y-1/2' />
                <Input
                  value={searchInput}
                  onChange={(e) => setSearchInput(e.target.value)}
                  placeholder={t('quotas.searchPlaceholder')}
                  className='h-8 pl-8 text-xs'
                  aria-label={t('quotas.searchPlaceholder')}
                />
              </div>

              <div className='bg-muted/60 inline-flex items-center rounded-lg border p-0.5'>
                <button
                  type='button'
                  className={cn(
                    'rounded-md px-2.5 py-1 text-xs font-medium transition-colors',
                    mode === 'remaining'
                      ? 'bg-background text-foreground shadow-xs'
                      : 'text-muted-foreground hover:text-foreground'
                  )}
                  onClick={() => setMode('remaining')}
                  aria-pressed={mode === 'remaining'}
                >
                  {t('quotas.mode.remaining')}
                </button>
                <button
                  type='button'
                  className={cn(
                    'rounded-md px-2.5 py-1 text-xs font-medium transition-colors',
                    mode === 'used'
                      ? 'bg-background text-foreground shadow-xs'
                      : 'text-muted-foreground hover:text-foreground'
                  )}
                  onClick={() => setMode('used')}
                  aria-pressed={mode === 'used'}
                >
                  {t('quotas.mode.used')}
                </button>
              </div>
            </div>
          </div>

          {/* Primary high-signal quota ledger table */}
          <div className='shadow-soft relative mt-4 flex-1 overflow-auto rounded-2xl border border-[var(--table-border)]'>
            <Table data-testid='quotas-table' className='border-separate border-spacing-0 rounded-2xl bg-[var(--table-background)]'>
              <TableHeader className='sticky top-0 z-20 bg-[var(--table-header)] shadow-xs'>
                <TableRow className='border-0'>
                  <TableHead className='w-9 border-0' />
                  <TableHead className='text-muted-foreground border-0 text-xs font-semibold tracking-wider uppercase'>
                    {t('quotas.columns.channel')}
                  </TableHead>
                  <TableHead className='text-muted-foreground w-36 border-0 text-xs font-semibold tracking-wider uppercase'>
                    {t('quotas.columns.keys')}
                  </TableHead>
                  <TableHead className='text-muted-foreground min-w-64 border-0 text-xs font-semibold tracking-wider uppercase'>
                    {t('quotas.columns.quota')}
                  </TableHead>
                  <TableHead className='text-muted-foreground w-32 border-0 text-xs font-semibold tracking-wider uppercase'>
                    {t('quotas.columns.nextReset')}
                  </TableHead>
                  <TableHead className='text-muted-foreground w-48 border-0 text-xs font-semibold tracking-wider uppercase'>
                    {t('quotas.columns.synced')}
                  </TableHead>
                  <TableHead className='text-muted-foreground w-24 border-0 text-right text-xs font-semibold tracking-wider uppercase'>
                    {t('common.columns.actions')}
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody className='!bg-[var(--table-background)]'>
                {isLoading ? (
                  <TableSkeleton rows={8} columns={7} />
                ) : filteredItems.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={7} className='text-muted-foreground h-28 text-center text-sm'>
                      {t('quotas.empty')}
                    </TableCell>
                  </TableRow>
                ) : (
                  filteredItems.map((item) => {
                    const overview = item.quotaOverview;
                    const expandable = canExpandChannelRow(item);
                    const isExpanded = expandable && expandedIDs.has(item.id);
                    const isAttention = isAttentionQuotaOverview(overview);
                    const isRefreshing = refreshingChannelIDs.has(item.id);
                    const retryAtMs = retryAtByChannel[item.id];
                    const inRetryCooldown = retryAtMs != null && retryAtMs > nowMs;
                    const config = CHANNEL_CONFIGS[item.type];
                    const provider = getProvider(item.type);
                    const IconComponent = config?.icon;

                    const nextResetLabel = formatRelativeReset(overview?.nextResetAt, nowMs, t);
                    const syncedAge = formatRelativeAge(overview?.checkedAt, nowMs, t);
                    const diag = overview?.probeDiagnostic;
                    const showDiagnostic = Boolean(diag && diag.status !== 'ok' && diag.status !== 'unsupported');

                    return (
                      <React.Fragment key={item.id}>
                        <TableRow
                          data-testid={`quota-row-${item.name}`}
                          className={cn(
                            'border-b transition-colors hover:bg-muted/35',
                            expandable && 'cursor-pointer',
                            isAttention && 'bg-destructive/[0.025]',
                            item.status === 'disabled' && 'opacity-70'
                          )}
                          onClick={() => {
                            if (expandable) {
                              toggleExpand(item.id);
                            }
                          }}
                        >
                          <TableCell className='w-9 pr-0 pl-3'>
                            {expandable ? (
                              <Button
                                type='button'
                                variant='ghost'
                                size='icon'
                                className='h-6 w-6'
                                onClick={(e) => {
                                  e.stopPropagation();
                                  toggleExpand(item.id);
                                }}
                                aria-label={isExpanded ? t('quotas.actions.collapse') : t('quotas.actions.expand')}
                                aria-expanded={isExpanded}
                              >
                                {isExpanded ? (
                                  <IconChevronDown className='h-3.5 w-3.5' />
                                ) : (
                                  <IconChevronRight className='h-3.5 w-3.5' />
                                )}
                              </Button>
                            ) : null}
                          </TableCell>

                          <TableCell className='py-3'>
                            <div className='flex min-w-0 flex-col gap-1'>
                              <div className='flex flex-wrap items-center gap-2'>
                                <span
                                  className={cn(
                                    'truncate font-medium',
                                    overview?.state === 'exhausted' && 'text-red-700 dark:text-red-400'
                                  )}
                                >
                                  {item.name}
                                </span>
                                {item.settings?.rawHTTP?.enabled ? (
                                  <Badge variant='outline' className='h-5 px-1.5 text-[10px] font-normal'>
                                    HTTP API
                                  </Badge>
                                ) : IconComponent ? (
                                  <Badge variant='outline' className={cn('h-5 gap-1 px-1.5 text-[10px] font-normal', config.color)}>
                                    <IconComponent size={12} className='shrink-0' />
                                    <span>{t(`channels.providers.${provider}`)}</span>
                                  </Badge>
                                ) : null}
                                {item.status === 'disabled' ? (
                                  <Badge variant='secondary' className='h-5 px-1.5 text-[10px] font-normal'>
                                    {t('channels.status.disabled')}
                                  </Badge>
                                ) : null}
                              </div>
                            </div>
                          </TableCell>

                          <TableCell className='py-3'>
                            {overview ? (
                              (() => {
                                const avail = getQuotaKeyAvailability(overview.counts);
                                return (
                                  <div className='flex flex-col gap-0.5'>
                                    <span
                                      className={cn(
                                        'font-mono text-xs font-medium tabular-nums',
                                        avail.total > 0 && avail.schedulable === 0 && 'text-red-700 dark:text-red-400'
                                      )}
                                    >
                                      {avail.schedulable}/{avail.total}
                                    </span>
                                    <div className='flex flex-wrap items-center gap-1.5'>
                                      {avail.exhausted > 0 ? (
                                        <span className='text-[11px] font-medium text-red-700 dark:text-red-400'>
                                          {t('quotas.counts.exhausted', { count: avail.exhausted })}
                                        </span>
                                      ) : null}
                                      {avail.disabled > 0 ? (
                                        <span className='text-muted-foreground text-[11px]'>
                                          {t('quotas.counts.disabled', { count: avail.disabled })}
                                        </span>
                                      ) : null}
                                      {avail.exhausted === 0 &&
                                      avail.disabled === 0 &&
                                      avail.unknown > 0 &&
                                      overview.counts.freshKnown === 0 ? (
                                        <span className='text-muted-foreground text-[11px]'>
                                          {t('quotas.counts.unknown', { count: avail.unknown })}
                                        </span>
                                      ) : null}
                                    </div>
                                  </div>
                                );
                              })()
                            ) : (
                              <span className='text-muted-foreground text-xs'>—</span>
                            )}
                          </TableCell>

                          <TableCell className='py-3'>
                            {overview ? (
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
                                mode={mode}
                                nowMs={nowMs}
                              />
                            ) : (
                              <span className='text-muted-foreground text-xs'>—</span>
                            )}
                          </TableCell>

                          <TableCell className='py-3'>
                            {nextResetLabel ? (
                              <Tooltip>
                                <TooltipTrigger asChild>
                                  <span className='text-foreground cursor-default font-mono text-xs tabular-nums'>
                                    {nextResetLabel}
                                  </span>
                                </TooltipTrigger>
                                <TooltipContent>
                                  {formatExactTimestamp(overview?.nextResetAt, i18n.language)}
                                </TooltipContent>
                              </Tooltip>
                            ) : (
                              <span className='text-muted-foreground text-xs'>—</span>
                            )}
                          </TableCell>

                          <TableCell className='py-3'>
                            <div className='flex flex-col gap-0.5'>
                              <div className='flex items-center gap-1.5 text-xs'>
                                {syncedAge ? (
                                  <Tooltip>
                                    <TooltipTrigger asChild>
                                      <span className='text-foreground cursor-default tabular-nums'>{syncedAge}</span>
                                    </TooltipTrigger>
                                    <TooltipContent>
                                      {formatExactTimestamp(overview?.checkedAt, i18n.language)}
                                    </TooltipContent>
                                  </Tooltip>
                                ) : (
                                  <span className='text-muted-foreground'>—</span>
                                )}
                                {overview?.source && overview.source !== 'none' ? (
                                  <span className='text-muted-foreground text-[11px]'>
                                    · {t(sourceLabelKey(overview.source))}
                                  </span>
                                ) : null}
                              </div>
                              {showDiagnostic ? (
                                <Tooltip>
                                  <TooltipTrigger asChild>
                                    <div className='inline-flex max-w-44 cursor-default items-center gap-1 text-[11px] text-amber-800 dark:text-amber-300'>
                                      <IconAlertTriangle className='h-3 w-3 shrink-0' />
                                      <span className='truncate'>
                                        {diag?.error || t('quotas.diagnostic.probeFailed')}
                                      </span>
                                    </div>
                                  </TooltipTrigger>
                                  <TooltipContent className='max-w-xs space-y-1 text-xs'>
                                    <div className='font-medium'>{t('quotas.diagnostic.title')}</div>
                                    <div className='break-words'>{diag?.error || diag?.status}</div>
                                    {diag?.nextProbeAt ? (
                                      <div className='text-muted-foreground'>
                                        {t('quotas.diagnostic.nextProbeAt', {
                                          time: formatExactTimestamp(diag.nextProbeAt, i18n.language),
                                        })}
                                      </div>
                                    ) : null}
                                  </TooltipContent>
                                </Tooltip>
                              ) : null}
                            </div>
                          </TableCell>

                          <TableCell
                            className='py-3 text-right'
                            onClick={(e) => e.stopPropagation()}
                          >
                            {canWrite ? (
                              <div className='flex items-center justify-end gap-1'>
                                <Tooltip>
                                  <TooltipTrigger asChild>
                                    <Button
                                      type='button'
                                      size='icon'
                                      variant='ghost'
                                      className='h-7 w-7'
                                      disabled={!overview?.canRefresh || isRefreshing || inRetryCooldown}
                                      onClick={() => handleRefreshChannel(item.id)}
                                      aria-label={t('quotas.actions.refresh')}
                                    >
                                      {isRefreshing ? (
                                        <IconLoader2 className='h-3.5 w-3.5 animate-spin' />
                                      ) : (
                                        <IconRefresh className='h-3.5 w-3.5' />
                                      )}
                                    </Button>
                                  </TooltipTrigger>
                                  <TooltipContent>
                                    {!overview?.canRefresh
                                      ? t('quotas.refresh.unsupported')
                                      : inRetryCooldown
                                        ? t('quotas.refresh.deferredUntil', {
                                            time: formatExactTimestamp(new Date(retryAtMs).toISOString(), i18n.language),
                                          })
                                        : t('quotas.actions.refresh')}
                                  </TooltipContent>
                                </Tooltip>

                                <Tooltip>
                                  <TooltipTrigger asChild>
                                    <Button
                                      type='button'
                                      size='icon'
                                      variant='ghost'
                                      className='h-7 w-7'
                                      onClick={() =>
                                        setKeysDialogTarget({
                                          channelID: item.id,
                                          tab: overview?.source === 'none' ? 'balanceProbe' : 'inventory',
                                        })
                                      }
                                      aria-label={t('quotas.actions.openKeysAndProbe')}
                                    >
                                      <IconKey className='h-3.5 w-3.5' />
                                    </Button>
                                  </TooltipTrigger>
                                  <TooltipContent>{t('quotas.actions.openKeysAndProbe')}</TooltipContent>
                                </Tooltip>
                              </div>
                            ) : null}
                          </TableCell>
                        </TableRow>

                        {isExpanded ? (
                          <TableRow key={`${item.id}-expanded`} className='border-0 hover:bg-transparent'>
                            <TableCell colSpan={7} className='border-0 p-0'>
                              <QuotaChannelKeysPanel
                                channelID={item.id}
                                sharedWallet={Boolean(overview?.sharedWallet)}
                                canRefresh={Boolean(overview?.canRefresh)}
                                canWrite={canWrite}
                                mode={mode}
                                nowMs={nowMs}
                                refreshingKeyIDs={refreshingKeyIDs}
                                onRefreshKey={handleRefreshKey}
                              />
                            </TableCell>
                          </TableRow>
                        ) : null}
                      </React.Fragment>
                    );
                  })
                )}
              </TableBody>
            </Table>
          </div>
        </div>
      </Main>

      {keysDialogTarget ? (
        <QuotaKeysDialogBridge
          channelID={keysDialogTarget.channelID}
          initialTab={keysDialogTarget.tab}
          onClose={() => setKeysDialogTarget(null)}
        />
      ) : null}
    </>
  );
}
