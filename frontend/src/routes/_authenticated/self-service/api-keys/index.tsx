import { createFileRoute } from '@tanstack/react-router';
import NormalUserPortal from '@/features/normal-user-portal';
import { validateSelfServiceHandoff } from '@/features/normal-user-portal/workflow';

export const Route = createFileRoute('/_authenticated/self-service/api-keys/')({
  validateSearch: validateSelfServiceHandoff,
  component: MyAPIKeysRoute,
});

function MyAPIKeysRoute() {
  const handoff = Route.useSearch();
  return <NormalUserPortal initialSection='keys' handoff={handoff} />;
}
