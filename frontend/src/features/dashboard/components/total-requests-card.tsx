import { Database } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { formatNumber } from '@/utils/format-number';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { useDashboardStats } from '../data/dashboard';
import { DashboardMetricCard } from './dashboard-metric-card';

export function TotalRequestsCard() {
  const { t } = useTranslation();
  const { data: stats, isLoading, error } = useDashboardStats();

  const calculateGrowth = (current: number, previous: number): { percentage: number; isPositive: boolean } => {
    if (previous === 0) {
      return { percentage: current > 0 ? 100 : 0, isPositive: current > 0 };
    }
    const percentage = ((current - previous) / previous) * 100;
    return { percentage, isPositive: percentage >= 0 };
  };

  const growth = stats?.requestStats
    ? calculateGrowth(stats.requestStats.requestsThisWeek, stats.requestStats.requestsLastWeek)
    : { percentage: 0, isPositive: true };

  if (isLoading) {
    return (
      <Card>
        <CardHeader className='flex flex-row items-center justify-between space-y-0 pb-2'>
          <Skeleton className='h-4 w-[120px]' />
          <Skeleton className='h-4 w-4' />
        </CardHeader>
        <CardContent>
          <div className='space-y-2'>
            <Skeleton className='h-8 w-[80px]' />
            <Skeleton className='mt-1 h-4 w-[140px]' />
          </div>
        </CardContent>
      </Card>
    );
  }

  if (error) {
    return (
      <Card>
        <CardHeader className='flex flex-row items-center justify-between space-y-0 pb-2'>
          <div className='flex items-center gap-2'>
            <div className='bg-primary/10 text-primary dark:bg-primary/20 rounded-lg p-1.5'>
              <Database className='h-4 w-4' />
            </div>
            <CardTitle className='text-sm font-medium'>{t('dashboard.stats.allTimeRequests')}</CardTitle>
          </div>
        </CardHeader>
        <CardContent>
          <div className='text-sm text-red-500'>{t('common.loadError')}</div>
        </CardContent>
      </Card>
    );
  }

  return (
    <DashboardMetricCard title={t('dashboard.stats.allTimeRequests')} icon={Database} value={formatNumber(stats?.totalRequests || 0)}>
      <div className={`flex items-center gap-1 text-xs font-medium ${growth.isPositive ? 'text-primary' : 'text-red-500'}`}>
        <span
          className={`rounded-md px-1.5 py-0.5 ${growth.isPositive ? 'bg-primary/10 border-primary/20 border' : 'border border-red-500/20 bg-red-500/10'}`}
        >
          {growth.isPositive ? '+' : ''}
          {growth.percentage.toFixed(0)}%
        </span>
        <span className='text-muted-foreground'>{t('dashboard.stats.vsLastWeek')}</span>
      </div>
    </DashboardMetricCard>
  );
}
