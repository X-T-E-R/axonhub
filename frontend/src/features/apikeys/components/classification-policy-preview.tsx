import { useTranslation } from 'react-i18next';

export type PolicyPreviewRow = {
  label: string;
  current: string;
  target: string;
};

export function PolicyComparisonPreview({ rows }: { rows: PolicyPreviewRow[] }) {
  const { t } = useTranslation();
  return (
    <div className='space-y-2'>
      <div className='text-sm font-medium'>{t('apikeys.classification.preview.title')}</div>
      <div className='overflow-x-auto rounded-md border'>
        <table className='w-full min-w-[36rem] border-separate border-spacing-0 text-left text-sm'>
          <thead>
            <tr className='bg-muted'>
              <th className='w-28 border-r p-2' scope='col'>
                <span className='sr-only'>{t('apikeys.classification.preview.dimension')}</span>
              </th>
              <th className='border-r p-2 font-medium' scope='col'>{t('apikeys.classification.preview.current')}</th>
              <th className='p-2 font-medium' scope='col'>{t('apikeys.classification.preview.target')}</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row, index) => (
              <tr key={row.label} className={index > 0 ? 'border-t' : undefined}>
                <th className='bg-background border-t border-r p-2 font-medium' scope='row'>{row.label}</th>
                <td className='bg-background min-w-0 border-t border-r p-2 break-words'>{row.current}</td>
                <td className='bg-background min-w-0 border-t p-2 break-words'>{row.target}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
