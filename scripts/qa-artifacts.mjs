import assert from 'node:assert/strict';
import {
  LIMITS, FRAME_SANDBOX, base64Bytes, buildTree, buildUpdate, checkNewPath, clampFrameHeight, contentBytes, diffFiles, draftFromManifest, extensionOf,
  formatSize, parseArtRoute, artRouteUrl, parseResizeMessage, publicLinksDisabledHint, refreshDelay, shareLabel, textBytes, validateDraft, validatePath, visibilityLabel,
} from '../web/src/artifactsCore.ts';

// ---- Paths and extensions.
for (const ok of ['index.html', 'a/b.js', 'a/b/c/d.css', 'img/Logo-1_x.PNG', 'data.json', 'x.y.z.txt']) assert.equal(validatePath(ok), null, ok);
for (const bad of ['', '/index.html', 'a//b.js', '../x.js', 'a/../b.js', 'a..b.js', '.hidden.js', '-x.js', 'a b.js', 'файл.js', 'a/b/c/d/e.js', 'run.exe', 'noext', 'a/b.html/', `${'a'.repeat(200)}.js`]) assert.notEqual(validatePath(bad), null, bad);
assert.equal(validatePath('a/b/c/d.js'), null);
assert.equal(validatePath(`${'a'.repeat(196)}.css`), null); // exactly 200 bytes
assert.equal(extensionOf('a/b/File.JPEG'), 'jpeg');
assert.equal(extensionOf('a.b/readme'), '');

// ---- Sizes.
assert.equal(textBytes('abc'), 3);
assert.equal(textBytes('привет'), 12);
assert.equal(textBytes('😀'), 4);
assert.equal(base64Bytes('QQ=='), 1);
assert.equal(base64Bytes('QUI='), 2);
assert.equal(base64Bytes('QUJD'), 3);
assert.equal(base64Bytes(''), 0);
assert.equal(contentBytes('QUJD', 'base64'), 3);
assert.equal(formatSize(0), '0 Б');
assert.equal(formatSize(1023), '1023 Б');
assert.equal(formatSize(1536), '1,5 КБ');
assert.equal(formatSize(20 * 1024), '20 КБ');
assert.equal(formatSize(5 * 1024 * 1024), '5 МБ');
assert.equal(formatSize(-1), '—');

// ---- Draft validation.
const file = (path, size = 10) => ({ path, size });
assert.deepEqual(validateDraft([file('index.html')]), []);
assert.ok(validateDraft([file('a.js')]).some(m => m.includes('index.html')), 'entry point is required');
assert.ok(validateDraft([file('index.html'), file('index.html')]).some(m => m.includes('уже есть')));
assert.ok(validateDraft([file('index.html'), file('big.png', LIMITS.maxFileBytes + 1)]).some(m => m.includes('на файл')));
assert.ok(validateDraft([file('index.html'), ...Array.from({ length: 5 }, (_, i) => file(`f${i}.png`, LIMITS.maxFileBytes))]).some(m => m.includes('на версию')));
assert.deepEqual(validateDraft([file('index.html'), file('a.png', LIMITS.maxFileBytes), file('b.png', LIMITS.maxFileBytes), file('c.png', LIMITS.maxFileBytes), file('d.png', LIMITS.maxFileBytes - 10)]), []);
assert.ok(validateDraft(Array.from({ length: LIMITS.maxFiles + 1 }, (_, i) => file(i ? `f${i}.js` : 'index.html'))).some(m => m.includes('Файлов больше')));
assert.deepEqual(validateDraft(Array.from({ length: LIMITS.maxFiles }, (_, i) => file(i ? `f${i}.js` : 'index.html'))), []);
assert.ok(validateDraft([file('index.html'), file('a'), file('a/b.js')]).length > 0, 'file and folder with the same name');
assert.ok(validateDraft([file('index.html'), file('a.js'), file('a.js/b.js')]).some(m => m.includes('папк')));

// ---- Adding or renaming a single file.
const existing = [{ path: 'index.html' }, { path: 'app/main.js' }];
assert.equal(checkNewPath('style.css', existing), null);
assert.match(checkNewPath('index.html', existing), /уже есть/);
assert.match(checkNewPath('app/main.js/x.js', existing), /папк/);
assert.match(checkNewPath('app', existing), /папк|Расширение/);
assert.equal(checkNewPath('app/main.js', existing, 'app/main.js'), null, 'renaming onto itself');
assert.equal(checkNewPath('app/other.js', existing, 'app/main.js'), null);
assert.match(checkNewPath('x.exe', existing), /Расширение/);
assert.match(checkNewPath('n.js', Array.from({ length: LIMITS.maxFiles }, (_, i) => ({ path: `f${i}.js` }))), /Файлов не больше/);

