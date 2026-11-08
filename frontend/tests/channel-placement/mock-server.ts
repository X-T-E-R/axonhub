import type { IncomingMessage, ServerResponse } from 'node:http';

export const MACHINE_IDS = ['node-alpha', 'node-beta', 'node-gamma'] as const;
export const MODEL_IDS = ['gpt-4o', 'claude-3-5-sonnet', 'gemini-2.5-pro'] as const;

const BUCKETS: Array<string | null> = [...MACHINE_IDS, null];
const CHANNEL_TYPES = ['openai', 'anthropic', 'gemini'] as const;

export type GraphQLLogEntry = {
  at: string;
  operationName?: string;
  variables?: Record<string, unknown>;
  resultSummary: Record<string, unknown>;
};

type SyntheticChannel = {
  id: string;
  createdAt: string;
  updatedAt: string;
  type: (typeof CHANNEL_TYPES)[number];
  baseURL: string;
  name: string;
  status: 'enabled' | 'disabled' | 'archived';
  supportedModels: string[];
  defaultTestModel: string;
  tags: string[];
  orderingWeight: number;
  errorMessage: null;
  remark: string;
  executionMachine: string | null;
};

const requestLog: GraphQLLogEntry[] = [];

const channels: SyntheticChannel[] = Array.from({ length: 96 }, (_, index) => {
  const bucketIndex = index % BUCKETS.length;
  const machine = BUCKETS[bucketIndex];
  const ordinal = Math.floor(index / BUCKETS.length) + 1;
  const model = MODEL_IDS[index % MODEL_IDS.length];
  const status = index % 17 === 0 ? 'archived' : index % 8 === 0 ? 'disabled' : 'enabled';
  const channelType = CHANNEL_TYPES[index % CHANNEL_TYPES.length];
  const machineLabel = machine ?? 'unassigned';

  return {
    id: `gid://axonhub/Channel/${index + 1}`,
    createdAt: new Date(Date.UTC(2026, 0, 1, 0, index)).toISOString(),
    updatedAt: new Date(Date.UTC(2026, 0, 2, 0, index)).toISOString(),
    type: channelType,
    baseURL: `https://synthetic.example.invalid/${machineLabel}/${ordinal}`,
    name: `Placement ${machineLabel} ${String(ordinal).padStart(2, '0')}`,
    status,
    supportedModels: [model, MODEL_IDS[(index + 1) % MODEL_IDS.length]],
    defaultTestModel: model,
    tags: ['synthetic', machineLabel],
    orderingWeight: 100 - index,
    errorMessage: null,
    remark: `Fixture row ${index + 1}`,
    executionMachine: machine,
  };
});

channels.push({
  id: 'gid://axonhub/Channel/97',
  createdAt: new Date(Date.UTC(2026, 0, 3, 0, 0)).toISOString(),
  updatedAt: new Date(Date.UTC(2026, 0, 3, 0, 0)).toISOString(),
  type: 'openai',
  baseURL: 'https://synthetic.example.invalid/unassigned/first-registration',
  name: 'Placement unassigned disabled',
  status: 'disabled',
  supportedModels: ['gpt-4o'],
  defaultTestModel: 'gpt-4o',
  tags: ['synthetic', 'unassigned', 'first-registration'],
  orderingWeight: 0,
  errorMessage: null,
  remark: 'Fixture first-registration row without a credential group',
  executionMachine: null,
});

const fixtureUser = {
  id: 'fixture-read-only-user',
  email: 'fixture@example.invalid',
  firstName: 'Channel',
  lastName: 'Fixture',
  isOwner: false,
  accessMode: 'legacy',
  preferLanguage: 'en',
  avatar: null,
  hasPassword: true,
  scopes: ['read_channels', 'read_system'],
  roles: [],
  projects: [],
  oidcIdentities: [],
};

type PlacementChange = {
  request_id: string;
  placement_version: number;
  channel_ids: number[];
  from: string;
  to: string;
  saved_at: string;
};

type PlacementState = {
  placement_version: number;
  owner: string;
  change?: PlacementChange;
  target?: { node: string; placement_version: number; ready: boolean };
  worker_ack?: { placement_version: number; worker_revision: string };
  observationReads: number;
};

