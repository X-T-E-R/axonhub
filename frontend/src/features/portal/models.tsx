import { useEffect, useState, type ComponentType } from 'react';
import * as Icons from '@lobehub/icons';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { KEYS_QUERY, MODELS_QUERY, type PortalKey, type PortalModel, usePortalQuery, usePortalCurrency } from './data';
import { PortalEmpty, PortalError, usePortalGroup } from './layout';
import { QuickStart } from './quick-start';
import { PortalTable } from './table';

function ModelIcon({ name }: { name: string }) {
  const Icon = Object.prototype.hasOwnProperty.call(Icons, name)
    ? (Icons[name as keyof typeof Icons] as ComponentType<{ className?: string }>)
    : null;
  return Icon ? <Icon className='text-muted-foreground size-5 shrink-0' /> : null;
}

export default function PortalModels() {
  const { t, i18n } = useTranslation();
  const group = usePortalGroup();
  const currency = usePortalCurrency();
  const [search, setSearch] = useState('');
  const [debounced, setDebounced] = useState('');
  const [type, setType] = useState('');
  const [example, setExample] = useState<PortalModel | null>(null);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(search), 250);
    return () => clearTimeout(timer);
  }, [search]);
  const models = usePortalQuery<{ models: PortalModel[] }>(
    'models',
    MODELS_QUERY,
    { projectID: group?.id, search: debounced, type },
    !!group?.member
  );
  const keys = usePortalQuery<{ keys: { items: PortalKey[] } }>('keys', KEYS_QUERY, { projectID: group?.id, first: 100 }, !!group);
  const price = (value: number | null) =>
    value === null
      ? t('portal.unpriced')
      : t('currencies.format', { val: value, currency, locale: i18n.language === 'zh' ? 'zh-CN' : 'en-US', minimumFractionDigits: 6 });
  if (!group) return <PortalEmpty title={t('portal.noGroups')} description={t('portal.noGroupsHelp')} />;
  if (!group.member) return <PortalEmpty title={t('portal.noModels')} description={t('portal.noModelsHelp')} />;
  return (
    <div className='space-y-6'>
      <h1 className='text-2xl font-semibold'>{t('portal.models')}</h1>
      <div className='flex flex-col gap-3 md:flex-row'>
        <Input
          aria-label={t('portal.search')}
          placeholder={t('portal.search')}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className='md:max-w-md'
        />
        <select
          aria-label={t('portal.allTypes')}
          className='bg-background rounded-md border p-2 text-sm'
          value={type}
          onChange={(e) => setType(e.target.value)}
        >
          <option value=''>{t('portal.allTypes')}</option>
          {['chat', 'embedding', 'rerank', 'image_generation', 'video_generation'].map((type) => (
            <option key={type} value={type}>
              {type}
            </option>
          ))}
        </select>
      </div>
      {models.error ? (
        <PortalError error={models.error} retry={() => void models.refetch()} />
      ) : models.isLoading ? (
        <Skeleton className='h-72 w-full' />
      ) : models.data?.models.length ? (
        <PortalTable
          data={models.data.models}
          columns={[
            {
              id: 'model',
              header: t('portal.model'),
              cell: ({ row }) => (
                <div className='flex max-w-80 items-start gap-3'>
                  <ModelIcon name={row.original.icon} />
                  <div className='min-w-0'>
                    <button
                      className='text-left font-mono text-xs underline-offset-4 hover:underline focus-visible:underline'
                      onClick={async () => {
                        try {
                          await navigator.clipboard.writeText(row.original.id);
                          toast.success(t('portal.copied'));
                        } catch {
                          toast.error(t('portal.copyFailed'));
                        }
                      }}
                    >
                      {row.original.id}
                    </button>
                    <p className='mt-1 font-medium'>{row.original.name}</p>
                    {row.original.remark && (
                      <p title={row.original.remark} className='text-muted-foreground max-w-64 truncate text-xs'>
                        {row.original.remark}
                      </p>
                    )}
                  </div>
                </div>
              ),
            },
            {
              id: 'type',
              header: t('portal.allTypes'),
              cell: ({ row }) => (
                <div className='flex max-w-56 flex-wrap gap-1'>
                  <Badge variant='secondary'>{row.original.type}</Badge>
                  {row.original.vision && <Badge variant='outline'>Vision</Badge>}
                  {row.original.toolCall && <Badge variant='outline'>Tool call</Badge>}
                  {row.original.reasoning && <Badge variant='outline'>Reasoning</Badge>}
                </div>
              ),
            },
            {
              id: 'context',
              header: t('portal.context'),
              cell: ({ row }) => (
                <span className='font-mono text-xs'>
                  {row.original.contextLength?.toLocaleString() || '-'} / {row.original.maxOutputTokens?.toLocaleString() || '-'}
                </span>
              ),
            },
            {
              id: 'price',
              header: t('portal.price'),
              cell: ({ row }) => (
                <span className='font-mono text-xs whitespace-nowrap'>
                  {price(row.original.inputPrice)} / {price(row.original.outputPrice)}
                </span>
              ),
            },
            {
              id: 'example',
              header: t('portal.actions'),
              cell: ({ row }) => (
                <Button variant='outline' size='sm' onClick={() => setExample(row.original)}>
                  {t('portal.example')}
                </Button>
              ),
            },
          ]}
        />
      ) : (
        <PortalEmpty title={t(search || type ? 'portal.noMatches' : 'portal.noModels')} description={t('portal.noModelsHelp')} />
      )}
      {example && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) setExample(null);
          }}
        >
          <DialogContent className='max-h-[90dvh] overflow-y-auto sm:max-w-4xl'>
            <DialogHeader>
              <DialogTitle>{example.name}</DialogTitle>
            </DialogHeader>
            <QuickStart keys={keys.data?.keys.items || []} initialModel={example} />
          </DialogContent>
        </Dialog>
      )}
    </div>
  );
}
