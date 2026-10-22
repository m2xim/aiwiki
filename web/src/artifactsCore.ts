// Pure logic of the artifacts UI (contract: docs/ARTIFACTS.md). No DOM and no React: Node imports this file directly (scripts/qa-artifacts.mjs).

export const LIMITS = { maxFiles: 50, maxFileBytes: 5 * 1024 * 1024, maxRevisionBytes: 20 * 1024 * 1024, maxPathBytes: 200, maxDepth: 4 } as const;
export const PATH_RE = /^[A-Za-z0-9][A-Za-z0-9._-]*(\/[A-Za-z0-9][A-Za-z0-9._-]*)*$/;
export const TEXT_EXTENSIONS = ['html', 'css', 'js', 'mjs', 'json', 'csv', 'txt', 'md', 'svg'] as const;
export const BINARY_EXTENSIONS = ['png', 'jpg', 'jpeg', 'gif', 'webp'] as const;
export const ALLOWED_EXTENSIONS: readonly string[] = [...TEXT_EXTENSIONS, ...BINARY_EXTENSIONS];
export const ENTRY_PATH = 'index.html';

export type Visibility = 'inherit' | 'private';
export type ManifestFile = { path: string; size: number; sha256: string; mime: string };
/** content === null: the file is unchanged and was not loaded into the editor. original: the text as it came from the server. */
export type DraftFile = { path: string; encoding: 'utf8' | 'base64'; content: string | null; original: string | null; size: number };
export type FileChange = { path: string; content: string; encoding: 'utf8' | 'base64' } | { path: string; delete: true };

export const textBytes = (text: string): number => {
  let bytes = 0;
  for (let i = 0; i < text.length; i++) {
    const code = text.charCodeAt(i);
    if (code < 0x80) bytes += 1;
    else if (code < 0x800) bytes += 2;
    else if (code >= 0xd800 && code <= 0xdbff && i + 1 < text.length && (text.charCodeAt(i + 1) & 0xfc00) === 0xdc00) { bytes += 4; i++; }
    else bytes += 3;
  }
  return bytes;
};
export const base64Bytes = (value: string): number => {
  const clean = value.replace(/\s+/g, '');
  if (!clean) return 0;
  const padding = clean.endsWith('==') ? 2 : clean.endsWith('=') ? 1 : 0;
  return Math.floor(clean.length * 3 / 4) - padding;
};
export const contentBytes = (content: string, encoding: 'utf8' | 'base64'): number => encoding === 'base64' ? base64Bytes(content) : textBytes(content);

export const extensionOf = (path: string): string => {
  const name = path.slice(path.lastIndexOf('/') + 1);
  const dot = name.lastIndexOf('.');
  return dot > 0 ? name.slice(dot + 1).toLowerCase() : '';
};
export const isTextPath = (path: string): boolean => (TEXT_EXTENSIONS as readonly string[]).includes(extensionOf(path));
export const isImagePath = (path: string): boolean => (BINARY_EXTENSIONS as readonly string[]).includes(extensionOf(path));

export function formatSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes < 1024) return `${Math.round(bytes)} Б`;
  const format = (value: number) => (value >= 10 ? Math.round(value).toString() : value.toFixed(1).replace(/\.0$/, '')).replace('.', ',');
  if (bytes < 1024 * 1024) return `${format(bytes / 1024)} КБ`;
  return `${format(bytes / 1024 / 1024)} МБ`;
}

/** Returns a Russian message or null when the path is acceptable. */
export function validatePath(path: string): string | null {
  if (!path) return 'Укажите имя файла.';
  if (textBytes(path) > LIMITS.maxPathBytes) return `Путь длиннее ${LIMITS.maxPathBytes} байт.`;
  if (path.includes('..')) return 'В пути нельзя использовать «..».';
  if (!PATH_RE.test(path)) return 'Путь может содержать латинские буквы, цифры, точку, дефис и подчёркивание; сегменты разделяются «/» и начинаются с буквы или цифры.';
  if (path.split('/').length > LIMITS.maxDepth) return `Не больше ${LIMITS.maxDepth} уровней вложенности.`;
  if (!ALLOWED_EXTENSIONS.includes(extensionOf(path))) return `Расширение не поддерживается. Разрешены: ${ALLOWED_EXTENSIONS.join(', ')}.`;
  return null;
}

