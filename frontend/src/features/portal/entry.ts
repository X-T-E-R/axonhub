import { routeConfigs } from '@/config/route-permission';
import type { AuthUser } from '@/stores/authStore';

// Only guarded management routes participate. Settings and unguarded playground
// entries must not turn an ordinary user into a console user.
export function managementLandingPath(user: AuthUser | null): string | null {
  if (!user || user.accessMode === 'portal') return null;
  if (user.isOwner) return '/';
  for (const group of routeConfigs) {
    if (group.title === 'Settings') continue;
    for (const route of group.routes) {
      if (!route.requiredScopes?.length) continue;
      const level = route.scopeLevel || group.scopeLevel || 'any';
      const has = (scopes: string[]) => scopes.includes('*') || route.requiredScopes!.some((scope) => scopes.includes(scope));
      if (!route.requireProjectOwner && level !== 'project' && has(user.scopes)) return route.path;
      if (level !== 'system' && user.projects.some((p) => (!route.requireProjectOwner || p.isOwner) && has(p.scopes))) return route.path;
    }
  }
  return null;
}
export const userLandingPath = (user: AuthUser | null) => managementLandingPath(user) || '/portal';