const placementStates = new Map<number, PlacementState>();
const restLog: Array<Record<string, unknown>> = [];

function placementGroup(channelID: number) {
  const channel = channels.find((item) => Number(item.id.split('/').at(-1)) === channelID);
  if (channel?.executionMachine === null) {
    return { owner: '', channelIDs: [channelID] };
  }
  const owner = channel?.executionMachine ?? 'node-alpha';
  const peer = channels.find(
    (item) => item.executionMachine === owner && item.status === 'disabled' && Number(item.id.split('/').at(-1)) !== channelID
  );
  return {
    owner,
    channelIDs: [channelID, peer ? Number(peer.id.split('/').at(-1)) : channelID],
  };
}

function getPlacementState(channelID: number): PlacementState {
  const existing = placementStates.get(channelID);
  if (existing) return existing;
  const group = placementGroup(channelID);
  const state: PlacementState = {
    placement_version: 7,
    owner: group.owner,
    observationReads: 0,
  };
  placementStates.set(channelID, state);
  return state;
}

function isPrivilegedRequest(request: IncomingMessage) {
  const authorization = String(request.headers.authorization ?? '');
  return authorization.includes('channel-placement-fixture-admin-token') || authorization.includes('channel-placement-fixture-owner-token');
}

function writeJSON(response: ServerResponse, statusCode: number, body: unknown) {
  response.statusCode = statusCode;
  response.setHeader('Content-Type', 'application/json; charset=utf-8');
  response.end(JSON.stringify(body));
}

function readRequestBody(request: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    request.on('data', (chunk: Buffer | string) => chunks.push(Buffer.from(chunk)));
    request.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    request.on('error', reject);
  });
}

function getOperationName(query: string, operationName?: string) {
  if (operationName) return operationName;
  return query.match(/\b(?:query|mutation|subscription)\s+(\w+)/)?.[1];
}

function fold(value: unknown) {
  return typeof value === 'string' ? value.toLocaleLowerCase('en-US') : '';
}

function cursorOffset(cursor: unknown) {
  if (typeof cursor !== 'string') return 0;
  const parsed = Number(cursor.replace(/^cursor:/, ''));
  return Number.isInteger(parsed) && parsed >= 0 ? parsed + 1 : 0;
}

function channelConnection(input: Record<string, any> | undefined) {
  const safeInput = input ?? {};
  const where = (safeInput.where ?? {}) as Record<string, unknown>;
  const statusIn = Array.isArray(where.statusIn) ? where.statusIn : ['enabled', 'disabled'];
  const typeIn = Array.isArray(where.typeIn) ? where.typeIn : undefined;
  const nameContainsFold = fold(where.nameContainsFold);
  const executionMachine = typeof safeInput.executionMachine === 'string' ? safeInput.executionMachine : undefined;
  const requestedModel = typeof safeInput.model === 'string' ? safeInput.model : undefined;
  const requestedTag = typeof safeInput.hasTag === 'string' ? safeInput.hasTag : undefined;

  const filtered = channels.filter((channel) => {
    if (!statusIn.includes(channel.status)) return false;
    if (typeIn && !typeIn.includes(channel.type)) return false;
    if (nameContainsFold && !fold(channel.name).includes(nameContainsFold)) return false;
    if (where.errorMessageNotNil === true && !channel.errorMessage) return false;
    if (executionMachine && channel.executionMachine !== executionMachine) return false;
    if (requestedModel && !channel.supportedModels.includes(requestedModel)) return false;
    if (requestedTag && !channel.tags.includes(requestedTag)) return false;
    return true;
  });

  const orderBy = safeInput.orderBy as { field?: string; direction?: string } | undefined;
  const orderField = orderBy?.field === 'NAME' ? 'name' : orderBy?.field === 'STATUS' ? 'status' : 'createdAt';
  const orderDirection = orderBy?.direction === 'ASC' ? 1 : -1;
  const ordered = [...filtered].sort((left, right) => {
    const leftValue = String(left[orderField]);
    const rightValue = String(right[orderField]);
    return (leftValue.localeCompare(rightValue, 'en-US', { numeric: true }) || left.id.localeCompare(right.id)) * orderDirection;
  });

  const pageSize = Math.max(1, Number(safeInput.first ?? 20));
  const start = cursorOffset(safeInput.after);
  const page = ordered.slice(start, start + pageSize);
  const end = start + page.length - 1;

  return {
    edges: page.map((node, index) => ({ node, cursor: `cursor:${start + index}` })),
    pageInfo: {
      hasNextPage: end >= start && end < filtered.length - 1,
      hasPreviousPage: start > 0,
      startCursor: page.length > 0 ? `cursor:${start}` : null,
      endCursor: page.length > 0 ? `cursor:${end}` : null,
    },
    totalCount: filtered.length,
  };
}

