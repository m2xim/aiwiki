import assert from 'node:assert/strict';
import { canEditRole, containerRole, countArticles, countArtifacts, countUsersOf, countUsers, describeAccessChange, eligibleTargets, plural, roleLabel, summarizePreview } from '../web/src/moveCore.ts';

// Plural forms, including the 11-14 exception and 21/101.
const forms = n => plural(n, 'статья', 'статьи', 'статей');
for (const [n, word] of [[0, 'статей'], [1, 'статья'], [2, 'статьи'], [4, 'статьи'], [5, 'статей'], [11, 'статей'], [12, 'статей'], [14, 'статей'], [21, 'статья'], [22, 'статьи'], [25, 'статей'], [101, 'статья'], [111, 'статей'], [112, 'статей']]) assert.equal(forms(n), word, `plural ${n}`);
assert.equal(countArticles(21), '21 статья');
assert.equal(countArtifacts(1), '1 артефакт');
assert.equal(countArtifacts(3), '3 артефакта');
assert.equal(countArtifacts(11), '11 артефактов');
assert.equal(countArtifacts(21), '21 артефакт');
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
assert.deepEqual(summarizePreview({ articles: 1, users_gaining: 0, users_losing: 0 }), ['Будет перемещено: 1 статья', 'Права доступа не изменятся']);
assert.deepEqual(summarizePreview({ articles: 12, users_gaining: 3, users_losing: 5 }), ['Будет перемещено: 12 статей', 'Права расширятся у: 3 пользователей', 'Права сузятся или пропадут у: 5 пользователей']);
assert.deepEqual(summarizePreview({ articles: 2, users_gaining: 1, users_losing: 0 }), ['Будет перемещено: 2 статьи', 'Права расширятся у: 1 пользователя']);
assert.equal(countUsersOf(21), '21 пользователя');
assert.equal(countUsersOf(11), '11 пользователей');
assert.equal(countUsersOf(2), '2 пользователей');
assert.equal(summarizePreview({ articles: 21, users_gaining: 1, users_losing: 0 })[0], 'Будет перемещено: 21 статья');
assert.deepEqual(summarizePreview({ articles: 3, users_gaining: 0, users_losing: 11 }).slice(1), ['Права сузятся или пропадут у: 11 пользователей']);
assert.equal(summarizePreview({ articles: 21, artifacts: 3, users_gaining: 0, users_losing: 0 })[0], 'Будет перемещено: 21 статья, 3 артефакта');
assert.equal(summarizePreview({ articles: 0, artifacts: 5, users_gaining: 0, users_losing: 0 })[0], 'Будет перемещено: 0 статей, 5 артефактов');
assert.equal(summarizePreview({ articles: 2, artifacts: 0, users_gaining: 0, users_losing: 0 })[0], 'Будет перемещено: 2 статьи');

// Eligible targets: managed, not archived, not limited.
const catalog = [
  { id: 's1', name: 'S1', effective_role: 'manager' },
  { id: 's2', name: 'S2', effective_role: 'editor' },
  { id: 's3', name: 'S3', effective_role: 'owner', limited: true },
  { id: 's4', name: 'S4', effective_role: 'owner', archived_at: 'x' },
  { id: 's5', name: 'S5', effective_role: 'owner' },
];
assert.deepEqual(eligibleTargets(catalog).map(t => t.space.id), ['s1', 's5']);

// Container roles.
assert.equal(containerRole(catalog, { space_id: 's1' }), 'manager');
assert.equal(containerRole(catalog, { space_id: 'nope' }), undefined);
assert.equal(canEditRole('editor') && canEditRole('owner') && !canEditRole('commenter') && !canEditRole(undefined), true);
console.log('Move checks passed: plural forms, roles, access changes, summaries, eligible targets.');
