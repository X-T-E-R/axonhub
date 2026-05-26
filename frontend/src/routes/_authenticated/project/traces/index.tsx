import { createFileRoute } from '@tanstack/react-router';
import { ProjectGuard } from '@/components/project-guard';
import { RouteGuard } from '@/components/route-guard';
import TracesManagement from '@/features/traces';

function ProtectedProjectTraces() {
  return (
    <ProjectGuard>
      <RouteGuard routePath='/project/traces'>
        <TracesManagement />
      </RouteGuard>
    </ProjectGuard>
  );
}

export const Route = createFileRoute('/_authenticated/project/traces/')({
  component: ProtectedProjectTraces,
});
