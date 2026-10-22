# Build Prompt: PersonalDrive Home Server + S3 Backup

> This is the build spec for PersonalDrive. It can be pasted into a coding agent (e.g. Claude Code)
> at the root of the repo. "Confirmed decisions" records the owner's answers.

---

## Role and goal

You are building **PersonalDrive**, a self-hosted replacement for Google Photos / Google Drive /
iCloud for one household. The owner's motivation is cost and control: commercial cloud storage keeps
getting more expensive, while raw object storage (S3 Deep Archive is ~$1/TB/month) and local hard
disks are cheap.

The target architecture is a **3-2-1 backup** setup:

```
 Phones / laptops / browsers  ──Wi-Fi (LAN)──►  Home server (pdrived)  ──scheduled, encrypted,──►  S3 (or S3-compatible)
   auto-sync clients                             local disks = PRIMARY      deduplicated backups       = OFFSITE BACKUP
                                                 copy of all data                                       (disaster recovery only)
```

- **Local disks are the primary store.** All day-to-day reads/writes happen on the LAN. This is what
  makes it fast and cheap.
- **S3 is the backup tier**, written on a schedule (e.g. nightly), never read during normal use.
  It exists so a dead disk, burnt house or stolen server is recoverable.
- **A bootstrap script** turns a fresh machine into a working server by restoring from S3.
- **Clients** (mobile/desktop/web, later phases) sync to the home server over Wi-Fi, support
  multiple users, and auto-upload photos.
- **Later**: local-only AI (photo understanding, "memories", document/bill organisation). Not built
  now, but every design decision must leave room for it.

## Current state of the repo (read this — it constrains you)

The repo is a skeleton. There is **no code and no interfaces yet**:

```
README.md            # says "client-side only, no server" — this is now OUTDATED, update it
desktop_app/README.md
mac_app/README.md
mobile_app/README.md
shared_lib/README.md
web_app/README.md
```

The original idea was client apps writing directly to the user's own S3. We are changing that:
a home server sits in the middle. You are responsible for defining the interfaces in this prompt in
code, and you must update the root README to describe the new architecture.

## Confirmed decisions (2026-10-23)

1. **Hardware**: the owner's Linux laptop (x86_64, Ubuntu, 512 GB SSD with ~142 GB free) runs the
   server. Keep arm64 cross-compilation working so it can move to a Pi/mini PC later.
2. **Disks** — three storage roles, every path set in config, nothing hardcoded:

   ```
   Laptop SSD  (paths.state)         server binary, SQLite DB, thumbnail cache, upload staging
   External A  (paths.library)       PRIMARY library — human-readable folders
   External B  (backup.targets[])    LOCAL backup repository — versioned, deduped, encrypted
   S3          (backup.targets[])    OFFSITE backup repository — same format as B
   ```

   - Disk B is a **backup target, not a mirror**: a mirror replicates accidental deletes, sync bugs
     and ransomware instantly, and RAID over USB is fragile. Restores from B are free and fast;
     S3 is only needed if A and B are both lost.
   - The external disks are not connected yet. Each configured disk has a **marker file**
     (`.pdrive-volume` containing a volume UUID written at init). If the library marker is
     missing or mismatched, the server goes **read-only** and alerts — it must never write into an
     empty mount-point directory and fill the laptop SSD. If a backup target's marker is missing,
     that target is skipped and an alert is raised.
   - The server must cope with the laptop leaving home: clients queue and resume.
3. **Data size / users**: multi-user household (2–5 people, ~10 devices). Space is tight, so
   dedup is a first-class feature (see below). Design for ~1–2 TB; nothing should assume it fits
   in RAM.
4. **Backup provider**: AWS S3 — data packs in **Deep Archive**, metadata in Standard. Must still
   work against any S3-compatible endpoint (MinIO is used for tests).
5. **Workflow**: build continuously, one branch + PR per phase, **stacked** (each phase branch is
   based on the previous one).

## Deduplication (first-class feature)

Family members hold many copies of the same photos and documents. Dedup at three layers:

