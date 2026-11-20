#!/usr/bin/env bash
# Builds a small (about 1 MB) BIOS boot ISO that installs Ubuntu Server over the network,
# unattended, for mounting as virtual media on a BMC:
#
#   1. iPXE (built from a pinned release, with HTTPS) shows a menu that boots the local disk
#      after --menu-timeout unless "Install" is picked, so a reboot with the ISO still mounted
#      does not reinstall.
#   2. Install: each NIC in turn gets the address (static or DHCP) and tries to download Ubuntu's
#      netboot kernel over HTTPS from releases.ubuntu.com. The NIC that works is the cabled one.
#   3. The installer downloads the live-server ISO into RAM (about 3 GB: the host needs roughly
#      6 GB of RAM) and runs autoinstall from the "cidata" volume, which is this ISO: keep it
#      mounted until the installer has started.
#   4. Autoinstall ERASES THE LARGEST DISK (LVM over all of it), installs OpenSSH (keys only),
#      creates the user with passwordless sudo, pins the network config to the boot NIC's MAC
#      and reboots.
#
# Rebuild the ISO right before you use it (it takes seconds). Netboot can only install the
# CURRENT point release of a series: Ubuntu keeps the netboot kernel paired with that release's
# ISO only in the series folder, and archived netboot files are stale (old-releases carries the
# beta kernel for 24.04.x and 25.04). Once a newer point release is out, an older ISO fails with
# "Unable to find a live file system on the network"; its menu shows the date it was built.
#
# Requirements: BIOS boot (or UEFI CSM), a roughly correct clock (HTTPS), direct internet access.
# Build tools (Debian/Ubuntu): git make gcc perl liblzma-dev isolinux syslinux-common xorriso curl
#
# Usage:
#   scripts/ubuntu-netinstall-iso.sh --ssh-keys ~/.ssh/id_ed25519.pub \
#     --ip 192.0.2.10/30 --gateway 192.0.2.9 [options]        # static address
#   scripts/ubuntu-netinstall-iso.sh --ssh-keys keys.pub        # DHCP
#
# Options:
#   --ssh-keys FILE        public keys for the user, one per line (required)
#   --ip ADDR/PREFIX       static address; omit for DHCP
#   --gateway ADDR         default gateway (required with --ip)
#   --dns "A B"            name servers (default: "1.1.1.1 8.8.8.8"; static only)
#   --hostname NAME        (default: ubuntu)
#   --user NAME            (default: ubuntu)
#   --password-hash HASH   console password, e.g. from `openssl passwd -6`; stored on the ISO.
#                          Without it the account has no password (SSH keys only).
#   --release SERIES       Ubuntu series (default: 26.04); its current point release is looked up
#   --point-release VER    for --config-only (offline): the point release to write in
#   --menu-timeout SECS    seconds before the menu boots the local disk (default: 300)
#   --out FILE             output ISO (default: ./ubuntu-netinstall.iso)
#   --work DIR             iPXE source and build cache (default: ~/.cache/ipmi-connect/netinstall)
#   --config-only DIR      only write boot.ipxe and cidata/ to DIR (no network, no build)
set -euo pipefail

IPXE_REPO=https://github.com/ipxe/ipxe.git
IPXE_TAG=v2.0.0
IPXE_COMMIT=12798ec29aa8a64d8675c4378b99f5fe28447afb

die() { echo "error: $*" >&2; exit 1; }
usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit "${1:-0}"; }

ssh_keys="" ip_cidr="" gateway="" dns="1.1.1.1 8.8.8.8" hostname="ubuntu" user="ubuntu"
password_hash="" release="26.04" point="" menu_timeout=300 out="./ubuntu-netinstall.iso"
work="${XDG_CACHE_HOME:-$HOME/.cache}/ipmi-connect/netinstall" config_only=""

while (($#)); do
  case "$1" in
    -h | --help) usage ;;
    --ssh-keys | --ip | --gateway | --dns | --hostname | --user | --password-hash | --release | \
      --point-release | --menu-timeout | --out | --work | --config-only)
      (($# >= 2)) || die "$1 needs a value"
      case "$1" in
        --ssh-keys) ssh_keys="$2" ;;
        --ip) ip_cidr="$2" ;;
        --gateway) gateway="$2" ;;
        --dns) dns="$2" ;;
        --hostname) hostname="$2" ;;
        --user) user="$2" ;;
        --password-hash) password_hash="$2" ;;
        --release) release="$2" ;;
        --point-release) point="$2" ;;
        --menu-timeout) menu_timeout="$2" ;;
        --out) out="$2" ;;
        --work) work="$2" ;;
        --config-only) config_only="$2" ;;
      esac
      shift 2
      ;;
    *) echo "unknown option: $1" >&2; usage 2 ;;
  esac
done

