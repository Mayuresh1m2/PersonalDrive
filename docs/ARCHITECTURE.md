# Architecture

```
 phones / laptops / browsers
        │  HTTPS on the LAN (private CA, pinned via pairing QR code), mDNS discovery
        ▼
 ┌──────────────────────────── pdrive serve ─────────────────────────────┐
 │  api ──► library service ──► storage (disk A, human-readable folders) │
 │              │   │                                                     │
 │              │   └──► metadata (SQLite on the laptop SSD)              │
 │              ▼                                                         │
 │          events bus ──► job queue ──► processors (thumbnails, EXIF;    │
 │              │                         AI later)                       │
 │              └──► change notifier ──► /sync/changes long-poll, /events │
 │  volume guard (disk markers) · TLS manager · health · trash/upload GC  │
 └────────────────────────────────────────────────────────────────────────┘
        │ scheduled, encrypted, deduplicated snapshots  (Phase 2)
        ▼
 disk B (local backup)  ─────►  S3 Deep Archive (offsite)
```

## Where things live

| Location | Config key | Contents | Backed up? |
| --- | --- | --- | --- |
| Laptop SSD | `paths.state` | `pdrive.db`, `secrets/`, `tls/`, `cache/thumbs/`, `staging/` (in-flight uploads) | DB, secrets and TLS: yes (Phase 2). Cache/staging: no |
| External disk A | `paths.library` | `users/<name>/…`, `shared/<space>/…`, `.trash/`, `.tmp/`, `.pdrive-volume` | Yes |
| External disk B | `backup.targets` | Backup repository (Phase 2) | — |
| S3 | `backup.targets` | Backup repository (Phase 2) | — |

## Server packages (`server/internal/…`)

| Package | Responsibility |
| --- | --- |
| `config` | YAML config, defaults, `PDRIVE_*` env overrides |
| `volume` | `.pdrive-volume` markers; the guard that makes the server read-only when disk A is missing or a different disk is mounted |
| `storage` | `FileStore`: atomic writes (temp → fsync → rename), trash, reflink/hardlink dedup |
| `metadata` | SQLite schema and queries: users, devices, spaces, files, change feed, uploads, jobs, processor results |
| `auth` | Argon2id passwords, HMAC access tokens, rotating device refresh tokens, pairing tokens |
| `library` | The core service: permissions, commit/conflict/version rules, on-disk dedup, move/trash/restore, reindex, import |
| `events` | In-process bus + change-sequence notifier |
| `jobs` | Durable SQLite job queue with retries and backoff |
| `processors` | Plugin interface + thumbnail and media-info processors (keyed by content hash) |
| `api` | HTTP handlers, auth middleware, rate limiting, metrics |
| `tlsutil` | Private CA + auto-reissued leaf for current hostnames/IPs |
| `discovery` | mDNS `_pdrive._tcp` advertisement |
| `health` | Disk space, SMART, job stats |
| `dupes` | Read-only duplicate scanner for existing folders |
| `app` | Wiring, background tasks, state-dir lock |

## Key invariants

1. **Files are never modified in place.** Every write lands in `.tmp/`, is fsynced and
   renamed into place. This makes crashes safe and hardlink dedup safe (an edit replaces
   one user's link, never the shared inode).
2. **Nothing is written unless disk A's marker matches.** Mount points exist even when the
   disk is unplugged; without the guard, uploads would silently fill the laptop SSD.
3. **The index can be rebuilt from disk** (`pdrive reindex`). Users, devices, spaces and
   processor results can't, so they go into every backup.
4. **Nothing is lost on conflict or overwrite.** Stale uploads become conflict copies;
   replaced versions and deletions go to the trash (30 days by default).
5. **File ids are never reused** (`AUTOINCREMENT`), so clients can key local state on them.
6. **One process mutates the library.** The server holds an exclusive lock on the state
   directory; `import` and `reindex` refuse to run while it is up.

## Sync protocol (summary)

- Initial sync: list folders, then `GET /sync/cursor`.
- Ongoing: `GET /sync/changes?cursor=N&wait=30` (long-poll) or the `/events` WebSocket as a
  wake-up signal.
- Upload: hash locally → `POST /uploads` (may complete instantly via dedup) → `PUT` parts →
  `POST …/complete`. Sessions are resumable for 24h.
- Phone camera backup uses `POST /sync/have` to skip content already on the server, and is
  upload-only (deleting on the phone never deletes on the server). Desktop folder sync is
  two-way using `base_version`.

See `docs/adr/` for the reasoning behind each decision.
