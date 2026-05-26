import { createFileRoute } from '@tanstack/react-router';
import { RouteGuard } from '@/components/route-guard';
import ChannelsManagement from '@/features/channels';

function ProtectedChannelsManagement() {
  return (
    <RouteGuard routePath='/channels'>
      <ChannelsManagement />
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/channels/')({
  component: ProtectedChannelsManagement,
});
