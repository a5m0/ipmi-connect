# Native window (Wails v3)

Status: Linux working, Flatpak moved to it; macOS compiles but has not been run. The plain
native build is unchanged and still opens the user's browser; the window exists only in builds
with `-tags desktop`.

![The connection manager in a native WebKitGTK window](../images/desktop-window-spike.png)

## What was built

- `cmd/ipmi-connect/window_desktop.go` (`desktop` tag): a Wails v3.1.0 application with one
  window. `run()` starts `web.Server` as before, but in desktop mode it hands the launch link to
  the window in memory instead of starting a browser. The page is the existing UI at
  `http://127.0.0.1:<port>/`, so the Host/Origin checks, token, WebSocket tickets and CSP
  stay exactly as they are.
- `window_default.go` (`!desktop`): returns no window, so the default build links no Wails code
  (`go version -m` shows no Wails module) and keeps opening the browser.
- Native dialogs: the server's new `SaveFile` hook is Wails' Save dialog, and `PickFile` falls
  back to Wails' Open dialog where the FileChooser portal is unavailable. Inside a Flatpak both
  go through the portal, because GTK4 uses it there.
- Closing the window quits through the normal path (`shutdown()`, then `Disconnect`). The Quit
  button, idle timeout and SIGTERM close the window.
- `--no-browser` still prints the link and opens no window (for scripts and the smoke test).
- `make desktop` builds `bin/ipmi-connect-desktop` with `-tags desktop,purego,production`.
  `production` turns off Wails' debug mode and WebKit's inspector.

## Build

Wails v3.1 has an experimental **purego** Linux backend: it `dlopen`s GTK4 and WebKitGTK 6.0
at run time, so the build needs neither cgo nor a C toolchain.

| Build | Result |
| :--- | :--- |
| `CGO_ENABLED=0 go build -tags desktop,purego,production` (linux/amd64) | Works; 15 MB stripped, dynamically linked to libc only |
| Same, cross-compiled to linux/arm64, darwin/amd64, darwin/arm64 | Compiles (not run: no Mac or arm64 desktop here) |
| `CGO_ENABLED=1 go build -tags desktop,gtk3` (GTK3 + WebKit2GTK 4.1) | Works |
| Default build | Unchanged: static, 10 MB, no Wails |

At run time the purego binary needs `libgtk-4-1` and `libwebkitgtk-6.0-4` (GTK 4.8+ and
WebKitGTK 2.40+, i.e. Debian 12, Ubuntu 24.04, Fedora 38 or newer). Wails reports one clear
error naming the missing package when they're absent.

Dependencies added: `wailsapp/wails/v3` (MIT), `ebitengine/purego` (Apache-2.0),
`go-ole/go-ole` (MIT); `coder/websocket` moves from 1.8.12 to 1.8.14 because Wails requires it.

## Video drawing

The page used to call `putImageData` once per changed 16-pixel strip as each message arrived:
about 50 calls per frame at 1024×768. That starved WebKit's main thread. `app.js` now copies
each message into an in-memory copy of the screen and paints the dirty box with one
`putImageData` per display frame (`requestAnimationFrame`). The screenshot path paints any
pending change first.

Ubuntu 24.04, 4 vCPUs, Xvfb with software rendering, WebKitGTK 2.52.6 and Chromium 153 headed on
the same display. Each run streams raw RGBA frames where every 16×16 tile changes every frame
(the worst case; a real BMC sends only changed tiles), measured for 10 s after connecting
through the real consent flow (`FAKEAPP_STRESS=WxH@fps` in the e2e fake app).

| Stream | WebKitGTK before | WebKitGTK after | Chromium after | WebKitGTK longest stall before → after |
| :--- | ---: | ---: | ---: | ---: |
| 1024×768 at 30 fps | 17.2 fps | 24.1 fps | 23.5 fps | 437 → 102 ms |
| 1024×768 at 60 fps | 12.7 fps | 40.8 fps | 22.9 fps | 268 → 98 ms |
| 1920×1080 at 30 fps | 7.0 fps | 19.3 fps | 9.1 fps | 403 → 192 ms |

WebKitGTK now matches or beats Chromium here. Chromium seems capped at about 75 MB/s in this
container, so treat its column as a floor, not a ceiling. Real hardware with GPU compositing
will do better than both.

## Behaviour in WebKitGTK

