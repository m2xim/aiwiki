export type Space = { limited?: boolean; id: string; name: string; description: string; archived_at?: string; personal?: boolean; article_count?: number; project_count?: number; last_updated?: string; tags?: string[]; effective_role?: AccessGrant['role'] };
export type Project = { id: string; name: string; description: string; space_id: string; parent_id: string; archived_at?: string; inherit_acl: boolean; article_count?: number; last_updated?: string; tags?: string[]; effective_role?: AccessGrant['role'] };
export type MCPDelegation = { id: string; name: string; project_ids: string[]; created_at: string; expires_at: string };
export type CreatedMCPDelegation = { token: string };
export type MCPCredential = { id: string; name: string; service_account_id?: string; project_ids: string[]; mode: 'read' | 'write'; created_at: string; expires_at: string; revoked_at?: string };
export type MCPServiceAccount = { id: string; name: string; sponsor_id: string; disabled: boolean; created_at: string };
export type Team = { id: string; name: string; description: string; archived_at?: string };
export type TeamMember = { id: string; name: string };
export type UserAccessResource = { resource_type: 'space' | 'project'; resource_id: string; name: string; role: AccessGrant['role']; archived: boolean; personal: boolean };
export type UserAccessSnapshot = { user_id: string; resources: UserAccessResource[] };
export type AccessGrant = { subject_type: 'user' | 'team'; subject_id: string; role: 'owner' | 'manager' | 'editor' | 'commenter' | 'reader' };
export type CatalogSpace = Space & { projects: Project[]; article_count: number };
export type AuthUser = { id: string; name: string; admin: boolean };
export type AuthSession = { mode: 'trusted' } | { mode: 'corporate'; user: AuthUser; csrf_token: string };
export type Article = {
  id: string; space_id: string; project_id?: string; parent_id: string; title: string; summary: string; body: string;
  kind: string; status: string; pinned: boolean; tags: string[]; properties: Record<string, string>;
  revision: number; created_at: string; updated_at: string; author: string;
  deleted_at?: string;
};
export type Revision = { id: string; article_id: string; revision: number; author: string; message: string; created_at: string; snapshot: Article };
export type AuditEvent = { id: number; article_id: string; title: string; revision: number; author: string; message: string; created_at: string; changes: string[]; space_id: string; previous_space_id: string; deleted: boolean };
export type Comment = { id: string; article_id: string; anchor: string; body: string; author: string; created_at: string; resolved: boolean };
export type Attachment = { id: string; article_id: string; name: string; mime: string; size: number; url: string; created_at: string };
export type Favorite = { id: string; title: string; space_id: string; parent_id: string; kind: string; status: string; summary: string; updated_at: string; favorited_at: string };
export type SearchMatch = 'title' | 'tags' | 'summary' | 'properties' | 'body';
export type SearchHit = { id: string; title: string; space_id: string; parent_id: string; kind: string; status: string; summary: string; tags: string[]; updated_at: string; pinned?: boolean; score?: number; snippet?: string; matched_in?: string[] };
export type SearchResponse = { total: number; offset: number; mode?: 'all' | 'any'; next_offset?: number; results: SearchHit[] };
export type MoveTarget = { space_id: string; project_id?: string; parent_id?: string };
export type MoveRequest = { kind: 'article' | 'project'; id: string; base_revision?: number; target: MoveTarget; fingerprint?: string };
export type MovePlace = { space_id: string; space_name: string; project_id: string; project_name: string };
export type MoveAccessChange = { subject_type: 'user' | 'team' | 'other'; subject_id: string; name: string; before: AccessGrant['role'] | null; after: AccessGrant['role'] | null; users?: number };
export type MovePreview = { fingerprint: string; articles: number; projects: number; artifacts?: number; source: MovePlace; target: MovePlace; access: MoveAccessChange[]; users_gaining: number; users_losing: number; warnings: string[] };
export type MoveApplyResult = { conflict: false; articles: number; projects: number; root: Article | Project } | { conflict: true; preview: MovePreview };
export type ArtifactSummary = {
  id: string; space_id: string; project_id?: string; title: string; description: string; visibility: Visibility;
  owner_id?: string; owner_name?: string; author?: string; revision: number; created_at?: string; updated_at: string; file_count: number; deleted_at?: string | null;
};
export type ArtifactDetail = ArtifactSummary & { manifest: ManifestFile[]; can_edit: boolean; can_share: boolean; can_manage: boolean };
export type ArtifactRevisionInfo = { revision: number; author: string; message: string; created_at: string; file_count: number };
export type ArtifactShare = { id: string; url?: string; revision: number | null; created_by?: string; created_at: string; revoked_at?: string | null };
export type ArtifactFileInput = { path: string; content: string; encoding: 'utf8' | 'base64' } | { path: string; delete: true };
export type ArtifactView = { frame_url: string; expires_at: string };
export type AdminSettings = { public_artifact_links: boolean };
export type Bootstrap = { instructions: Article[]; spaces: Space[]; recent: Article[]; guide: string };
import type { ManifestFile, Visibility } from './artifactsCore';
let corporateSession = false;
export const setCorporateSession = (value: boolean) => { corporateSession = value; };

