import { AuthUser, getTokenFromStorage } from '@/stores/authStore';

// Same domain, no need to add baseURL.
export const API_BASE_URL = '';

type ErrorResponseBody = {
  message?: string;
  error?: string | { message?: string };
};

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null;

const isErrorResponseBody = (value: unknown): value is ErrorResponseBody => {
  if (!isRecord(value)) {
    return false;
  }

  const message = value.message;
  const error = value.error;

  const hasValidMessage = message === undefined || typeof message === 'string';

  const hasValidError =
    error === undefined ||
    typeof error === 'string' ||
    (isRecord(error) && (error.message === undefined || typeof error.message === 'string'));

  return hasValidMessage && hasValidError;
};

interface ApiRequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'DELETE' | 'PATCH';
  headers?: Record<string, string>;
  body?: any;
  requireAuth?: boolean;
}

class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
    public response?: unknown
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

export async function apiRequest<T>(endpoint: string, options: ApiRequestOptions = {}): Promise<T> {
  const { method = 'GET', headers = {}, body, requireAuth = false } = options;

  const url = `${API_BASE_URL}${endpoint}`;

  const requestHeaders: Record<string, string> = {
    'Content-Type': 'application/json',
    ...headers,
  };

  // Add Authorization header if auth is required
  if (requireAuth) {
    const token = getTokenFromStorage();
    if (token) {
      requestHeaders['Authorization'] = `Bearer ${token}`;
    }
  }

  const requestOptions: RequestInit = {
    method,
    headers: requestHeaders,
  };

  if (body && method !== 'GET') {
    requestOptions.body = JSON.stringify(body);
  }

  try {
    const response = await fetch(url, requestOptions);

    if (!response.ok) {
      let errorMessage = `HTTP ${response.status}: ${response.statusText}`;
      let errorData: unknown = null;

      try {
        errorData = await response.json();
        if (isErrorResponseBody(errorData)) {
          if (errorData.message) {
            errorMessage = errorData.message;
          } else if (errorData.error) {
            errorMessage = typeof errorData.error === 'string' ? errorData.error : errorData.error?.message || errorMessage;
          }
        }
      } catch {
        // If response is not JSON, use status text
      }

      throw new ApiError(errorMessage, response.status, errorData);
    }

    // Handle empty responses
    const contentType = response.headers.get('content-type');
    if (contentType && contentType.includes('application/json')) {
      return (await response.json()) as T;
    }

    return {} as T;
  } catch (error) {
    if (error instanceof ApiError) {
      throw error;
    }

    const message = error instanceof Error ? error.message : 'Network error occurred';
    throw new ApiError(message, 0);
  }
}

export interface AdminRegistrationPolicy {
  enabled: boolean;
  oidcEnabled: boolean;
  inviteCode: string;
  inviteCodeRequired: boolean;
  defaultProjectId: number;
  autoJoinFirstProject: boolean;
  defaultProjectScopes: string[];
  allowRequestDetails: boolean;
  selfServicePresetNames: string[];
  passwordSignupAllowed: boolean;
}

// System API endpoints
export const systemApi = {
  getStatus: (): Promise<{ isInitialized: boolean }> => apiRequest('/admin/system/status'),

  initialize: (data: {
    ownerEmail: string;
    ownerPassword: string;
    ownerFirstName: string;
    ownerLastName: string;
    brandName: string;
    preferLanguage?: string;
  }): Promise<{ success: boolean; message: string }> =>
    apiRequest('/admin/system/initialize', {
      method: 'POST',
      body: data,
    }),
};

