# Rolling out PersonalDrive without risking your data

**Rule: keep every existing copy of your photos and documents (old disks, phone,
Google Photos / iCloud) untouched until the last stage.** PersonalDrive only
*adds* copies until you deliberately turn on a deletion feature.

## Safe mode (the default)

Every feature that can permanently delete data is **off** until you turn it on in
the config file:

```yaml
safety:
  purge_trash: false       # trash is never emptied automatically
  permanent_delete: false  # no "Delete forever" button
  prune_backups: false     # backups only grow; old snapshots are never removed
  free_up_space: false     # phones can't delete photos from themselves
```

With these switches off, the deletion features behave like this:

| Feature | In safe mode |
| --- | --- |
| Deleting in the web app / API | Goes to the trash, and the trash is kept forever |
| `pdrive import` | Copies only, never touches the source |
| `pdrive restore` | Never overwrites an existing file with different content; lists it instead (`--overwrite` to replace) |
| `pdrive restore --meta` | Refuses to overwrite an existing database (`--force` to override) |
| Desktop sync (`pdrive-sync add`) | `upload-only` by default: copies up, never changes or deletes anything on either side |
| Phone app | Backup is upload-only. "Free up space" is hidden |
| Backups | Snapshots accumulate. Prune only runs as `--dry-run` |

The trade-off: storage only grows. That's fine for weeks or months, and Deep
Archive is ~$1/TB/month.

## Stage 1: server and imports (week 1)

**On:** server, web app (browse only), `pdrive import` from your existing disks.

```bash
pdrive dupes scan /media/old-disk          # how much dedup will save
pdrive import --user you --dest "Old Laptop" /media/old-disk/Pictures
pdrive verify-copy /media/old-disk/Pictures # must say "every file is in the library"
```

**Move on when:**
- `verify-copy` reports 0 missing for everything you imported;
- you've browsed the library in the web app;
- `pdrive reindex` (with the server stopped) reports no surprises.

## Stage 2: backups to disk B (week 1–2)

**On:** the `disk-b` target (hourly).

```bash
pdrive backup init disk-b
pdrive backup run disk-b
pdrive backup check disk-b --read 1 --local   # reads back every byte, re-hashes the library
# Practice: restore a folder somewhere else and compare
pdrive restore --target disk-b --include library/users/you/Documents --library /tmp/restore-test
diff -r /tmp/restore-test/users/you/Documents /mnt/pdrive-a/library/users/you/Documents
```

**Move on when:** checks pass for a week and the practice restore matches exactly.

## Stage 3: offsite backup to S3 (week 2–3)

**On:** the `s3` target (nightly). **Save the recovery kit** (paper + password
manager).

```bash
pdrive backup init s3 --kit-out ~/kit.yaml
pdrive backup run s3                          # the first upload can take days; it resumes
pdrive backup check s3 --local
pdrive restore --target s3 --library /tmp/x --plan   # see what a disaster restore costs
```

Optional real drill (costs cents): restore one small folder from S3 with
`--tier Standard`.

**Move on when:** both targets show recent successes on the Server page, and the
health page shows no warnings.

## Stage 4: phones (week 3–4)

**On:** phone app camera-roll backup ("new photos only" first, then "everything").

**Move on when:** the phone's photo count matches `Camera Roll/` in the library,
and those photos are in a disk-B and S3 snapshot (Server page → Snapshots).

## Stage 5: desktop sync, gently (week 4+)

1. `pdrive-sync add ~/Documents --mode upload-only`. Watch for a week.
2. Then `--mode two-way` (without `--sync-deletes`): edits flow both ways,
   deletions don't.
3. Only later, if you want it: `--mode two-way --sync-deletes`. Files deleted on
   another device go to `pdrive-sync`'s own trash, not oblivion.

To change a folder's mode: `pdrive-sync remove <folder>`, then `add` it again.

## Stage 6: turning on deletion (after ~1–3 months of stable backups)

Turn these on one at a time, in this order, in the config file, then restart the
server:

1. `prune_backups: true`. Run `pdrive backup prune s3 --dry-run` first and read
   what it would delete. Deep Archive data younger than 180 days is never pruned
   anyway (early-deletion fees).
2. `purge_trash: true`. Trash older than `trash.retention` (30 days) is deleted.
   The files remain in backup snapshots taken while they existed, kept per the
   retention policy.
3. `permanent_delete: true`. Enables "Delete forever" in the web app.
4. `free_up_space: true`, last. Phones can then remove photos only if **all**
   of these hold: the server confirms it has the exact content; the photo was
   uploaded before the last complete backup to every target (disk B and S3);
   it's older than 30 days; it wasn't edited since. Phones also keep deleted
   photos in "Recently deleted" for 30 days.

**Only after stage 6** should you consider deleting old copies elsewhere (old
disks, Google Photos), and only after `pdrive verify-copy` reports 0 missing for
them.

## If something looks wrong

Stop the server (`systemctl --user stop pdrive`), and nothing changes. Every
stage is reversible: turning a switch back off takes effect on restart. File
history is in the trash, in disk-B snapshots (hourly) and in S3 snapshots
(nightly). See [RECOVERY.md](RECOVERY.md).
