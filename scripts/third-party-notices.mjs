import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const root = process.cwd();
const lines = [
  '# Third-Party Notices',
  '',
  '> Generated from dependencies installed on the current platform and the local Go module graph. Optional npm dependencies absent from this installation are listed separately; this inventory does not claim to cover them.',
  '',
];
const missing = [];

function readJson(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch { return null; }
}

function licenseText(directory) {
  let files;
  try { files = fs.readdirSync(directory, { withFileTypes: true }); } catch { return []; }
  return files.filter((entry) => entry.isFile() && /^(licen[sc]es?|copying|notice)([-_.].*)?$/i.test(entry.name))
    .sort((a, b) => a.name.localeCompare(b.name, 'en'))
    .map((entry) => ({ name: entry.name, text: fs.readFileSync(path.join(directory, entry.name), 'utf8') }))
    .filter((entry) => entry.text.length > 0);
}

function formatLicense(value) {
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value.map(formatLicense).filter(Boolean).join(' OR ');
  if (value && typeof value === 'object') return value.type || '';
  return '';
}

const lockFiles = [
  { file: path.join(root, 'package-lock.json'), base: root },
  { file: path.join(root, 'web', 'package-lock.json'), base: path.join(root, 'web') },
  { file: path.join(root, 'benchmarks', 'mcp', 'package-lock.json'), base: path.join(root, 'benchmarks', 'mcp') },
].map((entry) => ({ ...entry, data: readJson(entry.file) })).filter((entry) => entry.data?.packages);
const installedByPath = new Map();
const absentOptional = [];
for (const lock of lockFiles) {
  for (const [location, item] of Object.entries(lock.data.packages)) {
    if (!location) continue;
    const packagePart = location.slice(location.lastIndexOf('node_modules/') + 'node_modules/'.length);
    if (packagePart === location) continue;
    const inferredName = packagePart;
    const dir = path.resolve(lock.base, location);
    const pkgFile = path.join(dir, 'package.json');
    const exists = fs.existsSync(pkgFile);
    if (item.optional && !exists) absentOptional.push({ name: item.name || inferredName, version: item.version || 'unknown' });
    if (!exists) continue;
    const pkg = readJson(pkgFile) || {};
    const name = pkg.name || item.name || inferredName;
    const version = pkg.version || item.version || 'unknown';
    const files = licenseText(dir);
    if (!files.length && name === 'js-tiktoken' && version === '1.0.21') {
      const upstream = path.join(root, 'benchmarks', 'mcp', 'notices', 'js-tiktoken-1.0.21.LICENSE');
      if (fs.existsSync(upstream)) files.push({name: 'Upstream LICENSE (npm gitHead; omitted from package archive)', text: fs.readFileSync(upstream, 'utf8').trimEnd()});
    }
    installedByPath.set(`${name}\0${version}\0${dir}`, {
      name, version, license: formatLicense(pkg.license ?? item.license), files,
    });
  }
}
const npmPackages = [...installedByPath.values()].sort((a, b) => a.name.localeCompare(b.name, 'en') || a.version.localeCompare(b.version, 'en'));

lines.push('## npm packages installed for the current platform', '');
for (const pkg of npmPackages) {
  lines.push(`### ${pkg.name} ${pkg.version}`, '', `Declared license: ${pkg.license || 'not declared in package metadata'}`, '');
  if (!pkg.files.length) {
    lines.push('No root LICENSE, COPYING, or NOTICE text file was found.', '');
    missing.push(`npm ${pkg.name}@${pkg.version}`);
  }
  for (const file of pkg.files) lines.push(`#### ${file.name}`, '', '```text', file.text, '```', '');
}

const absentUnique = [...new Map(absentOptional.map((item) => [`${item.name}@${item.version}`, item])).values()]
  .sort((a, b) => a.name.localeCompare(b.name, 'en') || a.version.localeCompare(b.version, 'en'));
lines.push('## Optional npm packages absent from this platform installation', '');
if (!absentUnique.length) lines.push('None identified in package-lock.json.', '');
else for (const item of absentUnique) lines.push(`- [${item.name}@${item.version}](https://www.npmjs.com/package/${encodeURIComponent(item.name)}/v/${encodeURIComponent(item.version)}) — not installed here; license and notice text were not available locally.`, '');

const go = spawnSync('go', ['list', '-m', '-json', 'all'], { cwd: root, encoding: 'utf8', maxBuffer: 64 * 1024 * 1024, windowsHide: true });
if (go.error || go.status !== 0) throw new Error(`go list -m -json all failed: ${go.error?.message || go.stderr.trim()}`);
const modules = [];
const decoder = go.stdout;
let depth = 0, start = -1, inString = false, escaped = false;
for (let i = 0; i < decoder.length; i++) {
  const c = decoder[i];
  if (inString) { if (escaped) escaped = false; else if (c === '\\') escaped = true; else if (c === '"') inString = false; continue; }
  if (c === '"') inString = true;
  else if (c === '{') { if (depth++ === 0) start = i; }
  else if (c === '}' && --depth === 0) modules.push(JSON.parse(decoder.slice(start, i + 1)));
}
const goModules = modules.filter((mod) => !mod.Main).map((mod) => ({
  path: mod.Path,
  version: mod.Version || '(replacement)',
  dir: mod.Dir || mod.Replace?.Dir || '',
})).sort((a, b) => a.path.localeCompare(b.path, 'en') || a.version.localeCompare(b.version, 'en'));
lines.push('## Go modules in `go list -m -json all`', '');
for (const mod of goModules) {
  const files = mod.dir ? licenseText(mod.dir) : [];
  lines.push(`### ${mod.path} ${mod.version}`, '');
  if (!files.length) {
    lines.push('No root LICENSE, COPYING, or NOTICE text file was found.', '');
    missing.push(`Go ${mod.path}@${mod.version}`);
  }
  for (const file of files) lines.push(`#### ${file.name}`, '', '```text', file.text, '```', '');
}

lines.push('## Coverage summary', '', `- Installed npm packages inventoried: ${npmPackages.length}`, `- Go modules inventoried (excluding main module): ${goModules.length}`, `- Optional npm packages absent from this platform installation: ${absentUnique.length}`, `- Installed packages/modules without root license or notice text: ${missing.length}`, '');
if (missing.length) {
  lines.push('### Missing license or notice files', '');
  for (const item of missing.sort((a, b) => a.localeCompare(b, 'en'))) lines.push(`- ${item}`);
  lines.push('');
}
fs.writeFileSync(path.join(root, 'THIRD_PARTY_NOTICES.md'), `${lines.join('\n').replace(/\n+$/, '')}\n`, 'utf8');
const requestedChecks = Object.fromEntries(['@codemirror/', '@babel/', 'jsesc', 'caniuse'].map((needle) => {
  const matches = npmPackages.filter((pkg) => needle.endsWith('/') || needle === 'caniuse' ? pkg.name.startsWith(needle) : pkg.name === needle);
  return [needle, { packages: matches.length, licenseFiles: matches.reduce((count, pkg) => count + pkg.files.length, 0) }];
}));
console.log(JSON.stringify({ npmInstalled: npmPackages.length, goModules: goModules.length, optionalAbsent: absentUnique.length, requestedChecks, missing: missing.sort((a, b) => a.localeCompare(b, 'en')), outputBytes: fs.statSync(path.join(root, 'THIRD_PARTY_NOTICES.md')).size }));
