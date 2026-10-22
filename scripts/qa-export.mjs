import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { buildExportDocument, escapeHtml, exportFileName, EXPORT_CSS } from '../web/src/articleExportCore.ts';
import { markdownToPdfContent, parseMarkdown, buildPdfDocument, collectImageUrls, safeLinkUrl, classifyImageUrl } from '../web/src/articlePdf.ts';

// File names
assert.equal(exportFileName('Руководство по запуску', 3), 'Руководство по запуску-r3.html');
assert.equal(exportFileName('a/b\\c:d*e?f"g<h>i|j', 1), 'abcdefghij-r1.html');
assert.equal(exportFileName('x\u0000y\u001fz\u007f', 2), 'xyz-r2.html');
assert.equal(exportFileName('', 5), 'article-r5.html');
assert.equal(exportFileName('Статья', 7, 'pdf'), 'Статья-r7.pdf');
assert.equal(exportFileName('  /:*  ', 5), 'article-r5.html');
assert.equal(exportFileName('я'.repeat(200), 1), 'я'.repeat(80) + '-r1.html');
assert.equal([...exportFileName('😀'.repeat(100), 1)].length, 80 + '-r1.html'.length);

// Escaping
assert.equal(escapeHtml(`<script>"a" & 'b'</script>`), '&lt;script&gt;&quot;a&quot; &amp; &#39;b&#39;&lt;/script&gt;');

// Document
const doc = buildExportDocument({ title: `</title><script>alert(1)</script>"x"`, metaHtml: '<header>m</header>', bodyHtml: '<p>тело</p>', css: EXPORT_CSS });
assert.ok(doc.startsWith('<!doctype html>'));
assert.ok(doc.includes('<html lang="ru">'));
assert.ok(doc.includes('<meta charset="utf-8">'));
assert.ok(doc.includes('name="viewport"'));
assert.ok(doc.includes(`http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline'"`));
assert.ok(doc.includes('<title>&lt;/title&gt;&lt;script&gt;alert(1)&lt;/script&gt;&quot;x&quot;</title>'));
assert.ok(!/<script/i.test(doc), 'document must not contain <script');
assert.ok(doc.includes('<p>тело</p>'));
assert.ok(doc.includes('@media print') && doc.includes('@page{margin:16mm}') && doc.includes('white-space:pre-wrap'));
// A hostile stylesheet cannot close the style element early.
assert.ok(!/<\/style>[^]*<\/style>/i.test(buildExportDocument({ title: 't', metaHtml: '', bodyHtml: '', css: 'a{}</style><b>' })));

// ---- PDF: markdownToPdfContent (structure only, pdfmake is not run) ----
const ORIGIN = 'http://wiki.test';
const toPdf = (md, images = new Map()) => { const warnings = []; return { content: markdownToPdfContent(parseMarkdown(md), { origin: ORIGIN, images, warnings }), warnings }; };
const flat = (v) => typeof v === 'string' ? v : Array.isArray(v) ? v.map(flat).join('') : v && typeof v === 'object' ? flat(v.text ?? v.stack ?? v.ul ?? v.ol ?? v.table?.body ?? '') : '';

