import { createFileRoute } from '@tanstack/react-router';
import NormalUserPortal from '@/features/normal-user-portal';

export const Route = createFileRoute('/_authenticated/self-service/usage/')({
  component: UsageRoute,
});

function UsageRoute() {
  return <NormalUserPortal initialSection='usage' />;
}