1. **Backup layer** — content-defined chunk dedup across all users and snapshots (Phase 2).
2. **Upload layer** — clients send the content hash first (`POST /uploads` and `POST /sync/have`);
   if the server already has the bytes, the upload is skipped and the file is linked in.
3. **Primary disk layer** — identical files are stored once on disk A while still appearing in
   each user's folder:
   - `dedup.on_disk: auto | reflink | hardlink | off` (default `auto`: reflink if the filesystem
     supports it, else hardlink). Hardlinks are safe *because* FileStore never modifies a file in
     place — every write is a new file + atomic rename, so an edit only affects that user's path,
     and a delete only removes that user's link. Document that files on disk A must not be edited
     in place by other tools.
   - `dedup.scope: household | user` (default `household`). With `user`, "already have it"
     answers only consider the caller's own files and files shared with them, so a user can't
     probe whether someone else owns a file; on-disk/backup dedup still happens silently.
   - `pdrive dupes scan <path>...` reports duplicate files and potential savings for existing
     folders before import.
4. **Near-duplicates** (re-compressed/resized copies) need perceptual hashing — deferred to the
   AI phase as a "review similar items" tool, never automatic deletion.

## Non-negotiable principles

1. **Never lose data.** Every write is atomic (temp file → fsync → rename → fsync dir). Deletes go to
   a trash with retention. Backups are verified. When in doubt, keep both copies.
2. **No lock-in.** If PersonalDrive disappears tomorrow:
   - Files on local disk are in a **human-readable folder layout** with original filenames and
     content — browsable with a normal file manager. The database is an index, not the source of
     truth, and can be rebuilt from disk (`pdrive admin reindex`).
   - The S3 backup format is **fully documented** in `docs/BACKUP_FORMAT.md` so it can be decoded
     with a small standalone tool.
3. **Encrypt before upload.** S3 only ever sees ciphertext. The user holds the key via a passphrase
   + a printable **recovery kit**. Losing both means losing the backup — say so loudly in the setup
   flow.
4. **LAN-first, not internet-exposed.** The server binds to the LAN only by default. Remote access
   is a later, opt-in feature (recommend Tailscale/WireGuard rather than port-forwarding).
5. **Cost-aware.** Every S3 operation has a price (storage, PUT, retrieval, egress, minimum storage
   duration). Design to minimise all of them, and show cost estimates before expensive actions.
6. **Extensible.** File events go through an event bus and a job queue so later features
   (thumbnails, EXIF, AI tagging, OCR) plug in without touching the core.

## Tech stack (recommended — propose changes before deviating)

| Concern | Choice | Why |
| --- | --- | --- |
| Server + backup engine + CLI | **Go** (single static binary, cross-compiles to arm64) | Good S3 SDK, easy concurrency, cheap to run on a Pi |
| Metadata DB | **SQLite** in WAL mode (via a `MetadataStore` interface so Postgres can be added later) | Zero-ops, single file, easy to back up |
| S3 client | AWS SDK for Go v2, with custom endpoint support | Works with AWS, B2, R2, Wasabi, MinIO |
| Chunking | FastCDC (content-defined chunking), avg ~1 MiB chunks | Dedup survives inserts/edits |
| Hashing | BLAKE3 (keyed for chunk IDs) + SHA-256 for S3 upload checksums | Fast; keyed IDs don't leak content |
| Compression | zstd (adaptive: skip incompressible data) | Best ratio/speed trade-off |
| Encryption | XChaCha20-Poly1305 per blob, master key wrapped by Argon2id(passphrase) | Modern AEAD, nonce-misuse safe |
| API | REST + JSON, described in **OpenAPI 3.1** at `shared_lib/api/openapi.yaml`; WebSocket for change events | Generate client SDKs for every app |
| Deploy | Docker Compose **and** a systemd unit + plain binary | Users differ in comfort level |
| Integration tests | MinIO in docker-compose as a fake S3 | Real S3 semantics without cost |

## Target repository layout