/** Whole-draft check before saving: a list of Russian messages (empty = fine). */
export function validateDraft(files: Pick<DraftFile, 'path' | 'size'>[]): string[] {
  const errors: string[] = [];
  const seen = new Set<string>();
  let total = 0;
  if (files.length > LIMITS.maxFiles) errors.push(`Файлов больше ${LIMITS.maxFiles} (сейчас ${files.length}).`);
  for (const file of files) {
    const problem = validatePath(file.path);
    if (problem) errors.push(`${file.path || '(без имени)'}: ${problem}`);
    if (seen.has(file.path)) errors.push(`${file.path}: файл с таким именем уже есть.`);
    seen.add(file.path);
    if (file.size > LIMITS.maxFileBytes) errors.push(`${file.path}: ${formatSize(file.size)} — больше лимита ${formatSize(LIMITS.maxFileBytes)} на файл.`);
    total += file.size;
  }
  if (total > LIMITS.maxRevisionBytes) errors.push(`Общий размер ${formatSize(total)} больше лимита ${formatSize(LIMITS.maxRevisionBytes)} на версию.`);
  if (!seen.has(ENTRY_PATH)) errors.push(`Нужен файл ${ENTRY_PATH}: это точка входа артефакта.`);
  for (const path of seen) for (const other of seen) if (other.startsWith(`${path}/`)) errors.push(`«${path}» нельзя использовать одновременно как файл и как папку (${other}).`);
  return errors;
}

/** Message for adding or renaming one file in an existing draft (null = fine). `ignore` is the path being renamed. */
export function checkNewPath(path: string, files: Pick<DraftFile, 'path'>[], ignore = ''): string | null {
  const problem = validatePath(path);
  if (problem) return problem;
  const others = files.filter(file => file.path !== ignore);
  if (others.some(file => file.path === path)) return 'Файл с таким именем уже есть.';
  if (others.some(file => file.path.startsWith(`${path}/`) || path.startsWith(`${file.path}/`))) return 'Имя совпадает с папкой или находится внутри файла.';
  if (!ignore && others.length >= LIMITS.maxFiles) return `Файлов не больше ${LIMITS.maxFiles}.`;
  return null;
}

export type TreeNode = { name: string; path: string; type: 'dir' | 'file'; size: number; children: TreeNode[] };
export function buildTree(files: { path: string; size: number }[]): TreeNode[] {
  const root: TreeNode = { name: '', path: '', type: 'dir', size: 0, children: [] };
  for (const file of files) {
    const parts = file.path.split('/');
    let node = root;
    parts.forEach((part, index) => {
      const path = parts.slice(0, index + 1).join('/');
      const last = index === parts.length - 1;
      let child = node.children.find(item => item.name === part && item.type === (last ? 'file' : 'dir'));
      if (!child) { child = { name: part, path, type: last ? 'file' : 'dir', size: last ? file.size : 0, children: [] }; node.children.push(child); }
      node = child;
    });
  }
  const sort = (node: TreeNode) => {
    node.children.sort((a, b) => a.type === b.type ? a.name.localeCompare(b.name, 'en') : a.type === 'dir' ? -1 : 1);
    node.children.forEach(sort);
    if (node.type === 'dir') node.size = node.children.reduce((sum, item) => sum + item.size, 0);
  };
  sort(root);
  return root.children;
}

/** Draft from the server manifest: nothing loaded yet. */
export const draftFromManifest = (manifest: ManifestFile[]): DraftFile[] =>
  manifest.map(file => ({ path: file.path, encoding: isTextPath(file.path) ? 'utf8' : 'base64', content: null, original: null, size: file.size }));

/** Changes for PUT: only what differs from the manifest of base_revision. */
export function diffFiles(manifest: Pick<ManifestFile, 'path'>[], draft: DraftFile[]): FileChange[] {
  const before = new Set(manifest.map(file => file.path));
  const after = new Set(draft.map(file => file.path));
  const changes: FileChange[] = [];
  for (const file of manifest) if (!after.has(file.path)) changes.push({ path: file.path, delete: true });
  for (const file of draft) {
    if (file.content === null) { if (!before.has(file.path)) changes.push({ path: file.path, content: '', encoding: file.encoding }); continue; }
    if (before.has(file.path) && file.encoding === 'utf8' && file.original !== null && file.content === file.original) continue;
    changes.push({ path: file.path, content: file.content, encoding: file.encoding });
  }
  return changes;
}

