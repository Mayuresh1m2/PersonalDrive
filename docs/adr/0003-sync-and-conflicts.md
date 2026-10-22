# ADR 0003: Server-sequenced change feed, keep-both conflicts

Status: accepted (2026-10-23)

## Decision

- The server is the source of truth. Every mutation appends a row to `changes` with a
  global, monotonically increasing `seq`. Clients follow the feed by cursor
  (`/sync/changes`), with long-polling, and a WebSocket used only as a wake-up signal.
- Uploads carry `base_version`, the version the client last saw:
  - new path → create;
  - identical content already there → no-op (idempotent retries);
  - `base_version` matches → new version in place, with the old version moved to the trash
    (`trash_reason: replaced`);
  - anything else → stored as `name (conflict from <device> <date>).ext`. **Never overwrite
    silently.**
- Deletes always go to the trash (default 30 days).
- Directory moves and trashes are recorded once for the root; clients apply them to
  descendants.
- Camera-roll backup is upload-only. Desktop folder sync is two-way.

## Why not CRDTs or vector clocks?

There is one authoritative server and files are opaque blobs. A per-file version counter
plus keep-both on conflict is easy to reason about, and loses nothing.
