import type { AccessGrant, CatalogSpace, MovePreview, Project } from './api';

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
export const countProjects = (n: number) => `${n} ${plural(n, 'проект', 'проекта', 'проектов')}`;
export const countUsers = (n: number) => `${n} ${plural(n, 'пользователь', 'пользователя', 'пользователей')}`;
/** «у 1 пользователя», «у 2 пользователей», «у 21 пользователя» (genitive after a preposition). */
export const countUsersOf = (n: number) => `${n} ${Math.abs(n) % 10 === 1 && Math.abs(n) % 100 !== 11 ? 'пользователя' : 'пользователей'}`;
export const countMembers = (n: number) => `${n} ${plural(n, 'участник', 'участника', 'участников')}`;

/** Summary lines for a preview: what moves and who gains or loses access. */
export function summarizePreview(p: Pick<MovePreview, 'articles' | 'projects' | 'users_gaining' | 'users_losing'>): string[] {
  const lines = [p.projects > 0 ? `Будет перемещено: ${countProjects(p.projects)}, ${countArticles(p.articles)}` : `Будет перемещено: ${countArticles(p.articles)}`];
  const gaining = p.users_gaining || 0, losing = p.users_losing || 0;
  if (gaining) lines.push(`Права расширятся у: ${countUsersOf(gaining)}`);
  if (losing) lines.push(`Права сузятся или пропадут у: ${countUsersOf(losing)}`);
  if (!gaining && !losing) lines.push('Права доступа не изменятся');
  return lines;
}

export type TargetSpace = { space: CatalogSpace; canRoot: boolean; projects: Project[] };

/** All project ids under `rootId` (including itself), following parent_id. */
export function projectSubtreeIds(rootId: string, projects: Project[]): Set<string> {
  const ids = new Set<string>([rootId]);
  let grew = true;
  while (grew) {
    grew = false;
    for (const p of projects) if (!ids.has(p.id) && p.parent_id && ids.has(p.parent_id)) { ids.add(p.id); grew = true; }
  }
  return ids;
}

/**
 * Containers the actor may move into. canRoot: the space itself is a valid target (manager+ on the space).
 * Article moves: a space is listed when the space itself or any of its projects is eligible.
 * Project moves: the moved project and its descendants are never offered as parents.
 */
export function eligibleTargets(catalog: CatalogSpace[], kind: 'article' | 'project', movingProjectId = '', projects?: Project[]): TargetSpace[] {
  const all = projects ?? catalog.flatMap(s => s.projects || []);
  const excluded = kind === 'project' && movingProjectId ? projectSubtreeIds(movingProjectId, all) : new Set<string>();
  const result: TargetSpace[] = [];
  for (const space of catalog) {
    if (space.archived_at) continue;
    const canRoot = canManageRole(space.effective_role);
    const eligible = (space.projects || []).filter(p => !p.archived_at && canManageRole(p.effective_role) && !excluded.has(p.id));
    // The server allows a move under any project the actor manages, so a space qualifies through the space root or through one eligible project.
    if (!canRoot && !eligible.length) continue;
    result.push({ space, canRoot, projects: eligible });
  }
  return result;
}

type Place = { space_id: string; project_id?: string | null };
/** True when the target is the article's current space and project (then the move is a plain parent change). */
export const isSameContainer = (article: Place, target: Place): boolean =>
  article.space_id === target.space_id && (article.project_id || '') === (target.project_id || '');

/** Role on the container an article lives in: its project from the catalog, else its space; undefined when unknown. */
export function containerRole(catalog: CatalogSpace[], place: Place): string | undefined {
  const space = catalog.find(s => s.id === place.space_id);
  if (!space) return undefined;
  const project = place.project_id ? (space.projects || []).find(p => p.id === place.project_id) : undefined;
  return (project ? project.effective_role : undefined) || space.effective_role || undefined;
}
export const canEditRole = (role?: string | null): boolean => role === 'editor' || role === 'manager' || role === 'owner';
