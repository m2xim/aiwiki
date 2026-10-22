// Backup and restore drill. Starts its own disposable PostgreSQL and SeaweedFS
// containers (names aiwiki-drill-*), then runs the Go test
// TestBackupDrillPostgresS3, which builds a workload, takes a pg_dump plus an
// object copy, destroys the database and the bucket contents, restores,
// starts the server on the restored data and verifies it through the HTTP API.
// Containers are removed at the end (also on failure).
//
// Usage: node scripts/backup-drill.mjs
// Optional: AIWIKI_DRILL_PG_PORT (default 55440), AIWIKI_DRILL_S3_PORT (default 39100),
//           AIWIKI_DRILL_KEEP=1 keeps the containers for inspection.
// Requires podman and Go. It never touches containers it did not create.
import { spawnSync } from 'node:child_process'
import { mkdirSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const work = join(root, '.cache', 'drill')
const pgPort = process.env.AIWIKI_DRILL_PG_PORT || '55440'
const s3Port = process.env.AIWIKI_DRILL_S3_PORT || '39100'
const PG = 'aiwiki-drill-pg'
const S3 = 'aiwiki-drill-s3'
const PG_IMAGE = 'docker.io/library/postgres:16.15-alpine'
const S3_IMAGE = 'docker.io/chrislusf/seaweedfs:4.48'
const BUCKET = 'aiwiki-drill'
// Test-only values for disposable containers.
const S3_KEY = 'drill-access'
const S3_SECRET = 'drill-secret-only-local'
const env = { ...process.env, MSYS_NO_PATHCONV: '1' }

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { encoding: 'utf8', env, ...opts })
  if (r.error) throw r.error
  if (r.status !== 0 && !opts.allowFail) {
    throw new Error(`${cmd} ${args.slice(0, 3).join(' ')} failed (${r.status}):\n${r.stdout || ''}${r.stderr || ''}`)
  }
  return r
}
const podman = (args, opts) => run('podman', args, opts)
const log = (m) => console.log(`[drill] ${m}`)

function removeContainers() {
  for (const name of [PG, S3]) podman(['rm', '-f', name], { allowFail: true })
}

async function waitFor(what, probe, seconds = 60) {
  for (let i = 0; i < seconds; i++) {
    if (await probe()) return
    await new Promise((r) => setTimeout(r, 1000))
  }
  throw new Error(`timeout waiting for ${what}`)
}

