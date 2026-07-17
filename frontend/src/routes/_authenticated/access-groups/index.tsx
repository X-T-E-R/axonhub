import { createFileRoute, redirect } from '@tanstack/react-router';

export const Route = createFileRoute('/_authenticated/access-groups/')({
  beforeLoad: () => {
    throw redirect({ to: '/project/access-groups', replace: true });
  },
});
