# Running PersonalDrive on a laptop

The server runs on an always-on Ubuntu laptop. The library lives on external disk A; the
local backup (Phase 2) goes to external disk B.

## 1. Prepare the disks (when you plug them in)

Format disk A as **ext4** (hardlink dedup) or **btrfs** (reflink dedup, safest). exFAT/NTFS
work but can't dedup. Mount both disks by UUID so the USB port doesn't matter:

```bash
lsblk -f                         # find the UUIDs
sudo mkdir -p /mnt/pdrive-a /mnt/pdrive-b
# /etc/fstab — nofail lets the laptop boot without the disks
UUID=<disk-a-uuid>  /mnt/pdrive-a  ext4  defaults,nofail,x-systemd.device-timeout=10  0 2
UUID=<disk-b-uuid>  /mnt/pdrive-b  ext4  defaults,nofail,x-systemd.device-timeout=10  0 2
sudo mount -a
sudo mkdir -p /mnt/pdrive-a/library && sudo chown "$USER" /mnt/pdrive-a/library
```

Before importing, see how much dedup will save on your existing data:

```bash
pdrive dupes scan /mnt/old-disk/Pictures /mnt/old-disk/Documents
```

## 2. Install and initialise

```bash
make -C server build                       # builds server/bin/pdrive
sudo install server/bin/pdrive /usr/local/bin/
pdrive init --library /mnt/pdrive-a/library  # writes ~/.config/pdrive/config.yaml + disk marker
pdrive user add <you> --admin
pdrive space add "Family Photos"
pdrive space member "Family Photos" <you> write
pdrive import --user <you> --dest "Old Laptop" /mnt/old-disk/Pictures   # optional
```

If disk A isn't plugged in yet, `init` still writes the config. Run
`pdrive volume init --role library /mnt/pdrive-a/library` once it is mounted.

## 3. Run it as a service

```bash
mkdir -p ~/.config/systemd/user
cp deploy/systemd/pdrive.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now pdrive
loginctl enable-linger "$USER"     # keep running when you're logged out
journalctl --user -u pdrive -f     # logs (the first-run setup code appears here)
```

## 4. Keep the laptop awake with the lid closed

```bash
# /etc/systemd/logind.conf
HandleLidSwitch=ignore
HandleLidSwitchExternalPower=ignore
sudo systemctl restart systemd-logind
# never suspend
sudo systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target
```

Also turn off "Automatic suspend" in GNOME Settings → Power, and keep the laptop on its
charger. Consider limiting battery charge to ~80% in the BIOS, if supported.

## 5. What happens when…

| Event | Behaviour |
| --- | --- |
| Disk A unplugged | Server switches to read-only within 30s, health shows a warning, uploads get `503 library_unavailable` and phones keep them queued. Plug it back in and writes resume automatically. |
| A different disk mounted at the same path | Refused (marker id mismatch): read-only until the right disk is back. |
| Laptop leaves home | Phones and laptops can't reach it and queue uploads; they sync when it's back on the Wi-Fi. |
| Laptop IP changes | The server certificate is re-issued automatically; clients pin the CA, so nothing needs re-pairing. Use `<hostname>.local`. |
| Power cut mid-upload | Partial uploads stay in `staging/` and resume; files are only ever renamed into place complete. |

## Firewall

```bash
sudo ufw allow from 192.168.0.0/16 to any port 8443 proto tcp   # adjust to your LAN
sudo ufw allow 5353/udp                                          # mDNS discovery
```

Don't port-forward 8443 on your router. Remote access will come later via Tailscale/WireGuard.