export type MetaFields = { title: string; description: string; visibility: Visibility };
export const visibilityLabel = (value: string): string => value === 'private' ? 'Только мне' : 'Видно участникам пространства';

/** Body of PUT /api/artifacts/{id}: only changed fields. `changed` is false when nothing but base_revision/message would be sent. */
export function buildUpdate(base: MetaFields & { revision: number; manifest: ManifestFile[] }, next: MetaFields, draft: DraftFile[], message: string) {
  const body: Record<string, unknown> = { base_revision: base.revision };
  if (next.title.trim() !== base.title) body.title = next.title.trim();
  if (next.description.trim() !== base.description) body.description = next.description.trim();
  if (next.visibility !== base.visibility) body.visibility = next.visibility;
  const files = diffFiles(base.manifest, draft);
  if (files.length) body.files = files;
  const changed = Object.keys(body).length > 1;
  if (message.trim()) body.message = message.trim();
  return { body, changed };
}

// ---- Frame sizing and messages.
export const MIN_FRAME_HEIGHT = 120;
export const MAX_FRAME_HEIGHT = 4000;
export const FRAME_SANDBOX = 'allow-scripts allow-modals';
export const clampFrameHeight = (value: number): number => Math.min(MAX_FRAME_HEIGHT, Math.max(MIN_FRAME_HEIGHT, Math.round(value)));
/** Accepts a resize request only from the iframe's own window and only with a finite number; returns the clamped height or null. */
export function parseResizeMessage(event: { source: unknown; data: unknown }, frameWindow: unknown): number | null {
  if (!frameWindow || event.source !== frameWindow) return null;
  const data = event.data as { type?: unknown; height?: unknown } | null;
  if (!data || typeof data !== 'object' || data.type !== 'aiwiki:resize') return null;
  if (typeof data.height !== 'number' || !Number.isFinite(data.height)) return null;
  return clampFrameHeight(data.height);
}
/** Milliseconds until the capability is renewed: one minute before expiry, never sooner than 5 s, never beyond the timer limit. */
export function refreshDelay(expiresAt: string, now: number): number {
  const expiry = Date.parse(expiresAt);
  if (!Number.isFinite(expiry)) return 5 * 60 * 1000;
  return Math.min(2 ** 31 - 1, Math.max(5000, expiry - now - 60 * 1000));
}

// ---- Routes.
export type ArtRoute =
  | { kind: 'list'; space: string; q: string; project: string }
  | { kind: 'new'; space: string; project: string }
  | { kind: 'page'; id: string; mode: 'view' | 'edit' | 'history'; revision: number | null };

export function parseArtRoute(search: string): ArtRoute | null {
  const params = new URLSearchParams(search);
  const view = params.get('view');
  const id = params.get('artifact');
  if (id) {
    const revision = Number(params.get('revision'));
    return { kind: 'page', id, mode: view === 'edit' || view === 'history' ? view : 'view', revision: Number.isInteger(revision) && revision > 0 ? revision : null };
  }
  if (view === 'artifacts') return { kind: 'list', space: params.get('space') || '', q: params.get('q') || '', project: params.get('project') || '' };
  if (view === 'artifact-new') return { kind: 'new', space: params.get('space') || '', project: params.get('project') || '' };
  return null;
}
export function artRouteUrl(route: ArtRoute): string {
  const params = new URLSearchParams();
  if (route.kind === 'page') {
    params.set('artifact', route.id);
    if (route.mode !== 'view') params.set('view', route.mode);
    if (route.revision) params.set('revision', String(route.revision));
  } else {
    params.set('view', route.kind === 'list' ? 'artifacts' : 'artifact-new');
    if (route.space) params.set('space', route.space);
    if (route.kind === 'list' && route.q) params.set('q', route.q);
    if (route.project) params.set('project', route.project);
  }
  return `/?${params}`;
}

export const shareLabel = (revision: number | null | undefined): string => revision ? `версия ${revision}` : 'последняя версия';
/** Explanation when link creation failed because the administrator turned public links off (server text is matched loosely). */
export function publicLinksDisabledHint(message: string): string | null {
  return /public|публич|disabled|отключ|выключ/i.test(message) ? 'Публичные ссылки отключены администратором организации. Обратитесь к нему, если ссылка нужна.' : null;
}
