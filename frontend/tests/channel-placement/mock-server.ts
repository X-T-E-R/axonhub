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
    id: `synthetic-channel-${String(index + 1).padStart(3, '0')}`,
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
    case 'Me':
      data = { me: fixtureUser };
      break;
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
}
