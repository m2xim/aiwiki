import type { AccessGrant, CatalogSpace, MovePreview } from './api';

type Role = AccessGrant['role'];
export type AccessChange = 'gain' | 'lose' | 'upgrade' | 'downgrade';

const RANK: Record<Role, number> = { reader: 1, commenter: 2, editor: 3, manager: 4, owner: 5 };
const ROLE_LABELS: Record<Role, string> = { owner: 'Владелец', manager: 'Управление', editor: 'Запись', commenter: 'Комментарии', reader: 'Чтение' };

/** Russian label of a role; `null`/empty means no access. */
export const roleLabel = (role?: string | null): string => {
  if (!role) return 'Нет доступа';
  return ROLE_LABELS[role as Role] || role;
};

export const canManageRole = (role?: string | null): boolean => role === 'manager' || role === 'owner';

/** Classifies a change of effective role; `null` when nothing changes. */
export function describeAccessChange(before?: string | null, after?: string | null): AccessChange | null {
  const from = before ? RANK[before as Role] ?? 0 : 0;
  const to = after ? RANK[after as Role] ?? 0 : 0;
  if (from === to) return null;
  if (!from) return 'gain';
  if (!to) return 'lose';
  return to > from ? 'upgrade' : 'downgrade';
}

/** Russian plural form: 1 статья, 2 статьи, 5 статей; 11-14 always use the genitive plural. */
export function plural(n: number, one: string, few: string, many: string): string {
  const abs = Math.abs(Math.trunc(n));
  const last = abs % 10, tens = abs % 100;
  if (tens >= 11 && tens <= 14) return many;
  if (last === 1) return one;
  if (last >= 2 && last <= 4) return few;
  return many;
}

export const countArticles = (n: number) => `${n} ${plural(n, 'статья', 'статьи', 'статей')}`;
export const countArtifacts = (n: number) => `${n} ${plural(n, 'артефакт', 'артефакта', 'артефактов')}`;
export const countUsers = (n: number) => `${n} ${plural(n, 'пользователь', 'пользователя', 'пользователей')}`;
/** «у 1 пользователя», «у 2 пользователей», «у 21 пользователя» (genitive after a preposition). */
export const countUsersOf = (n: number) => `${n} ${Math.abs(n) % 10 === 1 && Math.abs(n) % 100 !== 11 ? 'пользователя' : 'пользователей'}`;
export const countMembers = (n: number) => `${n} ${plural(n, 'участник', 'участника', 'участников')}`;

/** Summary lines for a preview: what moves and who gains or loses access. */
export function summarizePreview(p: Pick<MovePreview, 'articles' | 'artifacts' | 'users_gaining' | 'users_losing'>): string[] {
  const parts = [countArticles(p.articles), ...((p.artifacts ?? 0) > 0 ? [countArtifacts(p.artifacts!)] : [])];
  const lines = [`Будет перемещено: ${parts.join(', ')}`];
  const gaining = p.users_gaining || 0, losing = p.users_losing || 0;
  if (gaining) lines.push(`Права расширятся у: ${countUsersOf(gaining)}`);
  if (losing) lines.push(`Права сузятся или пропадут у: ${countUsersOf(losing)}`);
  if (!gaining && !losing) lines.push('Права доступа не изменятся');
  return lines;
}

export type TargetSpace = { space: CatalogSpace };

/** Spaces the actor may move into: not archived, not limited and managed (manager+) by the actor. */
export function eligibleTargets(catalog: CatalogSpace[]): TargetSpace[] {
  return catalog.filter(space => !space.archived_at && !space.limited && canManageRole(space.effective_role)).map(space => ({ space }));
}

/** Role on the space an article lives in; undefined when unknown. */
export function containerRole(catalog: CatalogSpace[], place: { space_id: string }): string | undefined {
  return catalog.find(s => s.id === place.space_id)?.effective_role || undefined;
}
export const canEditRole = (role?: string | null): boolean => role === 'editor' || role === 'manager' || role === 'owner';
