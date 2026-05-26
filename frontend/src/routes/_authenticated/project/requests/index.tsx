import { createFileRoute } from '@tanstack/react-router';
import { ProjectGuard } from '@/components/project-guard';
import { RouteGuard } from '@/components/route-guard';
import RequestsManagement from '@/features/requests';

function ProtectedProjectRequests() {
  return (
    <ProjectGuard>
      <RouteGuard routePath='/project/requests'>
        <RequestsManagement />
      </RouteGuard>
    </ProjectGuard>
  );
}

export const Route = createFileRoute('/_authenticated/project/requests/')({
  component: ProtectedProjectRequests,
});
