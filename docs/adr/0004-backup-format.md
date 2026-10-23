# ADR 0004: Content-defined, encrypted packs instead of zipped archives

Status: accepted (2026-10-23)

## Context

The original idea was to "zip files and upload them on a schedule". The library is
mostly JPEG/HEIC photos and MP4/HEVC video, which are already compressed, so zip
saves ~0% on them. Zipping everything each run would also re-upload everything, and
one big archive per run means restoring a single file needs the whole archive. Deep
Archive charges per PUT and per GB retrieved, and has a 180-day minimum.

## Decision

Use a restic/borg-style repository (see BACKUP_FORMAT.md):

- **FastCDC chunking (~1 MiB) with keyed BLAKE3 ids**: dedup across files, users
  and snapshots, and edits only upload changed chunks.
- **Adaptive zstd**: compress a chunk only if it shrinks ≥5%. Documents, text, RAW
  and the database compress; photos and videos are stored as-is with no wasted CPU.
- **XChaCha20-Poly1305 everywhere**: the provider sees only ciphertext and opaque ids.
- **32 MiB packs**: few PUTs (~$0.05 per 1000 on Deep Archive), still small enough
  to restore selectively with range GETs.
- **Hot/cold split**: trees, indexes, snapshots and server metadata stay in Standard,
  so planning, browsing and metadata restores never wait 48h. Only file content goes
  to Deep Archive.
- **Snapshots as the version history**: `.trash` is included (renames dedup to
  nothing) and `.tmp` is excluded.

### Why not just run restic or kopia?

Both are excellent and fit this role. A native engine lets us:
- split hot and cold storage classes per object (restic puts everything in one class);
- make prune aware of minimum storage duration;
- integrate scheduling, status and alerts with the server;
- restore server metadata first so the server can start read-only.

The format is documented so the data is never locked in. If maintaining the engine
ever costs too much, the `ObjectStore` and `Source` boundaries allow swapping in
restic/kopia behind the same CLI.

### Not done (yet)

- **JPEG → JPEG XL lossless recompression** (~20%, bit-exact) is planned as an
  optional codec. It needs `cjxl`/`djxl` or a cgo library, plus round-trip
  verification before use.
- **Lossy video re-encoding**: opt-in only, per folder, never by default (see the spec).

## Testing

The S3 code path is tested against `gofakes3`, an in-process S3 server, rather than
MinIO, because MinIO stopped publishing community container images in 2025. Archive
tiers (thaw, restore state, early-deletion rules) are simulated by
`objectstore.ArchiveSim`. `scripts/disaster-drill.sh` runs the real binaries and
`bootstrap.sh` end to end.
