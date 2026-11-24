import { useSyncExternalStore } from 'react';

const MINUTE_MS = 60_000;

let currentNow = Date.now();
let intervalId: ReturnType<typeof setInterval> | null = null;
const listeners = new Set<() => void>();

function notify() {
  currentNow = Date.now();
  for (const listener of listeners) {
    listener();
  }
}

function handleVisibilityChange() {
  if (typeof document !== 'undefined' && document.visibilityState === 'visible') {
    notify();
  }
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  if (listeners.size === 1) {
    currentNow = Date.now();
    intervalId = setInterval(notify, MINUTE_MS);
    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', handleVisibilityChange);
    }
  }
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) {
      if (intervalId !== null) {
        clearInterval(intervalId);
        intervalId = null;
      }
      if (typeof document !== 'undefined') {
        document.removeEventListener('visibilitychange', handleVisibilityChange);
      }
    }
  };
}

function getSnapshot() {
  return currentNow;
}

function subscribeNoop() {
  return () => {};
}

function getStaticNow() {
  return currentNow;
}

/**
 * Page-level shared minute ticker. Multiple visible rows subscribe to one
 * single setInterval rather than creating one timer per row/key.
 */
export function useSharedMinuteNow(enabled = true): number {
  return useSyncExternalStore(enabled ? subscribe : subscribeNoop, enabled ? getSnapshot : getStaticNow, getStaticNow);
}
