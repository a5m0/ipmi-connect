# Flatpak sandbox

What the Flatpak build of ipmi-connect (`io.github.a5m0.IPMIConnect`) can and cannot do, and
what the sandbox does **not** protect. The permissions are in
[packaging/flatpak/io.github.a5m0.IPMIConnect.yml](../../packaging/flatpak/io.github.a5m0.IPMIConnect.yml).

## Permissions

| Permission | Granted | Why |
| :--- | :--- | :--- |
| `--share=network` | yes | Reach BMCs (and, with `--no-browser`, let a browser outside the sandbox reach the console on 127.0.0.1) |
| Filesystem (`--filesystem=…`) | **no** | Images come from the FileChooser portal or the private media folder |
| Home, host, SSH keys, Docker/Podman sockets | **no** | |
| `--socket=wayland`, `--socket=fallback-x11`, `--share=ipc` | yes | The console window (WebKitGTK). X11 only where there is no Wayland; `ipc` is X11 shared memory |
| `--device=dri` | yes | The GPU, for WebKit's compositing. No other devices |
| Session or system D-Bus (`--socket=session-bus`, `--talk-name`) | **no** | Portals (`org.freedesktop.portal.*`) are reachable by every Flatpak without a permission |
| `flatpak-spawn --host` (`org.freedesktop.Flatpak`) | **no** | |

`packaging/flatpak/smoke-test.sh` checks the installed app: permissions are exactly network,
display and GPU, a file created in `$HOME` is invisible, SSH keys and container sockets are
absent, no USB or disk devices, and `flatpak-spawn --host` fails. Where `xvfb-run` is available
it also starts the window and checks it quits cleanly.

The display permissions are what the window costs: a process with a Wayland or X11 connection
can draw windows, and under X11 (not Wayland) it can also read other X11 clients' input and
screen. WebKit renders the page in its own nested bubblewrap sandbox, and the page is our own
UI under a strict CSP. Users who prefer the browser can start the app with `--no-browser` and
open the printed link; the window permissions can then be revoked with
`flatpak override --user --nosocket=wayland --nosocket=fallback-x11 --nodevice=dri io.github.a5m0.IPMIConnect`. It ran successfully on x86_64 in development
(see [implementation-plan.md](../implementation-plan.md)); CI runs it on x86_64 and aarch64.

## Portals used

| Portal | For | Notes |
| :--- | :--- | :--- |
| OpenURI | Opening the console link in the default browser (CLI mode only; desktop mode uses the window) | Falls back to `xdg-open` (which in the runtime also uses the portal), then to the printed link |
| FileChooser (Save) | Saving a screenshot | The chosen file is written through the document portal; nothing else becomes visible |
| FileChooser + Documents | Choosing a disk image | The chosen file appears in the sandbox under `/run/user/<uid>/doc/…`; only that file. Random access goes through the document portal's FUSE filesystem, so large ISOs are read on demand, not copied into memory |

Unverified on real desktops (see [testing/hardware-validation.md](../testing/hardware-validation.md)):
whether a FileChooser-picked file is writable through the document portal (needed only for
writable `.img` images), and FUSE throughput for booting large ISOs.

### Private media folder

Images can also be copied into the app's private folder, which the sandbox can always read and
write:

```text
~/.var/app/io.github.a5m0.IPMIConnect/data/ipmi-connect/media/
```

### Optional: grant one folder

If you prefer a folder over the file chooser, grant just that folder, read-only:

```bash
flatpak override --user --filesystem=~/ISOs:ro io.github.a5m0.IPMIConnect
```

and start the app with `--media-dir ~/ISOs`. Avoid granting `~/Downloads` or `home`.

## Network: what Flatpak does not do

`--share=network` gives the sandbox the host's network namespace. **Flatpak cannot restrict
which hosts or ports the app connects to.** It is not a network isolation boundary, and it does
not make the unencrypted Supermicro KVM or obsolete TLS any safer.

What the application does instead (defence in depth, not a boundary against a compromised
process):

- The BMC is resolved once; every connection goes to that address (`bmc.Endpoint`).
- Only the ports the BMC's protocol uses are allowed (HTTPS, KVM, virtual media; JNLP-supplied
  ports are range-checked and added explicitly).
- HTTP redirects to another host, another port or plain http are refused.
- Proxy environment variables are ignored for BMC traffic.

### Stronger, external options (not built in)

These need privileges the Flatpak must not have, so they are documented, not automated:

- **Management VLAN** (recommended): put BMCs on a network only management machines can reach,
  and only from there.
- **Host firewall per user/cgroup**: nftables can match the systemd scope of the Flatpak
  instance (`app-flatpak-io.github.a5m0.IPMIConnect-*.scope`) with `socket cgroupv2` and allow
  only the BMC subnet. Example (adjust the subnet; run as root, outside the sandbox):

  ```text
  table inet ipmi_connect {
    chain out {
      type filter hook output priority 0; policy accept;
      socket cgroupv2 level 3 "user.slice/user-1000.slice/user@1000.service/app.slice" \
        ip daddr != { 127.0.0.0/8, 192.0.2.0/24 } meta l4proto tcp reject
    }
  }
  ```

  The cgroup level and path vary by distribution and systemd version; test it before relying
  on it. This was not validated in development.
- **Network namespaces**: run the native binary in a namespace that only routes to the BMC
  network (e.g. with `ip netns` or a VPN namespace). The browser must still reach its loopback
  port, so this suits the CLI with `--no-browser` better than desktop mode.

## ChromeOS (Crostini)

In Crostini the browser is ChromeOS's Chrome, outside the Linux container. It reaches the
container's loopback only through port forwarding, so desktop mode uses the fixed port 8080 and
`localhost` links there. Forward the port in ChromeOS settings (Developers → Linux → Port
forwarding) if it is not forwarded automatically. To use another port in the Flatpak:

```bash
flatpak override --user --env=IPMI_CONNECT_WEB_PORT=8085 io.github.a5m0.IPMIConnect
```

The forwarded port is reachable by ChromeOS's browser and by other Linux apps; the console
still requires its one-time link and token.