// ---- Tree.
const tree = buildTree([{ path: 'index.html', size: 5 }, { path: 'js/a.js', size: 3 }, { path: 'js/lib/b.js', size: 4 }, { path: 'css/s.css', size: 1 }, { path: 'Zed.txt', size: 2 }]);
assert.deepEqual(tree.map(n => `${n.type}:${n.name}`), ['dir:css', 'dir:js', 'file:index.html', 'file:Zed.txt']);
const js = tree.find(n => n.name === 'js');
assert.equal(js.size, 7);
assert.deepEqual(js.children.map(n => `${n.type}:${n.name}`), ['dir:lib', 'file:a.js']);
assert.equal(js.children[0].children[0].path, 'js/lib/b.js');
assert.deepEqual(buildTree([]), []);

// ---- Diff for PUT.
const manifest = [{ path: 'index.html', size: 5, sha256: 'a', mime: 'text/html' }, { path: 'app.js', size: 5, sha256: 'b', mime: 'text/javascript' }, { path: 'logo.png', size: 100, sha256: 'c', mime: 'image/png' }];
let draft = draftFromManifest(manifest);
assert.deepEqual(draft.map(f => f.encoding), ['utf8', 'utf8', 'base64']);
assert.deepEqual(diffFiles(manifest, draft), [], 'nothing loaded = nothing changed');
draft = draft.map(f => f.path === 'index.html' ? { ...f, content: 'same', original: 'same' } : f);
assert.deepEqual(diffFiles(manifest, draft), [], 'loaded but untouched');
draft = draft.map(f => f.path === 'index.html' ? { ...f, content: 'edited', size: 6 } : f);
assert.deepEqual(diffFiles(manifest, draft), [{ path: 'index.html', content: 'edited', encoding: 'utf8' }]);
draft = draft.map(f => f.path === 'index.html' ? { ...f, content: 'same' } : f);
assert.deepEqual(diffFiles(manifest, draft), [], 'edit reverted');
const changes = diffFiles(manifest, [
  ...draft.filter(f => f.path === 'index.html'),
  { path: 'new/x.css', encoding: 'utf8', content: '', original: null, size: 0 },
  { path: 'logo.png', encoding: 'base64', content: 'QUJD', original: null, size: 3 },
]);
assert.deepEqual(changes.filter(c => c.delete), [{ path: 'app.js', delete: true }]);
assert.deepEqual(changes.filter(c => !c.delete).map(c => c.path).sort(), ['logo.png', 'new/x.css']);
// rename = delete + create
const renamed = diffFiles(manifest, draft.map(f => f.path === 'app.js' ? { ...f, path: 'js/app.js', content: 'code', original: null, size: 4 } : f));
assert.deepEqual(renamed, [{ path: 'app.js', delete: true }, { path: 'js/app.js', content: 'code', encoding: 'utf8' }]);

const base = { title: 'T', description: 'D', visibility: 'inherit', revision: 7, manifest };
const none = buildUpdate(base, { title: 'T ', description: 'D', visibility: 'inherit' }, draftFromManifest(manifest), '');
assert.equal(none.changed, false);
assert.deepEqual(none.body, { base_revision: 7 });
const meta = buildUpdate(base, { title: 'New', description: 'D', visibility: 'private' }, draftFromManifest(manifest), ' fix ');
assert.equal(meta.changed, true);
assert.deepEqual(meta.body, { base_revision: 7, title: 'New', visibility: 'private', message: 'fix' });
const withFile = buildUpdate(base, base, draft.map(f => f.path === 'index.html' ? { ...f, content: 'x', size: 1 } : f), '');
assert.deepEqual(withFile.body, { base_revision: 7, files: [{ path: 'index.html', content: 'x', encoding: 'utf8' }] });

