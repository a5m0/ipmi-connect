#!/usr/bin/env bash
# Tests scripts/ubuntu-netinstall-iso.sh in --config-only mode (no network, no iPXE build).
# Run: scripts/ubuntu-netinstall-iso_test.sh
set -euo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ubuntu-netinstall-iso.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fails=0

fail() { echo "FAIL: $*" >&2; fails=$((fails + 1)); }
ok() { echo "ok: $*"; }
has() { grep -qF -- "$2" "$1" || fail "$1 lacks: $2"; }
lacks() { if grep -qF -- "$2" "$1"; then fail "$1 contains: $2"; fi; }

# parse_yaml checks user-data is valid YAML when a YAML parser is available.
parse_yaml() {
  local py=(python3)
  if ! python3 -c 'import yaml' 2>/dev/null; then
    if command -v uv >/dev/null; then py=(uv run -q --no-project --with pyyaml python3); else
      echo "skip: no YAML parser for $1"; return 0
    fi
  fi
  "${py[@]}" -I -c '
import sys, yaml
d = yaml.safe_load(open(sys.argv[1]))
a = d["autoinstall"]
keys = a["user-data"]["users"][0]["ssh_authorized_keys"]
print("\n".join(keys))
' "$1" >"$1.keys" || fail "$1 is not valid autoinstall YAML"
}

cat >"$TMP/keys" <<'EOF'
# comment lines and blank lines are skipped

ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyForTestsOnly000000000000000000000 alice@example: it's "quoted" # not a comment
sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29tAAAAIFakeSecurityKey0000000000000000000000AAAABHNzaDo= bob@example
EOF

# Static address.
out="$TMP/static"
"$SCRIPT" --config-only "$out" --point-release 26.04.1 --ssh-keys "$TMP/keys" \
  --ip 192.0.2.10/30 --gateway 192.0.2.9 --dns "192.0.2.53 198.51.100.53" \
  --hostname web-01 --user admin >/dev/null
for f in boot.ipxe cidata/user-data cidata/meta-data; do [[ -s "$out/$f" ]] || fail "static: no $f"; done
has "$out/boot.ipxe" "set ip      192.0.2.10"
has "$out/boot.ipxe" "set mask    255.255.255.252"
has "$out/boot.ipxe" "set gw      192.0.2.9"
# The kernel comes from the series folder: the only place Ubuntu keeps it paired with the
# current point release's ISO (archived netboot files are stale).
has "$out/boot.ipxe" "set base    https://releases.ubuntu.com/26.04/netboot/amd64"
has "$out/boot.ipxe" "set iso     https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-amd64.iso"
has "$out/boot.ipxe" "Built $(date +%Y-%m-%d) for 26.04.1: rebuild it once a newer 26.04.x is out."
lacks "$out/boot.ipxe" "old-releases"
has "$out/boot.ipxe" "--timeout 300000 --default local"
# shellcheck disable=SC2016 # iPXE syntax, not shell
has "$out/boot.ipxe" 'BOOTIF=01-${net${n}/mac:hexhyp}'
has "$out/cidata/user-data" "addresses: [192.0.2.10/30]"
has "$out/cidata/user-data" "via: 192.0.2.9"
has "$out/cidata/user-data" "addresses: [192.0.2.53, 198.51.100.53]"
has "$out/cidata/user-data" "hostname: web-01"
has "$out/cidata/user-data" "name: admin"
has "$out/cidata/user-data" "lock_passwd: true"
has "$out/cidata/meta-data" "local-hostname: web-01"
parse_yaml "$out/cidata/user-data"
if [[ -f "$out/cidata/user-data.keys" ]]; then
  diff <(grep -v '^#' "$TMP/keys" | grep .) "$out/cidata/user-data.keys" >/dev/null ||
    fail "static: SSH keys did not survive YAML quoting: $(cat "$out/cidata/user-data.keys")"
fi
ok "static address: boot script, netplan, user and keys"

# DHCP, password hash, short menu.
out="$TMP/dhcp"
# shellcheck disable=SC2016 # a literal crypt hash
"$SCRIPT" --config-only "$out" --release 24.04 --point-release 24.04.5 --ssh-keys "$TMP/keys" \
  --password-hash '$6$salt$hash' --menu-timeout 20 >/dev/null
has "$out/boot.ipxe" "dhcp net\${n}"
has "$out/boot.ipxe" "ip=dhcp"
lacks "$out/boot.ipxe" "set ip "
has "$out/boot.ipxe" "--timeout 20000 --default local"
has "$out/boot.ipxe" "set base    https://releases.ubuntu.com/24.04/netboot/amd64"
has "$out/cidata/user-data" "dhcp4: true"
has "$out/cidata/user-data" "lock_passwd: false"
has "$out/cidata/user-data" "passwd: '\$6\$salt\$hash'"
has "$out/cidata/user-data" "name: ubuntu"
parse_yaml "$out/cidata/user-data"
ok "DHCP, password hash and menu timeout"

# Refusals: each must fail and write nothing.
refuse() {
  local what="$1"
  shift
  if "$SCRIPT" --config-only "$TMP/bad" --point-release 26.04.1 "$@" >/dev/null 2>&1; then
    fail "accepted $what"
  elif [[ -e "$TMP/bad" ]]; then
    fail "$what: wrote output before refusing"
  fi
  rm -rf "$TMP/bad"
}
echo "not a key" >"$TMP/badkeys"
refuse "no SSH keys" --ssh-keys /dev/null
refuse "a file without keys" --ssh-keys "$TMP/badkeys"
refuse "an address without a prefix" --ssh-keys "$TMP/keys" --ip 192.0.2.10 --gateway 192.0.2.9
refuse "an address without a gateway" --ssh-keys "$TMP/keys" --ip 192.0.2.10/30
refuse "a bad address" --ssh-keys "$TMP/keys" --ip 192.0.2.300/30 --gateway 192.0.2.9
refuse "a gateway outside the subnet" --ssh-keys "$TMP/keys" --ip 192.0.2.10/30 --gateway 192.0.2.99
refuse "a bad hostname" --ssh-keys "$TMP/keys" --hostname 'a b'
refuse "a bad user name" --ssh-keys "$TMP/keys" --user 'root:x'
refuse "a bad release" --ssh-keys "$TMP/keys" --release 'x;y'
refuse "a point release of another series" --ssh-keys "$TMP/keys" --point-release 24.04.5
ok "bad input is refused before anything is written"

if ((fails)); then
  echo "$fails failure(s)" >&2
  exit 1
fi
echo "all passed"
