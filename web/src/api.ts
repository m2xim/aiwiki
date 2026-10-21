export type Space = { id: string; name: string; description: string; archived_at?: string };
export type Article = {
  id: string; space_id: string; parent_id: string; title: string; summary: string; body: string;
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
export type Bootstrap = { instructions: Article[]; spaces: Space[]; recent: Article[]; guide: string };

const api = async <T,>(url: string, init?: RequestInit): Promise<T> => {
  const response = await fetch(url, init);
  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`;
    try { message = (await response.json()).error || message; } catch { /* response is not JSON */ }
    throw new Error(message);
  }
  if (response.status === 204) return undefined as T;
  const type = response.headers.get('content-type') || '';
  return (type.includes('application/json') ? response.json() : response.text()) as Promise<T>;
};
const json = (method: string, data: unknown): RequestInit => ({ method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(data) });
export const client = {
  spaces: (includeArchived = false) => api<Space[]>(`/api/spaces${includeArchived ? '?include_archived=true' : ''}`),
  updateSpace: (id: string, data: { name?: string; description?: string; archived?: boolean }) => api<Space>(`/api/spaces/${encodeURIComponent(id)}`, json('PATCH', data)),
  tags: (spaceId: string) => api<string[]>(`/api/tags?space_id=${encodeURIComponent(spaceId)}`),
  createSpace: (name: string, description: string) => api<Space>('/api/spaces', json('POST', { name, description })),
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
};
