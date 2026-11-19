# Manual hardware validation checklist

Automated tests use fakes only: no real BMC, credentials, disk image or power action is touched
in CI or development. These checks need real hardware and a real desktop. Record the result,
date, firmware and ipmi-connect version for each; anything not checked here is **unverified**.

Use a test server whose host OS you can lose, on a management network.

## Before you start

- [ ] Note the BMC certificate fingerprint from a trusted source, e.g. on a machine on the
      management network: `openssl s_client -connect <bmc>:443 -tls1 </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256`
      (old OpenSSL builds may need `-cipher AES256-SHA@SECLEVEL=0`).

## Trust and TLS (both vendors)

- [ ] First connection shows a certificate dialog; its SHA-256 matches the one noted above.
- [ ] Obsolete TLS is reported (expected: TLS 1.0, `TLS_RSA_WITH_AES_256_CBC_SHA` on the
      Supermicro; TLS 1.2, `TLS_RSA_WITH_AES_256_GCM_SHA384` (not CBC) on the ASUS ASMB8;
      Dell's web server negotiates a suite Go supports, record which).
- [ ] After consent, a second connection asks nothing; the trust list shows the device.
- [ ] "Forget" makes the next connection ask again.
- [ ] Old firmware keys: the certificate's key is RSA ≥ 1024 bits (Go refuses smaller keys);
      record the key size shown.
- [ ] Changed certificate (regenerate it in the BMC, or point at another BMC under the same
      name): the connection is refused with a CHANGED warning until replaced.

## Dell PowerEdge C6220 II (Avocent 2.62.5)

- [ ] Port 7578 probe (APCP greeting + abandoned TLS handshake) does not disturb the BMC: the
      following login works, no stale session is left.
- [ ] Whether the KVM port presents the same certificate as the web server (one or two
      fingerprints in the dialog). Record it.
- [ ] Login, video, keyboard, mouse, macros, power status and actions work as before.
- [ ] Virtual media: ISO on CD boots an installer; IMG on removable/floppy, writable only after
      the confirmation, writes reach the file.
- [ ] Disconnect from the UI logs out (an immediate reconnect is not refused with "session
      already exists").
- [ ] Reconnect after a network interruption (unplug for ~20 s).
- [ ] No video first chunk larger than 4096×4096 is dropped in normal use (the log would say
      "implausible").

## Supermicro X8DTL-6F (ATEN 3.13)

- [ ] The insecure-channels consent lists KVM (plain RFB) and virtual media (RC4); the badge
      shows "Unencrypted channels" for the whole session.
- [ ] Video, keyboard, mouse (absolute position, buttons), power status and actions.
- [ ] Virtual media ISO and IMG as for Dell; byte-exact writes on an IMG.
- [ ] Framebuffer updates never exceed the 64 MiB per-update cap (no "not plausible" errors).
- [ ] JNLP KVM tokens are under 256 bytes (otherwise the ticket is refused).
- [ ] A mode change (BIOS → OS) still repaints: after the resize-once-per-update change,
      the picture recovers within a second or two.

## Desktop and lifecycle

- [ ] Desktop launcher starts the app and opens the browser (GNOME and KDE, X11 and Wayland).
- [ ] Connect, disconnect, connect to a second BMC without restarting.
- [ ] Closing the tab, then waiting out `--idle-timeout`: the BMC is logged out and the app quits.
- [ ] Quit button logs out and exits.
- [ ] Held key released when the tab is closed mid-keypress (hold Shift, close the tab; the host
      must not keep Shift held).

## Flatpak

- [ ] Install the bundle on Ubuntu 24.04 (x86_64) and an aarch64 machine; the launcher works.
- [ ] "Choose File…" opens the desktop file chooser; the chosen ISO boots the host (random
      access through the document portal; note boot time versus the native build).
- [ ] A chosen `.img` can be made writable and writes reach the file (document portal write
      access is unverified).
- [ ] Images in `~/.var/app/io.github.a5m0.IPMIConnect/data/ipmi-connect/media/` are listed.
- [ ] `packaging/flatpak/smoke-test.sh` passes on the machine.

## ChromeOS (Crostini)

- [ ] Native binary: `ipmi-connect` in the Linux terminal opens the console in ChromeOS Chrome
      at `http://localhost:8080/…`; forward port 8080 in Settings → Developers → Linux if needed.
- [ ] Flatpak in Crostini (needs `flatpak` and `xdg-desktop-portal-gtk` installed in the
      container): launcher, browser opening, FileChooser.
- [ ] The `localhost` launch link works (Chrome may resolve it to `::1` first; the server listens
      on 127.0.0.1).

## macOS

- [ ] `ipmi-connect` (desktop mode) opens Safari/Chrome; trust store under
      `~/Library/Application Support/ipmi-connect/`.

## MegaRAC smoke test

`test/e2e/megarac-smoke.sh` is an opt-in check against a real AMI MegaRAC BMC (e.g. the ASUS
ASMB8-iKVM); CI never runs it. It builds ipmi-connect, connects with `--no-browser` (no browser
is attached, so no keyboard, mouse, power or media input reaches the host), waits for
`KVM session validated` and the first `Host video mode`, holds the session, sends SIGINT and
passes only if the BMC was logged out (`Logged out of`) and nothing reconnected.

```sh
IPMI_CONNECT_SMOKE_HOST=bmc.example IPMI_CONNECT_SMOKE_USER=admin \
IPMI_CONNECT_SMOKE_PASSWORD=... test/e2e/megarac-smoke.sh
```

Optional: `IPMI_CONNECT_SMOKE_FINGERPRINT` (the SHA-256 noted above, passed as
`--trust-fingerprint` and pinned in your trust store), `IPMI_CONNECT_SMOKE_LEGACY_TLS=1`
(`--allow-legacy-tls`), `IPMI_CONNECT_SMOKE_HOLD` (seconds, default 30),
`IPMI_CONNECT_SMOKE_TIMEOUT` (seconds to wait for validation and video, default 60) and
`IPMI_CONNECT_SMOKE_TYPE` (default `megarac`). It never prompts: with an unpinned certificate and
no fingerprint it fails and shows the fingerprint the BMC presented; check it before rerunning
with it. The password reaches ipmi-connect only through `$IPMI_CONNECT_PASSWORD`, and the
console link is never printed.

- [ ] `test/e2e/megarac-smoke.sh` passes (record the video mode and hold time).
