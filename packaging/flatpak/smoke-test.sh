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
expect() { grep -qx "$1" <<<"$perms" || fail "expected $1 in: $perms"; }
expect 'shared=network;ipc;'
# Flatpak writes --socket=fallback-x11 as "x11;wayland;fallback-x11" for older clients, which
# still apply the fallback; anything beyond those three (pulseaudio, session-bus, ...) fails.
sockets=$(sed -n 's/^sockets=//p' <<<"$perms")
for s in wayland fallback-x11; do [[ ";$sockets" == *";$s;"* ]] || fail "socket $s missing: $sockets"; done
for s in ${sockets//;/ }; do
  case $s in (x11|wayland|fallback-x11) ;; (*) fail "unexpected socket $s" ;; esac
done
expect 'devices=dri;'
grep -qE '^(filesystems|persistent)=' <<<"$perms" && fail "unexpected permissions: $perms"
grep -qE '^\[(Session|System) Bus Policy\]' <<<"$perms" && fail "unexpected bus names: $perms"
pass "permissions are network, display and GPU only"

flatpak run --command=sh "$APP" -c 'ls /usr/lib/*/libwebkitgtk-6.0.so.4 /usr/lib/*/libgtk-4.so.1 >/dev/null' \
  || fail "the runtime lacks GTK4 or WebKitGTK 6.0"
pass "the runtime provides GTK4 and WebKitGTK 6.0"

# Confinement: the sandbox cannot see the user's files, container sockets or hardware.
echo secret >"$secret"
flatpak run --command=sh "$APP" -c "test ! -e '$secret'" || fail "the sandbox can read a file in \$HOME"
flatpak run --command=sh "$APP" -c 'test ! -e /var/run/docker.sock && test ! -e /run/podman && test ! -e "$HOME/.ssh/id_rsa"' \
  || fail "the sandbox can see sockets or SSH keys"
flatpak run --command=sh "$APP" -c 'test ! -e /dev/bus/usb && test ! -e /dev/sda && test ! -e /dev/nvme0n1' \
  || fail "the sandbox can see host devices"
flatpak run --command=sh "$APP" -c '! command -v flatpak-spawn >/dev/null || ! flatpak-spawn --host true 2>/dev/null' \
  || fail "the sandbox can run host commands"
pass "no access to home files, SSH keys, container sockets, devices other than the GPU, or host commands"

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

# The window, where there is a display to draw on: it starts WebKit's sandboxed web process,
# and quits cleanly on SIGTERM (logging out of any BMC first). Skipped without xvfb-run.
# WebKit hands its sandbox arguments to flatpak-spawn, which refuses them without a UTF-8
# locale; desktops always have one, bare CI containers often don't.
if command -v xvfb-run >/dev/null; then
  winlog=$(mktemp)
  LANG=${LANG:-C.UTF-8} env -u WAYLAND_DISPLAY xvfb-run -a flatpak run "$APP" --idle-timeout 5m >"$winlog" 2>&1 &
  pid=$!
  for _ in $(seq 1 60); do pgrep -f 'webkitgtk-6.0/WebKitWebProcess' >/dev/null && break; sleep 0.5; done
  pgrep -f 'webkitgtk-6.0/WebKitWebProcess' >/dev/null || { cat "$winlog"; fail "the window did not start WebKit"; }
  sleep 3
  pkill -TERM -x ipmi-connect || { cat "$winlog"; fail "the window exited early"; }
  for _ in $(seq 1 30); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
  kill -0 "$pid" 2>/dev/null && { cat "$winlog"; fail "the window did not quit on SIGTERM"; }
  pid=
  grep -q 'Shutting down' "$winlog" || { cat "$winlog"; fail "no clean shutdown"; }
  rm -f "$winlog"
  pass "window started WebKit's sandboxed web process and quit on SIGTERM"
else
  echo "skip: window check (no xvfb-run)"
fi
