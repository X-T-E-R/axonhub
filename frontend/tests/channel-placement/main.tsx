import { createMemoryHistory, createRootRouteWithContext, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import ReactDOM from 'react-dom/client';
import '@/index.css';
import i18n from '@/lib/i18n';
import { FontProvider } from '@/context/font-context';
import { SearchProvider } from '@/context/search-context';
import { ThemeProvider } from '@/context/theme-context';
import { useAuthStore } from '@/stores/authStore';
import ChannelsManagement from '@/features/channels';
import providersData from '@/features/models/data/providers.json';

// This fixture is intentionally loopback-only. The production provider catalog hook
// falls back to the checked-in catalog when the external catalog is unavailable; the
// guard below makes that fallback deterministic and prevents any external request.
const originalFetch = window.fetch.bind(window);
window.fetch = (input, init) => {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  if (/^https?:\/\//i.test(url) && !url.startsWith(window.location.origin)) {
    if (url.includes('raw.githubusercontent.com')) {
      return Promise.resolve(
        new Response(JSON.stringify(providersData), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    }
    return Promise.reject(new Error('External requests are disabled by the channel-placement fixture'));
  }
  return originalFetch(input, init);
};

const requestedLanguage = new URLSearchParams(window.location.search).get('lang');
await i18n.changeLanguage(requestedLanguage === 'zh' ? 'zh' : 'en');

const requestedRole = new URLSearchParams(window.location.search).get('role') ?? 'readonly';
const isPrivileged = requestedRole === 'admin' || requestedRole === 'owner';
const auth = useAuthStore.getState().auth;
auth.reset();
auth.setUser({
  id: `fixture-${isPrivileged ? requestedRole : 'read-only'}-user`,
  email: 'fixture@example.invalid',
  firstName: 'Channel',
  lastName: 'Fixture',
  isOwner: requestedRole === 'owner',
  accessMode: 'legacy',
  preferLanguage: 'en',
  scopes: ['read_channels', 'read_system', ...(isPrivileged ? ['write_channels'] : [])],
  roles: [],
  projects: [],
  oidcIdentities: [],
  hasPassword: true,
});
if (isPrivileged) {
  auth.setAccessToken(`channel-placement-fixture-${requestedRole}-token`);
}
localStorage.setItem('channels-table-page-size', '20');
localStorage.setItem('channels-table-sorting', JSON.stringify([{ id: 'createdAt', desc: true }]));
localStorage.removeItem('channels-table-column-visibility');

type FixtureRouterContext = {
  queryClient: QueryClient;
};

const rootRoute = createRootRouteWithContext<FixtureRouterContext>()({
  component: () => <Outlet />,
});

const channelsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: ChannelsManagement,
});

const routeTree = rootRoute.addChildren([channelsRoute]);
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: false,
      refetchOnWindowFocus: false,
    },
  },
});
const router = createRouter({
  routeTree,
  context: { queryClient },
  history: createMemoryHistory({ initialEntries: ['/'] }),
});

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router;
  }
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <QueryClientProvider client={queryClient}>
    <ThemeProvider defaultTheme='light' defaultColorScheme='claude'>
      <FontProvider>
        <SearchProvider>
          <RouterProvider router={router} />
        </SearchProvider>
      </FontProvider>
    </ThemeProvider>
  </QueryClientProvider>
);