| Check | Result |
| :--- | :--- |
| Sign-in with the launch code, code removed from the address, token in `sessionStorage` | Works |
| Consent dialog, connect, video, WebSocket | Works |
| CSP violations or script errors | None |
| `isSecureContext` on `http://127.0.0.1` | true |
| Clipboard read API, Fullscreen API | Present |
| Keyboard Lock (`navigator.keyboard.lock`) | Missing (Chromium-only). In a native window Ctrl+W, Ctrl+T, F5 etc. reach the page anyway; Alt+Tab and Super still go to the desktop |
| Screenshot button | `<a download>` is silently dropped by WebKitGTK, so the window build saves through Go: `POST /api/screenshot/save` encodes the server's own copy of the screen and shows the native Save dialog. Driven under Xvfb: the GTK file chooser appears with the suggested name and the file is written |
| Dropping a file or link on the window | Refused by the page (a drop would otherwise navigate away from the console); also applies in browsers |
| `window.open` | Returns null; no window opens |
| `<select>` styling | Native GTK widget, light on the dark theme (see image); needs CSS |
| Memory | Main process about 290 MB RSS with software GL, plus WebKit's network and web processes (each in WebKit's own bubblewrap sandbox) |

## Security notes

- The one-time code goes from the server to the webview in memory; it never appears on a
  command line. This closes the argv exposure noted in `browser.go` and the threat model, for
  the window build.
- Wails injects scripts into every page the window loads, including ours: `window._wails`,
  `window.wails` and a blob-fetch shim. WebKit user scripts bypass the page's CSP. The page can
  post to the `external` message handler; the messages Wails acts on are window drag and
  resize, and bare-name events only with `AllowSimpleEventEmit` (off). We register no bindings,
  services or `RawMessageHandler`, and Wails' HTTP runtime calls go to `/wails/runtime` on our
  origin, where our server answers 404.
- Navigation: there are no external links, the CSP has `form-action 'none'` and
  `base-uri 'none'`, drops are refused, `window.open` does nothing, and WebKit's context menu
  (Back, Open Link in New Window, Inspect) is disabled. What remains is script navigation
  (`location = …`) by an XSS, which the CSP's `script-src 'self'` already guards against.
  Wails v3.1 has no navigation policy hook on Linux, and `ExecJS` waits for the Wails runtime
  our page never loads, so a Go-side check after navigation isn't possible without patching
  Wails.
- WebKitGTK's web process runs in its own bubblewrap sandbox, nested inside the Flatpak's.

## Flatpak

The manifest now uses `org.gnome.Platform//51` (WebKitGTK 6.0; built on Freedesktop 26.08, so
the 26.08 golang extension still applies), builds with `-tags desktop,purego,production` and
adds `--socket=wayland`, `--socket=fallback-x11`, `--share=ipc` and `--device=dri`. CI uses
the `gnome-51` flatpak-github-actions image. `smoke-test.sh` checks that exact permission set,
that the runtime has GTK4 and WebKitGTK 6.0, and, where `xvfb-run` exists, that the window
starts WebKit and quits on SIGTERM. See [security/sandbox.md](../security/sandbox.md).

Built and tested locally (Flatpak 1.14.6, x86_64): the offline build succeeds, the bundle
installs, and the whole smoke test passes, window check included; the window shows the
connection manager. Inside the Flatpak, WebKit starts its web processes with
`flatpak-spawn --sandbox --no-network`, so the process that renders the page has no network of
its own. Two things learned:

- Flatpak 1.14 lists `--socket=fallback-x11` as `x11;wayland;fallback-x11`, so the smoke test
  checks for "only display sockets" rather than an exact string.
- Without a UTF-8 locale, `flatpak-spawn` refuses WebKit's sandbox arguments and the app aborts.
  Desktops always set one; the smoke test sets `LANG=C.UTF-8` when it's unset, for bare CI
  containers.

Whether the `gnome-51` CI image has `xvfb-run` is unknown; without it the window check is
skipped, not failed.

## macOS

Not run: there is no Mac in this environment. Both macOS targets cross-compile with the purego
backend (which Wails also marks experimental there); the cgo backend is the fallback, but it
needs a Mac to build. Before shipping on macOS:

1. Run `make desktop` on a Mac and go through the console: video, keyboard (Cmd mapping),
   fullscreen, the Save dialog.
2. Ship it as an `.app` bundle (Wails' `wails3 package` or a hand-made `Info.plist`), signed and
   notarised: an unsigned `.app` is harder to open than today's unsigned command-line binary.
3. Decide whether the release archive carries the window build, the browser build or both.

## Still open

- Window close while a BMC session is active was not tested under Xvfb (no window manager);
  SIGTERM was.
- Release archives still carry only the browser build; adding `ipmi-connect-desktop` for Linux
  is one more `go build` line in `scripts/build-release.sh` once it has had real-desktop use.
- The `<select>` styling for WebKit.

The browser build stays: CLI use, ChromeOS/Crostini without a Linux desktop, and machines
without GTK4/WebKitGTK still need it.
