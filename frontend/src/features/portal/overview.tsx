import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Bar, BarChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
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
  const quotas = item.quotas.filter((q) => q.active);
  return (
    <div className='space-y-2 text-sm'>
      {quotas.length ? (
        quotas.map((q) => (
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
            <p className='text-muted-foreground text-xs'>
              {q.start ? new Date(q.start).toLocaleString(i18n.language) : t('portal.allRetained')} /{' '}
              {q.end ? new Date(q.end).toLocaleString(i18n.language) : ''}
            </p>
          </div>
        ))
      ) : (
        <p>{t('portal.noQuota')}</p>
      )}
      <p className='text-muted-foreground text-xs'>{t('portal.independentQuota')}</p>
    </div>
  );
}
export default function PortalOverview() {
  const { t, i18n } = useTranslation();
  const group = usePortalGroup();
  const currency = usePortalCurrency();
  const [period, setPeriod] = useState('month');
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
      <div className='space-y-5'>
        <Skeleton className='h-9 w-48' />
        <Skeleton className='h-40 w-full' />
        <Skeleton className='h-72 w-full' />
      </div>
    );
  const data = stats.data.dashboard;
  const key = keys.data.keys.items.find((k) => k.id === selectedKeyID) || keys.data.keys.items[0];
  const pinned = content.data?.content.announcements.find((a) => a.pinned);
  const money = (cost: number | null) =>
    cost === null
      ? t('portal.unpriced')
      : t('currencies.format', { val: cost, currency, locale: i18n.language === 'zh' ? 'zh-CN' : 'en-US', minimumFractionDigits: 6 });
  return (
    <div className='space-y-6'>
      <div className='flex flex-wrap items-center justify-between gap-3'>
        <h1 className='text-2xl font-semibold tracking-tight'>{t('portal.overview')}</h1>
        <select
          aria-label={t('portal.window')}
          value={period}
          onChange={(e) => setPeriod(e.target.value)}
          className='bg-background rounded-md border p-2 text-sm'
        >
          {['today', 'week', 'month', 'all'].map((p) => (
            <option key={p} value={p}>
              {t(`portal.${p}`)}
            </option>
          ))}
        </select>
      </div>
      {pinned && (
        <Link
          to='/portal/guides'
          search={{ doc: pinned.slug }}
          className='bg-primary/5 flex items-center justify-between gap-4 rounded-xl border p-4 text-sm'
        >
          <span className='font-medium'>{pinned.title}</span>
          <span>{t('portal.example')} →</span>
        </Link>
      )}
      <div className='grid gap-4 md:grid-cols-3'>
        <Card className='rounded-2xl'>
          <CardHeader className='pb-2'>
            <CardTitle className='text-sm font-medium'>{t('portal.requests')}</CardTitle>
          </CardHeader>
          <CardContent>
            <p className='font-mono text-3xl tabular-nums'>{data.requests.toLocaleString()}</p>
            <p className='text-muted-foreground mt-2 text-xs'>
              {t('portal.success')}: {data.successfulRequests.toLocaleString()}
            </p>
          </CardContent>
        </Card>
        <Card className='rounded-2xl'>
          <CardHeader className='pb-2'>
            <CardTitle className='text-sm font-medium'>{t('portal.tokens')}</CardTitle>
          </CardHeader>
          <CardContent>
            <p className='font-mono text-3xl tabular-nums'>{data.totalTokens.toLocaleString()}</p>
            <p className='text-muted-foreground mt-2 text-xs'>
              {t('portal.input')} {data.inputTokens.toLocaleString()} · {t('portal.output')} {data.outputTokens.toLocaleString()}
            </p>
            <p className='text-muted-foreground mt-1 text-xs'>{t('portal.reportedCoverage', { count: data.usageCount })}</p>
          </CardContent>
        </Card>
        <Card className='rounded-2xl'>
          <CardHeader className='pb-2'>
            <CardTitle className='text-sm font-medium'>{t('portal.quota')}</CardTitle>
          </CardHeader>
          <CardContent>
            {key ? (
              <>
                <select
                  aria-label={t('portal.key')}
                  className='bg-background mb-3 w-full rounded-md border p-1.5 text-sm'
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
              <p className='text-muted-foreground text-sm'>{t('portal.noKeys')}</p>
            )}
          </CardContent>
        </Card>
      </div>
      {group.callable ? (
        <QuickStart keys={keys.data.keys.items} />
      ) : (
        <PortalEmpty title={t('portal.noModels')} description={t('portal.noModelsHelp')} />
      )}
      <section className='space-y-3'>
        <h2 className='text-lg font-semibold'>{t('portal.modelUsage')}</h2>
        <p className='text-muted-foreground text-sm'>{t('portal.modelUsageHelp')}</p>
        {data.models.length ? (
          <>
            <div className='h-56 w-full min-w-0'>
              <ResponsiveContainer width='100%' height='100%'>
                <BarChart data={data.models.slice(0, 10)}>
                  <XAxis dataKey='modelID' tick={{ fontSize: 11 }} />
                  <YAxis tick={{ fontSize: 11 }} />
                  <Tooltip />
                  <Bar dataKey='totalTokens' fill='var(--primary)' radius={[4, 4, 0, 0]} />
                </BarChart>
              </ResponsiveContainer>
            </div>
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
                  header: t('portal.tokens'),
                  cell: ({ row }) => <span className='font-mono tabular-nums'>{row.original.totalTokens.toLocaleString()}</span>,
                },
                {
                  id: 'cost',
                  header: t('portal.cost'),
                  cell: ({ row }) => (
                    <div className='font-mono text-xs'>
                      {money(row.original.cost)}
                      <p className='text-muted-foreground mt-1 font-sans'>
                        {t('portal.pricedCoverage', { priced: row.original.pricedCount, total: row.original.usageCount })}
                      </p>
                    </div>
                  ),
                },
              ]}
            />
          </>
        ) : (
          <PortalEmpty title={t('portal.noUsage')} />
        )}
      </section>
      {data.recentRequests.length > 0 && (
        <section className='space-y-3'>
          <h2 className='text-lg font-semibold'>{t('portal.recentRequests')}</h2>
          <PortalTable
            data={data.recentRequests}
            columns={[
              {
                id: 'time',
                header: t('portal.time'),
                cell: ({ row }) => <span className='text-xs whitespace-nowrap'>{new Date(row.original.createdAt).toLocaleString()}</span>,
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
              {
                id: 'cost',
                header: t('portal.cost'),
                cell: ({ row }) => (row.original.usageReported ? money(row.original.cost) : t('portal.notReported')),
              },
            ]}
          />
        </section>
      )}
    </div>
  );
}
