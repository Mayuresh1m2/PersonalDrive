# ADR 0001: Human-readable library with link-based dedup

Status: accepted (2026-10-23)

## Context

The household has many copies of the same photos and documents across family members,
and disk space is tight (a laptop SSD plus two USB disks). We also want zero lock-in: if
PersonalDrive disappears, the files must still be usable.

## Options considered

1. **Content-addressed blob store** (`blobs/ab/abcd…`) plus a database mapping paths to
   blobs. Maximum dedup, but the disk is unreadable without the database.
2. **Plain per-user copies.** Readable, but duplicates cost full space.
3. **Plain per-user folders, with identical files sharing storage via reflinks or hardlinks.**

## Decision

Option 3. Each user has a normal folder tree on disk A. When new content matches an
existing file (by SHA-256 + size), the server places a reflink (btrfs/XFS) or hardlink
(ext4 etc.) instead of a new copy. `dedup.on_disk: auto` probes the filesystem.

Hardlinks are safe only because **files are never modified in place**: every write is a new
file renamed over the path, which replaces that path's link and leaves other links alone.
Deleting one user's copy removes only their link.

`dedup.scope` controls what the *API* reveals: with `household`, any user's upload can be
skipped if anyone has the content; with `user`, a user can't probe for other users' files,
but on-disk dedup still happens silently after the bytes arrive.

## Consequences

- Hardlinked copies share one inode, so they share the on-disk mtime. The index stores
  each file's own mtime; the API reports that.
- Files on disk A must not be edited in place by other tools (documented). `pdrive reindex`
  picks up any outside changes.
- exFAT/FAT disks support neither reflinks nor hardlinks; dedup falls back to copies. Format
  the library disk as ext4 (or btrfs for reflinks).
- Backups dedup at the chunk level independently (Phase 2), so they benefit even with
  `on_disk: off`.
