import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { BarChart4, Database, KeyRound, ShieldCheck } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Progress } from '@/components/ui/progress';
import { Skeleton } from '@/components/ui/skeleton';
import { TimePeriodSelector, type TimePeriod } from '@/components/time-period-selector';
import { DailyRequestChartView } from '@/features/dashboard/components/daily-request-chart-view';
import { DashboardMetricCard } from '@/features/dashboard/components/dashboard-metric-card';
import { TokenStatsValues } from '@/features/dashboard/components/token-stats-values';
import {
  CONTENT_QUERY,
  DASHBOARD_QUERY,
  KEYS_QUERY,
  type PortalContent,
  type PortalDashboard,
  type PortalKey,
  usePortalQuery,
  usePortalCurrency,
} from './data';
import { PortalEmpty, PortalError, usePortalGroup } from './layout';
import { QuickStart } from './quick-start';
import { PortalTable } from './table';

export function KeyQuota({ item }: { item: PortalKey }) {
  const { t, i18n } = useTranslation();
  const currency = usePortalCurrency();
  const money = (value: string) =>
    t('currencies.format', { val: Number(value), currency, locale: i18n.language === 'zh' ? 'zh-CN' : 'en-US', minimumFractionDigits: 6 });
  const quotas = item.quotas.filter((q) => q.active && (q.requests !== null || q.totalTokens !== null || q.cost !== null));
  return quotas.length ? (
    <div className='space-y-2 text-xs'>
      {quotas.map((q) => (
        <div key={q.profileName} className='space-y-1 font-mono tabular-nums'>
          {q.requests !== null && (
            <p>
              {t('portal.requests')}: {q.usedRequests.toLocaleString()} / {q.requests.toLocaleString()}
            </p>
          )}
          {q.totalTokens !== null && (
            <p>
              Token: {q.usedTokens.toLocaleString()} / {q.totalTokens.toLocaleString()}
            </p>
          )}
          {q.cost !== null && (
            <p>
              {t('portal.cost')}: {money(q.usedCost)} / {money(q.cost)}
            </p>
          )}
        </div>
      ))}
    </div>
  ) : (
    <span className='text-muted-foreground text-xs'>{t('portal.noQuota')}</span>
  );
}

