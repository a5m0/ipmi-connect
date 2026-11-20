#!/usr/bin/env bash
# Opt-in hardware smoke test for the AMI MegaRAC backend (e.g. ASUS ASMB8-iKVM). It talks to a
# REAL BMC, so it is never run by CI. It builds ipmi-connect, connects with --no-browser, waits
# for the KVM session to validate and the first video frame, holds the session, then sends
# SIGINT and checks the BMC was logged out cleanly without any reconnect.
#
# Nothing is sent to the host: no browser is attached, so no keyboard, mouse, power or virtual
# media input reaches it (ipmi-connect has no view-only flag; it only reads video here).
#
# Run from anywhere:
#   IPMI_CONNECT_SMOKE_HOST=bmc.example IPMI_CONNECT_SMOKE_USER=admin \
#   IPMI_CONNECT_SMOKE_PASSWORD=... test/e2e/megarac-smoke.sh
#
# Required: IPMI_CONNECT_SMOKE_HOST, IPMI_CONNECT_SMOKE_USER, IPMI_CONNECT_SMOKE_PASSWORD
# Optional: IPMI_CONNECT_SMOKE_FINGERPRINT  SHA-256 of the BMC certificate (--trust-fingerprint)
#           IPMI_CONNECT_SMOKE_LEGACY_TLS=1 pass --allow-legacy-tls
#           IPMI_CONNECT_SMOKE_HOLD         seconds to keep the session up (default 30)
#           IPMI_CONNECT_SMOKE_TIMEOUT      seconds to wait for validation and video (default 60)
#           IPMI_CONNECT_SMOKE_TYPE         --type value (default megarac)
#
# Trust: stdin is /dev/null, so ipmi-connect never prompts. A certificate that is neither pinned
# in your trust store nor given in IPMI_CONNECT_SMOKE_FINGERPRINT makes it exit with the
# fingerprint it saw; check that against the BMC and rerun with it. A fingerprint passed here is
# pinned in your normal trust store, like --trust-fingerprint on the command line.
#
# The password reaches the child only through $IPMI_CONNECT_PASSWORD (never argv) and is never
# printed; the console link (which signs a browser in) is filtered out of every excerpt.
set -euo pipefail

die() { echo "megarac-smoke: $*" >&2; exit 2; }

HOST="${IPMI_CONNECT_SMOKE_HOST:-}"
USER_NAME="${IPMI_CONNECT_SMOKE_USER:-}"
PASSWORD="${IPMI_CONNECT_SMOKE_PASSWORD:-}"
FINGERPRINT="${IPMI_CONNECT_SMOKE_FINGERPRINT:-}"
LEGACY_TLS="${IPMI_CONNECT_SMOKE_LEGACY_TLS:-}"
HOLD="${IPMI_CONNECT_SMOKE_HOLD:-30}"
TIMEOUT="${IPMI_CONNECT_SMOKE_TIMEOUT:-60}"
TYPE="${IPMI_CONNECT_SMOKE_TYPE:-megarac}"
EXIT_TIMEOUT=45 # the app waits for the KVM session to close, then logs out of the web UI

