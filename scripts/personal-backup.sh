#!/bin/sh
# Backup of the personal mode: a pg_dump first, then a copy of the S3 objects
# (order matters, see docs/BACKUP.md). Run it on the host from the install
# directory (where compose.personal.yaml lives). Settings:
#   AIWIKI_COMPOSE_PROJECT  compose project name (-p); empty = compose default
#   AIWIKI_ENV_FILE         env file (default .env.personal)
#   AIWIKI_BACKUP_DIR       target directory (default ./backups/personal)
#   AIWIKI_BACKUP_KEEP      number of newest dumps to keep (default 14)
#   AIWIKI_BACKUP_UID/GID   user the backup container writes as (default: the
#                           invoking user, so the copy is not root-owned)
# DIR, KEEP and COMPOSE_PROJECT may also be set in the env file. Only those
# lines are read (the file is never sourced: it holds secrets); the shell
# environment wins over the file.
set -eu
umask 077

ENV_FILE=${AIWIKI_ENV_FILE:-.env.personal}

# env_value KEY: last "KEY=value" line of the env file, optional quotes removed.
env_value() {
  [ -f "$ENV_FILE" ] || return 0
  sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1 | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/"
}
if [ -z "${AIWIKI_BACKUP_DIR:-}" ]; then AIWIKI_BACKUP_DIR=$(env_value AIWIKI_BACKUP_DIR); fi
if [ -z "${AIWIKI_BACKUP_KEEP:-}" ]; then AIWIKI_BACKUP_KEEP=$(env_value AIWIKI_BACKUP_KEEP); fi
if [ -z "${AIWIKI_COMPOSE_PROJECT:-}" ]; then AIWIKI_COMPOSE_PROJECT=$(env_value AIWIKI_COMPOSE_PROJECT); fi

KEEP=${AIWIKI_BACKUP_KEEP:-14}
case $KEEP in '' | *[!0-9]* | 0) echo "AIWIKI_BACKUP_KEEP must be a positive number" >&2; exit 2 ;; esac

mkdir -p "${AIWIKI_BACKUP_DIR:-./backups/personal}"
# Absolute, so the compose volume refers to the same directory as this script.
DIR=$(cd "${AIWIKI_BACKUP_DIR:-./backups/personal}" && pwd)
export AIWIKI_BACKUP_DIR=$DIR
AIWIKI_BACKUP_UID=${AIWIKI_BACKUP_UID:-$(id -u)}
AIWIKI_BACKUP_GID=${AIWIKI_BACKUP_GID:-$(id -g)}
export AIWIKI_BACKUP_UID AIWIKI_BACKUP_GID
mkdir -p "$DIR/db" "$DIR/objects"

# One run at a time: flock where available (released when the script exits),
# else a lock directory.
if command -v flock >/dev/null 2>&1; then
  exec 9>"$DIR/.lock"
  flock -n 9 || { echo "another backup is running (lock $DIR/.lock)" >&2; exit 2; }
else
  mkdir "$DIR/.lock.d" 2>/dev/null || { echo "another backup is running (or remove a stale $DIR/.lock.d)" >&2; exit 2; }
  trap 'rmdir "$DIR/.lock.d"' EXIT
fi

dc() {
  if [ -n "${AIWIKI_COMPOSE_PROJECT:-}" ]; then
    docker compose -p "$AIWIKI_COMPOSE_PROJECT" --env-file "$ENV_FILE" -f compose.personal.yaml "$@"
  else
    docker compose --env-file "$ENV_FILE" -f compose.personal.yaml "$@"
  fi
}

TS=$(date -u +%Y%m%dT%H%M%SZ)
PART="$DIR/db/.aiwiki-$TS.dump.part"
FINAL="$DIR/db/aiwiki-$TS.dump"
if command -v flock >/dev/null 2>&1; then
  trap 'rm -f "$PART"' EXIT
else
  trap 'rm -f "$PART"; rmdir "$DIR/.lock.d"' EXIT
fi
trap 'exit 130' INT TERM

# 1. Database first: every row in the dump has its object in the copy below.
dc exec -T db pg_dump -U postgres -d aiwiki -Fc >"$PART"
[ -s "$PART" ] || { echo "pg_dump produced an empty file" >&2; exit 1; }
# pg_restore reads the whole archive (all data blocks), writing nothing.
dc exec -T db pg_restore -f /dev/null <"$PART" || { echo "dump is not a readable archive" >&2; exit 1; }
mv "$PART" "$FINAL"
SIZE=$(wc -c <"$FINAL" | tr -d ' ')

# 2. Then the objects (immutable, shared by all dumps; never deleted here).
# stdin closed: compose run would otherwise read the caller's input.
dc --profile backup run --rm backup </dev/null

# 3. Rotation only after both steps succeeded; names are read line by line, so
# spaces in the directory are fine (dump names themselves contain none).
ls -1r "$DIR"/db/aiwiki-*.dump | {
  i=0
  while IFS= read -r f; do
    i=$((i + 1))
    if [ "$i" -gt "$KEEP" ]; then rm -f -- "$f"; fi
  done
}

echo "backup ok: $TS dump=$(basename "$FINAL") ${SIZE} bytes; objects in $DIR/objects"
