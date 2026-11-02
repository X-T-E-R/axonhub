import { useQuery } from '@tanstack/react-query';
import { GraphQLRequestError } from '@/gql/graphql';
import { useAuthStore, getTokenFromStorage } from '@/stores/authStore';

export interface PortalIdentity {
  id: number;
  email: string;
  firstName: string;
  lastName: string;
  preferLanguage: string;
  accessMode: 'legacy' | 'portal';
  avatar: string;
  hasPassword: boolean;
}
export interface PortalGroup {
  id: number;
  name: string;
  callable: boolean;
  member: boolean;
}
export interface PortalDocument {
  slug: string;
  title: string;
  markdown: string;
  published: boolean;
  pinned: boolean;
  order: number;
}
export interface PortalContent {
  announcements: PortalDocument[];
  guides: PortalDocument[];
}
export interface PortalQuota {
  profileName: string;
  active: boolean;
  requests: number | null;
  totalTokens: number | null;
  cost: string | null;
  usedRequests: number;
  usedTokens: number;
  usedCost: string;
  start: string | null;
  end: string | null;
}
export interface PortalKey {
  id: number;
  projectID: number;
  name: string;
  maskedKey: string;
  status: 'enabled' | 'disabled' | 'archived';
  modelIDs: string[];
  allowedIPs: string[];
  createdAt: string;
  quotas: PortalQuota[];
}
export interface PortalKeyInput {
  name: string;
  modelIDs: string[];
  allowedIPs: string[];
}
export interface PortalModel {
  id: string;
  name: string;
  type: string;
  developer: string;
  icon: string;
  remark: string | null;
  vision: boolean;
  toolCall: boolean;
  reasoning: boolean;
  contextLength: number | null;
  maxOutputTokens: number | null;
  inputPrice: number | null;
  outputPrice: number | null;
  apiFormats: string[];
}
export interface PortalModelChannel {
  id: string;
  name: string;
  models: PortalModel[];
}
export interface ModelUsage {
  modelID: string;
  usageCount: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  totalTokens: number;
  pricedCount: number;
  cost: number | null;
}
export interface PortalRequest {
  id: number;
  createdAt: string;
  status: string;
  modelID: string;
  usageReported: boolean;
  totalTokens: number | null;
  cost: number | null;
}
export interface PortalDashboard {
  requests: number;
  successfulRequests: number;
  usageCount: number;
  inputTokens: number;
  outputTokens: number;
  cachedTokens: number;
  totalTokens: number;
  pricedCount: number;
  cost: number | null;
  models: ModelUsage[];
  days: { day: string; totalTokens: number }[];
  recentRequests: PortalRequest[];
}

export async function portalRequest<T>(query: string, variables?: Record<string, unknown>): Promise<T> {
  const response = await fetch('/portal/graphql', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${getTokenFromStorage()}` },
    body: JSON.stringify({ query, variables }),
    cache: 'no-store',
  });
  if (response.status === 401) throw new GraphQLRequestError('Session expired', { status: 401, isAuthError: true });
  const body = await response.json();
  if (!response.ok || body.errors?.length)
    throw new GraphQLRequestError(body.errors?.[0]?.message || 'Unable to load portal data', { status: response.status });
  return body.data as T;
}
export const IDENTITY_QUERY = `query PortalIdentity { me { id email firstName lastName preferLanguage accessMode avatar hasPassword } }`;
export const KEY_FIELDS = `id projectID name maskedKey status modelIDs allowedIPs createdAt quotas { profileName active requests totalTokens cost usedRequests usedTokens usedCost start end }`;
export const MODEL_FIELDS = `id name type developer icon remark vision toolCall reasoning contextLength maxOutputTokens inputPrice outputPrice apiFormats`;
export const DOCUMENT_FIELDS = `slug title markdown published pinned order`;
export function usePortalQuery<T>(name: string, query: string, variables: Record<string, unknown> = {}, enabled = true) {
  const userID = useAuthStore((s) => s.auth.user?.id);
  return useQuery({
    queryKey: ['portal', userID, name, variables],
    queryFn: () => portalRequest<T>(query, variables),
    enabled,
    retry: false,
    staleTime: 0,
  });
}
export function usePortalCurrency() {
  return usePortalQuery<{ currencyCode: string }>('currency', 'query PortalCurrency { currencyCode }').data?.currencyCode;
}
export const GROUPS_QUERY = `query PortalGroups { groups { id name callable member } }`;
export const KEYS_QUERY = `query PortalKeys($projectID: Int, $first: Int!, $after: Int) { keys(projectID: $projectID, first: $first, after: $after) { items { ${KEY_FIELDS} } totalCount nextCursor } }`;
export const MODELS_QUERY = `query PortalModels($projectID: Int!, $keyID: Int, $search: String, $type: String) { models(projectID: $projectID, keyID: $keyID, search: $search, type: $type) { ${MODEL_FIELDS} } }`;
export const MODEL_CHANNELS_QUERY = `query PortalModelChannels($projectID: Int!, $keyID: Int, $search: String, $type: String) { modelChannels(projectID: $projectID, keyID: $keyID, search: $search, type: $type) { id name models { ${MODEL_FIELDS} } } }`;
export const CONTENT_QUERY = `query PortalContent { content { announcements { ${DOCUMENT_FIELDS} } guides { ${DOCUMENT_FIELDS} } } }`;
export const DASHBOARD_QUERY = `query PortalDashboard($projectID: Int, $period: String!, $keyID: Int) { dashboard(projectID: $projectID, period: $period, keyID: $keyID) { requests successfulRequests usageCount inputTokens outputTokens cachedTokens totalTokens pricedCount cost models { modelID usageCount inputTokens outputTokens cachedTokens totalTokens pricedCount cost } days { day totalTokens } recentRequests { id createdAt status modelID usageReported totalTokens cost } } }`;
