import { createFileRoute } from '@tanstack/react-router';
import { RouteGuard } from '@/components/route-guard';
import MonitoringPage from '@/features/monitoring';

function ProtectedMonitoring() {
  return (
    <RouteGuard requiredScopes={['read_channels']} scopeLevel='system'>
      <MonitoringPage />
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/monitoring/')({
  component: ProtectedMonitoring,
});
