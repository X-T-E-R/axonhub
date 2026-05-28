import { createFileRoute } from '@tanstack/react-router';
import NormalUserPortal from '@/features/normal-user-portal';

export const Route = createFileRoute('/_authenticated/self-service/requests/')({
  component: MyRequestsRoute,
});

function MyRequestsRoute() {
  return <NormalUserPortal initialSection='requests' />;
}
