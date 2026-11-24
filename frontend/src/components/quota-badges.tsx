import { useEffect, useMemo, useState } from 'react';
import { Link, useRouterState } from '@tanstack/react-router';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import {
  ArrowRight,
  Battery,
  BatteryFull,
  BatteryLow,
  BatteryMedium,
  BatteryWarning,
  Loader2,
  RefreshCw,
  Zap,
} from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { z } from 'zod';
import { graphqlRequest } from '@/gql/graphql';
import { cn } from '@/lib/utils';
import { usePermissions } from '@/hooks/usePermissions';
import { useSharedMinuteNow } from '@/lib/quota-clock';
import {
  formatExactTimestamp,
  formatRelativeReset,
  getQuotaKeyAvailability,
  hasRealWalletMeter,
  hasRealWindowMeter,
  isAttentionQuotaOverview,
  quotaStateSortRank,
} from '@/lib/quota-format';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { QuotaWindowBars } from '@/components/quota-window-bars';
import { CHANNEL_QUOTA_OVERVIEW_FIELDS, useRefreshChannelQuota } from '@/features/channels/data/channels';
import { channelQuotaOverviewSchema, type ChannelQuotaOverview } from '@/features/channels/data/schema';
import { resetChannelQuotaNow } from '@/features/system/data/quotas';

const MAX_POPOVER_CHANNELS = 6;

const quotaBadgeChannelSchema = z.object({
  id: z.string(),
  name: z.string(),
  type: z.string(),
  status: z.string(),
  quotaOverview: channelQuotaOverviewSchema.optional().nullable(),
});

type QuotaBadgeChannel = z.infer<typeof quotaBadgeChannelSchema>;

const QUERY_QUOTA_BADGES_QUERY = `
  query QueryQuotaBadgesOverview($input: QueryChannelInput!) {
    queryChannels(input: $input) {
      edges {
        node {
          id
          name
          type
          status
          quotaOverview {
            ${CHANNEL_QUOTA_OVERVIEW_FIELDS}
          }
        }
      }
    }
  }
`;

function isQuotaBadgeCandidate(ch: QuotaBadgeChannel): boolean {
  if (ch.status !== 'enabled') return false;
  const ov = ch.quotaOverview;
  if (!ov) return false;
  return (
    ov.source !== 'none' ||
    ov.state === 'exhausted' ||
    ov.state === 'warning' ||
    ov.state === 'error' ||
    ov.windows.length > 0 ||
    ov.walletBalance != null ||
    ov.walletRemaining != null ||
    ov.unlimited ||
    ov.freeTier
  );
}

function useQuotaBadgesOverview(enabled: boolean) {
  return useQuery({
    queryKey: ['quota-badges-overview'],
    enabled,
    queryFn: async () => {
      const data = await graphqlRequest<{
        queryChannels: {
          edges: Array<{ node: QuotaBadgeChannel }>;
        };
      }>(QUERY_QUOTA_BADGES_QUERY, {
        input: {
          first: 100,
          where: {
            statusIn: ['enabled'],
          },
          orderBy: { field: 'ORDERING_WEIGHT', direction: 'DESC' },
        },
      });
      return (data?.queryChannels?.edges ?? [])
        .map((edge) => quotaBadgeChannelSchema.parse(edge.node))
        .filter(isQuotaBadgeCandidate);
    },
    refetchInterval: enabled ? 60_000 : false,
    refetchIntervalInBackground: false,
  });
}