function channelsByType(statusIn: unknown) {
  const statuses = Array.isArray(statusIn) && statusIn.length > 0 ? statusIn : ['enabled', 'disabled'];
  return CHANNEL_TYPES.map((type) => ({
    type,
    count: channels.filter((channel) => channel.type === type && statuses.includes(channel.status)).length,
  }));
}

function summarize(operationName: string | undefined, data: Record<string, any>, variables: Record<string, unknown> | undefined) {
  const input = (variables?.input ?? {}) as Record<string, any>;
  const connection = data.queryChannels as { edges?: Array<{ node?: SyntheticChannel }>; totalCount?: number } | undefined;
  return {
    operationName,
    totalCount: connection?.totalCount,
    rowNames: connection?.edges?.map((edge) => edge.node?.name).filter(Boolean).slice(0, 20),
    executionMachines: connection?.edges?.map((edge) => edge.node?.executionMachine ?? null).filter((value) => value !== undefined),
    input: {
      first: input.first,
      after: input.after,
      executionMachine: input.executionMachine,
      paginateModelMatches: input.paginateModelMatches,
      where: input.where,
      model: input.model,
      hasTag: input.hasTag,
    },
  };
}

export async function handleGraphQLRequest(request: IncomingMessage, response: ServerResponse) {
  if (request.method !== 'POST') {
    writeJSON(response, 405, { errors: [{ message: 'Synthetic GraphQL accepts POST only' }] });
    return;
  }

  let payload: { query?: string; operationName?: string; variables?: Record<string, unknown> };
  try {
    payload = JSON.parse(await readRequestBody(request));
  } catch {
    writeJSON(response, 400, { errors: [{ message: 'Invalid synthetic GraphQL JSON body' }] });
    return;
  }

  const operationName = getOperationName(payload.query ?? '', payload.operationName);
  const variables = payload.variables;
  let data: Record<string, any>;

  switch (operationName) {
    case 'ChannelExecutionInventory':
      data = { channelExecutionInventory: { clusterEnabled: true, machines: [...MACHINE_IDS] } };
      break;
    case 'QueryChannels':
      data = { queryChannels: channelConnection(variables?.input as Record<string, any> | undefined) };
      break;
    case 'CountChannelsByType':
      data = { countChannelsByType: channelsByType((variables?.input as Record<string, any> | undefined)?.statusIn) };
      break;
    case 'GetErrorChannelsCount':
      data = { channels: { totalCount: channels.filter((channel) => Boolean(channel.errorMessage)).length } };
      break;
    case 'AllChannelTags':
      data = { allChannelTags: ['synthetic', ...MACHINE_IDS, 'unassigned'] };
      break;
    case 'GetChannelProbeData':
      data = { channelProbeData: [] };
      break;
    case 'QueryChannelNames':
      data = {
        queryChannels: {
          edges: channels.map((node, index) => ({ node: { name: node.name }, cursor: `cursor:${index}` })),
          pageInfo: { hasNextPage: false, endCursor: null },
        },
      };
      break;
    case 'SystemChannelSettings':
      data = {
        systemChannelSettings: {
          probe: { enabled: false, frequency: 'FIVE_MINUTES' },
          autoSync: { frequency: 'ONE_DAY' },
          actionMenu: { advancedActionsMode: 'GROUPED' },
          routing: { strategy: 'round_robin', likelyAffinityTTLMinutes: null, exactAffinityTTLMinutes: null },
        },
      };
      break;
    case 'ProxyPresets':
      data = { proxyPresets: [] };
      break;
    case 'Models':
      data = { queryModels: MODEL_IDS.map((id) => ({ id, status: 'enabled' })) };
      break;
    case 'FetchModels':
      data = { fetchModels: { models: MODEL_IDS.map((id) => ({ id })), error: null } };
      break;
    case 'Me': {
      const privileged = isPrivilegedRequest(request);
      data = {
        me: {
          ...fixtureUser,
          id: privileged ? 'fixture-admin-user' : fixtureUser.id,
          isOwner: String(request.headers.authorization ?? '').includes('channel-placement-fixture-owner-token'),
          scopes: ['read_channels', 'read_system', ...(privileged ? ['write_channels'] : [])],
        },
      };
      break;
    }
    default:
      data = {};
      break;
  }

  requestLog.push({
    at: new Date().toISOString(),
    operationName,
    variables,
    resultSummary: summarize(operationName, data, variables),
  });

  writeJSON(response, 200, { data });
}

