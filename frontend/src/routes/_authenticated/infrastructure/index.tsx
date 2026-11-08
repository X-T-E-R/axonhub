import { createFileRoute } from '@tanstack/react-router';
import { RouteGuard } from '@/components/route-guard';
import InfrastructurePage from '@/features/infrastructure';

function ProtectedInfrastructure() {
  return (
    <RouteGuard requiredScopes={['read_channels']} scopeLevel='system'>
      <InfrastructurePage />
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/infrastructure/')({
  component: ProtectedInfrastructure,
});
