import { createFileRoute } from '@tanstack/react-router';
import NormalUserPortal from '@/features/normal-user-portal';

export const Route = createFileRoute('/_authenticated/self-service/api-keys/')({
  component: MyAPIKeysRoute,
});

function MyAPIKeysRoute() {
  return <NormalUserPortal initialSection='keys' />;
}
