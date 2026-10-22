import assert from 'node:assert/strict';
import { toggleWrap, toggleLines, makeLink, codeBlock, insertTable, insertRule } from '../web/src/markdownFormat.ts';

const apply = (doc, e) => doc.slice(0, e.from) + e.insert + doc.slice(e.to);
const run = (doc, from, to, fn, ...a) => { const e = fn(doc, from, to, ...a); const t = apply(doc, e); return [t, t.slice(e.anchor, e.head), e]; };

// wrap / unwrap
let [t, s, e] = run('привет мир', 0, 6, toggleWrap, '**');
assert.equal(t, '**привет** мир'); assert.equal(s, 'привет');
[t, s] = run('**привет** мир', 2, 8, toggleWrap, '**');
assert.equal(t, 'привет мир'); assert.equal(s, 'привет');
[t] = run('**привет** мир', 0, 10, toggleWrap, '**'); // marks inside the selection
assert.equal(t, 'привет мир');
[t, s, e] = run('abc', 1, 1, toggleWrap, '`'); // empty selection
assert.equal(t, 'a``bc'); assert.equal(e.anchor, 2); assert.equal(e.head, 2);
[t] = run('**bold**', 2, 6, toggleWrap, '*'); assert.equal(t, '***bold***'); // italic is not half of bold
[t] = run('*it*', 1, 3, toggleWrap, '*'); assert.equal(t, 'it');
[t] = run('text', 0, 4, toggleWrap, '~~'); assert.equal(t, '~~text~~');
[t] = run('~~text~~', 2, 6, toggleWrap, '~~'); assert.equal(t, 'text');

// line prefixes
[t] = run('один\nдва\nтри', 0, 12, toggleLines, 'ul'); assert.equal(t, '- один\n- два\n- три');
[t] = run('- один\n- два', 0, 12, toggleLines, 'ul'); assert.equal(t, 'один\nдва');
[t] = run('один\n\nдва\nтри', 0, 13, toggleLines, 'ol'); assert.equal(t, '1. один\n\n2. два\n3. три');
[t] = run('1. а\n2. б', 0, 8, toggleLines, 'ol'); assert.equal(t, 'а\nб');
[t] = run('- а\n- б', 0, 7, toggleLines, 'ol'); assert.equal(t, '1. а\n2. б');
[t, s, e] = run('строка', 3, 3, toggleLines, 'h2'); assert.equal(t, '## строка'); assert.equal(e.anchor, 6);
[t] = run('# строка', 3, 3, toggleLines, 'h2'); assert.equal(t, '## строка');
[t] = run('## строка', 3, 3, toggleLines, 'h2'); assert.equal(t, 'строка');
[t] = run('а\nб', 0, 3, toggleLines, 'task'); assert.equal(t, '- [ ] а\n- [ ] б');
[t] = run('- [x] а', 0, 7, toggleLines, 'task'); assert.equal(t, 'а');
[t] = run('- [ ] а', 0, 7, toggleLines, 'ul'); assert.equal(t, '- а');
[t] = run('а\nб', 0, 3, toggleLines, 'quote'); assert.equal(t, '> а\n> б');
[t] = run('> а\n> б', 0, 7, toggleLines, 'quote'); assert.equal(t, 'а\nб');
[t] = run('- а', 0, 3, toggleLines, 'quote'); assert.equal(t, '> - а');
[t] = run('а\nб\n', 0, 2, toggleLines, 'ul'); assert.equal(t, '- а\nб\n'); // selection ends at a line start
[t] = run('', 0, 0, toggleLines, 'ul'); assert.equal(t, '- ');

// link
[t, s] = run('смотри сюда', 7, 11, makeLink); assert.equal(t, 'смотри [сюда](url)'); assert.equal(s, 'url');
[t, s] = run('', 0, 0, makeLink); assert.equal(t, '[текст](url)'); assert.equal(s, 'url');
[t] = run('https://a.b/c', 0, 13, makeLink); assert.equal(t, '[](https://a.b/c)');

// code block
[t, s] = run('код', 0, 3, codeBlock); assert.equal(t, '```\nкод\n```'); assert.equal(s, 'код');
[t] = run('a код b', 2, 5, codeBlock); assert.equal(t, 'a \n```\nкод\n```\n b');
[t] = run('', 0, 0, codeBlock); assert.equal(t, '```\n\n```');

// table / rule
[t, s] = run('', 0, 0, insertTable);
assert.ok(t.startsWith('| Заголовок 1 | Заголовок 2 |\n| --- | --- |\n')); assert.equal(s, 'Заголовок 1');
[t] = run('текст', 5, 5, insertTable); assert.ok(t.startsWith('текст\n\n| Заголовок'));
[t] = run('', 0, 0, insertRule); assert.equal(t, '---\n');
[t] = run('текст', 5, 5, insertRule); assert.equal(t, 'текст\n\n---\n');

console.log('markdown-format ok');
