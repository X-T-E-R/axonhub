import type { ReactNode, ComponentType } from 'react';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';

export function DashboardMetricCard({
  title,
  icon: Icon,
  value,
  children,
}: {
  title: string;
  icon: ComponentType<{ className?: string }>;
  value?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <Card className='hover-card min-w-0'>
      <CardHeader className='flex flex-row items-center justify-between space-y-0 pb-2'>
        <div className='flex min-w-0 items-center gap-2'>
          <div className='bg-primary/10 text-primary dark:bg-primary/20 shrink-0 rounded-lg p-1.5'>
            <Icon className='h-4 w-4' />
          </div>
          <CardTitle className='text-sm font-medium'>{title}</CardTitle>
        </div>
      </CardHeader>
      <CardContent className='space-y-2'>
        {value !== undefined && <div className='font-mono text-3xl font-bold tabular-nums'>{value}</div>}
        {children}
      </CardContent>
    </Card>
  );
}
