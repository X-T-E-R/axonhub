import { createFileRoute } from '@tanstack/react-router';
import { ProjectGuard } from '@/components/project-guard';
import { RouteGuard } from '@/components/route-guard';
import AccessGroups from '@/features/access-groups';

function ProtectedProjectAccessGroups() {
  return (
    <ProjectGuard>
      <RouteGuard routePath='/project/access-groups'>
        <AccessGroups />
      </RouteGuard>
    </ProjectGuard>
  );
}

export const Route = createFileRoute('/_authenticated/project/access-groups/')({
  component: ProtectedProjectAccessGroups,
});
