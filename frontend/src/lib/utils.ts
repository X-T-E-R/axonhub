import { type ClassValue, clsx } from 'clsx';
import { twMerge } from 'tailwind-merge';

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export type EntityIDValue = string | { type: string; id: number } | null | undefined;

export const extractNumberID = (id: string) => {
  const lastSlashIndex = id.lastIndexOf('/');
  return id.slice(lastSlashIndex + 1);
};

export const extractNumberIDAsNumber = (id: string) => {
  return Number(extractNumberID(id));
};

export const buildGUID = (type: string, id: string) => {
  return `gid://axonhub/${type}/${id}`;
};

export const normalizeEntityID = (id: EntityIDValue): string => {
  if (typeof id === 'string') {
    return id;
  }

  if (id && typeof id === 'object' && 'type' in id && 'id' in id) {
    return buildGUID(id.type, String(id.id));
  }

  return '';
};
