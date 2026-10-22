# ADR 0002: SHA-256 for file identity, stable integer ids

Status: accepted (2026-10-23)

## Decision

- File content is identified by **SHA-256** (hex). Every client platform has it built in
  (WebCrypto in browsers, CryptoKit on iOS, `MessageDigest` on Android), which matters
  because clients hash files *before* uploading to enable upload skipping and integrity
  checks.
- The backup engine (Phase 2) uses keyed BLAKE3 for chunk ids instead. Those never leave
  the server, so speed wins there, and the keying stops chunk ids revealing content.
- File rows use `INTEGER PRIMARY KEY AUTOINCREMENT` so ids are **never reused**, even after
  purge or reindex. Sync clients key their local state on file ids.

## Consequences

SHA-256 is slower than BLAKE3 (~0.5–1 GB/s per core vs several GB/s). For a home library
that's fine: hashing happens once per upload, streamed while the bytes are written.
