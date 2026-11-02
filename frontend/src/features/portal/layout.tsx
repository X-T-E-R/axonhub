import { createContext, useContext, useEffect, useMemo, useState } from 'react';
import { Link, Outlet } from '@tanstack/react-router';
import { IconBook2, IconChartBar, IconKey, IconStack2 } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { useAuthStore } from '@/stores/authStore';
import { Button } from '@/components/ui/button';
import { SidebarProvider, SidebarTrigger } from '@/components/ui/sidebar';
import { Skeleton } from '@/components/ui/skeleton';
import { LanguageSwitch } from '@/components/language-switch';
import { AppSidebar } from '@/components/layout/app-sidebar';
import { ThemeSwitch } from '@/components/theme-switch';
import { GROUPS_QUERY, usePortalQuery, type PortalGroup } from './data';
import { managementLandingPath } from './entry';

const GroupContext = createContext<PortalGroup | undefined>(undefined);
export const usePortalGroup = () => useContext(GroupContext);
export function PortalError({ error, retry }: { error: unknown; retry: () => void }) {
  const { t } = useTranslation();
  return (
    <div role='alert' className='border-destructive/40 bg-card space-y-3 rounded-2xl border p-6'>
      <h2 className='font-semibold'>{t('portal.loadError')}</h2>
      <p className='text-muted-foreground text-sm'>{error instanceof Error ? error.message : ''}</p>
      <Button variant='outline' onClick={retry}>
        {t('portal.retry')}
      </Button>
    </div>
  );
}
export function PortalEmpty({ title, description, children }: { title: string; description?: string; children?: React.ReactNode }) {
  return (
    <section className='bg-card flex min-h-56 flex-col items-center justify-center gap-3 rounded-2xl border p-8 text-center'>
      <IconStack2 className='text-muted-foreground size-9' />
      <h2 className='text-lg font-semibold'>{title}</h2>
      {description && <p className='text-muted-foreground max-w-lg text-sm'>{description}</p>}
      {children}
    </section>
  );
}
export function PortalLayout({ children }: { children?: React.ReactNode }) {
  const { t } = useTranslation();
  const user = useAuthStore((s) => s.auth.user);
  const groups = usePortalQuery<{ groups: PortalGroup[] }>('groups', GROUPS_QUERY);
  const [selected, setSelected] = useState<number | null>(null);
  useEffect(() => {
    if (groups.data && !groups.data.groups.some((g) => g.id === selected)) setSelected(groups.data.groups[0]?.id ?? null);
  }, [groups.data, selected]);
  const group = groups.data?.groups.find((g) => g.id === selected);
  const name = [user?.firstName, user?.lastName].filter(Boolean).join(' ') || user?.email || '';
  const consolePath = managementLandingPath(user);
  const sidebar = useMemo(
    () => ({
      user: { name, email: user?.email || '', avatar: user?.avatar || name.slice(0, 1) },
      teams: [],
      navGroups: [
        {
          title: t('portal.title'),
          items: [
            { title: t('portal.overview'), url: '/portal' as const, icon: IconChartBar },
            { title: t('portal.keys'), url: '/portal/keys' as const, icon: IconKey },
            { title: t('portal.models'), url: '/portal/models' as const, icon: IconStack2 },
            { title: t('portal.guides'), url: '/portal/guides' as const, icon: IconBook2 },
          ],
        },
      ],
    }),
    [name, user?.email, user?.avatar, t]
  );
  return (
    <SidebarProvider className='min-h-[100dvh] flex-col'>
      <header className='bg-background sticky top-0 z-20 flex h-14 shrink-0 items-center justify-between border-b px-4 md:px-6'>
        <div className='flex min-w-0 items-center gap-3'>
          <SidebarTrigger />
          <img src='/logo.jpg' alt='' className='size-7 rounded' />
          <span className='hidden text-sm font-semibold sm:inline'>AxonHub</span>
          <label className='sr-only' htmlFor='portal-group'>
            {t('portal.group')}
          </label>
          <select
            id='portal-group'
            className='bg-background focus-visible:ring-ring max-w-44 rounded-md border px-2 py-1.5 text-sm focus-visible:ring-2 md:max-w-64'
            value={selected ?? ''}
            onChange={(e) => setSelected(Number(e.target.value))}
          >
            {!groups.data?.groups.length && <option value=''>{t('portal.selectGroup')}</option>}
            {groups.data?.groups.map((g) => (
              <option key={g.id} value={g.id}>
                {g.name}
                {!g.member ? ` (${t('portal.historyOnly')})` : ''}
              </option>
            ))}
          </select>
        </div>
        <div className='flex items-center gap-1'>
          {consolePath && (
            <Button asChild variant='ghost' size='sm' className='hidden md:inline-flex'>
              <Link to={consolePath}>{t('portal.console')}</Link>
            </Button>
          )}
          <LanguageSwitch />
          <ThemeSwitch />
        </div>
      </header>
      <div className='flex min-h-0 flex-1'>
        <AppSidebar sidebarData={sidebar} showMobileControls={false} />
        <main id='main-content' className='mx-auto min-w-0 flex-1 space-y-6 p-4 md:p-8 lg:max-w-7xl'>
          {groups.isLoading ? (
            <div className='space-y-4'>
              <Skeleton className='h-9 w-48' />
              <Skeleton className='h-72 w-full' />
            </div>
          ) : groups.error ? (
            <PortalError error={groups.error} retry={() => void groups.refetch()} />
          ) : (
            <GroupContext.Provider value={group}>
              <div key={selected ?? 'none'}>{children || <Outlet />}</div>
            </GroupContext.Provider>
          )}
        </main>
      </div>
    </SidebarProvider>
  );
}
