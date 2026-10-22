// Tiny reactive store: state() returns a shallow Proxy, effect() re-runs when the keys it read change.
// Updates are batched in a microtask; the first run of an effect is synchronous.

interface Effect { fn: () => void; deps: Set<Set<Effect>>; stopped: boolean }

let active: Effect | null = null;
const queue = new Set<Effect>();
let scheduled = false;

function run(e: Effect) {
  if (e.stopped) return;
  e.deps.forEach(s => s.delete(e));
  e.deps.clear();
  const prev = active;
  active = e;
  try { e.fn(); } catch (err) { console.error('[AIWiki.effect]', err); } finally { active = prev; }
}
function flush() {
  scheduled = false;
  for (let guard = 0; queue.size && guard < 100; guard++) {
    const batch = [...queue]; queue.clear();
    batch.forEach(run);
  }
  queue.clear();
}
function schedule(e: Effect) {
  queue.add(e);
  if (!scheduled) { scheduled = true; queueMicrotask(flush); }
}

/** Run fn now and again (batched) whenever state read inside it changes. Returns a stop function. */
export function effect(fn: () => void): () => void {
  const e: Effect = { fn, deps: new Set(), stopped: false };
  run(e);
  return () => { e.stopped = true; e.deps.forEach(s => s.delete(e)); e.deps.clear(); queue.delete(e); };
}

export type State<T extends object> = T & { $subscribe(fn: (key: string, value: unknown) => void): () => void };

/** Shallow reactive object. Assign new values (also new arrays/objects) to trigger updates. */
export function state<T extends object>(initial: T): State<T> {
  const target = { ...initial } as Record<PropertyKey, unknown>;
  const subs = new Map<PropertyKey, Set<Effect>>();
  const anyKey = new Set<Effect>();
  const listeners = new Set<(key: string, value: unknown) => void>();
  const bucket = (k: PropertyKey) => { let s = subs.get(k); if (!s) { s = new Set(); subs.set(k, s); } return s; };
  const track = (set: Set<Effect>) => { if (active) { set.add(active); active.deps.add(set); } };
  return new Proxy(target, {
    get(t, k, r) {
      if (k === '$subscribe') return (fn: (key: string, value: unknown) => void) => { listeners.add(fn); return () => listeners.delete(fn); };
      if (typeof k === 'symbol') return Reflect.get(t, k, r);
      track(bucket(k));
      return t[k];
    },
    set(t, k, v) {
      const old = t[k];
      t[k] = v;
      if (typeof k !== 'symbol' && !Object.is(old, v)) {
        bucket(k).forEach(schedule);
        anyKey.forEach(schedule);
        listeners.forEach(l => l(k, v));
      }
      return true;
    },
    has: (t, k) => k in t,
    ownKeys(t) { track(anyKey); return Reflect.ownKeys(t); },
    getOwnPropertyDescriptor: (t, k) => Reflect.getOwnPropertyDescriptor(t, k),
  }) as State<T>;
}