```
server/
  cmd/pdrived/            # server daemon
  cmd/pdrive/             # CLI: admin, backup, restore, check, user mgmt
  internal/
    storage/              # FileStore: human-readable on-disk layout, atomic writes, trash
    metadata/             # MetadataStore (SQLite), migrations
    auth/                 # users, sessions, device pairing, roles
    api/                  # HTTP handlers (generated from OpenAPI where practical)
    sync/                 # change feed, upload sessions, conflict handling
    events/               # in-process event bus
    jobs/                 # durable job queue (SQLite-backed) + worker pool
    processors/           # pluggable file processors (thumbnail, exif now; AI later)
    backup/
      chunker/            # FastCDC
      codec/              # zstd, jpeg->jxl lossless (optional)
      crypto/             # key derivation, AEAD, key wrapping
      pack/               # packfile writer/reader
      repo/               # repository: index, snapshots, config
      engine/             # Snapshot / Restore / Prune / Check orchestration
    objectstore/          # ObjectStore interface + s3 implementation + local-fs fake
    scheduler/            # cron-like schedules, quiet hours, bandwidth limits
    health/               # disk space, SMART, backup freshness, alerts
  migrations/
shared_lib/api/openapi.yaml
scripts/bootstrap.sh      # set up a new machine from S3
deploy/docker-compose.yml
deploy/systemd/pdrived.service
docs/ARCHITECTURE.md
docs/BACKUP_FORMAT.md
docs/RECOVERY.md          # disaster recovery runbook + recovery kit template
docs/adr/                 # one short Architecture Decision Record per major decision
```

## Core interfaces (implement these first, then the implementations)

Treat these as the contract. Refine signatures if needed, but keep the boundaries.

```go
// ---- Local primary storage ------------------------------------------------

// FileStore owns the on-disk, human-readable library (paths.library, e.g. external disk A):
//   <library>/users/<username>/<user's own folder tree>
//   <library>/shared/<space-name>/...
//   <library>/.trash/, <library>/.tmp/ (same filesystem, so renames and hardlinks are atomic)
// Thumbnails/cache, upload staging and the DB live under paths.state (laptop SSD); cache is NOT backed up.
type FileStore interface {
    // Put writes atomically and returns the content hash computed while streaming.
    Put(ctx context.Context, owner OwnerRef, relPath string, r io.Reader, mtime time.Time) (FileInfo, error)
    Open(ctx context.Context, owner OwnerRef, relPath string) (io.ReadSeekCloser, error)
    Stat(ctx context.Context, owner OwnerRef, relPath string) (FileInfo, error)
    Move(ctx context.Context, owner OwnerRef, from, to string) error
    Trash(ctx context.Context, owner OwnerRef, relPath string) error // never hard-delete directly
    Walk(ctx context.Context, owner OwnerRef, fn func(FileInfo) error) error
}

// MetadataStore is the index over FileStore plus users, devices, sync state, jobs.
// It must be rebuildable from FileStore (reindex) except for users/devices/shares,
// which are included in every backup snapshot.
type MetadataStore interface {
    Files() FileRepo        // path, owner, size, mtime, content hash, mime, version, server seq
    Users() UserRepo
    Devices() DeviceRepo
    Shares() ShareRepo
    Changes() ChangeFeed    // monotonically increasing server sequence numbers
    Tx(ctx context.Context, fn func(MetadataStore) error) error
}

// ---- Events and extensibility ---------------------------------------------

type EventBus interface {
    Publish(ctx context.Context, e Event) error // FileCreated, FileUpdated, FileTrashed, BackupFinished, ...
    Subscribe(topic string, h func(context.Context, Event) error) (unsubscribe func())
}

// Processor is how thumbnails, EXIF extraction and (later) AI features plug in.
// Processors run from the durable job queue, are idempotent, and store results
// in their own metadata tables keyed by content hash.
type Processor interface {
    Name() string
    Version() int                    // bump to reprocess everything
    Accepts(f FileInfo) bool
    Process(ctx context.Context, f FileInfo, src io.ReadSeeker) error
}

// ---- Backup tier ----------------------------------------------------------

// ObjectStore abstracts S3 and S3-compatible providers.
type ObjectStore interface {
    Put(ctx context.Context, key string, r io.Reader, size int64, opts PutOptions) error // StorageClass, SHA256 checksum, multipart for large
    Get(ctx context.Context, key string, rng *ByteRange) (io.ReadCloser, error)
    Head(ctx context.Context, key string) (ObjectInfo, error)                     // includes storage class + restore status
    List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error
    Delete(ctx context.Context, keys ...string) error
    RequestRestore(ctx context.Context, key string, tier RestoreTier, days int) error // Glacier/Deep Archive thaw
}

type Chunker interface { Next() (Chunk, error) }       // deterministic for a given key + params
type Codec interface {
    ID() byte
    Encode(dst, src []byte) ([]byte, error)
    Decode(dst, src []byte) ([]byte, error)
}
type Sealer interface {                                 // AEAD with the repository master key
    Seal(dst, plaintext, aad []byte) []byte
    Open(dst, ciphertext, aad []byte) ([]byte, error)
}

type BackupEngine interface {
    Snapshot(ctx context.Context, req SnapshotRequest) (SnapshotResult, error)  // incremental, resumable
    ListSnapshots(ctx context.Context) ([]SnapshotSummary, error)
    PlanRestore(ctx context.Context, req RestoreRequest) (RestorePlan, error)   // bytes, objects, tiers, ETA, $ estimate
    Restore(ctx context.Context, plan RestorePlan, progress chan<- Progress) error
    Prune(ctx context.Context, policy RetentionPolicy, dryRun bool) (PruneReport, error)
    Check(ctx context.Context, level CheckLevel) (CheckReport, error)
}

type Scheduler interface {
    Register(name string, spec Schedule, job func(context.Context) error) error // cron + quiet hours + jitter
}
```

