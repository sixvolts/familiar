# systemd units

Copy (or symlink) these into `~/.config/systemd/user/` (user units) or
`/etc/systemd/system/` (system units), then `systemctl daemon-reload`.

| Unit | Purpose |
|---|---|
| `familiar-embedder.service` | local embedding model (llama-server) |
| `familiar-sidecar.service`  | router + local inference model |
| `familiar-gateway.service`  | the Go gateway |
| `familiar-workspace.service`| the web workspace |
| `familiar-backup.service` + `.timer` | nightly `pg_dump` of the database |

## Backups

The database is the single source of truth (memories, wiki, notes,
credentials); nothing else backs it up. Enable the nightly dump:

```sh
systemctl enable --now familiar-backup.timer
systemctl start   familiar-backup.service   # run one now to verify
systemctl list-timers familiar-backup.timer # confirm next run
```

Dumps land in `~/.familiar/backups/` (override `FAMILIAR_BACKUP_DIR`),
pruned past 14 days (`FAMILIAR_BACKUP_RETENTION_DAYS`). The DSN comes
from `$FAMILIAR_DB_DSN` or `~/.familiar/gateway.toml`'s `local_dsn`.

**Restore** (custom-format dump → `pg_restore`, not `psql`):

```sh
pg_restore -l  familiar-YYYYMMDDTHHMMSSZ.dump          # inspect, no changes
pg_restore -d "$DSN" --clean --if-exists --no-owner \
           familiar-YYYYMMDDTHHMMSSZ.dump              # restore into an existing DB
```

Full details are in the header of `scripts/backup-db.sh`.
