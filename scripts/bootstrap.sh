#!/usr/bin/env bash
# Rebuild a PersonalDrive server on a new machine from a backup.
#
#   scripts/bootstrap.sh --kit recovery-kit.yaml --library /mnt/pdrive-a/library
#
# Steps (each is safe to re-run; restores resume where they stopped):
#   1. get the pdrive binary (given, on PATH, built from this checkout, or downloaded)
#   2. restore server metadata (DB, users, devices, TLS CA, config) — fast, from S3 Standard
#   3. show the library restore plan with size, wait time and cost
#   4. install + start the server as a systemd user service (read-only while restoring)
#   5. restore the library (in the background for Deep Archive, which takes up to 48h to thaw)
set -euo pipefail

KIT="" LIBRARY="" BINARY="" TIER="Bulk" SNAPSHOT="latest"
STATE="${XDG_DATA_HOME:-$HOME/.local/share}/pdrive"
CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/pdrive/config.yaml"
YES=0 SERVICE=1 FOREGROUND=0 FORCE=0
REPO="Mayuresh1m2/PersonalDrive"

usage() {
  cat <<EOF
usage: $0 --kit KIT.yaml --library DIR [options]

  --kit FILE        recovery kit printed by 'pdrive backup init'
  --library DIR     where disk A is mounted (empty directory on the new disk)
  --state DIR       server state directory (default $STATE)
  --config FILE     config file to write (default $CONFIG)
  --binary FILE     use this pdrive binary
  --snapshot ID     snapshot to restore (default latest)
  --tier TIER       Deep Archive retrieval tier: Bulk (cheapest, ≤48h) or Standard (≤12h)
  --yes             don't ask for confirmation
  --no-service      don't install/start the systemd service
  --foreground      run the library restore in this terminal
  --force           restore even if the library dir is not empty / low on space
EOF
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kit) KIT="$2"; shift 2 ;;
    --library) LIBRARY="$2"; shift 2 ;;
    --state) STATE="$2"; shift 2 ;;
    --config) CONFIG="$2"; shift 2 ;;
    --binary) BINARY="$2"; shift 2 ;;
    --snapshot) SNAPSHOT="$2"; shift 2 ;;
    --tier) TIER="$2"; shift 2 ;;
    --yes) YES=1; shift ;;
    --no-service) SERVICE=0; shift ;;
    --foreground) FOREGROUND=1; shift ;;
    --force) FORCE=1; shift ;;
    -h|--help) usage ;;
    *) echo "unknown option: $1" >&2; usage 1 ;;
  esac
done
[[ -n "$KIT" && -n "$LIBRARY" ]] || usage 1

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { echo "error: $*" >&2; exit 1; }
confirm() {
  [[ $YES == 1 ]] && return 0
  read -r -p "$1 [y/N] " ans
  [[ "$ans" == [yY]* ]]
}

[[ "$(uname -s)" == Linux ]] || die "only Linux is supported for now"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
[[ -f "$KIT" ]] || die "recovery kit not found: $KIT"
KIT="$(cd "$(dirname "$KIT")" && pwd)/$(basename "$KIT")"
chmod 600 "$KIT" 2>/dev/null || true

# 1. Binary -------------------------------------------------------------------
say "1/5 locating pdrive"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -z "$BINARY" ]]; then
  if command -v pdrive >/dev/null; then
    BINARY="$(command -v pdrive)"
  elif [[ -f "$SCRIPT_DIR/../server/go.mod" ]] && command -v go >/dev/null; then
    echo "building from source"
    make -C "$SCRIPT_DIR/../server" build >/dev/null
    BINARY="$SCRIPT_DIR/../server/bin/pdrive"
  else
    mkdir -p "$HOME/.local/bin"
    url="https://github.com/$REPO/releases/latest/download/pdrive-linux-$ARCH"
    echo "downloading $url"
    curl -fsSL -o "$HOME/.local/bin/pdrive.new" "$url"
    if curl -fsSL -o /tmp/pdrive.sha256 "$url.sha256" 2>/dev/null; then
      echo "$(cut -d' ' -f1 /tmp/pdrive.sha256)  $HOME/.local/bin/pdrive.new" | sha256sum -c - >/dev/null || die "checksum mismatch"
    fi
    chmod +x "$HOME/.local/bin/pdrive.new" && mv "$HOME/.local/bin/pdrive.new" "$HOME/.local/bin/pdrive"
    BINARY="$HOME/.local/bin/pdrive"
  fi