## Phase 1 — Local server core

Deliver a server that stores files for multiple users on local disk and serves them on the LAN.

**Storage**
- Human-readable layout described in `FileStore` above. Original bytes, original filenames,
  original mtimes. No proprietary blobs on the primary disk.
- Content-hash index of every file; on-disk dedup per the "Deduplication" section.
- Trash with configurable retention (default 30 days) and restore-from-trash.
- `pdrive admin reindex` rebuilds the file index by walking the disk.
- Startup self-check: data dir writable, free space, DB integrity check.

**Users and auth**
- Roles: `admin`, `member`. First-run wizard (CLI and web) creates the admin.
- Passwords hashed with Argon2id. Short-lived access tokens + refresh tokens bound to a device.
- **Device pairing**: an admin or user generates a pairing QR code (server URL + one-time token +
  TLS certificate fingerprint). Mobile apps scan it, so nobody types IPs or trusts unknown certs.
- Per-user quotas (optional), per-user root folder, and **shared spaces** (e.g. "Family Photos")
  with read / read-write ACLs.
- Be honest in docs: the server admin has disk access to everyone's files. Per-user end-to-end
  encryption is a non-goal for now.

**Networking**
- HTTPS on the LAN. On first run generate a local CA + server certificate; clients pin the
  fingerprint from the pairing QR code.
- Advertise via mDNS/DNS-SD as `_pdrive._tcp.local` so apps find the server automatically.
- Bind to LAN interfaces only by default.

**API (write `shared_lib/api/openapi.yaml` first, then implement)**

At minimum:

```
POST   /api/v1/auth/login | /auth/refresh | /auth/logout
POST   /api/v1/devices/pair                     # exchange pairing token for device credentials
GET    /api/v1/me
GET    /api/v1/files?path=&cursor=              # list a folder
GET    /api/v1/files/{id}                       # metadata
GET    /api/v1/files/{id}/content               # supports HTTP Range
GET    /api/v1/files/{id}/thumbnail?size=
PATCH  /api/v1/files/{id}                       # rename / move
DELETE /api/v1/files/{id}                       # -> trash
GET    /api/v1/trash  |  POST /api/v1/trash/{id}/restore

POST   /api/v1/uploads                          # start session: path, size, sha256/blake3, mtime, base_version
                                                #  -> 200 "already have it" (dedup) or upload id + chunk size
PUT    /api/v1/uploads/{id}/parts/{n}           # resumable chunked upload
POST   /api/v1/uploads/{id}/complete            # server verifies hash, commits atomically
POST   /api/v1/sync/have                        # bulk: "which of these hashes do you already have?"
GET    /api/v1/sync/changes?cursor=&wait=30     # change feed (long-poll), monotonic server sequence
WS     /api/v1/events                           # push notifications of changes

GET    /api/v1/admin/users | POST | PATCH | DELETE
GET    /api/v1/admin/backup/status | /snapshots
POST   /api/v1/admin/backup/run
GET    /api/v1/admin/health                     # disks, SMART, free space, last backup age
GET    /healthz  /metrics (Prometheus)
```

