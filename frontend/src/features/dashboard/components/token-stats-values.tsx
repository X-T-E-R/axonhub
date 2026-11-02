import { useTranslation } from 'react-i18next';
import { formatNumber } from '@/utils/format-number';

export function TokenStatsValues({ tokens }: { tokens: { input: number; output: number; cached: number } }) {
  const { t } = useTranslation();
  return (
    <div className='flex items-end justify-between gap-2 sm:flex-col sm:gap-2 xl:flex-row xl:items-end xl:justify-between'>
      {(['input', 'output', 'cached'] as const).map((kind, index) => (
        <div key={kind} className='contents'>
          {index > 0 && (
            <>
              <div className='bg-border h-8 w-px shrink-0 sm:hidden xl:block' />
              <div className='bg-border hidden h-px w-full shrink-0 sm:block xl:hidden' />
            </>
          )}
          <div className='min-w-0 text-center sm:flex sm:w-full sm:items-center sm:justify-between xl:block xl:flex-1 xl:text-center'>
            <div className='text-muted-foreground text-xs sm:mb-0 xl:mb-1'>{t(`dashboard.stats.${kind}`)}</div>
            <div className={`font-mono text-lg font-bold tabular-nums ${kind === 'cached' ? 'text-muted-foreground' : ''}`}>
              {formatNumber(tokens[kind])}
            </div>
          </div>
        </div>
      ))}
    </div>
  );
}