{ // headings
  const { content } = toPdf('# Один\n\n###### Шесть');
  assert.equal(content.length, 2);
  assert.equal(content[0].headlineLevel, 1); assert.equal(content[0].bold, true); assert.equal(flat(content[0]), 'Один');
  assert.ok(content[0].fontSize > content[1].fontSize); assert.equal(content[1].headlineLevel, 6);
}
{ // inline formatting
  const { content } = toPdf('a **b** *c* ~~d~~ `e`');
  const runs = content[0].text;
  assert.equal(runs.find(r => r.text === 'b').bold, true);
  assert.equal(runs.find(r => r.text === 'c').italics, true);
  assert.equal(runs.find(r => r.text === 'd').decoration, 'lineThrough');
  assert.ok(runs.find(r => r.text === 'e').background);
}
{ // lists, nested and ordered
  const { content } = toPdf('- один\n- два\n  - вложенный\n\n3. три\n4. четыре');
  assert.equal(content[0].ul.length, 2);
  assert.ok(content[0].ul[1].stack.some(b => b.ul), 'nested list inside item');
  assert.equal(content[1].ol.length, 2); assert.equal(content[1].start, 3);
}
{ // task list
  const { content } = toPdf('- [x] готово\n- [ ] нет');
  assert.equal(content[0].stack.length, 2);
  assert.ok(content[0].stack[0].columns[0].canvas.length > 1, 'checked box has a tick');
  assert.equal(content[0].stack[1].columns[0].canvas.length, 1);
}
{ // table
  const { content } = toPdf('| а | б |\n|:-:|---|\n| 1 | 2 |\n| 3 |');
  const t = content[0].table;
  assert.equal(t.headerRows, 1); assert.deepEqual(t.widths, ['*', '*']);
  assert.equal(t.body.length, 3); assert.ok(t.body.every(r => r.length === 2));
  assert.equal(t.body[0][0].bold, true); assert.equal(t.body[0][0].alignment, 'center');
  assert.ok(!t.dontBreakRows);
}
{ // code block: one row per line, long lines break anywhere
  const { content } = toPdf('```js\nconst a = 1;\n  indented\n```');
  const rows = content[0].table.body;
  assert.equal(rows.length, 2); assert.equal(rows[1][0].text, '  indented');
  assert.equal(rows[0][0].wordBreak, 'break-all'); assert.equal(rows[0][0].preserveLeadingSpaces, true);
}
{ // links
  const { content } = toPdf('[x](https://e.com/a) [bad](javascript:alert(1)) [in](/?article=ab%20c#sec) [anchor](#h) [m](mailto:a@b.c)');
  const runs = content[0].text.filter(r => r.text.trim());
  assert.equal(runs[0].link, 'https://e.com/a');
  assert.equal(runs[1].text, 'bad'); assert.equal(runs[1].link, undefined);
  assert.equal(runs[2].link, `${ORIGIN}/?article=ab%20c#sec`);
  assert.equal(runs[3].link, undefined);
  assert.equal(runs[4].link, 'mailto:a@b.c');
  assert.equal(safeLinkUrl('data:text/html,x', ORIGIN), null); assert.equal(safeLinkUrl(' vbscript:x', ORIGIN), null);
  assert.equal(safeLinkUrl('/?article=7', ORIGIN), `${ORIGIN}/?article=7`);
}
{ // raw HTML is shown as text and never interpreted
  const { content } = toPdf('<script>alert(1)</script>\n\nтекст <b>жирный</b>');
  assert.equal(content[0].text, '<script>alert(1)</script>');
  assert.ok(flat(content[1]).includes('<b>')); assert.ok(!JSON.stringify(content).includes('"link"'));
}
{ // quote, rule
  const { content } = toPdf('> цитата\n\n---');
  assert.ok(content[0].table && flat(content[0]).includes('цитата')); assert.ok(content[1].canvas);
}
{ // images: embedded, external, failed, missing
  const md = '![ok](/files/a)\n\n![ext](https://e.com/i.png)\n\n![bad](/files/b)\n\n![none](ftp://x/y)';
  assert.deepEqual(collectImageUrls(parseMarkdown(md)), ['/files/a', 'https://e.com/i.png', '/files/b', 'ftp://x/y']);
  const images = new Map([['/files/a', { dataUri: 'data:image/png;base64,AAAA', width: 2000, height: 100 }], ['https://e.com/i.png', { external: true }], ['/files/b', { error: 'ответ сервера 404' }]]);
  const { content, warnings } = toPdf(md, images);
  assert.ok(content[0].image && content[0].fit[0] <= 515.29 && content[0].fit[1] === 100, 'wide image is fitted to the text width');
  assert.ok(flat(content[1]).includes('[внешнее изображение: ext]') && content[1].text.some(r => r.link === 'https://e.com/i.png'));
  assert.ok(flat(content[2]).includes('[изображение не включено: bad]'));
  assert.ok(flat(content[3]).includes('[изображение не включено: none]'));
  assert.equal(warnings.length, 3);
  assert.equal(classifyImageUrl('data:image/svg+xml,x', ORIGIN).kind, 'invalid');
  assert.equal(classifyImageUrl('/files/x', ORIGIN).kind, 'file');
  assert.equal(classifyImageUrl('http://other/files/x', ORIGIN).kind, 'external');
}
{ // document definition: title logic, metadata, footer
  const art = { id: 'a1', title: 'Заголовок', revision: 4, updated_at: '2026-01-02T03:04:05Z', author: 'Анна', body: '# Заголовок\n\nтекст' };
  const own = buildPdfDocument(art, { origin: ORIGIN, images: new Map(), spaceName: 'Мой' }).docDefinition;
  assert.equal(own.info.title, 'Заголовок'); assert.equal(own.info.subject, 'AI Wiki'); assert.equal(own.info.creator, 'AI Wiki');
  assert.equal(own.pageSize, 'A4'); assert.equal(own.pageMargins[0], 40);
  const meta = flat(own.content[0]);
  assert.ok(meta.includes('Ревизия 4') && meta.includes('Автор: Анна') && meta.includes('Пространство: Мой'));
  assert.ok(JSON.stringify(own.content[0]).includes(`${ORIGIN}/?article=a1`));
  assert.equal(own.content.filter(n => n.headlineLevel === 1).length, 1, 'title is not repeated when the body starts with the same H1');
  assert.ok(JSON.stringify(own.footer(2, 5)).includes('стр. 2 из 5'));
  assert.equal(own.pageBreakBefore({ headlineLevel: 2 }, { getFollowingNodesOnPage: () => [] }), true);
  assert.equal(own.pageBreakBefore({ headlineLevel: 2 }, { getFollowingNodesOnPage: () => [{}] }), false);
  const other = buildPdfDocument({ ...art, body: 'просто текст' }, { origin: ORIGIN, images: new Map() }).docDefinition;
  assert.equal(other.content[0].text, 'Заголовок'); assert.equal(other.content[0].fontSize, 22);
}

