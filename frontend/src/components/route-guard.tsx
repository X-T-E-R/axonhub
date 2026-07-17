import { useEffect } from 'react';
import { useRouter } from '@tanstack/react-router';
import { IconShieldX, IconArrowLeft } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { useRoutePermissions } from '@/hooks/useRoutePermissions';
import { type ScopeLevel } from '@/config/route-permission';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';

interface RouteGuardProps {
  children: React.ReactNode;
  requiredScopes?: string[];
  routePath?: string;
  scopeLevel?: ScopeLevel;
  fallbackPath?: string;
  showForbidden?: boolean;
  requireProjectOwner?: boolean; // 是否需要项目所有者权限
}

export function RouteGuard({
  children,
  requiredScopes = [],
  routePath,
  scopeLevel,
  fallbackPath = '/',
  showForbidden = true,
  requireProjectOwner = false,
}: RouteGuardProps) {
  const router = useRouter();
  const { userScopes, systemScopes, projectScopes, isOwner, isProjectOwner, checkRouteAccess } = useRoutePermissions();

  const scopesToCheck = scopeLevel === 'system' ? systemScopes : scopeLevel === 'project' ? projectScopes : userScopes;
  const hasRequiredScopes =
    isOwner ||
    ((requireProjectOwner ? isProjectOwner : true) &&
      (requiredScopes.length === 0 ||
        scopesToCheck.includes('*') ||
        requiredScopes.some((scope) => scopesToCheck.includes(scope))));
  const hasConfiguredRouteAccess = routePath ? checkRouteAccess(routePath).hasAccess : true;
  const hasAccess = hasRequiredScopes && hasConfiguredRouteAccess;

  useEffect(() => {
    if (!hasAccess && !showForbidden) {
      // 如果没有权限且不显示禁止页面，则重定向
      router.navigate({ to: fallbackPath });
    }
  }, [hasAccess, showForbidden, fallbackPath, router]);

  if (!hasAccess) {
    if (showForbidden) {
      return <ForbiddenPage onGoBack={() => router.navigate({ to: fallbackPath })} />;
    }
    return null; // 重定向中，不显示任何内容
  }

  return <>{children}</>;
}

function ForbiddenPage({ onGoBack }: { onGoBack: () => void }) {
  const { t } = useTranslation();

  return (
    <div className='flex h-screen items-center justify-center'>
      <div className='max-w-md text-center'>
        <div className='mb-6'>
          <IconShieldX className='mx-auto h-16 w-16 text-red-500' />
        </div>

        <Alert className='mb-6'>
          <IconShieldX className='h-4 w-4' />
          <AlertTitle>{t('common.routeGuard.accessDenied')}</AlertTitle>
          <AlertDescription>{t('common.routeGuard.noPermission')}</AlertDescription>
        </Alert>

        <Button onClick={onGoBack} variant='outline' className='gap-2'>
          <IconArrowLeft className='h-4 w-4' />
          {t('common.routeGuard.goBack')}
        </Button>
      </div>
    </div>
  );
}
