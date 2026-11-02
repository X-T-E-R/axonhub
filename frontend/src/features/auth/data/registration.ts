import { useQuery } from '@tanstack/react-query';
import { authApi } from '@/lib/api-client';

export function useRegistrationStatus(enabled = true) {
  return useQuery({
    queryKey: ['invitation-registration'],
    queryFn: () => authApi.registrationStatus(),
    enabled,
    staleTime: 0,
    retry: false,
  });
}
