import { Link, useNavigate, useSearch } from '@tanstack/react-router';
import { useTranslation } from 'react-i18next';
import { Skeleton } from '@/components/ui/skeleton';
import { CONTENT_QUERY, type PortalContent, usePortalQuery } from './data';
import { PortalEmpty, PortalError } from './layout';
import { PortalMarkdown } from './markdown';

export default function PortalGuides() {
  const { t } = useTranslation();
  const search = useSearch({ strict: false }) as { doc?: string; tab?: string };
  const navigate = useNavigate();
  const content = usePortalQuery<{ content: PortalContent }>('content', CONTENT_QUERY);
  if (content.error) return <PortalError error={content.error} retry={() => void content.refetch()} />;
  if (!content.data) return <Skeleton className='h-96 w-full' />;
  const { announcements, guides } = content.data.content;
  const documents = [...announcements, ...guides];
  const selected =
    documents.find((d) => d.slug === search.doc) || (search.tab === 'announcements' ? announcements[0] : guides[0]) || documents[0];
  return (
    <div className='space-y-6'>
      <h1 className='text-2xl font-semibold'>{t('portal.guides')}</h1>
      {!selected ? (
        <PortalEmpty title={t('portal.noContent')} />
      ) : (
        <div className='grid min-w-0 gap-8 md:grid-cols-[240px_minmax(0,1fr)]'>
          <nav className='hidden space-y-6 md:block'>
            {[
              { title: t('portal.announcement'), docs: announcements },
              { title: t('portal.tutorial'), docs: guides },
            ].map((section) => (
              <div key={section.title} className='space-y-2'>
                <h2 className='text-muted-foreground text-xs font-medium'>{section.title}</h2>
                {section.docs.map((d) => (
                  <Link
                    key={d.slug}
                    to='/portal/guides'
                    search={{ doc: d.slug }}
                    className={`block rounded-md px-3 py-2 text-sm ${selected.slug === d.slug ? 'bg-accent font-medium' : 'hover:bg-muted'}`}
                  >
                    {d.title}
                  </Link>
                ))}
              </div>
            ))}
          </nav>
          <div className='max-w-3xl min-w-0 space-y-5'>
            <select
              aria-label={t('portal.guideSelect')}
              className='bg-background w-full rounded-md border p-2 text-sm md:hidden'
              value={selected.slug}
              onChange={(e) => void navigate({ to: '/portal/guides', search: { doc: e.target.value } })}
            >
              {documents.map((d) => (
                <option key={d.slug} value={d.slug}>
                  {d.title}
                </option>
              ))}
            </select>
            <h2 className='text-xl font-semibold'>{selected.title}</h2>
            <PortalMarkdown key={selected.slug}>{selected.markdown}</PortalMarkdown>
          </div>
        </div>
      )}
    </div>
  );
}
