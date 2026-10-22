# Backups: disk B and S3 Deep Archive

```
disk A (library) ──hourly──► disk B (local, versioned)    free, fast restores
              └──nightly──► S3 Deep Archive (offsite)     ~$1/TB/month, for disasters
```

Both targets get the same thing: incremental, deduplicated, compressed, encrypted
snapshots of the library **plus** the server's database, secrets and TLS CA.
Format: [BACKUP_FORMAT.md](BACKUP_FORMAT.md). Design reasoning:
[ADR 0004](adr/0004-backup-format.md).

## 1. Disk B (local backup)

```bash
sudo mkdir -p /mnt/pdrive-b/backup && sudo chown "$USER" /mnt/pdrive-b/backup
pdrive volume init --role backup /mnt/pdrive-b/backup
```

Add it to the config file:

```yaml
backup:
  targets:
    - name: disk-b
      type: local
      path: /mnt/pdrive-b/backup
      # defaults: hourly; keep 24 hourly, 14 daily, 8 weekly, 24 monthly, 10 yearly
```

If disk B is unplugged, its backups fail with a clear error and the health page
warns you. They never write to the laptop SSD by mistake.

## 2. S3 Deep Archive (offsite)

1. Create a bucket in the AWS console. Keep "Block all public access" on. A region
   near you is fine.
2. Create an IAM user for the server with this policy (replace `BUCKET`):
   ```json
   {"Version": "2012-10-17", "Statement": [
     {"Effect": "Allow", "Action": ["s3:ListBucket"], "Resource": "arn:aws:s3:::BUCKET"},
     {"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:RestoreObject"],
      "Resource": "arn:aws:s3:::BUCKET/*"}]}
   ```
   For the **recovery kit**, create a second user with only `s3:ListBucket`,
   `s3:GetObject` and `s3:RestoreObject`. A leaked kit then can't delete anything.
3. Add the target:
   ```yaml
   backup:
     targets:
       - name: s3
         type: s3
         bucket: my-pdrive-backup
         prefix: home
         region: ap-south-1
         access_key_id: AKIA...            # or omit and use ~/.aws/credentials / env
         secret_access_key: ...
         storage_class: DEEP_ARCHIVE         # default for s3 targets
         bandwidth_limit: 2000000            # bytes/s, so uploads don't saturate your link
         # defaults: nightly 02:00; keep 7 daily, 8 weekly, 12 monthly, 5 yearly
   ```
   The same target type works with Backblaze B2, Cloudflare R2, Wasabi or MinIO.
   Set `endpoint:` (and `path_style: true` for MinIO), and set `storage_class:
   STANDARD` because those services have no archive tiers.

## 3. Initialise and save the recovery kit

```bash
pdrive backup init disk-b
pdrive backup init s3 --kit-out ~/pdrive-recovery-kit.yaml
```

The first `init` asks for a **backup passphrase**. It's stored at
`<state>/secrets/backup-passphrase` (mode 0600) so scheduled backups can run, and
it never goes into the backup.

`init` prints a **recovery kit** with a **recovery key**, which is shown only once.
Print the kit or put it in a password manager, then delete the local file. With the
kit you can rebuild everything on a new machine. Without the kit and without the
passphrase, nobody can decrypt the backup, including you.

Lost the kit? Run `pdrive backup new-recovery-key s3` to make a new one (old keys
keep working).

## 4. Run

The server runs backups on schedule. By hand:

```bash
pdrive backup run            # all targets, then prune
pdrive backup status
pdrive backup snapshots s3
```

**The first S3 backup is slow.** A 1 TB library over a 20 Mbit/s uplink takes about
5 days. It checkpoints every 20 packs, so interruptions only lose minutes of work.
Later backups upload only what changed.

## What it costs (1 TB, AWS us-east-1; check current prices)

| Item | Cost |
| --- | --- |
| Deep Archive storage | ~$1.00/month |
| Metadata in Standard (trees, index, DB: ~1–2% of library) | ~$0.25–0.50/month |
| First upload: ~32,000 packs × $0.05/1000 PUT | ~$1.60 once |
| Nightly incrementals | cents/month |
| **Full disaster restore** (Bulk thaw $2.50 + download ~$90) | **~$93 once, 48h wait** |

Disk B makes most restores free and instant. S3 is for losing disk A **and** disk B.

## Retention and prune

Prune runs weekly and after `backup run`. It keeps snapshots per the retention
policy and deletes data no snapshot needs. Deep Archive bills every object for at
least 180 days, so prune **defers** deleting packs younger than that instead of
paying early-deletion fees, and never repacks archived data (that would need a
thaw). Small "hot" metadata packs are repacked when mostly unused.

```bash
pdrive backup prune s3 --dry-run
```

## Checks

The schedule runs these automatically: weekly for disk B, monthly for S3.

```bash
pdrive backup check s3 --local      # structure + re-hash library files (catches disk A bit rot)
pdrive backup check disk-b --read 1 # download and verify every pack (free on disk B)
pdrive backup check s3 --read 0.01 --thaw  # thaw ~1% of packs (costs cents), re-run after ~48h
```

## Alerts

```yaml
backup:
  notify:
    ntfy_url: https://ntfy.sh/<long-random-topic>   # push notifications on your phone (ntfy app)
    webhook_url: ""                                 # or any JSON webhook
    stale_after: 48h
```

You get an alert when a backup or check fails, and daily at 09:00 while a target
hasn't succeeded within `stale_after`.

## Practise a restore

Do this once now, not on the day you need it:

```bash
# restore one folder from disk B into a scratch directory
pdrive restore --target disk-b --include library/users/<you>/Documents --library /tmp/restore-test
# see what a full S3 restore would cost
pdrive restore --target s3 --library /tmp/x --plan
```

`scripts/disaster-drill.sh` runs a complete lose-everything-and-rebuild drill with
a fake S3. CI runs it on every change.