export default function PortalOverview() {
  const { t, i18n } = useTranslation();
  const group = usePortalGroup();
  const currency = usePortalCurrency();
  const [timePeriod, setTimePeriod] = useState<TimePeriod>('month');
  const period = timePeriod === 'allTime' ? 'all' : timePeriod === 'day' ? 'today' : timePeriod;
  const stats = usePortalQuery<{ dashboard: PortalDashboard }>('dashboard', DASHBOARD_QUERY, { projectID: group?.id, period }, !!group);
  const keys = usePortalQuery<{ keys: { items: PortalKey[] } }>('keys', KEYS_QUERY, { projectID: group?.id, first: 100 }, !!group);
  const content = usePortalQuery<{ content: PortalContent }>('content', CONTENT_QUERY);
  const [selectedKeyID, setSelectedKeyID] = useState<number | null>(null);
  if (!group) return <PortalEmpty title={t('portal.noGroups')} description={t('portal.noGroupsHelp')} />;
  if (stats.error || keys.error)
    return (
      <PortalError
        error={stats.error || keys.error}
        retry={() => {
          void stats.refetch();
          void keys.refetch();
        }}
      />
    );
  if (!stats.data || !keys.data)
    return (
      <div className='space-y-4'>
        <Skeleton className='h-9 w-48' />
        <div className='grid gap-6 md:grid-cols-2 lg:grid-cols-4'>
          {[0, 1, 2, 3].map((id) => (
            <Skeleton key={id} className='h-40' />
          ))}
        </div>
        <Skeleton className='h-[350px] w-full' />
      </div>
    );
  const data = stats.data.dashboard;
  const key = keys.data.keys.items.find((k) => k.id === selectedKeyID) || keys.data.keys.items[0];
  const pinned = content.data?.content.announcements.find((a) => a.pinned);
  const successRate = data.requests > 0 ? (data.successfulRequests / data.requests) * 100 : 0;
  const money = (cost: number | null) =>
    cost === null
      ? t('portal.unpriced')
      : t('currencies.format', { val: cost, currency, locale: i18n.language === 'zh' ? 'zh-CN' : 'en-US', minimumFractionDigits: 6 });
  return (
    <div className='flex-1 space-y-6'>
      <div className='flex flex-wrap items-center justify-between gap-3'>
        <h1 className='text-2xl font-bold tracking-tight'>{t('portal.overview')}</h1>
        <TimePeriodSelector value={timePeriod} onChange={setTimePeriod} />
      </div>
      {pinned && (
        <Link
          to='/portal/guides'
          search={{ doc: pinned.slug }}
          className='hover:bg-accent/50 bg-card block rounded-lg border px-4 py-3 text-sm font-medium'
        >
          {pinned.title}
        </Link>
      )}
      <section className='grid gap-6 md:grid-cols-2 lg:grid-cols-4'>
        <DashboardMetricCard title={t('portal.requests')} icon={Database} value={data.requests.toLocaleString()} />
        <DashboardMetricCard
          title={t('dashboard.cards.successRate')}
          icon={ShieldCheck}
          value={
            <>
              {successRate.toFixed(1)}
              <span className='text-muted-foreground text-lg'>%</span>
            </>
          }
        >
          <Progress value={successRate} className='h-2' />
          <p className='text-muted-foreground text-xs'>
            {(data.requests - data.successfulRequests).toLocaleString()} {t('dashboard.stats.failedRequests')}
          </p>
        </DashboardMetricCard>
        <DashboardMetricCard title={t('dashboard.cards.tokenStats')} icon={BarChart4}>
          <TokenStatsValues tokens={{ input: data.inputTokens, output: data.outputTokens, cached: data.cachedTokens }} />
        </DashboardMetricCard>
        <DashboardMetricCard title={t('portal.quota')} icon={KeyRound}>
          {key ? (
            <>
              <select
                aria-label={t('portal.key')}
                className='bg-background mb-2 w-full rounded-md border p-1.5 text-sm'
                value={key.id}
                onChange={(e) => setSelectedKeyID(Number(e.target.value))}
              >
                {keys.data.keys.items.map((k) => (
                  <option key={k.id} value={k.id}>
                    {k.name}
                  </option>
                ))}
              </select>
              <KeyQuota item={key} />
            </>
          ) : (
            <Link to='/portal/keys' className='text-primary text-sm hover:underline'>
              {t('portal.createKey')}
            </Link>
          )}
        </DashboardMetricCard>
      </section>
      <div className='grid gap-4 md:grid-cols-2 lg:grid-cols-7'>
        <Card className='hover-card min-w-0 lg:col-span-4'>
          <CardHeader>
            <CardTitle>{t('portal.tokens')}</CardTitle>
          </CardHeader>
          <CardContent className='pl-2'>
            <DailyRequestChartView
              data={data.days.map((day) => ({ date: day.day, tokens: day.totalTokens }))}
              showRequests={false}
              showCost={false}
            />
          </CardContent>
        </Card>
        <Card className='hover-card min-w-0 lg:col-span-3'>
          <CardHeader>
            <CardTitle>{t('portal.modelUsage')}</CardTitle>
          </CardHeader>
          <CardContent>
            {data.models.length ? (
              <PortalTable
                data={data.models}
                columns={[
                  {
                    accessorKey: 'modelID',
                    header: t('portal.model'),
                    cell: ({ row }) => <code className='text-xs'>{row.original.modelID}</code>,
                  },
                  {
                    accessorKey: 'totalTokens',
                    header: 'Token',
                    cell: ({ row }) => <span className='font-mono tabular-nums'>{row.original.totalTokens.toLocaleString()}</span>,
                  },
                  {
                    id: 'cost',
                    header: t('portal.cost'),
                    cell: ({ row }) => <span className='font-mono text-xs'>{money(row.original.cost)}</span>,
                  },
                ]}
              />
            ) : (
              <PortalEmpty title={t('portal.noUsage')} />
            )}
          </CardContent>
        </Card>
      </div>
      {group.callable && <QuickStart keys={keys.data.keys.items} />}
      {data.recentRequests.length > 0 && (
        <section className='space-y-3'>
          <h2 className='text-lg font-semibold'>{t('portal.recentRequests')}</h2>
          <PortalTable
            data={data.recentRequests}
            columns={[
              {
                id: 'time',
                header: t('portal.time'),
                cell: ({ row }) => (
                  <span className='text-xs whitespace-nowrap'>{new Date(row.original.createdAt).toLocaleString(i18n.language)}</span>
                ),
              },
              { accessorKey: 'modelID', header: t('portal.model') },
              { accessorKey: 'status', header: t('portal.status') },
              {
                id: 'usage',
                header: 'Token',
                cell: ({ row }) =>
                  row.original.usageReported ? (
                    <span className='font-mono'>{row.original.totalTokens?.toLocaleString()}</span>
                  ) : (
                    t('portal.notReported')
                  ),
              },
            ]}
          />
        </section>
      )}
    </div>
  );
}
