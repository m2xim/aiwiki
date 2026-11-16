import { useCallback, useState } from 'react';

// Per-browser UI preferences (filter and sort choices). Stored values are validated
// against the allowed list on read, so a stale or edited entry falls back to the default.
const prefix = 'aiwiki.pref.';

export function readPref<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
  try {
    const value = localStorage.getItem(prefix + key);
    return value !== null && (allowed as readonly string[]).includes(value) ? value as T : fallback;
  } catch { return fallback; }
}

export function writePref(key: string, value: string, fallback: string) {
  try {
    if (value === fallback) localStorage.removeItem(prefix + key);
    else localStorage.setItem(prefix + key, value);
  } catch { /* the choice lasts for this page only */ }
}

/** useState for a dropdown whose choice is remembered in localStorage. */
export function usePref<T extends string>(key: string, allowed: readonly T[], fallback: T): [T, (value: string) => void] {
  const [value, setValue] = useState<T>(() => readPref(key, allowed, fallback));
  const set = useCallback((next: string) => {
    if (!(allowed as readonly string[]).includes(next)) return;
    setValue(next as T); writePref(key, next, fallback);
  }, [key, fallback]);
  return [value, set];
}