// ---- PDF: real generation with pdfmake in Node (same browser build + vfs fonts as the app) ----
{
  const require = createRequire(new URL('../web/package.json', import.meta.url));
  const pdfMake = require('pdfmake/build/pdfmake');
  pdfMake.addVirtualFileSystem(require('pdfmake/build/vfs_fonts'));
  const rows = Array.from({ length: 80 }, (_, i) => `| строка ${i} | ${'очень длинный текст ячейки '.repeat(6)} |`).join('\n');
  const lines = Array.from({ length: 150 }, (_, i) => 'line ' + i).join('\n');
  const body = `# Руководство\n\nПривет, мир! Кириллица: ёЁ №.\n\n| Колонка | Описание |\n|---|---|\n${rows}\n\n> ${'цитата '.repeat(400)}\n\n\`\`\`\n${'x'.repeat(400)}\n${lines}\n\`\`\`\n\n## Конец\n\nфинал`;
  const { docDefinition } = buildPdfDocument({ id: '1', title: 'Руководство', revision: 1, updated_at: '2026-01-01T00:00:00Z', body }, { origin: ORIGIN, images: new Map() });
  const buf = await pdfMake.createPdf(docDefinition).getBuffer();
  assert.equal(buf.subarray(0, 5).toString('latin1'), '%PDF-');
  const text = buf.toString('latin1');
  assert.ok(/\/FontFile2/.test(text), 'TrueType font is embedded');
  assert.ok(/\/BaseFont\s*\/[A-Z]{6}\+Roboto/.test(text), 'Roboto subset is used');
  const pages = (text.match(/\/Type\s*\/Page\b/g) || []).length;
  assert.ok(pages >= 3, `multi-page document expected, got ${pages}`);
}

console.log('qa-export: ok');
