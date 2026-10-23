#!/usr/bin/env bash
# Disaster drill with the real binaries: back up a server to an S3-compatible
# endpoint, delete the machine, rebuild it with bootstrap.sh from the recovery
# kit alone, and verify every file and account came back.
#
# Uses an in-memory fake S3 (pdrive-fakes3), so it's free and offline. To
# drill against real S3, see docs/BACKUP.md ("Practise a restore").
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
PORT=19900
cleanup() {
  for pid in ${S3_PID:-} ${SRV_PID:-}; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$WORK"
}
trap cleanup EXIT
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { echo "DRILL FAILED: $*" >&2; exit 1; }

step "build"
make -C "$ROOT/server" build >/dev/null
(cd "$ROOT/server" && go build -o bin/pdrive-fakes3 ./cmd/pdrive-fakes3)
PDRIVE="$ROOT/server/bin/pdrive"

"$ROOT/server/bin/pdrive-fakes3" -addr "127.0.0.1:$PORT" 2>"$WORK/s3.log" &
S3_PID=$!
sleep 0.5

# ---- the original machine ------------------------------------------------------
OLD="$WORK/old"
mkdir -p "$OLD/disk-a/library"
OPD=("$PDRIVE" --config "$OLD/config.yaml")
step "set up the original server"
"${OPD[@]}" init --library "$OLD/disk-a/library" --state "$OLD/state" --listen 127.0.0.1:18643 >/dev/null
python3 - "$OLD/config.yaml" "$PORT" <<'PY'
import sys, re
p, port = sys.argv[1], sys.argv[2]
s = open(p).read()
s = s.replace("mdns: true", "mdns: false")
s = re.sub(r"backup:\n(    .*\n)*", "", s)
s += f"""backup:
    targets:
        - name: offsite
          type: s3
          bucket: drill
          prefix: home
          region: us-east-1
          endpoint: http://127.0.0.1:{port}
          access_key_id: test
          secret_access_key: test
          path_style: true
          storage_class: STANDARD
          retention: {{last: 10}}
"""
open(p, "w").write(s)
PY
printf 'alice drill password\n' | "${OPD[@]}" user add alice --admin
printf 'bob drill password\n' | "${OPD[@]}" user add bob
"${OPD[@]}" space add "Family Photos" >/dev/null
"${OPD[@]}" space member "Family Photos" bob read

mkdir -p "$WORK/camera/2024" "$WORK/docs"
head -c 3000000 /dev/urandom >"$WORK/camera/2024/IMG_0001.jpg"
head -c 1500000 /dev/urandom >"$WORK/camera/2024/IMG_0002.jpg"
cp "$WORK/camera/2024/IMG_0001.jpg" "$WORK/docs/scan-copy.jpg"
python3 -c 'print("electricity bill\n" * 12000, end="")' >"$WORK/docs/bill.txt"
"${OPD[@]}" import --user alice --dest Camera "$WORK/camera" >/dev/null
"${OPD[@]}" import --user bob --dest Docs "$WORK/docs" >/dev/null

step "backup init + two backups"
printf 'drill backup passphrase\n' | "${OPD[@]}" backup init offsite --create-bucket --kit-out "$WORK/kit.yaml" >/dev/null
"${OPD[@]}" backup run 2>/dev/null
echo "a later document" >"$WORK/later.txt"
mkdir -p "$WORK/later" && mv "$WORK/later.txt" "$WORK/later/"
"${OPD[@]}" import --user alice --dest Later "$WORK/later" >/dev/null
"${OPD[@]}" backup run 2>/dev/null
"${OPD[@]}" backup check offsite --local
"${OPD[@]}" backup snapshots offsite

(cd "$OLD/disk-a/library" && find . -type f ! -path './.tmp/*' -exec sha256sum {} + | sort -k2) >"$WORK/before.txt"
CA_BEFORE=$(sha256sum "$OLD/state/tls/ca.pem" 2>/dev/null | cut -d' ' -f1 || true)

step "DISASTER: delete the laptop SSD, disk A and the config"
rm -rf "$OLD"

# ---- the new machine -------------------------------------------------------------
NEW="$WORK/new"
mkdir -p "$NEW/disk-a/library"
step "bootstrap from the recovery kit"
"$ROOT/scripts/bootstrap.sh" --kit "$WORK/kit.yaml" --library "$NEW/disk-a/library" \
  --state "$NEW/state" --config "$NEW/config.yaml" --binary "$PDRIVE" --yes --no-service --foreground

step "verify"
(cd "$NEW/disk-a/library" && find . -type f ! -path './.tmp/*' -exec sha256sum {} + | sort -k2) >"$WORK/after.txt"
diff -u "$WORK/before.txt" "$WORK/after.txt" || fail "library differs after restore"
echo "library: $(wc -l <"$WORK/after.txt") files identical"
[[ ! -f "$NEW/state/RESTORE_IN_PROGRESS" ]] || fail "restore marker still present"
NPD=("$PDRIVE" --config "$NEW/config.yaml")
"${NPD[@]}" user list | grep -q bob || fail "users missing"
"${NPD[@]}" space list | grep -q "bob:read" || fail "space membership missing"

"${NPD[@]}" serve >"$WORK/server.log" 2>&1 &
SRV_PID=$!
for _ in $(seq 50); do curl -sk https://127.0.0.1:18643/healthz >/dev/null 2>&1 && break; sleep 0.2; done
health=$(curl -s --cacert "$NEW/state/tls/ca.pem" https://localhost:18643/healthz)
[[ "$health" == *'"read_only":false'* ]] || fail "server not writable after restore: $health"
CA_AFTER=$(sha256sum "$NEW/state/tls/ca.pem" | cut -d' ' -f1)
[[ -z "$CA_BEFORE" || "$CA_BEFORE" == "$CA_AFTER" ]] || fail "TLS CA changed"
tok=$(curl -s --cacert "$NEW/state/tls/ca.pem" -X POST https://localhost:18643/api/v1/auth/login \
  -d '{"username":"bob","password":"bob drill password","device_name":"drill"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
curl -s --cacert "$NEW/state/tls/ca.pem" -H "Authorization: Bearer $tok" "https://localhost:18643/api/v1/files?path=Docs" |
  grep -q bill.txt || fail "bob can't see his files through the API"
atok=$(curl -s --cacert "$NEW/state/tls/ca.pem" -X POST https://localhost:18643/api/v1/auth/login \
  -d '{"username":"alice","password":"alice drill password","device_name":"drill"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
curl -s --cacert "$NEW/state/tls/ca.pem" -H "Authorization: Bearer $atok" https://localhost:18643/api/v1/admin/backup/status |
  python3 -c 'import json,sys; d=json.load(sys.stdin); t=d["targets"][0]; assert t["name"]=="offsite" and t["next_run"], d; print("backup API: target", t["name"], "next run", t["next_run"])' ||
  fail "backup status API"
echo "server: healthy, writable, same CA, bob can log in and list his files"

step "after recovery: set the passphrase with the recovery key, back up again"
RK=$(awk '/recovery_key:/ {print $2}' "$WORK/kit.yaml")
printf '%s\n%s\n' "$RK" "a new drill passphrase" | "${NPD[@]}" backup passwd
"${NPD[@]}" backup run offsite 2>/dev/null | head -2

printf '\n\033[1;32mDISASTER DRILL PASSED\033[0m\n'
