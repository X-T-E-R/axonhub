import { createFileRoute } from '@tanstack/react-router';
import PortalGuides from '@/features/portal/guides';

export const Route = createFileRoute('/_authenticated/portal/guides')({
  component: PortalGuides,
  validateSearch: (search: Record<string, unknown>): { doc?: string; tab?: string } => ({
    doc: typeof search.doc === 'string' ? search.doc : undefined,
    tab: search.tab === 'announcements' ? search.tab : undefined,
  }),
});
