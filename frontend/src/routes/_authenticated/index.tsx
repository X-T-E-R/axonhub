import { createFileRoute } from '@tanstack/react-router';
import Dashboard from '@/features/dashboard';
import NormalUserPortal from '@/features/normal-user-portal';
import { useAuthStore } from '@/stores/authStore';

export const Route = createFileRoute('/_authenticated/')({
  component: Home,
});

function Home() {
  const user = useAuthStore((state) => state.auth.user);
  return user?.isOwner ? <Dashboard /> : <NormalUserPortal />;
}
