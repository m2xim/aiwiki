/** Local drafts of the article editor. Every access is guarded: storage may be blocked or full. */
export type DraftFields = { title: string; summary: string; body: string; kind: string; status: string; pinned: boolean; space_id: string; parent_id: string; tags: string; properties: string };
export type StoredDraft = { fields: DraftFields; base_revision: number; savedAt: number };

const PREFIX = 'aiwiki.draft.';
const PARKED_PREFIX = 'aiwiki.draftparked.';
const PARKED_LIMIT = 5;

/** `articleId` for an existing article, `new:<spaceId>` for a new one. */
export const draftKey = (articleId: string, spaceId: string) => articleId || `new:${spaceId}`;

const FIELD_KEYS: (keyof DraftFields)[] = ['title', 'summary', 'body', 'kind', 'status', 'pinned', 'space_id', 'parent_id', 'tags', 'properties'];

const validate = (value: unknown): StoredDraft | null => {
  const v = value as Partial<StoredDraft> | null;
  if (!v || typeof v !== 'object' || !v.fields || typeof v.fields !== 'object') return null;
  const fields = v.fields as unknown as Record<string, unknown>;
  for (const key of FIELD_KEYS) if (key === 'pinned' ? typeof fields[key] !== 'boolean' : typeof fields[key] !== 'string') return null;
  return { fields: fields as unknown as DraftFields, base_revision: Number(v.base_revision) || 0, savedAt: Number(v.savedAt) || 0 };
};
const readRaw = (prefix: string, key: string): unknown => { try { const raw = localStorage.getItem(prefix + key); return raw ? JSON.parse(raw) : null; } catch { return null; } };
const writeRaw = (prefix: string, key: string, value: unknown) => { try { localStorage.setItem(prefix + key, JSON.stringify(value)); return true; } catch { return false; } };
const remove = (prefix: string, key: string) => { try { localStorage.removeItem(prefix + key); } catch { /* nothing to remove */ } };

export const loadDraft = (key: string) => validate(readRaw(PREFIX, key));
export const saveDraft = (key: string, fields: DraftFields, baseRevision: number) => writeRaw(PREFIX, key, { fields, base_revision: baseRevision, savedAt: Date.now() });
export const removeDraft = (key: string) => remove(PREFIX, key);

export const sameFields = (a: DraftFields, b: DraftFields) => FIELD_KEYS.every(key => a[key] === b[key]);

/** «Parked» drafts: the user's own text set aside (newest first, a few kept) so that no version is ever overwritten. */
export const loadParkedList = (key: string): StoredDraft[] => {
  const raw = readRaw(PARKED_PREFIX, key);
  return (Array.isArray(raw) ? raw : raw ? [raw] : []).map(validate).filter((d): d is StoredDraft => d !== null);
};
export const pushParked = (key: string, draft: StoredDraft) => {
  const list = loadParkedList(key);
  const next = list.length && sameFields(list[0].fields, draft.fields) ? list : [draft, ...list];
  return writeRaw(PARKED_PREFIX, key, next.slice(0, PARKED_LIMIT));
};
/** Removes the newest parked draft and returns what is left. */
export const popParked = (key: string): StoredDraft[] => {
  const rest = loadParkedList(key).slice(1);
  if (rest.length) writeRaw(PARKED_PREFIX, key, rest); else remove(PARKED_PREFIX, key);
  return rest;
};

export const formatDraftTime = (ms: number) => ms ? new Intl.DateTimeFormat('ru-RU', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(ms)) : 'неизвестно когда';
