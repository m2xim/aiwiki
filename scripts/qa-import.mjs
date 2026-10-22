import assert from 'node:assert/strict';
import { parseFrontMatter, deriveTitle, analyzeMarkdown, rewriteImageUrls, normalizeText } from '../web/src/markdownImportCore.ts';

// --- front matter ---
let fm = parseFrontMatter('---\ntitle: "Мой: заголовок"\nsummary: Кратко\ntags:\n  - a\n  - "b c"\nauthor: Иван\ndraft: true\n---\n# Тело\n');
assert.equal(fm.found, true);
assert.equal(fm.data.title, 'Мой: заголовок');
assert.equal(fm.data.summary, 'Кратко');
assert.deepEqual(fm.data.tags, ['a', 'b c']);
assert.deepEqual(fm.ignoredFields, ['author', 'draft']);
assert.equal(fm.body, '# Тело\n');
fm = parseFrontMatter('---\ntags:\n- x\n- y\n---\nbody');
assert.deepEqual(fm.data.tags, ['x', 'y']);
assert.equal(fm.body, 'body');
fm = parseFrontMatter('---\ntags: [a, "b, c", d]\n---\nT');
assert.deepEqual(fm.data.tags, ['a', 'b, c', 'd']);
fm = parseFrontMatter('---\ntags: один, два,три\n---\nT');
assert.deepEqual(fm.data.tags, ['один', 'два', 'три']);
fm = parseFrontMatter('---\ntitle: x\n# no close\nбез закрытия');
assert.equal(fm.found, false);
assert.equal(fm.body.startsWith('---\n'), true);
assert.equal(fm.warnings.length, 1);
fm = parseFrontMatter('# Без front matter\n---\ntext');
assert.equal(fm.found, false); assert.equal(fm.warnings.length, 0);
fm = parseFrontMatter('---\ntitle: a\nweird line\nsummary: |\n  multi\n---\nb');
assert.equal(fm.data.title, 'a'); assert.equal(fm.data.summary, undefined);
assert.ok(fm.warnings.length >= 2);
assert.equal(normalizeText('﻿a\r\nb\rc'), 'a\nb\nc');

// --- title ---
assert.equal(deriveTitle('```\n# not a title\n```\n\n# Настоящий *заголовок*\n', 'f.md'), 'Настоящий заголовок');
assert.equal(deriveTitle('text only', 'путь/Моя заметка.md'), 'Моя заметка');
assert.equal(deriveTitle('```\n# code\n```', 'note.markdown'), 'note');
assert.equal(deriveTitle('# H1', 'f.md', ' Из FM '), 'Из FM');

