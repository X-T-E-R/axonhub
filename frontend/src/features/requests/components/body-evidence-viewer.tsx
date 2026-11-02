import type { ComponentProps } from 'react';
import { JsonViewer } from '@/components/json-tree-view';
import { isNativeDecisionFormat } from '@/features/channels/data/native-decisions';
import { bodyEvidence, type BodyEvidence, type BodyKind } from '../utils/body-evidence';

// Opt-in raw rendering: never parse native text through JSON.parse/Number.
// The tree viewer remains unchanged for all existing JSON scalar consumers.
export function BodyEvidenceViewer({
  evidence,
  kind,
  ...props
}: Omit<ComponentProps<typeof JsonViewer>, 'data'> & { evidence: BodyEvidence; kind: BodyKind }) {
  if (isNativeDecisionFormat(evidence.format)) {
    return (
      <pre className={`font-mono break-words whitespace-pre-wrap ${props.className ?? ''}`}>{bodyEvidence(evidence, kind) as string}</pre>
    );
  }
  return <JsonViewer {...props} data={bodyEvidence(evidence, kind)} />;
}
