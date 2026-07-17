import { createFileRoute } from '@tanstack/react-router';
import { RouteGuard } from '@/components/route-guard';
import Dashboard from '@/features/dashboard';
import NormalUserPortal from '@/features/normal-user-portal';
import { useAuthStore } from '@/stores/authStore';

function ProtectedDashboard() {
  return (
    <RouteGuard requiredScopes={['read_dashboard']} scopeLevel='system'>
      <Dashboard />
    </RouteGuard>
  );
}

export const Route = createFileRoute('/_authenticated/')({
  component: Home,
});

function Home() {
  const user = useAuthStore((state) => state.auth.user);
  return user?.isOwner ? <ProtectedDashboard /> : <NormalUserPortal />;
}
