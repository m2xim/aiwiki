import assert from 'node:assert/strict';
import { lineDiff } from '../web/src/diff.ts';

const separated = lineDiff('a\nb\nkeep\nd\ne', 'a\nB\nkeep\nD\ne');
assert.equal(separated.additions, 2);
assert.equal(separated.removals, 2);
assert.ok(separated.lines.some(line => line.type === 'context' && line.text === 'keep'));
assert.deepEqual(lineDiff('', '').lines, []);
assert.equal(lineDiff('', 'Первая строка').additions, 1);
assert.equal(lineDiff('Последняя строка', '').removals, 1);
assert.equal(lineDiff('a\n', 'a').removals, 1);
assert.equal(lineDiff('a\n'.repeat(5000), 'b').supported, false);

// Every emitted edit script must reconstruct both originals, including repeated lines.
let seed = 42;
const random = () => (seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0) / 2 ** 32;
const doc = () => Array.from({length: Math.floor(random() * 30)}, () => ['а', 'b', 'same', '', '# Heading'][Math.floor(random() * 5)]).join('\n');
for (let index = 0; index < 200; index++) {
  const old = doc(), next = doc();
  const result = lineDiff(old, next, 10000);
  assert.equal(result.supported, true);
  if (old === next) { assert.equal(result.lines.length, 0); continue; }
  assert.equal(result.lines.filter(line => line.type !== 'add').map(line => line.text).join('\n'), old);
  assert.equal(result.lines.filter(line => line.type !== 'remove').map(line => line.text).join('\n'), next);
  assert.deepEqual(result.lines.filter(line => line.oldLine !== null).map(line => line.oldLine), Array.from({length: old ? old.split('\n').length : 0}, (_, n) => n + 1));
  assert.deepEqual(result.lines.filter(line => line.newLine !== null).map(line => line.newLine), Array.from({length: next ? next.split('\n').length : 0}, (_, n) => n + 1));
}
console.log('Diff checks passed: separated edits, empty documents, trailing newline, size guard, and 200 reproducible reconstruction cases.');
