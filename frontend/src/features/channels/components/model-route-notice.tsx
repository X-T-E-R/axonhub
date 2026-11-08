import { useTranslation } from 'react-i18next';

export interface ModelRouteNoticeItem {
  model: string;
  machines: string[];
  winner?: string;
}

export function ModelRouteNotice({ routes }: { routes?: ModelRouteNoticeItem[] }) {
  const { t } = useTranslation();
  if (!routes?.length) return null;
  return (
    <div className='rounded-lg border p-3 text-sm' data-testid='model-route-notice'>
      <p className='mb-2 font-medium'>{t('channels.placement.modelRoutesTitle')}</p>
      <ul className='max-h-44 space-y-1 overflow-y-auto'>
        {routes.map((route) => (
          <li key={route.model} className='flex flex-wrap justify-between gap-x-4 gap-y-1'>
            <span className='font-mono'>{route.model}</span>
            <span className='text-muted-foreground'>{route.machines.join(', ')}</span>
            <span>{route.winner ? t('channels.placement.modelRouteWinner', { winner: route.winner }) : t('channels.placement.modelRouteKeySpecific')}</span>
          </li>
        ))}
      </ul>
      <p className='text-muted-foreground mt-2'>{t('channels.placement.modelRoutesHelp')}</p>
    </div>
  );
}
