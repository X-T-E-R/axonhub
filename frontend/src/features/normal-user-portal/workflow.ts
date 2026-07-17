import type { SelfAPIKey, SelfEvidenceAvailability, SelfModel } from '@/lib/api-client';

export type SelfServiceHandoff = {
  accessGroupId?: number;
  modelId?: string;
};

const positiveInteger = (value: unknown) => {
  const parsed = typeof value === 'number' ? value : typeof value === 'string' && value.trim() ? Number(value) : Number.NaN;
  return Number.isInteger(parsed) && parsed > 0 ? parsed : undefined;
};

export function validateSelfServiceHandoff(search: Record<string, unknown>): SelfServiceHandoff {
  const accessGroupId = positiveInteger(search.accessGroupId);
  const modelId = typeof search.modelId === 'string' ? search.modelId.trim() : '';
  return {
    ...(accessGroupId ? { accessGroupId } : {}),
    ...(modelId ? { modelId } : {}),
  };
}

export function modelSupportsAccessGroup(model: SelfModel, accessGroupId: number) {
  return (
    model.accessGroupId === accessGroupId ||
    model.presetId === accessGroupId ||
    model.profileId === accessGroupId ||
    model.accessGroups?.some((group) => group.id === accessGroupId || group.profileId === accessGroupId) === true
  );
}

export function selectCompatibleModel(models: SelfModel[], handoff: SelfServiceHandoff): SelfModel | undefined {
  const compatible = handoff.accessGroupId ? models.filter((model) => modelSupportsAccessGroup(model, handoff.accessGroupId!)) : models;
  if (handoff.modelId) {
    return compatible.find((model) => model.id === handoff.modelId || model.name === handoff.modelId);
  }
  return compatible[0];
}

export type SelfKeyClassification = 'legacy_unknown' | 'self_service_access_group' | 'self_service_snapshot' | 'other';

export function classifySelfAPIKey(key: SelfAPIKey): SelfKeyClassification {
  if (key.provisioningSource === 'legacy_unknown') return 'legacy_unknown';
  if (key.provisioningSource === 'self_service' && key.profileMode === 'access_group') return 'self_service_access_group';
  if (key.provisioningSource === 'self_service' && key.profileMode === 'snapshot') return 'self_service_snapshot';
  return 'other';
}

export function keySupportsAccessGroup(key: SelfAPIKey, accessGroupId?: number) {
  if (!accessGroupId) return true;
  return key.profileMode === 'access_group' && key.accessGroupId === accessGroupId;
}

export type RevealedSecret = { keyId: number; keyName: string; value: string; accessGroupId?: number };
export type SecretLifecycleAction =
  | { type: 'revealed'; secret: RevealedSecret }
  | { type: 'key-transition'; keyId: number }
  | { type: 'clear' };

export function reduceRevealedSecret(current: RevealedSecret | null, action: SecretLifecycleAction): RevealedSecret | null {
  if (action.type === 'revealed') return action.secret;
  if (action.type === 'key-transition') return current?.keyId === action.keyId ? null : current;
  return null;
}

export function secretSupportsAccessGroup(secret: RevealedSecret | null, accessGroupId?: number) {
  return Boolean(secret && accessGroupId && secret.accessGroupId === accessGroupId);
}

export function isPartialEvidence(availability: SelfEvidenceAvailability | undefined) {
  if (!availability) return false;
  return Object.values(availability).some((evidence) => evidence.state !== 'available' && evidence.state !== 'notApplicable');
}