// Auth API endpoints
export const authApi = {
  signIn: (data: {
    email: string;
    password: string;
  }): Promise<{
    user: AuthUser;
    token: string;
  }> =>
    apiRequest('/admin/auth/signin', {
      method: 'POST',
      body: data,
    }),

  signUpPolicy: (): Promise<{
    enabled: boolean;
    oidcSignupAllowed: boolean;
    inviteCodeRequired: boolean;
    passwordSignupAllowed: boolean;
    allowRequestDetails: boolean;
  }> => apiRequest('/admin/auth/signup-policy'),

  adminRegistrationPolicy: (): Promise<{ data: AdminRegistrationPolicy }> =>
    apiRequest('/admin/auth/registration-policy', { requireAuth: true }),

  updateAdminRegistrationPolicy: (
    data: Omit<AdminRegistrationPolicy, 'inviteCodeRequired' | 'passwordSignupAllowed'>
  ): Promise<{ data: AdminRegistrationPolicy }> =>
    apiRequest('/admin/auth/registration-policy', {
      method: 'PUT',
      requireAuth: true,
      body: data,
    }),

  signUp: (data: {
    email: string;
    password: string;
    firstName?: string;
    lastName?: string;
    inviteCode?: string;
  }): Promise<{
    user: AuthUser;
    token: string;
  }> =>
    apiRequest('/admin/auth/signup', {
      method: 'POST',
      body: data,
    }),

  getOIDCProviders: (): Promise<{
    data: {
      id: string;
      name: string;
      display_name: string;
      jit_enabled: boolean;
      icon_url: string;
      button_color: string;
      active?: boolean;
      oidc_login_only: boolean;
      is_linked: boolean;
      linked_identity_id?: string;
      linked_email?: string;
    }[];
  }> => apiRequest('/oauth/oidc/providers', { requireAuth: true }),

  getOIDCAuthorizeURL: (provider: string): Promise<{ data: { url: string; state: string } }> =>
    apiRequest(`/oauth/oidc/authorize/${provider}`),

  getOIDCLinkAuthorizeURL: (provider: string): Promise<{ data: { url: string; state: string } }> =>
    apiRequest(`/admin/oidc/link/${provider}`, { requireAuth: true }),

  exchangeOIDCCode: (code: string): Promise<{
    data: {
      user: AuthUser;
      token: string;
    }
  }> =>
    apiRequest('/oauth/oidc/exchange', {
      method: 'POST',
      body: { code },
    }),
};

export interface SelfRoutingPreset {
  id: number;
  name: string;
  description: string;
  profile?: {
    name?: string;
    modelIDs?: string[];
    channelTags?: string[];
    quota?: unknown;
  };
}

export interface SelfAPIKey {
  id: number;
  name: string;
  status: string;
  type: string;
  createdAt: string;
  updatedAt: string;
  activeProfile: string;
  key?: string;
}

export interface SelfModel {
  id: string;
  name: string;
  developers?: string[];
  groups?: string[];
  presetId?: number;
}

export interface SelfRequest {
  id: number;
  createdAt: string;
  modelId: string;
  apiKeyId?: number;
  status: string;
  source: string;
  format: string;
  stream: boolean;
  latencyMs?: number;
  detailsVisible: boolean;
}

export interface SelfUsage {
  requests: number;
  promptTokens: number;
  completionTokens: number;
  totalTokens: number;
  totalCost: number;
}

const dataOf = <T>(promise: Promise<{ data: T }>) => promise.then((response) => response.data);

export const selfServiceApi = {
  routingPresets: (projectId: string): Promise<SelfRoutingPreset[]> =>
    dataOf(apiRequest<{ data: SelfRoutingPreset[] }>(`/admin/self/routing-presets?project_id=${encodeURIComponent(projectId)}`, { requireAuth: true })),
  apiKeys: (projectId: string): Promise<SelfAPIKey[]> =>
    dataOf(apiRequest<{ data: SelfAPIKey[] }>(`/admin/self/api-keys?project_id=${encodeURIComponent(projectId)}`, { requireAuth: true })),
  createAPIKey: (input: { projectId: string; name: string; presetId: string }): Promise<SelfAPIKey> =>
    dataOf(apiRequest<{ data: SelfAPIKey }>('/admin/self/api-keys', { method: 'POST', requireAuth: true, body: input })),
  rotateAPIKey: (id: number): Promise<SelfAPIKey> =>
    dataOf(apiRequest<{ data: SelfAPIKey }>(`/admin/self/api-keys/${id}/rotate`, { method: 'POST', requireAuth: true })),
  updateAPIKeyStatus: (id: number, status: string): Promise<SelfAPIKey> =>
    dataOf(apiRequest<{ data: SelfAPIKey }>(`/admin/self/api-keys/${id}/status`, { method: 'PATCH', requireAuth: true, body: { status } })),
  models: (projectId: string, presetId?: number): Promise<SelfModel[]> => {
    const params = new URLSearchParams({ project_id: projectId });
    if (presetId) {
      params.set('preset_id', String(presetId));
    }
    return dataOf(apiRequest<{ data: SelfModel[] }>(`/admin/self/models?${params.toString()}`, { requireAuth: true }));
  },
  requests: (projectId: string): Promise<SelfRequest[]> =>
    dataOf(apiRequest<{ data: SelfRequest[] }>(`/admin/self/requests?project_id=${encodeURIComponent(projectId)}`, { requireAuth: true })),
  usage: (projectId: string): Promise<SelfUsage> =>
    dataOf(apiRequest<{ data: SelfUsage }>(`/admin/self/usage?project_id=${encodeURIComponent(projectId)}`, { requireAuth: true })),
};