// ---- Frame: resize messages and sandbox.
assert.equal(FRAME_SANDBOX, 'allow-scripts allow-modals');
for (const forbidden of ['allow-same-origin', 'allow-top-navigation', 'allow-popups', 'allow-forms']) assert.ok(!FRAME_SANDBOX.includes(forbidden));
assert.equal(clampFrameHeight(10), 120);
assert.equal(clampFrameHeight(99999), 4000);
assert.equal(clampFrameHeight(500.4), 500);
assert.equal(clampFrameHeight(120), 120);
assert.equal(clampFrameHeight(4000), 4000);
const frameWindow = {}; const other = {};
const msg = (source, data) => ({ source, data });
assert.equal(parseResizeMessage(msg(frameWindow, { type: 'aiwiki:resize', height: 300 }), frameWindow), 300);
assert.equal(parseResizeMessage(msg(frameWindow, { type: 'aiwiki:resize', height: 1 }), frameWindow), 120);
assert.equal(parseResizeMessage(msg(frameWindow, { type: 'aiwiki:resize', height: 1e9 }), frameWindow), 4000);
assert.equal(parseResizeMessage(msg(other, { type: 'aiwiki:resize', height: 300 }), frameWindow), null, 'foreign source');
assert.equal(parseResizeMessage(msg(null, { type: 'aiwiki:resize', height: 300 }), frameWindow), null);
assert.equal(parseResizeMessage(msg(frameWindow, { type: 'aiwiki:resize', height: 300 }), null), null, 'no frame');
assert.equal(parseResizeMessage(msg(undefined, { type: 'aiwiki:resize', height: 300 }), undefined), null);
for (const data of [null, 'aiwiki:resize', 5, {}, { type: 'other', height: 300 }, { type: 'aiwiki:resize' }, { type: 'aiwiki:resize', height: '300' }, { type: 'aiwiki:resize', height: NaN }, { type: 'aiwiki:resize', height: Infinity }, { type: 'aiwiki:resize', height: null }]) {
  assert.equal(parseResizeMessage(msg(frameWindow, data), frameWindow), null, JSON.stringify(data));
}

// ---- Capability renewal.
const now = Date.parse('2026-10-22T12:00:00Z');
assert.equal(refreshDelay('2026-10-22T12:15:00Z', now), 14 * 60 * 1000);
assert.equal(refreshDelay('2026-10-22T12:00:30Z', now), 5000, 'about to expire: not a busy loop');
assert.equal(refreshDelay('2026-10-22T11:00:00Z', now), 5000);
assert.equal(refreshDelay('garbage', now), 5 * 60 * 1000);
assert.ok(refreshDelay('2099-01-01T00:00:00Z', now) <= 2 ** 31 - 1);

// ---- Routes.
assert.equal(parseArtRoute(''), null);
assert.equal(parseArtRoute('?article=x'), null);
assert.deepEqual(parseArtRoute('?view=artifacts&space=s1&q=%D1%82%D0%B5%D1%81%D1%82&project=p'), { kind: 'list', space: 's1', q: 'тест', project: 'p' });
assert.deepEqual(parseArtRoute('?view=artifact-new&space=s1'), { kind: 'new', space: 's1', project: '' });
assert.deepEqual(parseArtRoute('?artifact=a1'), { kind: 'page', id: 'a1', mode: 'view', revision: null });
assert.deepEqual(parseArtRoute('?artifact=a1&view=edit'), { kind: 'page', id: 'a1', mode: 'edit', revision: null });
assert.deepEqual(parseArtRoute('?artifact=a1&view=history&revision=3'), { kind: 'page', id: 'a1', mode: 'history', revision: 3 });
assert.deepEqual(parseArtRoute('?artifact=a1&view=history&revision=-3'), { kind: 'page', id: 'a1', mode: 'history', revision: null });
for (const route of [
  { kind: 'list', space: 's 1', q: 'а&б=в', project: '' }, { kind: 'list', space: '', q: '', project: 'p' }, { kind: 'new', space: 's', project: 'p' },
  { kind: 'page', id: 'a/1', mode: 'view', revision: null }, { kind: 'page', id: 'a1', mode: 'history', revision: 4 }, { kind: 'page', id: 'a1', mode: 'edit', revision: null },
]) assert.deepEqual(parseArtRoute(new URL(artRouteUrl(route), 'http://x').search), route, artRouteUrl(route));
assert.equal(artRouteUrl({ kind: 'page', id: 'a1', mode: 'view', revision: null }), '/?artifact=a1');

// ---- Labels.
assert.equal(visibilityLabel('inherit'), 'Видно участникам пространства');
assert.equal(visibilityLabel('private'), 'Только мне');
assert.equal(shareLabel(null), 'последняя версия');
assert.equal(shareLabel(4), 'версия 4');
assert.ok(publicLinksDisabledHint('public links are disabled'));
assert.ok(publicLinksDisabledHint('Публичные ссылки отключены'));
assert.equal(publicLinksDisabledHint('500 Internal Server Error'), null);

console.log('Artifact checks passed: paths, sizes, limits, tree, PUT diff, resize messages, capability renewal, routes.');
