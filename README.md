# PersonalDrive

A self-hosted, household replacement for Google Photos / Google Drive, built because commercial
cloud storage keeps getting more expensive while raw storage (local disks, S3 Deep Archive at
~$1/TB/month) stays cheap.

## Architecture

```
 Phones / laptops / browsers ──Wi-Fi──► Home server (pdrived) ──► Local backup disk ──► S3 Deep Archive
   auto-sync, multi-user                 primary copy on            versioned, deduped,   offsite copy,
                                         local disks                encrypted             same format
```

- **Local disks are the primary store** — fast, free, on your LAN. Files are kept in a normal
  human-readable folder layout, so nothing is locked in.
- **Backups are incremental, deduplicated, compressed and encrypted**, written on a schedule to a
  local backup disk and to S3.
- **One bootstrap script** rebuilds a server on a new machine from the backup.
- **Client apps** (`mobile_app`, `web_app`, `desktop_app`, `mac_app`, sharing `shared_lib`) sync
  to the home server and auto-backup phone photos.
- **Later**: local-only AI for searching photos, memories, and organising documents and bills.

The full spec and the agreed decisions are in
[`docs/prompts/01-home-server-and-s3-backup.md`](docs/prompts/01-home-server-and-s3-backup.md).

## Docs

- [Running the server on a laptop](docs/LAPTOP_SERVER.md)
- [Backups to disk B and S3](docs/BACKUP.md), and the [disaster recovery runbook](docs/RECOVERY.md)
- [Architecture](docs/ARCHITECTURE.md), [backup format](docs/BACKUP_FORMAT.md), [decisions (ADRs)](docs/adr/)
- API: [`shared_lib/api/openapi.yaml`](shared_lib/api/openapi.yaml)

## Project structure

| Path | Contents |
| --- | --- |
| `server/` | Go home server (`pdrived`) and admin/backup CLI (`pdrive`) |
| `shared_lib/` | OpenAPI spec and shared client code |
| `mobile_app/`, `web_app/`, `desktop_app/`, `mac_app/` | Client applications |
| `scripts/` | Bootstrap / restore-to-new-machine script |
| `deploy/` | systemd unit, docker-compose (MinIO for tests) |
| `docs/` | Architecture, backup format, recovery runbook, ADRs |

## Why S3 Deep Archive

Approximate AWS us-east-1 storage pricing for 100 GB (verify current prices):

| Storage class | $/GB-month | 100 GB / month | Notes |
| --- | --- | --- | --- |
| S3 Standard | 0.023 | $2.30 | Frequent access |
| S3 Standard-IA | 0.0125 | $1.25 | 30-day minimum |
| S3 Glacier Instant Retrieval | 0.004 | $0.40 | 90-day minimum |
| S3 Glacier Deep Archive | 0.00099 | $0.099 | 180-day minimum, restores take up to 48h |

Requests, retrievals and egress (~$0.09/GB) are extra — which is why the local disks are the
primary copy and S3 is only read in a disaster.
