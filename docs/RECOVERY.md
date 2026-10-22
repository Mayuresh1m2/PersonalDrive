# Disaster recovery runbook

Pick the row that matches what you lost.

| Lost | Use | Cost / time |
| --- | --- | --- |
| A few files, still in the 30-day trash | the app's trash, or `DELETE`/`restore` via the API | free, instant |
| Files deleted longer ago, or an old version | `pdrive restore --target disk-b --snapshot <id> --include … --library /tmp/x`, then `pdrive import` | free, minutes |
| Disk A died | new disk, then [Replace disk A](#replace-disk-a) from disk B | free, hours |
| Laptop died (disks fine) | [New laptop, disks survived](#new-laptop-disks-survived) | free, minutes |
| Everything (fire, theft) | [Rebuild from S3](#rebuild-from-s3) using the recovery kit | ~$93/TB, ~2–3 days |

## Replace disk A

```bash
# format and mount the new disk (docs/LAPTOP_SERVER.md §1), then:
systemctl --user stop pdrive
pdrive restore --target disk-b --library /mnt/pdrive-a/library --plan
pdrive restore --target disk-b --library /mnt/pdrive-a/library
systemctl --user start pdrive
```

The restore puts back the original `.pdrive-volume` marker, so the server accepts
the new disk as the library. Files changed since the last disk-B backup (at most
an hour old) are lost from the server, but phones still have them and will
re-upload on their next sync.

## New laptop, disks survived

Install `pdrive`, plug in both disks, then restore only the metadata from disk B:

```bash
pdrive volume status   # should list nothing yet: there's no config
pdrive init --library /mnt/pdrive-a/library      # writes a fresh config; rerun with the same paths
# add the disk-b target to the config, then:
pdrive restore --target disk-b --meta --library /mnt/pdrive-a/library --force
rm ~/.local/share/pdrive/RESTORE_IN_PROGRESS    # the library is intact; no file restore needed
systemctl --user enable --now pdrive
```

## Rebuild from S3

You need:
1. **The recovery kit** (`kit.yaml`).
2. If the kit has no S3 keys: AWS credentials that can read the bucket (export
   `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`).
3. A Linux machine and a disk at least as large as the library.

```bash
git clone https://github.com/Mayuresh1m2/PersonalDrive && cd PersonalDrive
# mount the new library disk at /mnt/pdrive-a (docs/LAPTOP_SERVER.md §1)
scripts/bootstrap.sh --kit ~/kit.yaml --library /mnt/pdrive-a/library
```

The script:
1. finds or downloads `pdrive`;
2. restores users, devices, settings and the TLS CA within minutes, because they're
   stored in S3 Standard;
3. shows the library restore plan with its cost and asks before spending money;
4. starts the server read-only, so people can browse their file list while data
   arrives;
5. requests a Deep Archive thaw (Bulk tier: up to 48h, cheapest), waits, then
   downloads and verifies every file. Follow it with
   `journalctl --user -u pdrive-restore -f`. If anything interrupts it, run the
   script again; it resumes.

When it finishes, the server switches to read-write by itself, and phones and
laptops reconnect without re-pairing.

Then:
- `pdrive backup passwd`: set the backup passphrase. It accepts the recovery key.
- Add a new disk B (`docs/BACKUP.md` §1) and `pdrive backup init disk-b`.
- Thumbnails regenerate in the background.

Need it faster? `--tier Standard` thaws within ~12h for ~8× the retrieval cost
(~$20/TB instead of $2.50). Download cost is the same either way.

---

## Recovery kit template

`pdrive backup init` prints this. If you have to recreate it by hand:

```yaml
# PersonalDrive RECOVERY KIT — keep this OFF the server.
server: <laptop hostname>
created: 2026-10-23T00:00:00Z
target:
  name: s3
  type: s3
  bucket: <bucket>
  prefix: home
  region: <region>
  storage_class: DEEP_ARCHIVE
  # access_key_id / secret_access_key: a READ-ONLY recovery key (optional)
recovery_key: XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX-XXXX
```

Keep one printed copy somewhere other than the house, and one in a password
manager.
