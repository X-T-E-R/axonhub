import { useId } from 'react';
import { useTranslation } from 'react-i18next';
import { Area, AreaChart, CartesianGrid, Legend, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { formatNumber } from '@/utils/format-number';

export interface DailyUsagePoint {
  date: string;
  count?: number;
  tokens: number;
  cost?: number;
}

export function DailyRequestChartView({
  data,
  currencyCode,
  showRequests = true,
  showCost = true,
}: {
  data: DailyUsagePoint[];
  currencyCode?: string;
  showRequests?: boolean;
  showCost?: boolean;
}) {
  const { t, i18n } = useTranslation();
  const id = useId().replaceAll(':', '');
  const locale = i18n.language.startsWith('zh') ? 'zh-CN' : 'en-US';
  const currency = (value: number) =>
    t('currencies.format', { val: value, currency: currencyCode, locale, minimumFractionDigits: 0, maximumFractionDigits: 0 });
  const points = data.map((point) => ({
    name: new Date(point.date + 'T00:00:00Z').toLocaleDateString(locale, { month: '2-digit', day: '2-digit', timeZone: 'UTC' }),
    requests: point.count,
    tokens: point.tokens,
    cost: point.cost,
  }));
  const requestsMax = Math.max(10, Math.ceil(Math.max(0, ...data.map((point) => point.count ?? 0)) * 1.1));
  const tokensMax = Math.max(1000, Math.ceil(Math.max(0, ...data.map((point) => point.tokens)) * 1.1));
  const costMax = Math.max(0.1, Math.max(0, ...data.map((point) => point.cost ?? 0)) * 1.1);
  return (
    <ResponsiveContainer width='100%' height={350}>
      <AreaChart data={points} margin={{ top: 10, right: 10, left: 0, bottom: 0 }}>
        <defs>
          {['Requests', 'Tokens', 'Cost'].map((name, index) => (
            <linearGradient key={name} id={`${id}${name}`} x1='0' y1='0' x2='0' y2='1'>
              <stop offset='5%' stopColor={`var(--chart-${index + 1})`} stopOpacity={0.3} />
              <stop offset='95%' stopColor={`var(--chart-${index + 1})`} stopOpacity={0} />
            </linearGradient>
          ))}
        </defs>
        <CartesianGrid strokeDasharray='3 3' stroke='var(--border)' vertical={false} />
        <XAxis dataKey='name' stroke='var(--muted-foreground)' fontSize={12} tickLine axisLine padding={{ right: 24 }} />
        {showRequests && (
          <YAxis
            yAxisId='requests'
            stroke='var(--chart-1)'
            fontSize={12}
            domain={[0, requestsMax]}
            tickFormatter={(value) => formatNumber(Number(value))}
            width={40}
            tickMargin={8}
          />
        )}
        <YAxis
          yAxisId='tokens'
          orientation={showRequests ? 'right' : 'left'}
          stroke='var(--chart-2)'
          fontSize={12}
          domain={[0, tokensMax]}
          tickFormatter={(value) => formatNumber(Number(value))}
          width={48}
          tickMargin={8}
        />
        {showCost && (
          <YAxis
            yAxisId='cost'
            orientation='right'
            stroke='var(--chart-3)'
            fontSize={12}
            domain={[0, costMax]}
            tickFormatter={(value) => currency(Number(value))}
            width={60}
            tickMargin={8}
          />
        )}
        <Tooltip
          formatter={(value, name) => [
            name === t('dashboard.stats.totalCost') ? currency(Number(value)) : formatNumber(Number(value)),
            name,
          ]}
          contentStyle={{
            backgroundColor: 'var(--background)',
            borderColor: 'var(--border)',
            borderRadius: 'var(--radius)',
            fontSize: '12px',
          }}
          itemStyle={{ padding: '2px 0' }}
        />
        <Legend verticalAlign='top' height={36} />
        {showRequests && (
          <Area
            yAxisId='requests'
            type='monotone'
            dataKey='requests'
            name={t('dashboard.stats.requests')}
            stroke='var(--chart-1)'
            strokeWidth={2}
            fill={`url(#${id}Requests)`}
            dot={false}
            activeDot={{ r: 5 }}
          />
        )}
        <Area
          yAxisId='tokens'
          type='monotone'
          dataKey='tokens'
          name={t('dashboard.stats.totalTokens')}
          stroke='var(--chart-2)'
          strokeWidth={2}
          fill={`url(#${id}Tokens)`}
          dot={false}
          activeDot={{ r: 4 }}
        />
        {showCost && (
          <Area
            yAxisId='cost'
            type='monotone'
            dataKey='cost'
            name={t('dashboard.stats.totalCost')}
            stroke='var(--chart-3)'
            strokeWidth={2}
            fill={`url(#${id}Cost)`}
            dot={false}
            activeDot={{ r: 4 }}
          />
        )}
      </AreaChart>
    </ResponsiveContainer>
  );
}