// --- images ---
const body = [
  '![a](img/one.png) ![b](./two.png) ![c](my%20pic.png)',
  '![d](dup.png) ![e](sub/uniq.jpg) ![f](nope.png)',
  '![g](https://x.org/a.png) ![h](data:image/png;base64,AAAA) ![i](<with space.png> "t")',
].join('\n');
const names = ['one.png', 'two.png', 'my pic.png', 'dup.png', 'other/dup.png', 'uniq.jpg', 'with space.png'];
const a = analyzeMarkdown(body, names);
const by = Object.fromEntries(a.images.map(i => [i.alt, i]));
assert.equal(by.a.status, 'local'); assert.equal(by.a.fileIndex, 0);
assert.equal(by.b.status, 'local'); assert.equal(by.b.fileIndex, 1);
assert.equal(by.c.status, 'local'); assert.equal(by.c.fileIndex, 2);
assert.equal(by.d.status, 'local'); assert.equal(by.d.fileIndex, 3, 'exact path wins over basename');
assert.equal(by.e.status, 'local'); assert.equal(by.e.fileIndex, 5);
assert.equal(by.f.status, 'missing');
assert.equal(by.g.status, 'external'); assert.equal(by.h.status, 'data');
assert.equal(by.i.status, 'local'); assert.equal(by.i.fileIndex, 6);
for (const i of a.images) if (i.start >= 0) assert.equal(body.slice(i.start, i.end), i.url === 'my%20pic.png' ? 'my%20pic.png' : body.slice(i.start, i.end));
assert.equal(body.slice(by.a.start, by.a.end), 'img/one.png');
assert.equal(body.slice(by.i.start, by.i.end), 'with space.png');
assert.equal(body.slice(by.h.start, by.h.end), 'data:image/png;base64,AAAA');
const kinds = k => a.warnings.filter(w => w.kind === k).length;
assert.equal(kinds('image-missing'), 1); assert.equal(kinds('image-external'), 1); assert.equal(kinds('image-data'), 1);
// ambiguous basename (no exact path match)
const amb = analyzeMarkdown('![x](pics/dup.png)', ['a/dup.png', 'b/dup.png']);
assert.equal(amb.images[0].status, 'ambiguous'); assert.equal(amb.warnings[0].kind, 'image-ambiguous');
// reference-style image
const ref = '![r][id]\n\n[id]: pics/ref.png "title"\n';
const ra = analyzeMarkdown(ref, ['ref.png']);
assert.equal(ra.images[0].status, 'local'); assert.equal(ref.slice(ra.images[0].start, ra.images[0].end), 'pics/ref.png');
// definition with escapes / entities: the raw source text is located, not the decoded url
const esc = '![r][id]\n\n[id]: my\_pic.png "t"\n';
const ea = analyzeMarkdown(esc, ['my_pic.png']);
assert.equal(ea.images[0].status, 'local'); assert.equal(esc.slice(ea.images[0].start, ea.images[0].end), 'my\_pic.png');
const ent = '![r][id]\n\n[id]: a&amp;b.png\n';
const na = analyzeMarkdown(ent, ['a&b.png']);
assert.equal(na.images[0].status, 'local'); assert.equal(ent.slice(na.images[0].start, na.images[0].end), 'a&amp;b.png');
const ang = '![r][id]\n\n[id]: <my pic.png> "t"\n';
const ga = analyzeMarkdown(ang, ['my pic.png']);
assert.equal(ang.slice(ga.images[0].start, ga.images[0].end), 'my pic.png');
// image inside code is ignored
assert.equal(analyzeMarkdown('`![a](x.png)`\n\n```\n![a](y.png)\n```', []).images.length, 0);

// --- links, html, wikilinks ---
const w = analyzeMarkdown([
  '[ok](https://a.b) [m](mailto:a@b.c) [h](#якорь) [w](/?article=123#x) <https://auto.link>',
  '[rel](other.md) [up](../x) [js](javascript:alert(1))',
  '<div>raw</div>', '', '<!-- comment -->', '',
  'Текст [[Заметка]] и ![[pic.png]]',
  '`[[in code]]`', '', '```', '[[in fence]] <b>x</b>', '```',
].join('\n'), []).warnings;
const k = kind => w.filter(x => x.kind === kind);
assert.equal(k('link-relative').length, 2);
assert.equal(k('link-scheme').length, 1);
assert.equal(k('html').length, 1);
assert.equal(k('wikilink').length, 2);
assert.equal(w.length, 6);

// --- rewrite with Cyrillic / emoji (offsets are UTF-16 code units) ---
const src = 'Привет 😀 ![а](каз%20.png) мир 🎉🎉 ![b](./b.png)\n![c](c.png) ![d](https://x/y.png)';
const an = analyzeMarkdown(src, ['каз .png', 'b.png', 'c.png']);
assert.deepEqual(an.images.map(i => i.status), ['local', 'local', 'local', 'external']);
assert.equal(src.slice(an.images[0].start, an.images[0].end), 'каз%20.png');
assert.equal(src.slice(an.images[1].start, an.images[1].end), './b.png');
const out = rewriteImageUrls(src, an.images.filter(i => i.status === 'local').map((i, n) => ({ start: i.start, end: i.end, url: `/files/id${n}` })));
assert.equal(out, 'Привет 😀 ![а](/files/id0) мир 🎉🎉 ![b](/files/id1)\n![c](/files/id2) ![d](https://x/y.png)');
assert.equal(rewriteImageUrls('abc', []), 'abc');
console.log('Markdown import checks passed: front matter, titles, image matching, warnings, UTF-16 offset rewriting.');
