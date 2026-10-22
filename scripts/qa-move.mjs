import assert from 'node:assert/strict';
import { canEditRole, containerRole, countArticles, countUsersOf, countProjects, countUsers, describeAccessChange, eligibleTargets, isSameContainer, plural, projectSubtreeIds, roleLabel, summarizePreview } from '../web/src/moveCore.ts';

// Plural forms, including the 11-14 exception and 21/101.
const forms = n => plural(n, 'статья', 'статьи', 'статей');
for (const [n, word] of [[0, 'статей'], [1, 'статья'], [2, 'статьи'], [4, 'статьи'], [5, 'статей'], [11, 'статей'], [12, 'статей'], [14, 'статей'], [21, 'статья'], [22, 'статьи'], [25, 'статей'], [101, 'статья'], [111, 'статей'], [112, 'статей']]) assert.equal(forms(n), word, `plural ${n}`);
assert.equal(countArticles(21), '21 статья');
assert.equal(countProjects(2), '2 проекта');
assert.equal(countUsers(11), '11 пользователей');

// Roles.
assert.equal(roleLabel('owner'), 'Владелец');
assert.equal(roleLabel('manager'), 'Управление');
assert.equal(roleLabel('editor'), 'Запись');
assert.equal(roleLabel('commenter'), 'Комментарии');
assert.equal(roleLabel('reader'), 'Чтение');
assert.equal(roleLabel(null), 'Нет доступа');
assert.equal(roleLabel('custom'), 'custom');

assert.equal(describeAccessChange(null, 'reader'), 'gain');
assert.equal(describeAccessChange('editor', null), 'lose');
assert.equal(describeAccessChange('reader', 'editor'), 'upgrade');
assert.equal(describeAccessChange('owner', 'commenter'), 'downgrade');
assert.equal(describeAccessChange('reader', 'reader'), null);
assert.equal(describeAccessChange(null, null), null);

// Summary lines.
assert.deepEqual(summarizePreview({ articles: 1, projects: 0, users_gaining: 0, users_losing: 0 }), ['Будет перемещено: 1 статья', 'Права доступа не изменятся']);
assert.deepEqual(summarizePreview({ articles: 12, projects: 0, users_gaining: 3, users_losing: 5 }), ['Будет перемещено: 12 статей', 'Права расширятся у: 3 пользователей', 'Права сузятся или пропадут у: 5 пользователей']);
assert.deepEqual(summarizePreview({ articles: 2, projects: 0, users_gaining: 1, users_losing: 0 }), ['Будет перемещено: 2 статьи', 'Права расширятся у: 1 пользователя']);
assert.equal(countUsersOf(21), '21 пользователя');
assert.equal(countUsersOf(11), '11 пользователей');
assert.equal(countUsersOf(2), '2 пользователей');
assert.equal(summarizePreview({ articles: 21, projects: 2, users_gaining: 1, users_losing: 0 })[0], 'Будет перемещено: 2 проекта, 21 статья');
assert.deepEqual(summarizePreview({ articles: 3, projects: 1, users_gaining: 0, users_losing: 11 }).slice(1), ['Права сузятся или пропадут у: 11 пользователей']);

// Eligible targets.
const project = (id, space_id, parent_id, effective_role = 'owner', extra = {}) => ({ id, name: id, description: '', space_id, parent_id, inherit_acl: true, effective_role, ...extra });
const catalog = [
  { id: 's1', name: 'S1', effective_role: 'manager', projects: [project('a', 's1', ''), project('a1', 's1', 'a'), project('a2', 's1', 'a1'), project('b', 's1', ''), project('arch', 's1', '', 'owner', { archived_at: 'x' }), project('weak', 's1', '', 'editor')] },
  { id: 's2', name: 'S2', effective_role: 'editor', projects: [project('c', 's2', '', 'manager'), project('d', 's2', '', 'reader')] },
  { id: 's3', name: 'S3', effective_role: 'reader', projects: [project('e', 's3', '', 'editor')] },
  { id: 's4', name: 'S4', effective_role: 'owner', archived_at: 'x', projects: [] },
  { id: 's5', name: 'S5', effective_role: 'owner', projects: [] },
];
const ids = list => list.map(t => t.space.id);
const art = eligibleTargets(catalog, 'article');
assert.deepEqual(ids(art), ['s1', 's2', 's5'], 'archived and unmanaged spaces are excluded; project-only manager keeps its space');
assert.equal(art[0].canRoot, true);
assert.equal(art[1].canRoot, false);
assert.deepEqual(art[0].projects.map(p => p.id), ['a', 'a1', 'a2', 'b'], 'archived and non-manager projects are excluded');
assert.deepEqual(art[1].projects.map(p => p.id), ['c']);

const prj = eligibleTargets(catalog, 'project', 'a');
assert.deepEqual(ids(prj), ['s1', 's2', 's5'], 'a space qualifies through a manager project even without the space root');
assert.equal(prj[1].canRoot, false);
assert.deepEqual(prj[1].projects.map(p => p.id), ['c']);
assert.deepEqual(prj[0].projects.map(p => p.id), ['b'], 'subtree of the moved project is excluded');
assert.deepEqual([...projectSubtreeIds('a', catalog[0].projects)].sort(), ['a', 'a1', 'a2']);
assert.deepEqual(eligibleTargets(catalog, 'project', 'a1')[0].projects.map(p => p.id), ['a', 'b'], 'ancestors stay valid, only descendants are excluded');
// An explicit project list (with parent links) takes precedence over the catalog.
assert.deepEqual(eligibleTargets(catalog, 'project', 'b', [project('b', 's1', ''), project('a', 's1', 'b'), project('a1', 's1', 'a')])[0].projects.map(p => p.id), ['a2']);

// Container roles.
assert.equal(containerRole(catalog, { space_id: 's1', project_id: 'weak' }), 'editor');
assert.equal(containerRole(catalog, { space_id: 's1', project_id: 'missing' }), 'manager', 'unknown project falls back to the space');
assert.equal(containerRole(catalog, { space_id: 's1' }), 'manager');
assert.equal(containerRole(catalog, { space_id: 'nope' }), undefined);
assert.equal(canEditRole('editor') && canEditRole('owner') && !canEditRole('commenter') && !canEditRole(undefined), true);

// Same container.
assert.equal(isSameContainer({ space_id: 's1', project_id: '' }, { space_id: 's1' }), true);
assert.equal(isSameContainer({ space_id: 's1' }, { space_id: 's1', project_id: '' }), true);
assert.equal(isSameContainer({ space_id: 's1', project_id: 'a' }, { space_id: 's1', project_id: 'a' }), true);
assert.equal(isSameContainer({ space_id: 's1', project_id: 'a' }, { space_id: 's1', project_id: 'b' }), false);
assert.equal(isSameContainer({ space_id: 's1' }, { space_id: 's2' }), false);
console.log('Move checks passed: plural forms, roles, access changes, summaries, eligible targets, subtree exclusion, same-container test.');
