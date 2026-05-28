import { createFileRoute } from '@tanstack/react-router';
import NormalUserPortal from '@/features/normal-user-portal';

export const Route = createFileRoute('/_authenticated/self-service/models/')({
  component: ModelMarketplaceRoute,
});

function ModelMarketplaceRoute() {
  return <NormalUserPortal initialSection='models' />;
}
