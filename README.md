# PersonalDrive

A self-hosted replacement for Google Photos and Google Drive for one household.
It exists because commercial cloud storage keeps getting more expensive, while raw
storage (your own disks, plus S3 Deep Archive at ~$1/TB/month) stays cheap.

```
 Phones / laptops / browsers ──Wi-Fi──► Home server (laptop) ──hourly──► Disk B (local backup)
   web app, folder sync,                 Disk A = main library  └─nightly─► S3 Deep Archive (offsite)
   camera-roll backup                    normal folders on disk            encrypted, deduplicated
```

- **Your disks hold the main copy.** Everyday use stays on your Wi-Fi: fast and free.
  Files are kept as normal folders, so nothing is locked in.
- **Backups are incremental, deduplicated, compressed and encrypted.** They go to a
  second disk every hour and to S3 every night. AWS only ever sees encrypted data.
- **Nothing is ever permanently deleted until you switch it on.** See
  [Safety](#safety-start-here).

## What's built

| Part | What it does | Status |
| --- | --- | --- |
| **Home server** (`server/`, `pdrive`) | Multiple users and shared spaces (e.g. "Family Photos"); quotas; QR-code device pairing; HTTPS with its own certificate authority; resumable uploads; conflicts keep both copies; trash; thumbnails and photo metadata; health and disk monitoring. Goes read-only if disk A is unplugged and recovers on its own | ✅ tested |
| **Dedup** | Identical files from different family members are stored once on disk; uploads of files the server already has finish instantly; backups store each piece of data once | ✅ tested |
| **Backups** (`pdrive backup`) | Encrypted, deduplicated snapshots to disk B and S3 Deep Archive on a schedule. Prune is aware of AWS minimum storage periods; checks catch disk corruption; alerts by phone push (ntfy) or webhook | ✅ tested against a fake S3. ⏳ real AWS not tried yet |
| **Restore and disaster recovery** (`pdrive restore`, `scripts/bootstrap.sh`) | Shows the cost and wait time before restoring. Rebuilds a whole server on a new machine from a printed recovery kit: users, passwords, paired devices and files all come back | ✅ "lose everything" drill runs in CI |
| **Web app** (`web_app/`) | Built into the server. Browse, preview, drag-and-drop upload, trash, device pairing, admin pages for people, spaces, disks and backups | ✅ tested in a real browser |
| **Desktop sync** (`desktop_app/`, `pdrive-sync`) | Keeps folders on Linux/Mac/Windows in sync with the server. Upload-only by default | ✅ tested on Linux. ⏳ Mac/Windows untested |
| **Phone app** (`mobile_app/`) | iOS/Android camera-roll backup (Wi-Fi/charging rules, background), library browsing, "free up space" | ⚠️ type-checked, unit-tested and bundled, but **never run on a phone yet** |
| **Shared client library** (`shared_lib/`) | The API contract (OpenAPI) and a TypeScript library used by the web and phone apps | ✅ tested against a real server |
| **AI features** (search, memories, bills) | Not built yet; the server has hooks ready for them | 🔜 later |

## Safety (start here)

Every feature that can permanently delete or overwrite data is **off** until you
turn it on in the config file, one at a time, by following
**[docs/ROLLOUT.md](docs/ROLLOUT.md)**:

```yaml
safety:
  purge_trash: false       # trash is never emptied
  permanent_delete: false  # no "Delete forever"
  prune_backups: false     # backups only grow
  free_up_space: false     # phones never delete photos
```

Restores never overwrite a file whose content differs from the backup.
`pdrive-sync` starts in upload-only mode. `pdrive verify-copy <folder>` tells you
whether every file in an old copy (old disk, Google Photos export) is in the
library before you delete that copy. Keep all your existing copies until the last
rollout stage.

## Quick start

On the laptop, with Go installed:

```bash
make -C server build-full                      # server + web app → server/bin/pdrive
sudo install server/bin/pdrive /usr/local/bin/
pdrive init --library /mnt/pdrive-a/library    # disk A; writes ~/.config/pdrive/config.yaml
pdrive user add <you> --admin
pdrive serve                                   # then open https://<laptop>.local:8443
```

Then follow [docs/ROLLOUT.md](docs/ROLLOUT.md): import your existing files, add
the disk-B backup, then S3, then phones and computers. Setting the laptop up as an
always-on server (lid closed, start at boot, mounting the disks) is in
[docs/LAPTOP_SERVER.md](docs/LAPTOP_SERVER.md).

## S3 in one paragraph

You create a bucket in your AWS account and name it in the config
(`backup.targets`: `bucket`, `prefix`, `region`). Use an access key for an IAM user
that can only touch that bucket (policy in [docs/BACKUP.md](docs/BACKUP.md)). No
bucket is built in. Encrypted file data goes to Deep Archive; the small index and
snapshot list stay in Standard so browsing backups and planning restores are
instant. Backblaze B2, Cloudflare R2, Wasabi and MinIO work too (set `endpoint`).
Approximate cost for 1 TB: ~$1/month stored, ~$93 one-off for a full disaster
restore (mostly AWS's download fee). Disk B makes ordinary restores free.

## Docs

| Doc | For |
| --- | --- |
| [ROLLOUT.md](docs/ROLLOUT.md) | **Start here**: the staged plan and safety switches |
| [LAPTOP_SERVER.md](docs/LAPTOP_SERVER.md) | Disks, service, keeping the laptop awake |
| [BACKUP.md](docs/BACKUP.md) | Disk B and S3 setup, costs, alerts, checks |
| [RECOVERY.md](docs/RECOVERY.md) | What to do when something is lost; recovery kit |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md), [BACKUP_FORMAT.md](docs/BACKUP_FORMAT.md), [adr/](docs/adr/) | How it works and why |
| [desktop_app](desktop_app/README.md), [mobile_app](mobile_app/README.md), [web_app](web_app/README.md), [shared_lib](shared_lib/README.md) | Each app |
| [Build spec](docs/prompts/01-home-server-and-s3-backup.md) | The original plan and decisions |

## Project layout

| Path | Contents |
| --- | --- |
| `server/` | Go server and admin CLI (`pdrive`); backup engine; the web app is embedded at build time |
| `web_app/` | Browser app (React) |
| `desktop_app/` | `pdrive-sync` folder sync (Go) |
| `mobile_app/` | Phone app (React Native / Expo) |
| `shared_lib/` | OpenAPI contract and TypeScript client library |
| `scripts/` | `bootstrap.sh` (rebuild a server from backup), `disaster-drill.sh`, demo |
| `deploy/` | systemd unit and an example config with every option |
| `docs/` | Guides, runbooks and design decisions |

## Development

```bash
make -C server test-race      # Go tests (server + backups + disaster drill)
make -C server drill          # lose-everything drill with the real binaries
(cd desktop_app && go test ./...)
pnpm install && pnpm -r test  # SDK (against a real server), web and phone tests
```

CI runs all of the above on every pull request.