**Sync semantics (the clients in Phase 3 depend on these — get them right now)**
- Server is the source of truth. Every change gets a monotonically increasing server sequence
  number; clients sync by cursor.
- Uploads carry `base_version`. If it doesn't match the current version → **keep both**:
  `name (conflict from <device> <date>).ext`. Never overwrite silently.
- Two sync modes per folder: **backup-only** (phone camera roll: deleting on the phone does NOT
  delete on the server) and **two-way** (desktop folder sync).
- Uploads are resumable across app restarts and network drops; partial uploads live in
  `.staging/` and expire after 24h.

**Built-in processors (prove the plugin design works)**
- Thumbnails/previews for images (incl. HEIC) and video posters → stored in `.cache/`, never
  backed up (regenerable).
- EXIF/metadata extraction (capture time, GPS, camera) → metadata table.

**Exit criteria:** two users on two devices can upload, list, download, rename, trash and restore
files via the API; reindex rebuilds an identical index; all of it covered by tests.

## Phase 2 — S3 backup engine, scheduler, restore and bootstrap

### Why not literally "zip the files"

Photos (JPEG/HEIC) and videos (MP4/HEVC) are already compressed — zipping them saves ~0%, and
re-zipping everything each run means re-uploading everything. "Zipped and backed up on a
frequency" should be implemented as **incremental, deduplicated, compressed, encrypted pack
files**, which delivers what zipping was meant to deliver (fewer, smaller objects) without the
waste. Put this reasoning in `docs/adr/0001-backup-format.md`.

### Repository format in the bucket

```
<bucket>/<prefix>/
  config                  # format version, chunker params, KDF params            (STANDARD)
  keys/<id>               # master key wrapped by Argon2id(passphrase); >1 key    (STANDARD)
                          #   allowed (e.g. passphrase key + recovery key)
  snapshots/<id>          # encrypted manifest: tree of paths -> chunk lists,     (STANDARD)
                          #   file hash, size, mtime, mode, owner
  index/<id>              # encrypted: chunk id -> (pack id, offset, length)      (STANDARD)
  meta/<snapshot-id>      # encrypted dump of metadata DB (users, devices,        (STANDARD)
                          #   shares, processor results) + server config, no plaintext secrets
  data/<xx>/<pack-id>     # packs of ~32–64 MiB of encrypted, compressed chunks   (DEEP_ARCHIVE by default;
                                                                                    configurable)
```

Rules:
- Chunk ID = keyed BLAKE3(plaintext). Pipeline per chunk: **chunk → dedup lookup → compress
  (if it helps) → encrypt → append to pack**.
- Everything needed to plan a restore or an incremental backup (`config`, `keys`, `snapshots`,
  `index`, `meta`) lives in **Standard** storage — it's tiny. Only bulk data goes to cold tiers.
- Keep a **local cache of the index** so incremental backups never read from S3. If the cache is
  lost, rebuild it from `index/` (Standard tier, cheap).
- **Commit point = snapshot manifest upload.** Packs and index are uploaded first; a crash before
  the manifest leaves only orphan packs that the next run reuses or prune collects.
- Packs are staged on local disk, then uploaded with multipart, SHA-256 checksums, retries with
  exponential backoff, a configurable bandwidth cap, and resume after crash.

### Size and cost optimisations (implement in this order, measure each)

1. **Content-defined dedup** across all files, all users, all snapshots (biggest win for repeated
   photos, re-saved documents, edited videos).
2. **Adaptive zstd**: compress chunks of compressible types (documents, text, RAW/DNG, databases);
   skip chunks that don't shrink by ≥5%. Record the codec per chunk.
