import { useEffect, useState } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Skeleton } from '@/components/ui/skeleton';
import { Switch } from '@/components/ui/switch';
import { Textarea } from '@/components/ui/textarea';
import { DOCUMENT_FIELDS, type PortalContent, type PortalDocument } from '@/features/portal/data';
import { PortalError } from '@/features/portal/layout';
import { PortalMarkdown } from '@/features/portal/markdown';

export function PortalSettings() {
  const { t } = useTranslation();
  const query = useQuery({
    queryKey: ['portal-settings'],
    queryFn: () =>
      graphqlRequest<{ portalSettings: PortalContent }>(
        `query PortalSettings { portalSettings { announcements { ${DOCUMENT_FIELDS} } guides { ${DOCUMENT_FIELDS} } } }`
      ),
  });
  const [draft, setDraft] = useState<PortalContent>({ announcements: [], guides: [] });
  const [category, setCategory] = useState<'announcements' | 'guides'>('guides');
  const [selected, setSelected] = useState(0);
  useEffect(() => {
    if (query.data) setDraft(query.data.portalSettings);
  }, [query.data]);
  const save = useMutation({
    mutationFn: () =>
      graphqlRequest(
        `mutation SavePortal($input: PortalSettingsInput!) { updatePortalSettings(input: $input) { guides { ${DOCUMENT_FIELDS} } announcements { ${DOCUMENT_FIELDS} } } }`,
        { input: draft }
      ),
    onSuccess: () => void query.refetch(),
  });
  const doc = draft[category][selected];
  function change(patch: Partial<PortalDocument>) {
    setDraft((old) => ({ ...old, [category]: old[category].map((d, i) => (i === selected ? { ...d, ...patch } : d)) }));
  }
  if (query.error) return <PortalError error={query.error} retry={() => void query.refetch()} />;
  if (query.isLoading) return <Skeleton className='h-96 w-full' />;
  return (
    <div className='space-y-5'>
      <div className='flex flex-wrap gap-3'>
        <select
          aria-label={t('portal.portalSettings')}
          className='bg-background rounded-md border p-2 text-sm'
          value={category}
          onChange={(e) => {
            setCategory(e.target.value as typeof category);
            setSelected(0);
          }}
        >
          <option value='announcements'>{t('portal.announcement')}</option>
          <option value='guides'>{t('portal.tutorial')}</option>
        </select>
        <Button
          variant='outline'
          onClick={() => {
            setDraft((old) => ({
              ...old,
              [category]: [
                ...old[category],
                {
                  slug: `document-${Date.now()}`,
                  title: t('portal.documentTitle'),
                  markdown: '',
                  published: false,
                  pinned: false,
                  order: old[category].length,
                },
              ],
            }));
            setSelected(draft[category].length);
          }}
        >
          {t('portal.addDocument')}
        </Button>
        <Button disabled={save.isPending} onClick={() => save.mutate()}>
          {t('portal.save')}
        </Button>
      </div>
      {(save.error || save.isSuccess) && (
        <p role={save.error ? 'alert' : 'status'} className={save.error ? 'text-destructive text-sm' : 'text-sm'}>
          {save.error?.message || t('portal.saved')}
        </p>
      )}
      <div className='grid gap-6 md:grid-cols-[200px_minmax(0,1fr)]'>
        <nav className='space-y-1'>
          {draft[category].map((d, i) => (
            <button
              key={i}
              className={`block w-full rounded-md px-3 py-2 text-left text-sm ${i === selected ? 'bg-accent' : 'hover:bg-muted'}`}
              onClick={() => setSelected(i)}
            >
              {d.title}
            </button>
          ))}
        </nav>
        {doc && (
          <div className='min-w-0 space-y-4'>
            <div className='grid gap-3 md:grid-cols-2'>
              <div className='space-y-1'>
                <Label htmlFor='doc-slug'>{t('portal.slug')}</Label>
                <Input id='doc-slug' value={doc.slug} onChange={(e) => change({ slug: e.target.value })} />
              </div>
              <div className='space-y-1'>
                <Label htmlFor='doc-title'>{t('portal.documentTitle')}</Label>
                <Input id='doc-title' value={doc.title} onChange={(e) => change({ title: e.target.value })} />
              </div>
            </div>
            <div className='flex flex-wrap items-center gap-4'>
              <Label className='flex items-center gap-2'>
                <Switch checked={doc.published} onCheckedChange={(published) => change({ published })} />
                {t('portal.published')}
              </Label>
              <Label className='flex items-center gap-2'>
                <Switch checked={doc.pinned} onCheckedChange={(pinned) => change({ pinned })} />
                {t('portal.pinned')}
              </Label>
              <Label className='flex items-center gap-2'>
                {t('portal.order')}
                <Input type='number' className='w-20' value={doc.order} onChange={(e) => change({ order: Number(e.target.value) })} />
              </Label>
              <Button
                variant='ghost'
                onClick={() => {
                  setDraft((old) => ({ ...old, [category]: old[category].filter((_, i) => i !== selected) }));
                  setSelected(0);
                }}
              >
                {t('portal.delete')}
              </Button>
            </div>
            <div className='grid min-w-0 gap-4 lg:grid-cols-2'>
              <div className='space-y-2'>
                <Label htmlFor='doc-markdown'>{t('portal.markdown')}</Label>
                <Textarea
                  id='doc-markdown'
                  className='min-h-96 font-mono text-sm'
                  value={doc.markdown}
                  onChange={(e) => change({ markdown: e.target.value })}
                />
              </div>
              <div className='min-w-0 space-y-2'>
                <Label>{t('portal.preview')}</Label>
                <div className='max-h-[600px] overflow-auto rounded-lg border p-4'>
                  <PortalMarkdown>{doc.markdown}</PortalMarkdown>
                </div>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
