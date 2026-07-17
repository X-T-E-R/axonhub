import { createFileRoute } from '@tanstack/react-router';
import NormalUserPortal from '@/features/normal-user-portal';
import { validateSelfServiceHandoff } from '@/features/normal-user-portal/workflow';

export const Route = createFileRoute('/_authenticated/self-service/quickstart/')({
  validateSearch: validateSelfServiceHandoff,
  component: QuickstartRoute,
});

function QuickstartRoute() {
  const handoff = Route.useSearch();
  return <NormalUserPortal initialSection='quickstart' handoff={handoff} />;
}
