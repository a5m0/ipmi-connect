#!/usr/bin/env bash
# Smoke test for an installed io.github.a5m0.IPMIConnect Flatpak: starts desktop mode without a
# browser, signs in over loopback, checks the API and the sandbox's confinement, and quits.
# Contacts no BMC. Needs curl and a session bus (CI: run under dbus-run-session).
set -euo pipefail
APP=io.github.a5m0.IPMIConnect
log=$(mktemp)
secret="$HOME/ipmi-connect-smoke-secret-$$.iso"
cleanup() { rm -f "$secret" "$log"; [[ -n "${pid:-}" ]] && kill "$pid" 2>/dev/null || true; }
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "ok: $*"; }

flatpak run "$APP" --version | grep -q '^ipmi-connect ' || fail "--version"
pass "runs in the sandbox: $(flatpak run "$APP" --version)"

perms=$(flatpak info --show-permissions "$APP")
grep -q 'shared=network' <<<"$perms" || fail "network permission missing"
grep -qE '^(filesystems|devices|sockets)=' <<<"$perms" && fail "unexpected permissions: $perms"
pass "permissions are network only"

# Confinement: the sandbox cannot see the user's files, container sockets or hardware.
echo secret >"$secret"
flatpak run --command=sh "$APP" -c "test ! -e '$secret'" || fail "the sandbox can read a file in \$HOME"
flatpak run --command=sh "$APP" -c 'test ! -e /var/run/docker.sock && test ! -e /run/podman && test ! -e "$HOME/.ssh/id_rsa"' \
  || fail "the sandbox can see sockets or SSH keys"
flatpak run --command=sh "$APP" -c 'test ! -e /dev/bus/usb && test ! -e /dev/dri && test ! -e /dev/sda && test ! -e /dev/nvme0n1' \
  || fail "the sandbox can see host devices"
flatpak run --command=sh "$APP" -c '! command -v flatpak-spawn >/dev/null || ! flatpak-spawn --host true 2>/dev/null' \
  || fail "the sandbox can run host commands"
pass "no access to home files, SSH keys, container sockets, devices or host commands"

flatpak run "$APP" --desktop --no-browser --idle-timeout 5m >"$log" 2>&1 &
pid=$!
for _ in $(seq 1 60); do grep -q 'Console: http' "$log" && break; sleep 0.5; done
url=$(grep -o 'http://127.0.0.1:[0-9]*/#c=[0-9a-f]*' "$log" | head -1)
[[ -n "$url" ]] || { cat "$log"; fail "no console link"; }
origin=${url%%/#*}
code=${url##*#c=}
pass "desktop mode listening on $origin"
[[ "$origin" =~ ^http://127\.0\.0\.1:[0-9]+$ ]] || fail "not bound to loopback: $origin"

body=$(curl -fsS -H 'Content-Type: application/json' -d "{\"code\":\"$code\"}" "$origin/api/session")
token=$(sed -n 's/.*"token":"\([0-9a-f]*\)".*/\1/p' <<<"$body")
[[ ${#token} -eq 64 ]] || fail "sign-in: $body"
curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d "{\"code\":\"$code\"}" "$origin/api/session" | grep -q 401 \
  || fail "the launch code worked twice"
pass "signed in with the one-time code; replay refused"

auth=(-H "Authorization: Bearer $token")
status=$(curl -fsS "${auth[@]}" "$origin/api/status")
grep -q '"phase":"idle"' <<<"$status" && grep -q '"desktop":true' <<<"$status" || fail "status: $status"
[[ $(curl -s -o /dev/null -w '%{http_code}' "$origin/api/status") == 401 ]] || fail "status without a token"
[[ $(curl -s -o /dev/null -w '%{http_code}' -H 'Host: evil.example' "${auth[@]}" "$origin/api/status") == 403 ]] || fail "foreign Host accepted"
pass "API answers, refuses unauthenticated and rebinding requests"

files=$(curl -fsS "${auth[@]}" "$origin/api/files?mode=file")
grep -q "/.var/app/$APP/" <<<"$files" || fail "media root is not the private folder: $files"
[[ $(curl -s -o /dev/null -w '%{http_code}' "${auth[@]}" "$origin/api/files?path=$(dirname "$secret")") == 400 ]] || fail "home folder listed"
pass "virtual media confined to the private folder"

curl -fsS -X POST "${auth[@]}" "$origin/api/shutdown" >/dev/null
for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
kill -0 "$pid" 2>/dev/null && fail "did not quit"
pid=
pass "quit from the API"