# --- Validation: everything is checked before anything is written. ------------------------

ipv4_re='^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$'
ip_to_int() { # prints the address as an integer, or fails
  [[ "$1" =~ $ipv4_re ]] || return 1
  local n=0 i o
  for i in 1 2 3 4; do
    o="${BASH_REMATCH[$i]}"
    ((10#$o <= 255)) || return 1
    n=$((n * 256 + 10#$o))
  done
  echo "$n"
}
int_to_ip() { echo "$(($1 >> 24 & 255)).$(($1 >> 16 & 255)).$(($1 >> 8 & 255)).$(($1 & 255))"; }

[[ -n "$ssh_keys" ]] || die "--ssh-keys is required"
[[ -r "$ssh_keys" ]] || die "cannot read $ssh_keys"
key_re='^(ssh-[a-z0-9-]+|ecdsa-sha2-[a-z0-9-]+|sk-[a-z0-9-]+@openssh\.com) [A-Za-z0-9+/]+=*( .*)?$'
keys=()
while IFS= read -r line || [[ -n "$line" ]]; do
  line="${line%$'\r'}"
  [[ -z "${line//[[:space:]]/}" || "$line" == \#* ]] && continue
  [[ "$line" =~ $key_re ]] || die "not an SSH public key in $ssh_keys: ${line:0:40}…"
  keys+=("$line")
done <"$ssh_keys"
((${#keys[@]})) || die "no SSH public keys in $ssh_keys"

if [[ -n "$ip_cidr" ]]; then
  [[ "$ip_cidr" == */* ]] || die "--ip needs a prefix length, e.g. 192.0.2.10/24"
  ip="${ip_cidr%/*}" prefix="${ip_cidr#*/}"
  ip_int=$(ip_to_int "$ip") || die "bad address: $ip"
  if ! [[ "$prefix" =~ ^[0-9]+$ ]] || ((prefix < 1 || prefix > 30)); then die "bad prefix length: $prefix"; fi
  [[ -n "$gateway" ]] || die "--gateway is required with --ip"
  gw_int=$(ip_to_int "$gateway") || die "bad gateway: $gateway"
  mask_int=$(((0xFFFFFFFF << (32 - prefix)) & 0xFFFFFFFF))
  (((ip_int & mask_int) == (gw_int & mask_int))) || die "gateway $gateway is not in $ip_cidr"
  ((ip_int != gw_int)) || die "the address and the gateway are the same"
  netmask=$(int_to_ip "$mask_int")
  read -r -a dns_list <<<"$dns"
  ((${#dns_list[@]})) || die "--dns needs at least one name server"
  for d in "${dns_list[@]}"; do ip_to_int "$d" >/dev/null || die "bad name server: $d"; done
elif [[ -n "$gateway" ]]; then
  die "--gateway needs --ip (without --ip the address comes from DHCP)"
fi

[[ "$hostname" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$ ]] ||
  die "bad hostname (lower-case letters, digits, '-' and '.'): $hostname"
[[ "$user" =~ ^[a-z_][a-z0-9_-]{0,31}$ && "$user" != root ]] || die "bad user name: $user"
[[ "$release" =~ ^[0-9]{2}\.[0-9]{2}$ ]] || die "bad release (e.g. 26.04): $release"
[[ -z "$point" || "$point" =~ ^${release//./\\.}(\.[0-9]+)?$ ]] ||
  die "bad point release for $release: $point"
if ! [[ "$menu_timeout" =~ ^[0-9]+$ ]] || ((menu_timeout < 5)); then die "--menu-timeout must be at least 5 seconds"; fi
[[ -z "$password_hash" || "$password_hash" =~ ^\$[0-9a-z]+\$[^[:space:]]+$ ]] ||
  die "--password-hash must be a crypt hash, e.g. from: openssl passwd -6"

# --- Point release: the netboot kernel and the live ISO must come from the same one. --------

if [[ -z "$config_only" ]]; then
  cfg_url="https://releases.ubuntu.com/$release/netboot/amd64/pxelinux.cfg/default"
  current=$(curl -fsSL "$cfg_url" | grep -oE "ubuntu-${release//./\\.}(\.[0-9]+)?-live-server-amd64\.iso" | head -1 |
    sed -E 's/^ubuntu-(.*)-live-server-amd64\.iso$/\1/') || true
  [[ -n "$current" ]] || die "could not find the current $release point release in $cfg_url"
  [[ -z "$point" || "$point" == "$current" ]] ||
    die "netboot can only install the current $release point release ($current), not $point"
  point="$current"
  iso_url="https://releases.ubuntu.com/$release/ubuntu-$point-live-server-amd64.iso"
  curl -fsSI "$iso_url" >/dev/null || die "$iso_url is not available"
  echo "==> Ubuntu $point"
elif [[ -z "$point" ]]; then
  die "--config-only needs --point-release (it does not use the network)"
fi

# --- Configuration. ---------------------------------------------------------------------

yaml_quote() { printf "'%s'" "${1//\'/\'\'}"; } # a YAML single-quoted scalar

write_boot_ipxe() {
  local f="$1" net_setup ip_args
  if [[ -n "$ip_cidr" ]]; then
    net_setup="set net\${n}/ip      \${ip}
set net\${n}/netmask \${mask}
set net\${n}/gateway \${gw}
set dns \${dns1}
ifopen net\${n} || goto nextnic"
    ip_args="ip=\${ip}::\${gw}:\${mask}:\${host}::off:\${dns1}:\${dns2}"
  else
    net_setup="dhcp net\${n} || goto nextnic"
    ip_args="ip=dhcp"
  fi
  {
    echo "#!ipxe"
    echo "# Ubuntu Server $point network install. Generated by scripts/ubuntu-netinstall-iso.sh."
    echo "# The ISO carrying this script is labelled \"cidata\" and holds the autoinstall config,"
    echo "# so it must stay mounted until the installer has started."
    echo
    if [[ -n "$ip_cidr" ]]; then
      echo "set ip      $ip"
      echo "set mask    $netmask"
      echo "set gw      $gateway"
      echo "set dns1    ${dns_list[0]}"
      echo "set dns2    ${dns_list[1]:-${dns_list[0]}}"
    fi
    echo "set host    $hostname"
    echo "# The series folder pairs its netboot kernel with the current point release's ISO."
    echo "set base    https://releases.ubuntu.com/$release/netboot/amd64"
    echo "set iso     https://releases.ubuntu.com/$release/ubuntu-$point-live-server-amd64.iso"
    echo
    echo "menu Ubuntu Server $point network install"
    echo "item --gap Built $(date +%Y-%m-%d) for $point: rebuild it once a newer $release.x is out."
    echo "item --gap Boots the local disk after $menu_timeout seconds unless you pick the installer."
    cat <<'EOF'
item --gap
item local    Boot from local disk
item install  Install Ubuntu Server  --  ERASES THE LARGEST DISK
item shell    iPXE shell
EOF
    echo "choose --timeout $((menu_timeout * 1000)) --default local target || goto local"
    cat <<'EOF'
goto ${target}

:local
echo Leaving iPXE: the BIOS continues with the next boot device.
exit 1

:shell
shell
goto local

:install
# Try each NIC in turn: the one that can fetch the kernel is the cabled one.
set n:int32 0
:nic
isset ${net${n}/mac} || goto nonic
echo
echo == net${n} (${net${n}/mac})
ifclose
EOF
    echo "$net_setup"
    echo "imgfree"
    echo "kernel \${base}/linux root=/dev/ram0 ramdisk_size=1500000 $ip_args BOOTIF=01-\${net\${n}/mac:hexhyp} iso-url=\${iso} ds=nocloud autoinstall --- || goto nextnic"
    cat <<'EOF'
initrd ${base}/initrd || goto nextnic
boot || goto nextnic

:nextnic
ifclose
inc n
goto nic

:nonic
echo
echo No NIC could download the installer from ${base}.
echo Check the cable/switch port, the address and gateway, and that the BIOS clock is
echo correct (HTTPS certificates are checked against it).
prompt Press any key for the iPXE shell...
shell
goto local
EOF
  } >"$f"
}

write_user_data() {
  local f="$1" k
  # The boot NIC is the one iPXE used: its MAC is BOOTIF=01-aa-bb-... on the kernel command line.
  local find_mac="mac=\$(sed -n 's/.*BOOTIF=01-\\([0-9a-fA-F-]*\\).*/\\1/p' /proc/cmdline | tr 'A-F-' 'a-f:')"
  {
    cat <<'EOF'
#cloud-config
# Generated by scripts/ubuntu-netinstall-iso.sh.
autoinstall:
  version: 1
  locale: en_US.UTF-8
  keyboard:
    layout: us
  timezone: Etc/UTC
  # Erases the largest disk: LVM over all of it.
  storage:
    layout:
      name: lvm
      sizing-policy: all
  ssh:
    install-server: true
    allow-pw: false
  apt:
    fallback: offline-install
  updates: security
  shutdown: reboot
EOF
    if [[ -n "$ip_cidr" ]]; then
      echo "  early-commands:"
      echo "    # Make sure the live system can resolve the Ubuntu mirrors."
      echo "    - |"
      echo "      $find_mac"
      echo "      for d in /sys/class/net/*; do"
      echo "        [ \"\$(cat \$d/address)\" = \"\$mac\" ] && resolvectl dns \"\${d##*/}\" ${dns_list[*]} || true"
      echo "      done"
    fi
    echo "  late-commands:"
    echo "    # Pin the network config to the NIC iPXE booted through (matched by MAC), replacing"
    echo "    # whatever the installer derived from its own boot network."
    echo "    - |"
    echo "      set -e"
    echo "      $find_mac"
    echo "      echo \"\$mac\" | grep -Eq '^([0-9a-f]{2}:){5}[0-9a-f]{2}\$'"
    echo "      rm -f /target/etc/netplan/*.yaml"
    echo "      cat > /target/etc/netplan/60-boot-nic.yaml <<EOF"
    echo "      network:"
    echo "        version: 2"
    echo "        ethernets:"
    echo "          uplink:"
    echo "            match:"
    echo "              macaddress: \"\$mac\""
    if [[ -n "$ip_cidr" ]]; then
      echo "            addresses: [$ip_cidr]"
      echo "            routes:"
      echo "              - to: default"
      echo "                via: $gateway"
      echo "            nameservers:"
      local servers="${dns_list[*]}"
      echo "              addresses: [${servers// /, }]"
    else
      echo "            dhcp4: true"
    fi
    echo "      EOF"
    echo "      chmod 600 /target/etc/netplan/60-boot-nic.yaml"
    echo "      echo 'network: {config: disabled}' > /target/etc/cloud/cloud.cfg.d/99-disable-network-config.cfg"
    echo "  user-data:"
    echo "    hostname: $hostname"
    echo "    ssh_pwauth: false"
    echo "    users:"
    echo "      - name: $user"
    echo "        groups: [sudo, adm]"
    echo "        shell: /bin/bash"
    if [[ -n "$password_hash" ]]; then
      echo "        lock_passwd: false"
      echo "        passwd: $(yaml_quote "$password_hash")"
    else
      echo "        lock_passwd: true"
    fi
    echo "        sudo: \"ALL=(ALL) NOPASSWD:ALL\""
    echo "        ssh_authorized_keys:"
    for k in "${keys[@]}"; do echo "          - $(yaml_quote "$k")"; done
  } >"$f"
}

write_config() { # DIR
  mkdir -p "$1/cidata"
  write_boot_ipxe "$1/boot.ipxe"
  write_user_data "$1/cidata/user-data"
  printf 'instance-id: ubuntu-netinstall-%s\nlocal-hostname: %s\n' "$(date +%s)" "$hostname" >"$1/cidata/meta-data"
}

if [[ -n "$config_only" ]]; then
  write_config "$config_only"
  echo "wrote $config_only/boot.ipxe and $config_only/cidata/"
  exit 0
fi

# --- Build. ---------------------------------------------------------------------------

missing=()
for c in git make gcc perl xorriso curl; do command -v "$c" >/dev/null || missing+=("$c"); done
[[ -e /usr/include/lzma.h ]] || missing+=("liblzma-dev")
[[ -e /usr/lib/ISOLINUX/isolinux.bin || -e /usr/share/syslinux/isolinux.bin ]] || missing+=("isolinux")
((${#missing[@]} == 0)) || die "missing: ${missing[*]} (Debian/Ubuntu: sudo apt-get install git make gcc perl liblzma-dev isolinux syslinux-common xorriso curl)"

mkdir -p "$work"
src="$work/ipxe-$IPXE_TAG"
if [[ ! -d "$src/.git" ]]; then
  echo "==> Fetching iPXE $IPXE_TAG"
  git clone -q --depth 1 --branch "$IPXE_TAG" "$IPXE_REPO" "$src"
fi
[[ "$(git -C "$src" rev-parse HEAD)" == "$IPXE_COMMIT" ]] ||
  die "$src is not iPXE $IPXE_TAG ($IPXE_COMMIT); delete it to fetch again"

cfg="$(mktemp -d)"
trap 'rm -rf "$cfg"' EXIT
write_config "$cfg"

echo "==> Building iPXE (BIOS, HTTPS enabled)"
# BIOS builds leave HTTPS out by default; the installer is fetched over HTTPS.
printf '#define DOWNLOAD_PROTO_HTTPS\n' >"$src/src/config/local/general.h"
LC_ALL=C make -s -C "$src/src" -j"$(nproc)" bin/ipxe.iso EMBED="$cfg/boot.ipxe" NO_WERROR=1 >/dev/null

echo "==> Writing $out"
# The iPXE ISO re-mastered: same boot image, plus the cloud-init seed, volume id "cidata".
rm -f "$out"
xorriso -indev "$src/src/bin/ipxe.iso" -outdev "$out" \
  -volid cidata -joliet on -rockridge on \
  -map "$cfg/cidata/user-data" /user-data \
  -map "$cfg/cidata/meta-data" /meta-data \
  -boot_image any replay >/dev/null 2>&1 || die "xorriso failed"
ls -l "$out"
sha256sum "$out"
