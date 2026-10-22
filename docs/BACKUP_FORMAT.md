# Backup repository format (v1)

This document is enough to write an independent decoder, so backups never
depend on PersonalDrive continuing to exist. Implementation:
`server/internal/backup/{repo,crypto,chunker}`.

## Object layout

```
config                  sealed  repository config
keys/<label>-<id>       JSON    master key wrapped by a passphrase or recovery key
snapshots/<id>          sealed  snapshot description
index/<id>              sealed  blob → pack location tables
data/<xx>/<pack-id>     binary  packs of sealed blobs; <xx> = first two hex chars of <pack-id>
locks/<id>              JSON    writer lock (host, pid, purpose, time)
```

Local targets store the same keys as files under `<path>/repo/`.

**Storage classes.** Packs of library file content ("cold" packs) use the target's
`storage_class` (e.g. `DEEP_ARCHIVE`). Everything else (config, keys, snapshots,
index, and "hot" packs holding directory trees and server metadata) uses
`STANDARD`. Listing snapshots, planning restores, incremental backups and
restoring server metadata therefore never require thawing archived data.

## Cryptography

- **Master key**: 64 random bytes = `enc` (32) ‖ `mac` (32).
- **Sealing**: XChaCha20-Poly1305 with `enc`. A sealed value is
  `nonce (24) ‖ ciphertext ‖ tag (16)`, with a random nonce.
- **Blob id**: keyed BLAKE3-256 of the blob plaintext with key `mac`. Ids reveal
  nothing about content without the key.
- **Key files** (`keys/*`, plain JSON):
  ```json
  {"version":1,"label":"passphrase","kdf":"argon2id","time":3,"memory_kib":131072,
   "threads":4,"salt":"<base64>","sealed":"<base64>","created":"..."}
  ```
  `KEK = Argon2id(secret, salt, time, memory_kib, threads, 32 bytes)`;
  `sealed = Seal_KEK(enc ‖ mac, aad = "pdrive key v1")`. For `label: recovery`,
  the secret is normalised first: uppercase, with spaces, dashes and newlines
  removed. Recovery keys are 32 random bytes in unpadded base32, printed in
  groups of four.
- **Sealed JSON objects** (`config`, `snapshots/*`, `index/*`):
  `Seal_enc(zstd(payload), aad)` with aad `"pdrive config"`, `"pdrive snapshot"`
  or `"pdrive index"`.

## Config (`config`, JSON)

```json
{"version":1,"id":"<hex>","chunker_seed":"<base64 32 bytes>",
 "chunker":{"Min":262144,"Avg":1048576,"Max":4194304},"pack_size":33554432,"created":"..."}
```

## Chunking

FastCDC with a gear table derived from `chunker_seed`:
`gear[i]` = big-endian uint64 from bytes `8i..8i+7` of the BLAKE3 XOF output of
`"pdrive chunker gear v1" ‖ seed`. Rolling hash `fp = (fp << 1) + gear[byte]`,
starting after `Min` bytes. Before `Avg`, cut when `fp & maskS == 0`; after `Avg`,
when `fp & maskL == 0`; always cut at `Max`. With `b = log2(Avg)`, `maskS` has the
top `b+2` bits set and `maskL` the top `b-2` bits set.

## Packs

```
sealed blob 0 | sealed blob 1 | ... | sealed header | u32le header length | "PDPK" | 0x01
```

- **Sealed blob**: `Seal_enc(codec ‖ payload, aad = blob id)`, where codec `0x00`
  means the payload is raw plaintext and `0x01` means zstd.
- **Header plaintext**: 45-byte entries, each
  `id (32) ‖ type (1: 1=data, 2=tree) ‖ offset u32le ‖ length u32le ‖ raw length u32le`.
  The header is sealed with aad `"pdrive pack header v1"`.
- **Pack id**: hex SHA-256 of the whole pack file. It matches the S3 upload checksum.

## Index (`index/*`, gob)

A Go `gob` encoding of
`struct{ Version int; Files []string; Packs []PackInfo }`, where `PackInfo` is
`{ID string; Hot bool; Size int64; Blobs []{ID [32]byte; Type uint8; Offset, Length, Raw uint32}}`.
Index files are additive. Prune rewrites them into one file. The same data can be
rebuilt from pack headers if every index file is lost.

## Trees (blob type 2, JSON)

```json
{"nodes":[{"name":"IMG_0001.jpg","type":"file","size":3000000,"mtime":"...","mode":420,
           "inode":1234,"sha256":"<hex of whole file>","content":["<blob id hex>", ...]},
          {"name":"2024","type":"dir","mtime":"...","mode":488,"subtree":"<tree blob id hex>"}]}
```

Nodes are sorted by name. A file's content is the concatenation of its data blobs.
`sha256` verifies the reassembled file.

## Snapshots (`snapshots/<id>`, JSON)

```json
{"time":"...","host":"laptop","tree":"<root tree id>","parent":"<snapshot id>",
 "stats":{...},"program":"pdrive v...","complete":true}
```

The root tree has one directory node per source:

- **`library`**: disk A, excluding `.tmp`. It includes `.trash` and the `.pdrive-volume` marker.
- **`meta`**: `pdrive.db` (a consistent `VACUUM INTO` copy), `secrets/token.key`,
  `tls/ca.pem`, `tls/ca.key`, and `config.yaml`. The backup passphrase is never included.

## Commit protocol

1. Upload packs. Every 20 packs, upload an index file for the packs uploaded so far.
   These checkpoints let an interrupted multi-day first backup resume.
2. Upload the final index file.
3. Upload the snapshot. **The snapshot is the commit point.** Packs without a
   snapshot are harmless, and `prune` deletes them once they're past the minimum
   storage duration.