export async function handlePortalGraphQLRequest(request: IncomingMessage, response: ServerResponse) {
  if (request.method !== 'POST') {
    writeJSON(response, 405, { errors: [{ message: 'Synthetic portal GraphQL accepts POST only' }] });
    return;
  }

  await readRequestBody(request);
  writeJSON(response, 200, {
    data: {
      me: {
        id: fixtureUser.id,
        email: fixtureUser.email,
        firstName: fixtureUser.firstName,
        lastName: fixtureUser.lastName,
        preferLanguage: fixtureUser.preferLanguage,
        accessMode: fixtureUser.accessMode,
        avatar: null,
        hasPassword: true,
      },
    },
  });
}

export function getRequestLog() {
  return requestLog.map((entry) => ({ ...entry }));
}

export function clearRequestLog() {
  requestLog.length = 0;
  restLog.length = 0;
  placementStates.clear();
  channels.forEach((channel, index) => {
    channel.executionMachine = channel.id.endsWith('/97') ? null : BUCKETS[index % BUCKETS.length];
  });
}

function placementObservation(channelID: number, state: PlacementState) {
  if (state.change && !state.worker_ack) {
    if (state.observationReads > 0) {
      state.observationReads -= 1;
    } else {
      state.worker_ack = {
        placement_version: state.placement_version,
        worker_revision: 'synthetic-worker-revision-1',
      };
    }
  }

  return {
    placement_version: state.placement_version,
    owner: state.owner,
    ...(state.change ? { change: state.change } : {}),
    ...(state.target ? { target: state.target } : {}),
    ...(state.worker_ack ? { worker_ack: state.worker_ack } : {}),
    channel_id: channelID,
  };
}

function recordRest(request: IncomingMessage, path: string, body: unknown, response: unknown) {
  restLog.push({
    at: new Date().toISOString(),
    method: request.method,
    path,
    body,
    response,
    authorized: isPrivilegedRequest(request),
  });
}

function responseUnauthorized(request: IncomingMessage, response: ServerResponse, path: string) {
  const body = { message: 'Synthetic placement endpoint requires fixture authorization' };
  recordRest(request, path, undefined, body);
  writeJSON(response, 403, body);
}