let code = 1
try {
  // Refuse to run over something that is not ours.
  for (const name of [PG, S3]) {
    const r = podman(['container', 'exists', name], { allowFail: true })
    if (r.status === 0) throw new Error(`container ${name} already exists; remove it first (podman rm -f ${name})`)
  }
  mkdirSync(work, { recursive: true })
  const s3json = join(work, 's3.json')
  writeFileSync(s3json, JSON.stringify({ identities: [{ name: 'drill', credentials: [{ accessKey: S3_KEY, secretKey: S3_SECRET }], actions: ['Admin', 'Read', 'Write', 'List', 'Tagging'] }] }))

  log(`starting PostgreSQL on 127.0.0.1:${pgPort}`)
  podman(['run', '-d', '--name', PG, '-e', 'POSTGRES_PASSWORD=isolated-ci-only', '-e', 'POSTGRES_DB=aiwiki_ci', '-p', `127.0.0.1:${pgPort}:5432`, PG_IMAGE])
  log(`starting SeaweedFS S3 on 127.0.0.1:${s3Port}`)
  podman(['run', '-d', '--name', S3, '-p', `127.0.0.1:${s3Port}:9000`, '-v', `${s3json}:/etc/s3.json:ro`, S3_IMAGE,
    'server', '-ip=127.0.0.1', '-ip.bind=0.0.0.0', '-dir=/data', '-filer', '-s3', '-s3.port=9000', '-s3.config=/etc/s3.json',
    '-master.volumeSizeLimitMB=100', '-volume.max=3', '-master.telemetry=false'])

  await waitFor('PostgreSQL', () => podman(['exec', PG, 'pg_isready', '-U', 'postgres', '-d', 'aiwiki_ci'], { allowFail: true }).status === 0)
  // pg_isready can answer during the init restart; wait until a real query works.
  await waitFor('PostgreSQL query', () => podman(['exec', PG, 'psql', '-U', 'postgres', '-d', 'aiwiki_ci', '-tc', 'select 1'], { allowFail: true }).status === 0)
  // Roles as in CI, plus the backup role (docs/BACKUP.md).
  podman(['exec', PG, 'psql', '-U', 'postgres', '-d', 'aiwiki_ci', '-v', 'ON_ERROR_STOP=1', '-c',
    `CREATE ROLE aiwiki_owner LOGIN PASSWORD 'owner-ci-only' NOSUPERUSER NOBYPASSRLS;
     CREATE ROLE aiwiki_app LOGIN PASSWORD 'app-ci-only' NOSUPERUSER NOBYPASSRLS;
     CREATE ROLE aiwiki_backup LOGIN PASSWORD 'backup-ci-only' NOSUPERUSER BYPASSRLS;
     GRANT pg_read_all_data TO aiwiki_backup;
     ALTER DATABASE aiwiki_ci OWNER TO aiwiki_owner;
     GRANT ALL ON SCHEMA public TO aiwiki_owner;`])

  await waitFor('S3 endpoint', async () => { try { await fetch(`http://127.0.0.1:${s3Port}/`); return true } catch { return false } }, 90)
  await waitFor('bucket creation', () => {
    const r = podman(['exec', '-i', S3, 'weed', 'shell'], { allowFail: true, input: `s3.bucket.create -name ${BUCKET}\nexit\n` })
    return r.status === 0 && !/error|failed/i.test(r.stdout + r.stderr)
  }, 60)

  log('building aiwiki binary')
  const binary = join(work, process.platform === 'win32' ? 'aiwiki.exe' : 'aiwiki')
  run('go', ['build', '-o', binary, './cmd/aiwiki'], { cwd: root })

  const testEnv = {
    ...env,
    AIWIKI_DRILL_PG_OWNER: `postgres://aiwiki_owner:owner-ci-only@127.0.0.1:${pgPort}/aiwiki_ci?sslmode=disable`,
    AIWIKI_DRILL_PG_APP: `postgres://aiwiki_app:app-ci-only@127.0.0.1:${pgPort}/aiwiki_ci?sslmode=disable`,
    AIWIKI_DRILL_PG_BACKUP: `postgres://aiwiki_backup:backup-ci-only@127.0.0.1:${pgPort}/aiwiki_ci?sslmode=disable`,
    AIWIKI_DRILL_PG_ADMIN: `postgres://postgres:isolated-ci-only@127.0.0.1:${pgPort}/postgres?sslmode=disable`,
    AIWIKI_DRILL_PG_CONTAINER: PG,
    AIWIKI_DRILL_S3_ENDPOINT: `http://127.0.0.1:${s3Port}`,
    AIWIKI_DRILL_S3_BUCKET: BUCKET,
    AIWIKI_DRILL_BINARY: binary,
    AWS_ACCESS_KEY_ID: S3_KEY,
    AWS_SECRET_ACCESS_KEY: S3_SECRET,
    AWS_EC2_METADATA_DISABLED: 'true',
  }
  log('running the drill (go test)')
  const t = spawnSync('go', ['test', './internal/server', '-run', '^TestBackupDrillPostgresS3$', '-count=1', '-v', '-timeout', '10m'], { cwd: root, env: testEnv, stdio: 'inherit' })
  code = t.status ?? 1
  log(code === 0 ? 'DRILL PASSED' : 'DRILL FAILED')
} catch (err) {
  console.error(`[drill] ${err.message}`)
} finally {
  if (process.env.AIWIKI_DRILL_KEEP === '1') {
    log('AIWIKI_DRILL_KEEP=1: containers left running')
  } else {
    removeContainers()
    log('containers removed')
  }
}
process.exit(code)