fi
BINARY="$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")"
echo "using $BINARY ($("$BINARY" version))"
PD=("$BINARY" --config "$CONFIG")

# Library disk sanity ----------------------------------------------------------
mkdir -p "$LIBRARY"
LIBRARY="$(cd "$LIBRARY" && pwd)"
if [[ -n "$(ls -A "$LIBRARY" 2>/dev/null | grep -v '^\.pdrive-volume$' || true)" && $FORCE == 0 ]]; then
  if [[ -f "$STATE/RESTORE_IN_PROGRESS" ]]; then
    echo "resuming an earlier restore into $LIBRARY"
  else
    die "$LIBRARY is not empty (use --force to restore into it anyway)"
  fi
fi
if [[ "$(df --output=source "$LIBRARY" | tail -1)" == "$(df --output=source / | tail -1)" ]]; then
  echo "warning: $LIBRARY is on the same disk as the system. Is the library disk mounted?"
  confirm "continue anyway?" || exit 1
fi

# 2. Metadata --------------------------------------------------------------------
say "2/5 restoring server metadata (users, devices, settings, TLS)"
if [[ -f "$STATE/pdrive.db" && ! -f "$STATE/RESTORE_IN_PROGRESS" ]]; then
  die "$STATE already holds a server database; refusing to overwrite it"
elif [[ -f "$STATE/RESTORE_IN_PROGRESS" ]]; then
  echo "metadata already restored (resuming)"
else
  "${PD[@]}" restore --kit "$KIT" --meta --library "$LIBRARY" --state "$STATE" --snapshot "$SNAPSHOT"
fi

# 3. Plan -------------------------------------------------------------------------
say "3/5 library restore plan"
plan_flags=(restore --kit "$KIT" --library "$LIBRARY" --state "$STATE" --snapshot "$SNAPSHOT" --tier "$TIER")
[[ $FORCE == 1 ]] && plan_flags+=(--force)
"${PD[@]}" "${plan_flags[@]}" --plan
confirm "start the library restore?" || { echo "stopped. Re-run this script to continue."; exit 0; }

# 4. Service --------------------------------------------------------------------
if [[ $SERVICE == 1 ]] && command -v systemctl >/dev/null; then
  say "4/5 installing the pdrive service"
  unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$unit_dir"
  sed -e "s|^ExecStart=.*|ExecStart=$BINARY --config $CONFIG serve --log-json|" \
    "$SCRIPT_DIR/../deploy/systemd/pdrive.service" >"$unit_dir/pdrive.service" 2>/dev/null ||
    cat >"$unit_dir/pdrive.service" <<EOF
[Unit]
Description=PersonalDrive home server
After=network-online.target
[Service]
ExecStart=$BINARY --config $CONFIG serve --log-json
Restart=on-failure
[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now pdrive
  loginctl enable-linger "$USER" 2>/dev/null || echo "note: run 'sudo loginctl enable-linger $USER' to keep the server running after logout"
  echo "server started (read-only until the library restore finishes)"
else
  say "4/5 skipping service install"
fi

# 5. Library ------------------------------------------------------------------------
say "5/5 restoring the library"
run_flags=("${plan_flags[@]}" --yes)
if [[ $FOREGROUND == 0 && $SERVICE == 1 ]] && command -v systemd-run >/dev/null; then
  envs=()
  for v in AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_REGION AWS_PROFILE PDRIVE_BACKUP_PASSPHRASE; do
    [[ -n "${!v:-}" ]] && envs+=(--setenv="$v=${!v}")
  done
  systemctl --user stop pdrive-restore 2>/dev/null || true
  systemd-run --user --unit pdrive-restore --collect "${envs[@]}" "${PD[@]}" "${run_flags[@]}" >/dev/null
  cat <<EOF
The library restore is running in the background (unit pdrive-restore).
Archived data can take up to 48h to thaw; the restore waits and then downloads.

  follow progress:   journalctl --user -u pdrive-restore -f
  if it stops:       re-run this script (it resumes)

When it finishes, the server switches to read-write by itself.
EOF
else
  "${PD[@]}" "${run_flags[@]}"
fi

cat <<EOF

Devices paired before keep working: the server's certificate authority and
device tokens were restored. To add a new device: $BINARY --config $CONFIG pair <user>
EOF
