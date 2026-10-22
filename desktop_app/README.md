# desktop_app — `pdrive-sync`

Two-way folder sync between a computer and your PersonalDrive server, for
Linux, macOS and Windows (one static binary; `mac_app` was folded into this, see
ADR 0005).

```bash
go build -o pdrive-sync ./cmd/pdrive-sync      # or: GOOS=darwin / GOOS=windows

pdrive-sync pair '<payload>'     # from the web app: Devices → Show pairing code,
                                 # or on the server: pdrive pair <user>
pdrive-sync add ~/PersonalDrive  # ⇄ server folder "PersonalDrive"
pdrive-sync add ~/Documents --remote Documents
pdrive-sync once                 # sync now
pdrive-sync run                  # keep syncing (watches files + server changes)
pdrive-sync service              # how to start it at login (systemd / launchd / Task Scheduler)
```

Pairing verifies the server's certificate against the fingerprint in the pairing
code, then pins that CA for every connection.

## How it decides what to do

For every path it compares three states: the **local** file, the **server** file,
and the **base** (both as of the last successful sync, kept in
`<config-dir>/state/`). Only a side that differs from the base has changed.

| Local | Server | Result |
| --- | --- | --- |
| changed | unchanged | upload (or create the folder) |
| unchanged | changed | download |
| deleted | unchanged | trash it on the server (folders only once empty) |
| unchanged | deleted | move the local copy to the **sync trash** |
| changed | changed (different content) | **keep both**: the server saves your edit as `name (conflict from <this computer> <date>).ext`, and both versions end up on every device |
| deleted | changed | keep the server's edit (re-download it) |
| changed | deleted | keep your edit (re-upload it) |

Safety details:
- Files modified in the last 2 s are left for the next cycle (still being copied).
- A file is never overwritten while it holds content the server doesn't have.
- Files removed because they were deleted on another device go to
  `<config-dir>/state/trash/`, not the OS's permanent delete.
- Downloads land in a temp file, are SHA-256 verified, then renamed into place.
- Moves and renames upload nothing new: the server already has the bytes (dedup),
  and downloads reuse local copies.
- The server change-feed cursor only advances once every change was applied, so a
  crash or network drop just replays the cycle.
- Office lock files, `.DS_Store`, `Thumbs.db`, `*.tmp`, `*.part` and similar are
  ignored.

## Tests

`go test ./...` runs two simulated computers against a real `pdrive` server. It
covers create, edit, concurrent edits (conflict copies), delete, delete vs edit,
folder rename, first sync with pre-existing differing files, files still being
written, and ignored junk.
