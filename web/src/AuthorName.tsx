import type { AuthorKind } from './api';

type AuthorLike = { author?: string; author_name?: string; author_kind?: AuthorKind };

/** Display name of an author: the resolved name, falling back to the stored id. */
export const authorLabel = (item: AuthorLike, fallback = '') => item.author_name || item.author || fallback;

/** Author name with a muted "агент" badge for MCP service accounts. */
export function AuthorName({ item, fallback = '' }: { item: AuthorLike; fallback?: string }) {
  return <>{authorLabel(item, fallback)}{item.author_kind === 'service' && <span className="author-badge" title="Сервисная учётная запись MCP">агент</span>}</>;
}
