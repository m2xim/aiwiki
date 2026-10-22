import assert from 'node:assert/strict';
import { buildExportDocument, escapeHtml, exportFileName, EXPORT_CSS } from '../web/src/articleExportCore.ts';

// File names
assert.equal(exportFileName('Руководство по запуску', 3), 'Руководство по запуску-r3.html');
assert.equal(exportFileName('a/b\\c:d*e?f"g<h>i|j', 1), 'abcdefghij-r1.html');
assert.equal(exportFileName('x\u0000y\u001fz\u007f', 2), 'xyz-r2.html');
assert.equal(exportFileName('', 5), 'article-r5.html');
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

console.log('qa-export: ok');
