import { createFileRoute } from '@tanstack/react-router';
import { RouteGuard } from '@/components/route-guard';
import AccessGroups from '@/features/access-groups';

function ProtectedAccessGroups() {
  return (
    <RouteGuard routePath='/access-groups'>
      <AccessGroups />
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/access-groups/')({
  component: ProtectedAccessGroups,
});