3. **Exclude regenerable data**: thumbnails, previews, caches, staging, trash older than N days.
4. **Pack small files** into 32–64 MiB packs: cold tiers charge per PUT request and have per-object
   overhead, so millions of small objects are expensive.
5. **Storage-class tiering**: data packs → Deep Archive (or Glacier Instant Retrieval if the user
   wants faster restores); metadata → Standard. Make this per-repository config.
6. **Optional lossless JPEG recompression** (`codec: jpegxl-lossless`): JPEG → JPEG XL lossless
   transcoding saves ~20% and reconstructs the original JPEG **bit-for-bit**. Only store the
   transcoded form after verifying the round-trip reproduces the exact original hash; otherwise
   fall back to the raw bytes.
7. **Lossy "space saver"** (re-encoding videos to HEVC/AV1, downscaling) is **opt-in per folder,
   never default, never applied to originals on local disk**. Clearly label it as lossy.

Report per snapshot: files scanned, new bytes, deduped bytes, compression savings, bytes uploaded,
PUT count, estimated monthly cost delta.

### Scheduling

- Default: nightly at 02:00 local, with quiet hours, jitter, and bandwidth cap. Also allow
  "run when ≥ X GB has changed".
- Only one backup at a time (lock). Skip if on low disk space.
- Notifications on failure or when the last successful backup is older than N days: webhook,
  ntfy, and email. Shown in the admin UI/API.

### Retention and prune

- Policy like `keep-daily 7, keep-weekly 4, keep-monthly 12, keep-yearly 5`.
- Prune is **cost-aware**: Deep Archive bills a minimum of 180 days (Glacier IR 90 days). Do not
  delete or repack packs younger than the minimum; repack only when a pack's dead fraction exceeds
  a threshold (e.g. 50%) and it's past the minimum. `--dry-run` shows what would happen and the cost.

### Verification (`pdrive backup check`)

- `structure`: every snapshot references only chunks in the index, every indexed pack exists
  (HEAD/List — no retrieval cost).
- `local`: re-hash local files and compare with the latest snapshot manifest (detects bit rot on the
  primary disk too).
- `sample-restore`: monthly, thaw N random packs via the Bulk tier within a configured $ budget,
  decrypt, verify every chunk hash.

### Restore CLI and bootstrap script

`pdrive restore` must:
1. Read config/keys/snapshots (Standard tier) using passphrase or recovery key.
2. `--plan` first: show bytes, object count, storage classes, restore tier (Bulk/Standard/Expedited),
   ETA (Deep Archive Bulk ≈ up to 48h), and **estimated cost including egress**. Require
   confirmation.
3. Restore metadata DB first so the server can start in a "restoring" state with the full file tree
   visible and files marked pending.
4. Issue thaw requests, poll, download, decrypt, verify, write files with original mtimes, mark
   each file restored. Fully resumable.
5. Support partial restore: `--user`, `--path`, `--snapshot`, `--since`.

`scripts/bootstrap.sh` (idempotent, bash, Debian/Ubuntu/Raspberry Pi OS first; macOS later):
1. Detect OS/arch; check disk space against the restore plan.
2. Install the `pdrive`/`pdrived` binaries (or Docker Compose — ask which).
3. Ask for the **recovery kit**: endpoint, region, bucket/prefix, access keys, passphrase or
   recovery key (or read them from a file).
4. Run `pdrive restore --plan`, confirm, run the restore in the background, install the systemd
   unit, start the server, regenerate thumbnails via the job queue.
5. Print the server URL and a new pairing QR code.

`docs/RECOVERY.md` contains a step-by-step runbook and a printable recovery kit template.
The first-run wizard forces the user to download/print the recovery kit before backups start.

### Phase 2 exit criteria — the disaster drill

An automated end-to-end test (MinIO in docker-compose) that:
1. Creates users and a library with photos, videos, docs, duplicates, and nested folders.
2. Runs a backup, modifies/adds/deletes files, runs another backup.
3. **Wipes the server completely**, runs `bootstrap.sh` non-interactively against MinIO.
4. Asserts the restored library, users, shares and file hashes exactly match the snapshot.
5. Runs prune and check, then restores an older snapshot and verifies it too.

