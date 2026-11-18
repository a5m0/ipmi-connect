#!/usr/bin/env bash
# Captures the desktop-mode documentation screenshots: test/e2e/fakeapp in the native Wails
# window (GTK4 + WebKitGTK 6.0) under Xvfb at 2x scale, driven with xdotool. No BMC is contacted.
#
# Needs: libgtk-4-1 libwebkitgtk-6.0-4 xvfb xdotool imagemagick dbus-x11 metacity
# Run from the repository root: test/e2e/desktop-screenshots.sh [host-screen.png]
#
# The optional PNG (1024x768) is shown as the host's screen. Notes from making this work:
# WebKitGTK ignores XTEST clicks unless GDK_CORE_DEVICE_EVENTS=1, a window manager is needed for
# reliable focus, and a click on a text field leaves WebKit selecting text (later clicks are
# lost), so form fields are filled with the keyboard.
set -euo pipefail

OUT=docs/images
SCREEN="${1:-}"
WORK="$(mktemp -d)"
BIN="$WORK/fakeapp"
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$WORK"' EXIT

CGO_ENABLED=0 go build -tags desktop,purego,production -o "$BIN" ./test/e2e/fakeapp

export DISPLAY=:97
Xvfb "$DISPLAY" -screen 0 2720x1880x24 >/dev/null 2>&1 &
sleep 1
eval "$(dbus-launch --sh-syntax)"
metacity --replace >/dev/null 2>&1 &
sleep 1

win() { xdotool search --name 'IPMI Connect' | head -1; }
click() { xdotool mousemove --window "$(win)" $(($1 * 2)) $(($2 * 2)) click 1; sleep 0.4; } # CSS px
typekeys() { xdotool type --delay 60 "$1"; sleep 1; }
park() { xdotool mousemove --window "$(win)" 2400 "$1"; sleep 0.5; }
shot() { import -window "$(win)" "$OUT/$1"; }
start() {
  LANG=C.UTF-8 GDK_SCALE=2 GDK_CORE_DEVICE_EVENTS=1 FAKEAPP_DOCS=1 FAKEAPP_WINDOW=1 \
    FAKEAPP_SCREEN="$SCREEN" FAKEAPP_CONNECTED="$1" "$BIN" >"$WORK/app.log" 2>&1 &
  APP=$!
  sleep 7
}

# Connection manager and consent, filled in with the keyboard.
start 0
park 1600
typekeys 192.0.2.10; xdotool key Tab ctrl+a; typekeys ADMIN; xdotool key Tab; typekeys test-password
sleep 1
shot desktop-connection-manager.png
xdotool key Return; sleep 3
shot desktop-consent.png
kill "$APP"; wait "$APP" 2>/dev/null || true

# Console and the type-text panel, starting connected.
start 1
park 1700; sleep 1
shot desktop-console.png
click 960 28 # Paste
typekeys 'mount -o loop,ro /dev/sr0 /mnt/cdrom && ls /mnt/cdrom'
click 216 833 # Press Enter after
park 400
shot desktop-type-text.png
echo "wrote $OUT/desktop-connection-manager.png, desktop-consent.png, desktop-console.png, desktop-type-text.png"