function readSharedQuotaBadgeChannels(
  queryClient: ReturnType<typeof useQueryClient>,
  pathname: string
): QuotaBadgeChannel[] | null {
  if (pathname.startsWith('/quotas')) {
    const queries = queryClient
      .getQueryCache()
      .findAll({ queryKey: ['quotas-channels'] })
      .sort((a, b) => b.state.dataUpdatedAt - a.state.dataUpdatedAt);
    // Prefer the most recent unfiltered /quotas snapshot first.
    const preferred =
      queries.find((q) => q.queryKey[1] === '' && Boolean((q.state.data as { items?: QuotaBadgeChannel[] } | undefined)?.items)) ??
      queries.find((q) => Boolean((q.state.data as { items?: QuotaBadgeChannel[] } | undefined)?.items));
    const items = (preferred?.state.data as { items?: QuotaBadgeChannel[] } | undefined)?.items;
    return items ? items.filter(isQuotaBadgeCandidate) : null;
  }
  if (pathname.startsWith('/channels')) {
    const queries = queryClient
      .getQueryCache()
      .findAll({ queryKey: ['channels'] })
      .sort((a, b) => b.state.dataUpdatedAt - a.state.dataUpdatedAt);
    for (const q of queries) {
      const edges = (q.state.data as { edges?: Array<{ node: QuotaBadgeChannel }> } | undefined)?.edges;
      if (edges && edges.length > 0) {
        return edges.map((edge) => edge.node).filter(isQuotaBadgeCandidate);
      }
    }
    return null;
  }
  return null;
}

type BatteryLevel = 'full' | 'medium' | 'low' | 'warning';

function getBatteryIcon(level: BatteryLevel) {
  switch (level) {
    case 'full':
      return BatteryFull;
    case 'medium':
      return BatteryMedium;
    case 'low':
      return BatteryLow;
    case 'warning':
      return BatteryWarning;
    default:
      return Battery;
  }
}

function stateBadgeVariant(state: ChannelQuotaOverview['state']): {
  variant: 'default' | 'secondary' | 'destructive' | 'outline';
  className?: string;
} {
  switch (state) {
    case 'exhausted':
      return { variant: 'destructive' };
    case 'warning':
      return {
        variant: 'outline',
        className: 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400',
      };
    case 'error':
      return {
        variant: 'outline',
        className: 'border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400',
      };
    case 'available':
    case 'unlimited':
      return {
        variant: 'outline',
        className: 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400',
      };
    default:
      return { variant: 'secondary' };
  }
}

function QuotaBadgeTrigger({
  channels,
  attentionCount,
  isLoading,
  isError,
}: {
  channels: QuotaBadgeChannel[];
  attentionCount: number;
  isLoading?: boolean;
  isError?: boolean;
}) {
  if (isLoading) {
    return <Loader2 className='text-muted-foreground h-5 w-5 animate-spin' />;
  }

  if (isError) {
    return <BatteryWarning className='h-5 w-5 text-red-500 transition-colors' />;
  }

  const hasExhausted = channels.some((c) => c.quotaOverview?.state === 'exhausted' || (c.quotaOverview?.counts.exhausted ?? 0) > 0);
  const hasWarningOrError = channels.some(
    (c) => c.quotaOverview?.state === 'warning' || c.quotaOverview?.state === 'error'
  );

  const level: BatteryLevel = hasExhausted ? 'warning' : hasWarningOrError ? 'low' : 'full';
  const BatteryIcon = getBatteryIcon(level);
  const textColor =
    level === 'warning' ? 'text-destructive' : level === 'low' ? 'text-amber-500' : 'text-muted-foreground';

  return (
    <span className='relative inline-flex items-center'>
      <BatteryIcon className={cn('h-5 w-5 transition-colors', textColor)} />
      {attentionCount > 0 ? (
        <span className='bg-destructive text-destructive-foreground absolute -top-1.5 -right-2 flex h-4 min-w-4 items-center justify-center rounded-full px-1 font-mono text-[10px] leading-none font-semibold'>
          {attentionCount}
        </span>
      ) : null}
    </span>
  );
}

