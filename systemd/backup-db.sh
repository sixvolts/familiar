#!/usr/bin/env bash
# backup-db.sh — nightly custom-format dump of the Familiar database.
#
# Run by familiar-backup.service (see systemd/README.md). The database is
# the single source of truth (memories, wiki, notes, credentials), so a
# failed dump must fail the unit: nothing here swallows an error.
#
# Environment:
#   FAMILIAR_DB_DSN                  DSN to dump; default: local_dsn from
#                                    $FAMILIAR_HOME/gateway.toml
#   FAMILIAR_HOME                    default ~/.familiar
#   FAMILIAR_BACKUP_DIR              default $FAMILIAR_HOME/backups
#   FAMILIAR_BACKUP_RETENTION_DAYS   default 14
#
# Output: $FAMILIAR_BACKUP_DIR/familiar-YYYYMMDDTHHMMSSZ.dump (pg_dump -Fc),
# written to a temp file, checked with pg_restore --list, then renamed, so
# a dump that exists is a dump that restores.
#
# Restore (custom format → pg_restore, not psql):
#   pg_restore -l familiar-….dump                       # inspect, no changes
#   pg_restore -d "$DSN" --clean --if-exists --no-owner familiar-….dump

set -euo pipefail
umask 077

FAMILIAR_HOME="${FAMILIAR_HOME:-$HOME/.familiar}"
DIR="${FAMILIAR_BACKUP_DIR:-$FAMILIAR_HOME/backups}"
DAYS="${FAMILIAR_BACKUP_RETENTION_DAYS:-14}"

DSN="${FAMILIAR_DB_DSN:-}"
if [ -z "$DSN" ] && [ -f "$FAMILIAR_HOME/gateway.toml" ]; then
    DSN=$(sed -n 's/^[[:space:]]*local_dsn[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
        "$FAMILIAR_HOME/gateway.toml" | head -1)
fi
if [ -z "$DSN" ]; then
    echo "backup-db: no DSN (set FAMILIAR_DB_DSN or local_dsn in $FAMILIAR_HOME/gateway.toml)" >&2
    exit 1
fi

# Keep the password off the command line (visible to every local user in
# the process list): move it into PGPASSWORD and dump with the rest.
if [[ "$DSN" =~ ^(postgres(ql)?://)([^:@/]+):([^@]*)@(.*)$ ]]; then
    pw="${BASH_REMATCH[4]}"
    export PGPASSWORD
    PGPASSWORD=$(printf '%b' "${pw//%/\\x}")
    DSN="${BASH_REMATCH[1]}${BASH_REMATCH[3]}@${BASH_REMATCH[5]}"
fi

mkdir -p "$DIR"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
out="$DIR/familiar-$stamp.dump"
tmp="$out.partial"
trap 'rm -f "$tmp"' EXIT

pg_dump --format=custom --no-owner --file="$tmp" "$DSN"
# Count the table of contents rather than `| grep -q .`: grep exits on the
# first line, pg_restore dies of SIGPIPE, and pipefail fails a good dump.
entries=$(pg_restore --list "$tmp" | wc -l)
if [ "$entries" -eq 0 ]; then
    echo "backup-db: $tmp has an empty table of contents" >&2
    exit 1
fi
mv "$tmp" "$out"
echo "backup-db: wrote $out ($(wc -c < "$out" | tr -d ' ') bytes)"

find "$DIR" -maxdepth 1 -name 'familiar-*.dump' -type f -mtime +"$DAYS" -print -delete
