'use client';

import { useTranslation } from 'react-i18next';
import { Skeleton } from '@/components/ui/skeleton';
import { useGeneralSettings } from '../../system/data/system';
import { useDailyRequestStats } from '../data/dashboard';
import { DailyRequestChartView } from './daily-request-chart-view';

export function DailyRequestStats() {
  const { t } = useTranslation();
  const { data: dailyStats, isLoading: isStatsLoading, error } = useDailyRequestStats();
  const { data: generalSettings, isLoading: isSettingsLoading } = useGeneralSettings();
  if (isStatsLoading || isSettingsLoading)
    return (
      <div className='h-[350px]'>
        <Skeleton className='h-full w-full' />
      </div>
    );
  if (error)
    return (
      <div className='flex h-[350px] items-center justify-center text-red-500'>
        {t('dashboard.charts.errorLoadingChart')} {error.message}
      </div>
    );
  return <DailyRequestChartView data={dailyStats || []} currencyCode={generalSettings?.currencyCode || 'USD'} />;
}
