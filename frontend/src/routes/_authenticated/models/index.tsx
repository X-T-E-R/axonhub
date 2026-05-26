import { createFileRoute } from '@tanstack/react-router';
import { RouteGuard } from '@/components/route-guard';
import ModelsManagement from '@/features/models';

function ProtectedModelsManagement() {
  return (
    <RouteGuard routePath='/models'>
      <ModelsManagement />
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/models/')({
  component: ProtectedModelsManagement,
});