export function QuotaBadges() {
  const { t, i18n } = useTranslation();
  const { channelPermissions } = usePermissions();
  const canWrite = channelPermissions.canWrite;
  const queryClient = useQueryClient();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const [open, setOpen] = useState(false);
  const [refreshingId, setRefreshingId] = useState<string | null>(null);
  const [resettingId, setResettingId] = useState<string | null>(null);
  const [cacheTick, setCacheTick] = useState(0);

  const isQuotasPage = pathname.startsWith('/quotas');
  const isChannelsPage = pathname.startsWith('/channels');

  useEffect(() => {
    if (!isQuotasPage && !isChannelsPage) return;
    const unsubscribe = queryClient.getQueryCache().subscribe((event) => {
      const key = event.query.queryKey[0];
      if ((isQuotasPage && key === 'quotas-channels') || (isChannelsPage && key === 'channels')) {
        setCacheTick((v) => v + 1);
      }
    });
    return unsubscribe;
  }, [isChannelsPage, isQuotasPage, queryClient]);

  // Share the page's single list query on /quotas and defer the full badge query on /channels until opened.
  const shouldFetchOwnOverview = !isQuotasPage && (!isChannelsPage || open);
  const ownQuery = useQuotaBadgesOverview(shouldFetchOwnOverview);
  const sharedChannels = useMemo(() => {
    void cacheTick;
    return readSharedQuotaBadgeChannels(queryClient, pathname);
  }, [cacheTick, pathname, queryClient]);

  const channels = ownQuery.data ?? sharedChannels ?? [];
  const isLoading = shouldFetchOwnOverview && ownQuery.isLoading && sharedChannels == null;
  const isError = shouldFetchOwnOverview && ownQuery.isError && sharedChannels == null;
  const error = ownQuery.error;
  const refreshQuota = useRefreshChannelQuota();
  const nowMs = useSharedMinuteNow(open && channels.length > 0);

  const sortedChannels = useMemo(() => {
    return [...channels].sort((a, b) => {
      const oa = a.quotaOverview;
      const ob = b.quotaOverview;
      const attnA = isAttentionQuotaOverview(oa) ? 0 : 1;
      const attnB = isAttentionQuotaOverview(ob) ? 0 : 1;
      if (attnA !== attnB) return attnA - attnB;
      const rankA = quotaStateSortRank(oa?.state, Boolean(oa?.probeDiagnostic));
      const rankB = quotaStateSortRank(ob?.state, Boolean(ob?.probeDiagnostic));
      if (rankA !== rankB) return rankA - rankB;
      return a.name.localeCompare(b.name);
    });
  }, [channels]);

  const attentionCount = useMemo(
    () => sortedChannels.filter((c) => isAttentionQuotaOverview(c.quotaOverview)).length,
    [sortedChannels]
  );

  const visibleChannels = sortedChannels.slice(0, MAX_POPOVER_CHANNELS);
  const hiddenCount = Math.max(0, sortedChannels.length - visibleChannels.length);

  if (!isLoading && !isError && sortedChannels.length === 0 && !isChannelsPage && !isQuotasPage) {
    return null;
  }

  const handleRefreshChannel = async (channelID: string) => {
    setRefreshingId(channelID);
    try {
      const res = await refreshQuota.mutateAsync({ channelID });
      if (res.status === 'updated') {
        toast.success(t('quotas.refresh.updated', { count: Math.max(1, res.updatedKeys) }));
      } else if (res.status === 'deferred') {
        const retryStr = res.retryAt ? formatExactTimestamp(res.retryAt, i18n.language) : '';
        toast.info(retryStr ? t('quotas.refresh.deferredUntil', { time: retryStr }) : res.message || t('quotas.refresh.deferred'));
      } else if (res.status === 'unsupported') {
        toast.info(res.message || t('quotas.refresh.unsupported'));
      } else {
        toast.error(res.message || t('quotas.refresh.error'));
      }
    } catch {
      // Handled by hook.
    } finally {
      setRefreshingId(null);
    }
  };

  const handleResetCodex = async (channelID: string) => {
    setResettingId(channelID);
    try {
      await resetChannelQuotaNow(channelID);
      toast.success(t('quota.codex.resetSuccess'));
      await queryClient.invalidateQueries({ queryKey: ['quota-badges-overview'] });
      await queryClient.invalidateQueries({ queryKey: ['quotas-channels'] });
    } catch (err) {
      toast.error(t('quota.codex.resetError'), {
        description: err instanceof Error ? err.message : String(err),
      });
    } finally {
      setResettingId(null);
    }
  };

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type='button'
          className='hover:bg-muted relative rounded-md p-2 transition-colors'
          aria-label={t('quotas.popover.title')}
          data-testid='quota-badges-trigger'
        >
          <QuotaBadgeTrigger
            channels={sortedChannels}
            attentionCount={attentionCount}
            isLoading={isLoading}
            isError={isError}
          />
        </button>
      </PopoverTrigger>
      <PopoverContent
        className={
          !isLoading && !isError && visibleChannels.length > 4
            ? 'w-[640px] max-w-[calc(100vw-2rem)]'
            : 'w-80 max-w-[calc(100vw-2rem)]'
        }
        align='end'
      >
        <div className='space-y-2.5'>
          <div className='flex items-center justify-between gap-2 border-b pb-2'>
            <div className='flex items-center gap-2'>
              <span className='text-foreground text-xs font-semibold tracking-wide'>
                {t('quotas.popover.title')}
              </span>
              {attentionCount > 0 ? (
                <Badge variant='destructive' className='h-4 px-1.5 text-[10px]'>
                  {t('quotas.popover.attentionCount', { count: attentionCount })}
                </Badge>
              ) : null}
            </div>
            <Link
              to='/quotas'
              onClick={() => setOpen(false)}
              className='text-primary inline-flex items-center gap-1 text-xs font-medium hover:underline'
            >
              <span>{t('quotas.popover.viewAll')}</span>
              <ArrowRight className='h-3 w-3' />
            </Link>
          </div>

          {isLoading ? (
            <div className='text-muted-foreground flex items-center gap-2 py-3 text-xs'>
              <Loader2 className='h-3.5 w-3.5 animate-spin' />
              <span>{t('system.providerQuota.loading')}</span>
            </div>
          ) : isError ? (
            <div className='rounded bg-red-500/10 p-2 text-xs break-words text-red-500'>
              <span className='font-medium'>{t('system.providerQuota.error')}:</span>{' '}
              {error instanceof Error ? error.message : t('quota.label.unavailable')}
            </div>
          ) : (
            <div
              className={cn(
                'max-h-[60vh] overflow-y-auto pr-0.5',
                visibleChannels.length > 4 ? 'grid grid-cols-1 gap-2 sm:grid-cols-2' : 'space-y-2'
              )}
            >
              {visibleChannels.map((channel) => {
                const ov = channel.quotaOverview!;
                const avail = getQuotaKeyAvailability(ov.counts);
                const badge = stateBadgeVariant(ov.state);
                const hasVisualMeter =
                  ov.windows.some((w) => hasRealWindowMeter(w)) ||
                  hasRealWalletMeter({
                    walletLimit: ov.walletLimit,
                    walletUsed: ov.walletUsed,
                    walletRemaining: ov.walletRemaining,
                    unlimited: ov.unlimited,
                    freeTier: ov.freeTier,
                  });
                const showHeaderStateBadge =
                  hasVisualMeter || ov.state === 'exhausted' || ov.state === 'warning' || ov.state === 'error';
                const resetLabel = formatRelativeReset(ov.nextResetAt, nowMs, t);
                const isRefreshingThis = refreshingId === channel.id;
                const isResettingThis = resettingId === channel.id;

                return (
                  <div
                    key={channel.id}
                    className={cn(
                      'border-border/70 flex flex-col gap-1.5 rounded-lg border p-2.5',
                      isAttentionQuotaOverview(ov) && 'border-destructive/30 bg-destructive/[0.025]'
                    )}
                  >
                    <div className='flex items-center justify-between gap-2'>
                      <div className='flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-0.5'>
                        <span className='truncate text-xs font-medium'>{channel.name}</span>
                        {avail.total > 0 ? (
                          <span
                            className={cn(
                              'shrink-0 font-mono text-[10px] tabular-nums',
                              avail.schedulable === 0 ? 'font-medium text-red-700 dark:text-red-400' : 'text-muted-foreground'
                            )}
                          >
                            {avail.schedulable}/{avail.total}
                          </span>
                        ) : null}
                        {avail.exhausted > 0 ? (
                          <span className='shrink-0 text-[10px] font-medium text-red-700 dark:text-red-400'>
                            {t('quotas.counts.exhausted', { count: avail.exhausted })}
                          </span>
                        ) : null}
                        {avail.disabled > 0 ? (
                          <span className='text-muted-foreground shrink-0 text-[10px]'>
                            {t('quotas.counts.disabled', { count: avail.disabled })}
                          </span>
                        ) : null}
                      </div>
                      <div className='flex shrink-0 items-center gap-1'>
                        {showHeaderStateBadge ? (
                          <Badge variant={badge.variant} className={cn('h-4 px-1.5 text-[10px]', badge.className)}>
                            {t(`quotas.state.${ov.state}`)}
                          </Badge>
                        ) : null}
                        {canWrite && ov.canRefresh ? (
                          <button
                            type='button'
                            className='text-muted-foreground hover:text-foreground rounded p-0.5 transition-colors disabled:opacity-40'
                            disabled={isRefreshingThis}
                            onClick={() => handleRefreshChannel(channel.id)}
                            aria-label={t('quotas.actions.refresh')}
                          >
                            {isRefreshingThis ? (
                              <Loader2 className='h-3 w-3 animate-spin' />
                            ) : (
                              <RefreshCw className='h-3 w-3' />
                            )}
                          </button>
                        ) : null}
                      </div>
                    </div>

                    <QuotaWindowBars
                      windows={ov.windows}
                      walletBalance={ov.walletBalance}
                      walletUsed={ov.walletUsed}
                      walletLimit={ov.walletLimit}
                      walletRemaining={ov.walletRemaining}
                      walletUnit={ov.walletUnit}
                      state={ov.state}
                      unlimited={ov.unlimited}
                      freeTier={ov.freeTier}
                      sharedWallet={ov.sharedWallet}
                      planName={ov.planName}
                      source={ov.source}
                      mode='remaining'
                      compact
                      maxWindows={2}
                      nowMs={nowMs}
                    />

                    <div className='text-muted-foreground flex items-center justify-between gap-2 text-[10px]'>
                      <span className='truncate'>
                        {ov.probeDiagnostic?.error
                          ? ov.probeDiagnostic.error
                          : resetLabel
                            ? t('quotas.popover.resetsIn', { time: resetLabel })
                            : ''}
                      </span>
                      {canWrite &&
                      channel.type === 'codex' &&
                      (ov.state === 'exhausted' || ov.state === 'warning') ? (
                        <Button
                          type='button'
                          size='sm'
                          variant='outline'
                          className='h-5 px-1.5 text-[10px]'
                          disabled={isResettingThis}
                          onClick={() => handleResetCodex(channel.id)}
                        >
                          {isResettingThis ? (
                            <Loader2 className='mr-1 h-2.5 w-2.5 animate-spin' />
                          ) : (
                            <Zap className='mr-1 h-2.5 w-2.5' />
                          )}
                          {t('quota.codex.resetNow')}
                        </Button>
                      ) : null}
                    </div>
                  </div>
                );
              })}
            </div>
          )}

          {hiddenCount > 0 ? (
            <div className='border-t pt-2 text-center'>
              <Link
                to='/quotas'
                onClick={() => setOpen(false)}
                className='text-muted-foreground hover:text-foreground text-xs'
              >
                {t('quotas.popover.moreChannels', { count: hiddenCount })}
              </Link>
            </div>
          ) : null}
        </div>
      </PopoverContent>
    </Popover>
  );
}
