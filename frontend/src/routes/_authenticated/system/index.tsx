import { createFileRoute } from '@tanstack/react-router';
import { RouteGuard } from '@/components/route-guard';
import SystemManagement from '@/features/system';

type SystemTabKey =
  | 'brand'
  | 'storage'
  | 'retry'
  | 'webhook'
  | 'about'
  | 'general'
  | 'registration'
  | 'proxy'
  | 'quota'
  | 'backup'
  | 'diagnostics';

function ProtectedSystem() {
  const search = Route.useSearch();

  return (
    <RouteGuard routePath='/system' requiredScopes={['read_settings']} scopeLevel='system'>
      <SystemManagement initialTab={search.tab as SystemTabKey | undefined} />
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/system/')({
  component: ProtectedSystem,
  validateSearch: (search: { tab?: SystemTabKey }) => search,
});
