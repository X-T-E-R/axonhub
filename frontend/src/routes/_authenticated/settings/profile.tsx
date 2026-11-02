import { createFileRoute } from '@tanstack/react-router';
import { useAuthStore } from '@/stores/authStore';
import PortalProfile from '@/features/portal/profile';
import SettingsProfile from '@/features/settings/profile';

function AccountProfile() {
  const portal = useAuthStore((s) => s.auth.user?.accessMode === 'portal');
  return portal ? <PortalProfile /> : <SettingsProfile />;
}

export const Route = createFileRoute('/_authenticated/settings/profile')({
  component: AccountProfile,
  validateSearch: (search: Record<string, unknown>) => {
    return {
      oidc_link: search.oidc_link as string | undefined,
    };
  },
});
