#!/usr/bin/env bash
# Phase 1 demo: a throwaway server with two users, an upload, instant dedup,
# a shared space, the change feed, and read-only mode when the disk "unplugs".
# Everything lives in a temp dir; nothing touches your real config.
set -euo pipefail

BIN="$(cd "$(dirname "$0")/../server" && pwd)/bin/pdrive"
[[ -x "$BIN" ]] || { echo "build first: make -C server build"; exit 1; }
command -v python3 >/dev/null || { echo "python3 is needed for JSON parsing"; exit 1; }

DEMO="$(mktemp -d)"
export PDRIVE_CONFIG="$DEMO/config.yaml"
PORT=18543
URL="https://localhost:$PORT"
CURL=(curl -sS --cacert "$DEMO/state/tls/ca.pem")
trap '[[ -n "${SERVER_PID:-}" ]] && kill "$SERVER_PID" 2>/dev/null; rm -rf "$DEMO"' EXIT
json() { python3 -c "import json,sys; print(json.load(sys.stdin)$1)"; }
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

step "init (disk A is a temp folder here)"
mkdir -p "$DEMO/disk-a"
"$BIN" init --library "$DEMO/disk-a" --state "$DEMO/state" --listen "127.0.0.1:$PORT" >/dev/null
sed -i 's/mdns: true/mdns: false/' "$PDRIVE_CONFIG"
printf 'alice demo password\n' | "$BIN" user add alice --admin
printf 'bob demo password\n' | "$BIN" user add bob
"$BIN" space add "Family Photos" >/dev/null
"$BIN" space member "Family Photos" alice write
"$BIN" space member "Family Photos" bob read

step "start server"
"$BIN" serve >"$DEMO/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 50); do [[ -f "$DEMO/state/tls/ca.pem" ]] && "${CURL[@]}" "$URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
"${CURL[@]}" "$URL/api/v1/server"; echo

login() { "${CURL[@]}" -X POST "$URL/api/v1/auth/login" -d "{\"username\":\"$1\",\"password\":\"$1 demo password\",\"device_name\":\"$1-demo\"}" | json "['access_token']"; }
ALICE=$(login alice); BOB=$(login bob)

step "alice uploads a 3 MiB file in parts"
head -c 3145728 /dev/urandom >"$DEMO/photo.raw"
SHA=$(sha256sum "$DEMO/photo.raw" | cut -d' ' -f1)
START=$("${CURL[@]}" -H "Authorization: Bearer $ALICE" -X POST "$URL/api/v1/uploads" \
  -d "{\"path\":\"Photos/photo.raw\",\"size\":3145728,\"sha256\":\"$SHA\"}")
ID=$(echo "$START" | json "['upload_id']"); PARTS=$(echo "$START" | json "['parts']"); PS=$(echo "$START" | json "['part_size']")
for ((n = 0; n < PARTS; n++)); do
  dd if="$DEMO/photo.raw" bs="$PS" skip="$n" count=1 status=none |
    "${CURL[@]}" -H "Authorization: Bearer $ALICE" -X PUT --data-binary @- "$URL/api/v1/uploads/$ID/parts/$n"
done
"${CURL[@]}" -H "Authorization: Bearer $ALICE" -X POST "$URL/api/v1/uploads/$ID/complete" | json "['file']['path']"

step "bob uploads the same photo: completes instantly, no bytes sent"
"${CURL[@]}" -H "Authorization: Bearer $BOB" -X POST "$URL/api/v1/uploads" \
  -d "{\"path\":\"Phone/IMG_0001.raw\",\"size\":3145728,\"sha256\":\"$SHA\"}" |
  python3 -c "import json,sys; d=json.load(sys.stdin); print('  ', d['status'], '| deduplicated =', d.get('deduplicated', False))"
stat -c '   %h links, inode %i  %n' "$DEMO"/disk-a/users/*/*/*.raw

step "alice's change feed"
"${CURL[@]}" -H "Authorization: Bearer $ALICE" "$URL/api/v1/sync/changes?cursor=0" |
  python3 -c "import json,sys; [print('  ', c['seq'], c['op'], c['path']) for c in json.load(sys.stdin)['changes']]"

step "unplug disk A (remove its marker) — server goes read-only within 30s"
mv "$DEMO/disk-a/.pdrive-volume" "$DEMO/marker.bak"
for _ in $(seq 40); do
  [[ "$("${CURL[@]}" "$URL/healthz" | json "['read_only']")" == "True" ]] && break; sleep 1
done
"${CURL[@]}" -H "Authorization: Bearer $ALICE" -X POST "$URL/api/v1/uploads" \
  -d "{\"path\":\"x\",\"size\":1,\"sha256\":\"$SHA\"}"; echo
mv "$DEMO/marker.bak" "$DEMO/disk-a/.pdrive-volume"
echo "   (marker restored; writes resume automatically on the next check)"

step "done — server log:"
cut -c1-160 "$DEMO/server.log"
