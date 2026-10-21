/** Local drafts of the article editor. Every access is guarded: storage may be blocked or full. */
export type DraftFields = { title: string; summary: string; body: string; kind: string; status: string; pinned: boolean; space_id: string; project_id: string; parent_id: string; tags: string; properties: string };
export type StoredDraft = { fields: DraftFields; base_revision: number; savedAt: number };

const LEGACY_PREFIX = 'aiwiki.draft.';
const LEGACY_PARKED_PREFIX = 'aiwiki.draftparked.';
let identity: { corporate: boolean; userId: string } = { corporate: false, userId: '' };
export const setDraftIdentity = (userId: string, corporate: boolean) => { identity = { userId, corporate }; };
export const clearCorporateDrafts = (userId: string) => {
  try {
    const prefix = `aiwiki.corporate.${encodeURIComponent(userId)}.`;
    for (let index = sessionStorage.length - 1; index >= 0; index--) {
      const key = sessionStorage.key(index);
      if (key?.startsWith(prefix)) sessionStorage.removeItem(key);
    }
  } catch { /* Session storage can be unavailable. */ }
};
const scoped = (parked: boolean) => identity.corporate
  ? `aiwiki.corporate.${encodeURIComponent(identity.userId)}.${parked ? 'draftparked.' : 'draft.'}`
  : parked ? LEGACY_PARKED_PREFIX : LEGACY_PREFIX;
const storage = () => identity.corporate ? sessionStorage : localStorage;
const PARKED_LIMIT = 5;

/** `articleId` for an existing article, `new:<spaceId>` for a new one. */
export const draftKey = (articleId: string, spaceId: string) => articleId || `new:${spaceId}`;

const FIELD_KEYS: (keyof DraftFields)[] = ['title', 'summary', 'body', 'kind', 'status', 'pinned', 'space_id', 'project_id', 'parent_id', 'tags', 'properties'];

const validate = (value: unknown): StoredDraft | null => {
  const v = value as Partial<StoredDraft> | null;
  if (!v || typeof v !== 'object' || !v.fields || typeof v.fields !== 'object') return null;
  const fields: Record<string, unknown> = { ...(v.fields as unknown as Record<string, unknown>), project_id: (v.fields as Partial<DraftFields>).project_id || '' };
  for (const key of FIELD_KEYS) if (key === 'pinned' ? typeof fields[key] !== 'boolean' : typeof fields[key] !== 'string') return null;
  return { fields: fields as unknown as DraftFields, base_revision: Number(v.base_revision) || 0, savedAt: Number(v.savedAt) || 0 };
};
const readRaw = (prefix: string, key: string): unknown => { try { const raw = storage().getItem(prefix + key); return raw ? JSON.parse(raw) : null; } catch { return null; } };
const writeRaw = (prefix: string, key: string, value: unknown) => { try { storage().setItem(prefix + key, JSON.stringify(value)); return true; } catch { return false; } };
const remove = (prefix: string, key: string) => { try { storage().removeItem(prefix + key); } catch { /* nothing to remove */ } };

export const loadDraft = (key: string) => validate(readRaw(scoped(false), key));
export const saveDraft = (key: string, fields: DraftFields, baseRevision: number) => writeRaw(scoped(false), key, { fields, base_revision: baseRevision, savedAt: Date.now() });
export const removeDraft = (key: string) => remove(scoped(false), key);

export const sameFields = (a: DraftFields, b: DraftFields) => FIELD_KEYS.every(key => a[key] === b[key]);

/** «Parked» drafts: the user's own text set aside (newest first, a few kept) so that no version is ever overwritten. */
export const loadParkedList = (key: string): StoredDraft[] => {
  const raw = readRaw(scoped(true), key);
  return (Array.isArray(raw) ? raw : raw ? [raw] : []).map(validate).filter((d): d is StoredDraft => d !== null);
};
export const pushParked = (key: string, draft: StoredDraft) => {
  const list = loadParkedList(key);
  const next = list.length && sameFields(list[0].fields, draft.fields) ? list : [draft, ...list];
  return writeRaw(scoped(true), key, next.slice(0, PARKED_LIMIT));
};
/** Removes the newest parked draft and returns what is left. */
export const popParked = (key: string): StoredDraft[] => {
  const rest = loadParkedList(key).slice(1);
  if (rest.length) writeRaw(scoped(true), key, rest); else remove(scoped(true), key);
  return rest;
};

export const formatDraftTime = (ms: number) => ms ? new Intl.DateTimeFormat('ru-RU', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(ms)) : 'неизвестно когда';
