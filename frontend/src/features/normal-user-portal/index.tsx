import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Check, Copy, Eye, EyeOff, RefreshCw } from 'lucide-react';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Badge } from '@/components/ui/badge';
import { selfServiceApi, type SelfAPIKey, type SelfUsage } from '@/lib/api-client';
import { normalizeEntityID, extractNumberID } from '@/lib/utils';
import { useAuthStore } from '@/stores/authStore';
import { useProjectStore, useSelectedProjectId } from '@/stores/projectStore';

type RevealedSecret = {
  keyId: number;
  keyName: string;
  value: string;
};

const formatNumber = (value?: number | null) => {
  if (value === null || value === undefined) {
    return '—';
  }

  return value.toLocaleString();
};

const formatProjectLabel = (projectID: string, index: number) => {
  const numericId = extractNumberID(projectID);
  return numericId ? `Project #${numericId}` : `Project ${index + 1}`;
};

const formatUsageCost = (usage?: SelfUsage) => {
  if (!usage || usage.totalCost <= 0) {
    return '—';
  }

  return usage.totalCost.toFixed(6);
};

export default function NormalUserPortal() {
  const queryClient = useQueryClient();
  const { user } = useAuthStore((state) => state.auth);
  const selectedProjectId = useSelectedProjectId();
  const setSelectedProjectId = useProjectStore((state) => state.setSelectedProjectId);
  const [keyName, setKeyName] = useState('');
  const [presetID, setPresetID] = useState('');
  const [revealedSecret, setRevealedSecret] = useState<RevealedSecret | null>(null);
  const [showSecret, setShowSecret] = useState(false);
  const [copiedTarget, setCopiedTarget] = useState<'base-url' | 'api-key' | null>(null);

  const userProjects = user?.projects ?? [];
  const projectOptions = useMemo(
    () =>
      userProjects
        .map((project, index) => {
          const projectID = normalizeEntityID(project.projectID);
          return {
            id: projectID,
            label: formatProjectLabel(projectID, index),
            scopes: project.scopes,
            isOwner: project.isOwner,
          };
        })
        .filter((project) => project.id),
    [userProjects],
  );

  const firstProjectID = projectOptions[0]?.id ?? '';
  const selectedProjectBelongsToUser = useMemo(
    () => Boolean(selectedProjectId && projectOptions.some((project) => project.id === selectedProjectId)),
    [projectOptions, selectedProjectId],
  );
  const projectID = selectedProjectBelongsToUser && selectedProjectId ? selectedProjectId : firstProjectID;
  const baseURL = useMemo(() => (typeof window === 'undefined' ? '/v1' : `${window.location.origin}/v1`), []);

  useEffect(() => {
    if (projectID && projectID !== selectedProjectId) {
      setSelectedProjectId(projectID);
    }
  }, [projectID, selectedProjectId, setSelectedProjectId]);

  useEffect(() => {
    setPresetID('');
    setRevealedSecret(null);
    setShowSecret(false);
  }, [projectID]);

  const enabled = Boolean(projectID);
  const presets = useQuery({
    queryKey: ['self', 'routing-presets', projectID],
    queryFn: () => selfServiceApi.routingPresets(projectID),
    enabled,
  });
  const keys = useQuery({
    queryKey: ['self', 'api-keys', projectID],
    queryFn: () => selfServiceApi.apiKeys(projectID),
    enabled,
  });
  const models = useQuery({
    queryKey: ['self', 'models', projectID, presetID],
    queryFn: () => selfServiceApi.models(projectID, presetID ? Number(presetID) : undefined),
    enabled,
  });
  const requests = useQuery({
    queryKey: ['self', 'requests', projectID],
    queryFn: () => selfServiceApi.requests(projectID),
    enabled,
  });
  const usage = useQuery({
    queryKey: ['self', 'usage', projectID],
    queryFn: () => selfServiceApi.usage(projectID),
    enabled,
  });

  useEffect(() => {
    if (!presets.data?.length) {
      setPresetID('');
      return;
    }

    if (presetID && presets.data.some((preset) => String(preset.id) === presetID)) {
      return;
    }

    if (presets.data.length === 1) {
      setPresetID(String(presets.data[0].id));
      return;
    }

    setPresetID('');
  }, [presetID, presets.data]);

  const selectedPreset = presets.data?.find((preset) => String(preset.id) === presetID);
  const requestDetailsVisible = requests.data?.some((request) => request.detailsVisible) ?? false;
  const createDisabledReason =
    !enabled
      ? 'Select a project before creating a key.'
      : presets.isLoading
        ? 'Loading available routing presets...'
        : presets.isError
          ? 'Could not load routing presets for this project.'
          : !presets.data?.length
            ? 'No self-service routing presets are available in this project yet.'
            : !keyName.trim()
              ? 'Enter a memorable key name first.'
              : !presetID
                ? 'Choose a routing preset first.'
                : undefined;

  const copyText = async (value: string, target: 'base-url' | 'api-key') => {
    try {
      await navigator.clipboard.writeText(value);
      setCopiedTarget(target);
      toast.success('Copied to clipboard!');
      window.setTimeout(() => {
        setCopiedTarget((current) => (current === target ? null : current));
      }, 1500);
    } catch {
      toast.error('Failed to copy to clipboard.');
    }
  };

  const createKey = useMutation({
    mutationFn: () =>
      selfServiceApi.createAPIKey({
        projectId: projectID,
        name: keyName.trim(),
        presetId: presetID,
      }),
    onSuccess: async (created) => {
      await queryClient.invalidateQueries({ queryKey: ['self', 'api-keys', projectID] });
      setKeyName('');

      if (created.key) {
        setRevealedSecret({
          keyId: created.id,
          keyName: created.name,
          value: created.key,
        });
        setShowSecret(false);
        toast.success('API key created. Copy it now because the secret is only revealed once.');
        return;
      }

      toast.success('API key created.');
    },
    onError: (error: any) => toast.error(error.message || 'Failed to create API key'),
  });

  const rotateKey = useMutation({
    mutationFn: async (key: SelfAPIKey) => ({ key, rotated: await selfServiceApi.rotateAPIKey(key.id) }),
    onSuccess: async ({ key, rotated }) => {
      await queryClient.invalidateQueries({ queryKey: ['self', 'api-keys', projectID] });

      if (rotated.key) {
        setRevealedSecret({
          keyId: rotated.id,
          keyName: rotated.name,
          value: rotated.key,
        });
        setShowSecret(false);
      }

      toast.success(`Rotated ${key.name}.`);
    },
    onError: (error: any) => toast.error(error.message || 'Failed to rotate API key'),
  });

  const updateStatus = useMutation({
    mutationFn: async ({ id, status }: { id: number; status: string }) => selfServiceApi.updateAPIKeyStatus(id, status),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['self', 'api-keys', projectID] });
      toast.success('API key status updated.');
    },
    onError: (error: any) => toast.error(error.message || 'Failed to update API key status'),
  });

  if (!enabled) {
    return (
      <div className='mx-auto max-w-3xl p-6'>
        <Card>
          <CardHeader>
            <CardTitle>No project access yet</CardTitle>
            <CardDescription>Ask an administrator to add your account to a project before creating API keys.</CardDescription>
          </CardHeader>
        </Card>
      </div>
    );
  }

  return (
    <div className='space-y-6 p-6'>
      <div className='space-y-2'>
        <h1 className='text-2xl font-semibold tracking-tight'>Start using AxonHub</h1>
        <p className='text-muted-foreground max-w-3xl text-sm'>
          Browse approved models, create your own user key from an allowed routing preset, and review only your own request activity.
        </p>
      </div>

      <div className='grid gap-4 md:grid-cols-4'>
        <Card>
          <CardHeader>
            <CardTitle>{models.isLoading ? '—' : formatNumber(models.data?.length)}</CardTitle>
            <CardDescription>Accessible models</CardDescription>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>{keys.isLoading ? '—' : formatNumber(keys.data?.length)}</CardTitle>
            <CardDescription>My API keys</CardDescription>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>{usage.isLoading ? '—' : formatNumber(usage.data?.requests)}</CardTitle>
            <CardDescription>Requests attributed to my keys</CardDescription>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>{usage.isLoading ? '—' : formatNumber(usage.data?.totalTokens)}</CardTitle>
            <CardDescription>Tokens used</CardDescription>
          </CardHeader>
        </Card>
      </div>

      <div className='grid gap-4 xl:grid-cols-[1.1fr_0.9fr]'>
        <Card>
          <CardHeader>
            <CardTitle>Create my API key</CardTitle>
            <CardDescription>Keys created here belong to your account and can only use the approved preset you choose.</CardDescription>
          </CardHeader>
          <CardContent className='space-y-4'>
            {projectOptions.length > 1 && (
              <div className='space-y-2'>
                <p className='text-sm font-medium'>Project</p>
                <select
                  className='border-input bg-background h-9 w-full rounded-md border px-3 text-sm'
                  value={projectID}
                  onChange={(event) => setSelectedProjectId(event.target.value)}
                >
                  {projectOptions.map((project) => (
                    <option key={project.id} value={project.id}>
                      {project.label}
                    </option>
                  ))}
                </select>
              </div>
            )}

            <div className='grid gap-3 md:grid-cols-[1.1fr_0.9fr]'>
              <Input value={keyName} onChange={(event) => setKeyName(event.target.value)} placeholder='Key name, e.g. local app' />
              <select
                className='border-input bg-background h-9 rounded-md border px-3 text-sm'
                value={presetID}
                onChange={(event) => setPresetID(event.target.value)}
              >
                <option value=''>Select a routing preset</option>
                {presets.data?.map((preset) => (
                  <option key={preset.id} value={preset.id}>
                    {preset.name}
                  </option>
                ))}
              </select>
            </div>

            {selectedPreset && (
              <div className='rounded-md border border-dashed p-3 text-sm'>
                <div className='font-medium'>{selectedPreset.name}</div>
                <p className='text-muted-foreground mt-1 text-xs'>
                  {selectedPreset.description || 'This preset controls the models and routing rules your key can use.'}
                </p>
              </div>
            )}

            <div className='flex flex-wrap items-center gap-2'>
              <Button disabled={Boolean(createDisabledReason) || createKey.isPending} onClick={() => createKey.mutate()}>
                {createKey.isPending ? 'Creating key...' : 'Create key'}
              </Button>
              {createDisabledReason && <p className='text-muted-foreground text-xs'>{createDisabledReason}</p>}
            </div>

            <div className='flex flex-wrap items-center justify-between gap-2 rounded-md border p-3 text-xs'>
              <div>
                <div className='text-muted-foreground'>Base URL</div>
                <code className='text-sm'>{baseURL}</code>
              </div>
              <Button variant='outline' size='sm' onClick={() => copyText(baseURL, 'base-url')}>
                {copiedTarget === 'base-url' ? <Check className='h-4 w-4' /> : <Copy className='h-4 w-4' />}
                Copy
              </Button>
            </div>

            {revealedSecret && (
              <div className='space-y-3 rounded-md border border-dashed p-3'>
                <div className='flex flex-wrap items-center justify-between gap-2'>
                  <div>
                    <p className='font-medium'>{revealedSecret.keyName}</p>
                    <p className='text-muted-foreground text-xs'>This secret is shown only after create or rotate. Copy it before leaving this page.</p>
                  </div>
                  <div className='flex gap-2'>
                    <Button variant='outline' size='sm' onClick={() => setShowSecret((current) => !current)}>
                      {showSecret ? <EyeOff className='h-4 w-4' /> : <Eye className='h-4 w-4' />}
                      {showSecret ? 'Hide' : 'Reveal'}
                    </Button>
                    <Button variant='outline' size='sm' onClick={() => copyText(revealedSecret.value, 'api-key')}>
                      {copiedTarget === 'api-key' ? <Check className='h-4 w-4' /> : <Copy className='h-4 w-4' />}
                      Copy key
                    </Button>
                  </div>
                </div>
                <code className='block rounded bg-muted px-3 py-2 text-xs break-all'>
                  {showSecret ? revealedSecret.value : '•'.repeat(Math.max(revealedSecret.value.length, 16))}
                </code>
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>My API keys</CardTitle>
            <CardDescription>Only keys owned by your account in the selected project are listed here.</CardDescription>
          </CardHeader>
          <CardContent className='space-y-3'>
            {keys.isLoading && <p className='text-muted-foreground text-sm'>Loading your keys...</p>}
            {keys.isError && <p className='text-sm text-destructive'>Could not load your API keys.</p>}
            {!keys.isLoading && !keys.isError && !keys.data?.length && <p className='text-muted-foreground text-sm'>No keys yet. Create one from a routing preset.</p>}
            {keys.data?.map((key) => {
              const isStatusUpdating = updateStatus.isPending && updateStatus.variables?.id === key.id;
              const isRotating = rotateKey.isPending && rotateKey.variables?.id === key.id;
              const nextStatus = key.status === 'enabled' ? 'disabled' : 'enabled';

              return (
                <div key={key.id} className='space-y-3 rounded-md border p-3 text-sm'>
                  <div className='flex flex-wrap items-start justify-between gap-2'>
                    <div>
                      <div className='font-medium'>{key.name}</div>
                      <div className='text-muted-foreground text-xs'>
                        {key.activeProfile || 'No active profile'} · updated {new Date(key.updatedAt).toLocaleString()}
                      </div>
                    </div>
                    <Badge variant={key.status === 'enabled' ? 'default' : 'secondary'}>{key.status}</Badge>
                  </div>

                  <div className='flex flex-wrap gap-2'>
                    <Button variant='outline' size='sm' disabled={isRotating} onClick={() => rotateKey.mutate(key)}>
                      <RefreshCw className='h-4 w-4' />
                      Rotate
                    </Button>
                    <Button
                      variant='outline'
                      size='sm'
                      disabled={isStatusUpdating}
                      onClick={() => updateStatus.mutate({ id: key.id, status: nextStatus })}
                    >
                      {key.status === 'enabled' ? 'Disable' : 'Enable'}
                    </Button>
                    <Button
                      variant='outline'
                      size='sm'
                      disabled={isStatusUpdating}
                      onClick={() => updateStatus.mutate({ id: key.id, status: 'archived' })}
                    >
                      Archive
                    </Button>
                  </div>

                  {revealedSecret?.keyId === key.id && (
                    <div className='rounded-md border border-dashed p-3 text-xs text-muted-foreground'>
                      The rotated or newly created secret for this key is ready above. Copy it before closing the page.
                    </div>
                  )}
                </div>
              );
            })}
          </CardContent>
        </Card>
      </div>

      <div className='grid gap-4 xl:grid-cols-[1fr_0.95fr]'>
        <Card>
          <CardHeader>
            <CardTitle>Model marketplace</CardTitle>
            <CardDescription>
              {presetID
                ? `Showing models available through ${selectedPreset?.name || 'the selected preset'}.`
                : 'Showing all models allowed by your current project and visible self-service presets.'}
            </CardDescription>
          </CardHeader>
          <CardContent className='space-y-3'>
            {models.isLoading && <p className='text-muted-foreground text-sm'>Loading accessible models...</p>}
            {models.isError && <p className='text-sm text-destructive'>Could not load models for the selected project.</p>}
            {!models.isLoading && !models.isError && !models.data?.length && (
              <p className='text-muted-foreground text-sm'>
                {presetID ? 'No models are available for the selected routing preset.' : 'No accessible models are available in this project yet.'}
              </p>
            )}
            <div className='grid gap-2 sm:grid-cols-2'>
              {models.data?.slice(0, 12).map((model) => (
                <div key={model.id} className='space-y-2 rounded-md border p-3 text-sm'>
                  <div className='font-medium'>{model.name}</div>
                  <div className='flex flex-wrap gap-1'>
                    {(model.groups?.length ? model.groups : ['model gateway']).map((group) => (
                      <Badge key={`${model.id}-${group}`} variant='outline'>
                        {group}
                      </Badge>
                    ))}
                  </div>
                  {model.developers?.length ? <p className='text-muted-foreground text-xs'>{model.developers.join(', ')}</p> : null}
                </div>
              ))}
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>My recent requests and usage</CardTitle>
            <CardDescription>
              {requestDetailsVisible
                ? 'Request details are enabled for this project.'
                : 'Prompt, response, provider, and channel internals stay hidden unless an administrator enables them.'}
            </CardDescription>
          </CardHeader>
          <CardContent className='space-y-4'>
            <div className='grid gap-3 sm:grid-cols-3'>
              <div className='rounded-md border p-3'>
                <div className='text-muted-foreground text-xs'>Requests</div>
                <div className='text-lg font-semibold'>{requests.isLoading ? '—' : formatNumber(usage.data?.requests)}</div>
              </div>
              <div className='rounded-md border p-3'>
                <div className='text-muted-foreground text-xs'>Prompt tokens</div>
                <div className='text-lg font-semibold'>{usage.isLoading ? '—' : formatNumber(usage.data?.promptTokens)}</div>
              </div>
              <div className='rounded-md border p-3'>
                <div className='text-muted-foreground text-xs'>Operational cost</div>
                <div className='text-lg font-semibold'>{usage.isLoading ? '—' : formatUsageCost(usage.data)}</div>
              </div>
            </div>

            {requests.isLoading && <p className='text-muted-foreground text-sm'>Loading your recent requests...</p>}
            {requests.isError && <p className='text-sm text-destructive'>Could not load your recent requests.</p>}
            {!requests.isLoading && !requests.isError && !requests.data?.length && <p className='text-muted-foreground text-sm'>No requests from your keys yet.</p>}

            <div className='space-y-2'>
              {requests.data?.slice(0, 8).map((request) => (
                <div key={request.id} className='rounded-md border p-3 text-sm'>
                  <div className='flex flex-wrap items-center justify-between gap-2'>
                    <span className='font-medium'>{request.modelId}</span>
                    <div className='flex gap-2'>
                      <Badge variant='secondary'>{request.status}</Badge>
                      <Badge variant='outline'>{request.detailsVisible ? 'details enabled' : 'metadata only'}</Badge>
                    </div>
                  </div>
                  <div className='text-muted-foreground mt-1 text-xs'>
                    {new Date(request.createdAt).toLocaleString()} · {request.source} · {request.stream ? 'streaming' : 'non-streaming'}
                    {request.latencyMs ? ` · ${request.latencyMs} ms` : ''}
                  </div>
                </div>
              ))}
            </div>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