export async function handlePlacementRequest(request: IncomingMessage, response: ServerResponse) {
  const parsedURL = new URL(request.url ?? '/', 'http://127.0.0.1');
  const path = parsedURL.pathname;
  if (path === '/admin/cluster/model-routes' && request.method === 'GET') {
    if (!isPrivilegedRequest(request)) {
      responseUnauthorized(request, response, path);
      return true;
    }
    const channelID = Number(parsedURL.searchParams.get('channel_id'));
    const status = parsedURL.searchParams.get('status') ?? '';
    const body = {
      model_routes: [
        { model: 'gpt-4o', machines: ['node-alpha', 'node-beta'], winner: 'node-alpha' },
        { model: 'deepseek-v4-flash', machines: ['node-alpha', 'node-beta'] },
      ],
    };
    recordRest(request, path, { channel_id: channelID, status }, body);
    writeJSON(response, 200, body);
    return true;
  }
  if (path === '/admin/cluster/model-routes/preview' && request.method === 'POST') {
    if (!isPrivilegedRequest(request)) {
      responseUnauthorized(request, response, path);
      return true;
    }
    const raw = await readRequestBody(request);
    const input = raw ? JSON.parse(raw) : {};
    const body = {
      model_routes: [
        { model: input.model || 'gpt-4o', machines: ['node-alpha', 'node-beta'], winner: 'node-alpha' },
      ],
    };
    recordRest(request, path, input, body);
    writeJSON(response, 200, body);
    return true;
  }
  const placementMatch = path.match(/^\/admin\/cluster\/placement\/(\d+)(\/preview)?$/);
  const isSave = path === '/admin/cluster/placement' && request.method === 'POST';
  if (!placementMatch && !isSave) return false;

  const isPreview = Boolean(placementMatch?.[2]);
  if (!isPrivilegedRequest(request) && (isPreview || isSave)) {
    responseUnauthorized(request, response, path);
    return true;
  }

  if (placementMatch && request.method === 'GET') {
    const channelID = Number(placementMatch[1]);
    const state = getPlacementState(channelID);
    if (placementMatch[2]) {
      const target = parsedURL.searchParams.get('target') ?? '';
      const group = placementGroup(channelID);
      const preview = {
        placement_version: state.placement_version,
        model_routes: [{ model: 'shared-model', machines: ['source', 'target'], winner: 'target' }],
        channel_id: channelID,
        from: state.owner,
        to: target,
        channels: group.channelIDs
          .map((id) => channels.find((item) => Number(item.id.split('/').at(-1)) === id))
          .filter((item): item is SyntheticChannel => Boolean(item))
          .map((item) => ({
            id: Number(item.id.split('/').at(-1)),
            name: item.name,
            status: item.status,
          })),
      };
      recordRest(request, path, { target }, preview);
      writeJSON(response, 200, preview);
      return true;
    }

    const observation = placementObservation(channelID, state);
    recordRest(request, path, undefined, observation);
    writeJSON(response, 200, observation);
    return true;
  }

  if (isSave) {
    let input: {
      channel_id?: number;
      target?: string;
      expected_version?: number;
      request_id?: string;
      channel_ids?: number[];
    };
    try {
      input = JSON.parse(await readRequestBody(request));
    } catch {
      const body = { message: 'Invalid synthetic placement JSON body' };
      recordRest(request, path, undefined, body);
      writeJSON(response, 400, body);
      return true;
    }

    const channelID = Number(input.channel_id);
    const state = getPlacementState(channelID);
    if (state.change?.request_id === input.request_id) {
      const observation = placementObservation(channelID, state);
      recordRest(request, path, input, observation);
      writeJSON(response, 200, observation);
      return true;
    }

    const group = placementGroup(channelID);
    const target = input.target ?? '';
    const expectedIDs = [...group.channelIDs].sort((left, right) => left - right);
    const receivedIDs = [...(input.channel_ids ?? [])].sort((left, right) => left - right);
    if (
      !MACHINE_IDS.includes(target as (typeof MACHINE_IDS)[number]) ||
      target === state.owner ||
      input.expected_version !== state.placement_version ||
      JSON.stringify(expectedIDs) !== JSON.stringify(receivedIDs)
    ) {
      const body = { message: 'Synthetic placement preview is stale or incomplete' };
      recordRest(request, path, input, body);
      writeJSON(response, 409, body);
      return true;
    }

    const savedAt = new Date().toISOString();
    state.placement_version += 1;
    state.owner = target;
    state.change = {
      request_id: input.request_id ?? '',
      placement_version: state.placement_version,
      channel_ids: group.channelIDs,
      from: group.owner,
      to: target,
      saved_at: savedAt,
    };
    state.target = { node: target, placement_version: state.placement_version, ready: true };
    state.worker_ack = undefined;
    state.observationReads = 1;
    for (const id of group.channelIDs) {
      placementStates.set(id, state);
      const channel = channels.find((item) => Number(item.id.split('/').at(-1)) === id);
      if (channel) channel.executionMachine = target;
    }

    const observation = placementObservation(channelID, state);
    recordRest(request, path, input, observation);
    writeJSON(response, 200, observation);
    return true;
  }

  const body = { message: 'Synthetic placement endpoint accepts GET or POST only' };
  recordRest(request, path, undefined, body);
  writeJSON(response, 405, body);
  return true;
}

export function getRestRequestLog() {
  return restLog.map((entry) => ({ ...entry }));
}