let csrfToken = '';
export const setCsrfToken = (token: string) => { csrfToken = token; };
let authExpiredHandler: (() => void) | null = null;
export const setAuthExpiredHandler = (handler: (() => void) | null) => { authExpiredHandler = handler; };
const api = async <T,>(url: string, init?: RequestInit): Promise<T> => {
  const method = (init?.method || 'GET').toUpperCase();
  const headers = new Headers(init?.headers);
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS' && csrfToken) headers.set('X-CSRF-Token', csrfToken);
  const response = await fetch(url, { ...init, headers });
  if (!response.ok) {
    if (response.status === 401 && !location.pathname.startsWith('/auth/')) {
      if (authExpiredHandler) authExpiredHandler(); else location.assign('/auth/login');
    }
    let message = response.status === 403 ? 'Недостаточно прав для этого действия.' : `${response.status} ${response.statusText}`;
    try { const body = await response.json(); if (body.error && body.error !== 'forbidden') message = body.error; } catch { /* response is not JSON */ }
    throw Object.assign(new Error(message), { status: response.status });
  }
  if (response.status === 204) return undefined as T;
  const type = response.headers.get('content-type') || '';
  return (type.includes('application/json') ? response.json() : response.text()) as Promise<T>;
};
const api204 = async (url: string, init: RequestInit) => {
  const method = (init.method || 'GET').toUpperCase();
  const headers = new Headers(init.headers);
  if (method !== 'GET' && method !== 'HEAD' && method !== 'OPTIONS' && csrfToken) headers.set('X-CSRF-Token', csrfToken);
  const response = await fetch(url, { ...init, headers });
  if (!response.ok) {
    let message = response.status === 403 ? 'Недостаточно прав для выхода из системы.' : `${response.status} ${response.statusText}`;
    try { const body = await response.json(); if (body.error && body.error !== 'forbidden') message = body.error; } catch { /* response is not JSON */ }
    if (response.status === 401 && authExpiredHandler) authExpiredHandler();
    throw new Error(message);
  }
  if (response.status !== 204) throw new Error('Сервер не подтвердил завершение сеанса.');
};
// Apply needs the fresh preview from a 409, which api() would flatten into a message.
const moveApply = async (request: MoveRequest): Promise<MoveApplyResult> => {
  const headers = new Headers({ 'Content-Type': 'application/json' });
  if (csrfToken) headers.set('X-CSRF-Token', csrfToken);
  const response = await fetch('/api/moves/apply', { method: 'POST', headers, body: JSON.stringify(request) });
  let body: { error?: string; preview?: MovePreview; articles?: number; projects?: number; root?: Article | Project } = {};
  try { body = await response.json(); } catch { /* response is not JSON */ }
  if (response.status === 409 && body.preview) return { conflict: true, preview: body.preview };
  if (!response.ok) {
    if (response.status === 401 && !location.pathname.startsWith('/auth/')) {
      if (authExpiredHandler) authExpiredHandler(); else location.assign('/auth/login');
    }
    throw new Error(response.status === 403 ? 'Недостаточно прав для этого действия.' : body.error && body.error !== 'forbidden' ? body.error : `${response.status} ${response.statusText}`);
  }
  return { conflict: false, articles: body.articles ?? 0, projects: body.projects ?? 0, root: body.root as Article | Project };
};
const json = (method: string, data: unknown): RequestInit => ({ method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(data) });
const listOf = <T,>(value: unknown, key: string): T[] => Array.isArray(value) ? value as T[] : Array.isArray((value as Record<string, unknown> | null)?.[key]) ? (value as Record<string, T[]>)[key] : [];
const flag = (root: Record<string, unknown>, name: string): boolean => Boolean(root[name] ?? (root.permissions as Record<string, unknown> | undefined)?.[name]);
const normalizeArtifact = (raw: Record<string, unknown>): ArtifactDetail => ({
  ...(raw as unknown as ArtifactSummary),
  manifest: listOf<ManifestFile>(raw.manifest ?? raw.files, 'files'),
  can_edit: flag(raw, 'can_edit'), can_share: flag(raw, 'can_share'), can_manage: flag(raw, 'can_manage'),
});
const absoluteUrl = (url: string | undefined) => url && url.startsWith('/') ? `${location.origin}${url}` : url;
const enc = encodeURIComponent;
const encPath = (path: string) => path.split('/').map(enc).join('/');
export const errorStatus = (error: unknown): number => Number((error as { status?: number } | null)?.status) || 0;
const artifactApi = {
  artifacts: async (params: Record<string, string>) => listOf<ArtifactSummary>(await api<unknown>(`/api/artifacts?${new URLSearchParams(Object.entries(params).filter(([, value]) => value))}`), 'artifacts'),
  artifact: async (id: string) => normalizeArtifact(await api<Record<string, unknown>>(`/api/artifacts/${enc(id)}`)),
  createArtifact: async (data: { space_id: string; project_id?: string; title: string; description: string; visibility: Visibility; files: ArtifactFileInput[]; message?: string }) => normalizeArtifact(await api<Record<string, unknown>>('/api/artifacts', json('POST', data))),
  updateArtifact: async (id: string, data: Record<string, unknown>) => normalizeArtifact(await api<Record<string, unknown>>(`/api/artifacts/${enc(id)}`, json('PUT', data))),
  deleteArtifact: (id: string, baseRevision: number) => api<unknown>(`/api/artifacts/${enc(id)}`, json('DELETE', { base_revision: baseRevision })),
  restoreArtifact: (id: string, baseRevision: number) => api<unknown>(`/api/artifacts/${enc(id)}/restore`, json('POST', { base_revision: baseRevision })),
  artifactHistory: async (id: string) => listOf<ArtifactRevisionInfo>(await api<unknown>(`/api/artifacts/${enc(id)}/history`), 'revisions'),
  artifactView: (id: string, revision?: number | null) => api<ArtifactView>(`/api/artifacts/${enc(id)}/view`, json('POST', revision ? { revision } : {})),
  artifactShares: async (id: string) => listOf<ArtifactShare>(await api<unknown>(`/api/artifacts/${enc(id)}/shares`), 'shares'),
  createArtifactShare: async (id: string, revision: number | null) => { const share = await api<ArtifactShare>(`/api/artifacts/${enc(id)}/shares`, json('POST', { revision })); return { ...share, url: absoluteUrl(share.url) }; },
  revokeArtifactShare: (id: string, shareId: string) => api<void>(`/api/artifacts/${enc(id)}/shares/${enc(shareId)}`, { method: 'DELETE' }),
  /** Raw bytes of one file (for the editor). */
  artifactFile: async (id: string, path: string, revision?: number): Promise<ArrayBuffer> => {
    const response = await fetch(`/api/artifacts/${enc(id)}/files/${encPath(path)}${revision ? `?revision=${revision}` : ''}`);
    if (!response.ok) {
      if (response.status === 401 && !location.pathname.startsWith('/auth/')) { if (authExpiredHandler) authExpiredHandler(); else location.assign('/auth/login'); }
      throw new Error(response.status === 403 ? 'Недостаточно прав для этого действия.' : `${response.status} ${response.statusText}`);
    }
    return response.arrayBuffer();
  },
  adminSettings: () => api<AdminSettings>('/api/admin/settings'),
  setAdminSettings: (data: AdminSettings) => api<AdminSettings>('/api/admin/settings', json('PUT', data)),
};
export const client = {
  ...artifactApi,
  me: () => api<AuthSession>('/api/me'),
  logout: () => api204('/auth/logout', { method: 'POST' }),
  spaces: (includeArchived = false) => api<Space[]>(`/api/spaces${includeArchived ? '?include_archived=true' : ''}`),
  updateSpace: (id: string, data: { name?: string; description?: string; archived?: boolean }) => api<Space>(`/api/spaces/${encodeURIComponent(id)}`, json('PATCH', data)),
  tags: (spaceId: string) => api<string[]>(`/api/tags?space_id=${encodeURIComponent(spaceId)}`),
  createSpace: (name: string, description: string) => corporateSession
    ? api<Space>('/api/corporate/spaces', json('POST', { name, description, personal: true }))
    : api<Space>('/api/spaces', json('POST', { name, description })),
  articles: (params: Record<string, string>) => api<Article[]>(`/api/articles?${new URLSearchParams(params)}`),
  favorites: () => api<Favorite[]>('/api/favorites'),
  addFavorite: (id: string) => api<void>(`/api/favorites/${encodeURIComponent(id)}`, { method: 'PUT' }),
  removeFavorite: (id: string) => api<void>(`/api/favorites/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  search: (params: Record<string, string>) => api<SearchResponse>(`/api/search?${new URLSearchParams(params)}`),
  article: (id: string) => api<Article>(`/api/articles/${encodeURIComponent(id)}`),
  createArticle: (value: Record<string, unknown>) => api<Article>('/api/articles', json('POST', value)),
  updateArticle: (id: string, value: Record<string, unknown>) => api<Article>(`/api/articles/${encodeURIComponent(id)}`, json('PUT', value)),
  deleteArticle: (id: string, data: unknown) => api<Article>(`/api/articles/${encodeURIComponent(id)}`, json('DELETE', data)),
  undeleteArticle: (id: string, data: unknown) => api<Article>(`/api/articles/${encodeURIComponent(id)}/undelete`, json('POST', data)),
  bootstrap: (space: string) => api<Bootstrap>(`/api/bootstrap?space_id=${encodeURIComponent(space)}`),
  history: (id: string) => api<Revision[]>(`/api/articles/${encodeURIComponent(id)}/history`),
  audit: (spaceId: string, limit = 50, offset = 0, author = '') => api<AuditEvent[]>(`/api/spaces/${encodeURIComponent(spaceId)}/audit?${new URLSearchParams({ limit: String(limit), offset: String(offset), ...(author ? { author } : {}) })}`),
  restore: (id: string, data: unknown) => api<Article>(`/api/articles/${encodeURIComponent(id)}/restore`, json('POST', data)),
  links: (id: string) => api<{ outgoing: Article[]; incoming: Article[] }>(`/api/articles/${encodeURIComponent(id)}/links`),
  comments: (id: string) => api<Comment[]>(`/api/articles/${encodeURIComponent(id)}/comments`),
  addComment: (id: string, data: unknown) => api<Comment>(`/api/articles/${encodeURIComponent(id)}/comments`, json('POST', data)),
  resolveComment: (id: string, resolved: boolean) => api<Comment>(`/api/comments/${encodeURIComponent(id)}`, json('PATCH', { resolved })),
  updateComment: (id: string, data: { body?: string; anchor?: string; resolved?: boolean }) => api<Comment>(`/api/comments/${encodeURIComponent(id)}`, json('PATCH', data)),
  attachments: (id: string) => api<Attachment[]>(`/api/articles/${encodeURIComponent(id)}/attachments`),
  deleteAttachment: (articleId: string, fileId: string) => api<void>(`/api/articles/${encodeURIComponent(articleId)}/attachments/${encodeURIComponent(fileId)}`, { method: 'DELETE' }),
  upload: (id: string, file: File) => { const data = new FormData(); data.append('file', file); return api<Attachment>(`/api/articles/${encodeURIComponent(id)}/attachments`, { method: 'POST', body: data }); },
  movePreview: (request: MoveRequest) => api<MovePreview>('/api/moves/preview', json('POST', request)),
  moveApply,
  catalog: () => api<CatalogSpace[]>('/api/catalog'),
  teams: () => api<Team[]>('/api/teams'),
  createTeam: (name: string, description = '') => api<Team>('/api/teams', json('POST', { name, description })),
  updateTeam: (id: string, data: { name?: string; description?: string; archived?: boolean }) => api<Team>(`/api/teams/${encodeURIComponent(id)}`, json('PATCH', data)),
  teamMembers: (teamId: string) => api<TeamMember[]>(`/api/teams/${encodeURIComponent(teamId)}/members`),
  setTeamMember: (teamId: string, userId: string) => api<void>(`/api/teams/${encodeURIComponent(teamId)}/members`, json('PUT', { user_id: userId })),
  removeTeamMember: (teamId: string, userId: string) => api<void>(`/api/teams/${encodeURIComponent(teamId)}/members`, json('DELETE', { user_id: userId })),
  adminUsers: () => api<{ id: string; name: string; disabled: boolean; admin: boolean }[]>('/api/admin/users'),
  adminUserAccess: (userId: string) => api<UserAccessSnapshot>(`/api/admin/users/${encodeURIComponent(userId)}/access`),
  setUserDisabled: (userId: string, disabled: boolean) => api<void>(`/api/admin/users/${encodeURIComponent(userId)}`, json('PATCH', { disabled })),
  createSpaceFor: (data: { name: string; description: string; personal: boolean }) => api<Space>('/api/corporate/spaces', json('POST', data)),
  projects: (spaceId = '') => api<Project[]>(`/api/projects${spaceId ? `?space_id=${encodeURIComponent(spaceId)}` : ''}`),
  mcpDelegations: () => api<MCPDelegation[]>('/api/mcp/delegations'),
  createMCPDelegation: (data: { name: string; project_ids: string[]; ttl_seconds: 300 }) => api<CreatedMCPDelegation>('/api/mcp/delegations', json('POST', data)),
  revokeMCPDelegation: (id: string) => api<void>(`/api/mcp/delegations/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  mcpCredentials: () => api<{ enabled: boolean; credentials: MCPCredential[] }>('/api/mcp/credentials'),
  createMCPCredential: (data: { name: string; project_ids: string[]; mode: 'read' | 'write'; ttl_seconds: number; service_account_id?: string }) => api<MCPCredential & { token: string }>('/api/mcp/credentials', json('POST', data)),
  revokeMCPCredential: (id: string) => api<void>(`/api/mcp/credentials/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  mcpServiceAccounts: () => api<{ enabled: boolean; service_accounts: MCPServiceAccount[] }>('/api/mcp/service-accounts'),
  createMCPServiceAccount: (name: string) => api<MCPServiceAccount>('/api/mcp/service-accounts', json('POST', { name })),
  disableMCPServiceAccount: (id: string) => api<void>(`/api/mcp/service-accounts/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  createProject: (data: { name: string; description: string; space_id: string; parent_id?: string; inherit_acl?: boolean }) => api<Project>('/api/projects', json('POST', data)),
  updateProject: (id: string, data: Partial<Pick<Project, 'name' | 'description' | 'archived_at' | 'inherit_acl'>> & { archived?: boolean }) => {
    const { archived_at, ...fields } = data;
    return api<Project>(`/api/projects/${encodeURIComponent(id)}`, json('PATCH', { ...fields, ...(archived_at !== undefined ? { archived: Boolean(archived_at) } : {}) }));
  },
  users: (resourceType: 'space' | 'project', resourceId: string) => api<TeamMember[]>(`/api/users?${new URLSearchParams({ resource_type: resourceType, resource_id: resourceId })}`),
  access: async (resourceType: 'space' | 'project', resourceId: string) => (await api<{ grants: AccessGrant[] }>(`/api/access?${new URLSearchParams({ resource_type: resourceType, resource_id: resourceId })}`)).grants,
  effectiveAccess: (resourceType: 'space' | 'project', resourceId: string, userId: string) => api<{ role: AccessGrant['role'] | null; sources: unknown[] }>(`/api/access/effective?${new URLSearchParams({ resource_type: resourceType, resource_id: resourceId, user_id: userId })}`),
  setAccess: (resourceType: 'space' | 'project', resourceId: string, grant: AccessGrant) => api<AccessGrant>('/api/access?' + new URLSearchParams({ resource_type: resourceType, resource_id: resourceId }), json('PUT', { grant })),
  removeAccess: (resourceType: 'space' | 'project', resourceId: string, grant: Pick<AccessGrant, 'subject_type' | 'subject_id'>) => api<void>('/api/access?' + new URLSearchParams({ resource_type: resourceType, resource_id: resourceId, ...grant }), { method: 'DELETE' }),
};
