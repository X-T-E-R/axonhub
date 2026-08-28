export interface ToolChoiceDisplay {
  kind: 'text' | 'json';
  value: string;
}

export function formatToolChoiceForDisplay(value: unknown): ToolChoiceDisplay | null {
  if (value === undefined || value === null) {
    return null;
  }
  if (typeof value === 'string') {
    return { kind: 'text', value };
  }

  try {
    return { kind: 'json', value: JSON.stringify(value, null, 2) };
  } catch {
    return { kind: 'text', value: String(value) };
  }
}
