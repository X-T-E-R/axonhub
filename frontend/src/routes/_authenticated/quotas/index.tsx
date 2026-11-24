import { createFileRoute } from '@tanstack/react-router';
import { RouteGuard } from '@/components/route-guard';
import QuotasPage from '@/features/quotas';

function ProtectedQuotas() {
  return (
    <RouteGuard requiredScopes={['read_channels']} scopeLevel='system'>
      <QuotasPage />
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/quotas/')({
  component: ProtectedQuotas,
});