## Phase 3 — Client apps (design now, build after Phase 2 — propose a plan before starting)

The existing `mobile_app/`, `web_app/`, `desktop_app/`, `mac_app/`, `shared_lib/` folders are the
clients. Requirements:
- `shared_lib`: SDK generated from `openapi.yaml` + hand-written sync engine logic (cursor
  handling, resumable upload queue, conflict naming) shared by every client.
- **Mobile**: server discovery via mDNS + QR pairing; camera-roll **auto backup** (backup-only
  mode) on Wi-Fi, optionally only while charging; background uploads; "free up space on this phone"
  for items confirmed backed up; browse/search the library with thumbnails.
- **Desktop**: two-way folder sync with the conflict rules from Phase 1. Consider merging
  `mac_app/` into `desktop_app/` with a cross-platform framework (e.g. Tauri) — write an ADR.
- **Web**: library browser, upload, sharing, admin dashboard (users, quotas, backup status, disk health).
- Multi-user everywhere: switching accounts on shared devices, per-user libraries, shared spaces.

Propose the client tech stack in an ADR (consider React Native vs Flutter for background photo
upload on iOS/Android) before writing client code.

## Phase 4 — Local AI assistant (do NOT build; just keep the doors open)

Future features: searching photos by content, face clustering, "memories" / "on this day",
document and bill organisation (OCR → extract vendor, amount, due date), smart albums, a chat
assistant over your own files. All processing must stay **local** (e.g. Ollama/llama.cpp, CLIP
embeddings, face models, Tesseract/PaddleOCR).

What Phases 1–2 must already support:
- `Processor` plugins on the durable job queue, keyed by content hash and processor version.
- Extensible metadata: tags, albums, people, document fields, and a place for vector embeddings
  (e.g. `sqlite-vec`).
- Processor outputs that are expensive to recompute (embeddings, face clusters, user-confirmed
  labels) **are** included in the `meta/` backup; cheap derivatives (thumbnails) are not.

## Non-goals (for now)

Internet exposure / port forwarding, end-to-end encryption between users, a Windows server build,
multiple server replicas, real-time collaborative editing.

## Quality bar and working style

- Work **phase by phase**. At the end of each phase: all tests green, docs updated, a short demo
  script (`make demo-phaseN`), and a summary to me of what was built, what was deferred, and any
  decisions I need to make. Stop and wait for me before the next phase.
- Write an ADR for every significant decision (storage layout, backup format, crypto, sync protocol,
  client stack).
- Tests: unit tests everywhere; property tests for chunker determinism and
  encode→decode / seal→open round-trips; fault-injection tests (kill the process mid-upload,
  mid-backup, mid-restore, disk full) proving no corruption and correct resume.
- Observability: structured JSON logs, Prometheus metrics (backup duration, bytes uploaded, dedup
  ratio, last success timestamp, disk free, job queue depth).
- Security: no secrets in logs, config file permissions 0600, rate-limited auth, CSRF protection for
  the web UI, dependency vulnerability scanning in CI.
- CI (GitHub Actions): lint, test, build arm64 + amd64, run the disaster-drill test against MinIO.
- Config in one YAML file (`/etc/pdrive/config.yaml`) with environment-variable overrides; every
  option documented with its default.

## Reference costs (verify current pricing before relying on them)

AWS S3 us-east-1, approximate:
- Deep Archive storage ≈ $0.00099/GB-month (≈ $1/TB-month); minimum 180 days; Bulk retrieval
  ≈ $0.0025/GB with up to 48h delay.
- Glacier Instant Retrieval ≈ $0.004/GB-month; minimum 90 days; retrieval ≈ $0.03/GB.
- PUT requests to cold tiers cost more than Standard (hence packs).
- **Egress ≈ $0.09/GB** — restoring 1 TB costs roughly $90+ in transfer alone. This is the
  biggest cost of a full disaster recovery; the restore plan must show it.
- Alternatives worth supporting through the same `ObjectStore` interface: Backblaze B2
  (cheap storage, free egress up to a limit), Cloudflare R2 (no egress fees), Wasabi.