missing=()
[[ -n "$HOST" ]] || missing+=(IPMI_CONNECT_SMOKE_HOST)
[[ -n "$USER_NAME" ]] || missing+=(IPMI_CONNECT_SMOKE_USER)
[[ -n "$PASSWORD" ]] || missing+=(IPMI_CONNECT_SMOKE_PASSWORD)
((${#missing[@]} == 0)) || die "missing required environment: ${missing[*]} (see the header of $0)"
[[ "$HOLD" =~ ^[0-9]+$ ]] || die "IPMI_CONNECT_SMOKE_HOLD must be a whole number of seconds"
[[ "$TIMEOUT" =~ ^[0-9]+$ ]] || die "IPMI_CONNECT_SMOKE_TIMEOUT must be a whole number of seconds"
command -v go >/dev/null || die "go is not installed"
command -v setsid >/dev/null || die "setsid (util-linux) is not installed"

cd "$(dirname "$0")/../.."
WORK="$(mktemp -d)"
BIN="$WORK/ipmi-connect"
LOG="$WORK/app.log"
CHILD=""

# stop_child sends exactly one SIGINT (a second one makes ipmi-connect quit without logging out)
# and waits for the child, killing it if it does not exit in time.
stop_child() {
  [[ -n "$CHILD" ]] || return 0
  local pid="$CHILD" i
  CHILD=""
  kill -0 "$pid" 2>/dev/null || { wait "$pid" 2>/dev/null || true; return 0; }
  kill -INT "$pid" 2>/dev/null || true
  for ((i = 0; i < EXIT_TIMEOUT; i++)); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "megarac-smoke: ipmi-connect did not exit ${EXIT_TIMEOUT}s after SIGINT; killing it" >&2
    kill -KILL "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}
cleanup() {
  trap - EXIT INT TERM
  stop_child
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# excerpt prints the app's own log lines without the console link and with the password masked.
excerpt() {
  local line
  [[ -f "$LOG" ]] || return 0
  echo "--- ipmi-connect log (console link removed) ---"
  while IFS= read -r line; do
    # The console link (http://127.0.0.1:<port>/#c=<code>) signs a browser in: drop any line
    # with a URL carrying a fragment or query; plain URLs in error messages stay.
    [[ "$line" =~ https?://[^[:space:]]*[#?] ]] && continue
    printf '%s\n' "${line//"$PASSWORD"/***}"
  done < <(tail -n 40 "$LOG")
  echo "---"
}
fail() {
  echo "FAIL: $*"
  excerpt
  if grep -q -- '--trust-fingerprint' "$LOG" 2>/dev/null; then
    echo "The BMC certificate is not trusted. Check the fingerprint above against the BMC and set"
    echo "IPMI_CONNECT_SMOKE_FINGERPRINT to it (or connect once interactively to pin it)."
  fi
  if grep -q -- '--allow-legacy-tls' "$LOG" 2>/dev/null; then
    echo "The BMC needs obsolete TLS: set IPMI_CONNECT_SMOKE_LEGACY_TLS=1."
  fi
  exit 1
}
logged() { grep -qF -- "$1" "$LOG"; }

echo "Building ipmi-connect..."
CGO_ENABLED=0 go build -trimpath -o "$BIN" ./cmd/ipmi-connect

args=("$HOST" --type "$TYPE" --user "$USER_NAME" --no-browser --web-port 0 --idle-timeout 0)
[[ -n "$FINGERPRINT" ]] && args+=(--trust-fingerprint "$FINGERPRINT")
[[ "$LEGACY_TLS" == 1 ]] && args+=(--allow-legacy-tls)

echo "Connecting (type $TYPE, no input is sent to the host)..."
# setsid: a Ctrl+C on this script must not reach the child as well as the trap's SIGINT.
IPMI_CONNECT_PASSWORD="$PASSWORD" setsid "$BIN" "${args[@]}" </dev/null >"$LOG" 2>&1 &
CHILD=$!

for want in "KVM session validated" "Host video mode"; do
  deadline=$((SECONDS + TIMEOUT))
  until logged "$want"; do
    kill -0 "$CHILD" 2>/dev/null || fail "ipmi-connect exited before logging \"$want\""
    ((SECONDS < deadline)) || fail "no \"$want\" within ${TIMEOUT}s"
    sleep 1
  done
  echo "ok: $want"
done
grep -F "Host video mode" "$LOG" | head -1 | sed 's/^.*\] //'

echo "Holding the session for ${HOLD}s..."
deadline=$((SECONDS + HOLD))
while ((SECONDS < deadline)); do
  kill -0 "$CHILD" 2>/dev/null || fail "ipmi-connect exited during the hold"
  sleep 1
done

echo "Sending SIGINT..."
stop_child

logged "Logged out of" || fail "no \"Logged out of\" after SIGINT"
if grep -qi reconnecting "$LOG"; then
  fail "the KVM session dropped and reconnected"
fi
echo "PASS: validated, video received, held ${HOLD}s, logged out cleanly, no reconnects"
