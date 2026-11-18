# Desktop application design

Status: implemented; protocol-facing behaviour still needs hardware validation (see
[testing/hardware-validation.md](../testing/hardware-validation.md)).

## Goals and constraints

- One statically linked Go executable with the web UI embedded; the user's own browser is the
  UI. No Electron, Tauri, embedded browser, Java, Python, Node.js runtime, WebAssembly or
  remote service.
- Usable without a terminal: a desktop launcher opens a connection manager.
- The existing CLI (`ipmi-connect <host>`) keeps working for scripts.
- Security first: no credential leaves the process before the BMC is trusted; obsolete
  protocols need consent and stay visible.

## Modes

| | CLI mode | Desktop mode |
| :--- | :--- | :--- |
| Started by | `ipmi-connect <host> [flags]` | `ipmi-connect`, `ipmi-connect --desktop`, the desktop launcher |
| Local port | `--web-port` (default 8080, else a free port) | a free port chosen by the OS (8080 on ChromeOS) |
| BMC | connected before the server starts; trust prompts on the terminal | chosen in the browser; trust dialogs in the page |
| Idle timeout | off unless `--idle-timeout` | 30 minutes without a browser (configurable) |
| Quit | Ctrl+C | Quit button, Ctrl+C, or the idle timeout |

Both modes run the same server and session manager; in CLI mode the user can still disconnect
and connect elsewhere from the page.

## Components

```text
cmd/ipmi-connect   flags, CLI prompts, browser launch, wiring
  └─ internal/web       local HTTP + WebSocket server, auth, media roots, API
  └─ internal/session   one BMC session at a time: trust gate, vendors, lifecycle
       └─ internal/trust     certificate policy, probes, trust store
       └─ internal/bmc       Endpoint (scoped dialler, HTTP client), shared types
       └─ internal/dell, internal/supermicro   vendor protocols (unchanged wire formats)
  └─ internal/config    XDG paths, profiles, recents
  └─ internal/portal    xdg-desktop-portal OpenURI and FileChooser
```

`session.Manager` talks to the server only through the `session.Display` interface (screen,
status, power, attach/detach); `web.Server` talks to the manager through `web.Sessions`. Neither
imports the other's internals.

## Connection flow

```mermaid
sequenceDiagram
    participant UI as Browser (connection manager)
    participant S as web.Server
    participant M as session.Manager
    participant B as BMC
    UI->>S: POST /api/connect {address, user, password, vendor}
    S->>M: Connect
    M->>M: ParseAddress, resolve once (bmc.Endpoint)
    M->>B: port checks 7578 / 5900 (no data) for auto-detection
    M->>B: TLS probe 443 (modern, then legacy); Dell: APCP greeting + TLS probe 7578
    Note over M: no credential has been sent
    alt unknown/changed cert, legacy TLS or insecure channel not yet consented
        M-->>S: ConsentRequired
        S-->>UI: 409 {consent}
        UI->>UI: show fingerprint(s), ask for each decision
        UI->>S: POST /api/connect {..., trust_fingerprints, allow_*}
        S->>M: Connect (probes again; pins only observed fingerprints)
    end
    M->>B: login (HTTPS, policy enforced on the handshake again)
    M->>B: KVM channels (policy enforced)
    M->>S: Attach(console)
    S-->>UI: 200 {phase: connected, channels}
    UI->>S: POST /api/ws-ticket, then GET /ws?ticket=…
```

Every TLS handshake (probe or real) goes through `trust.Policy`, so a certificate that changes
between the probe and the login still fails.

## Session lifecycle

`session.Manager` phases: `idle → connecting → connected → disconnecting → idle`.

- **Concurrent attempts**: a second `Connect` while connecting or disconnecting gets `ErrBusy`.
- **Another BMC**: `Connect` while connected tears the old session down first.
- **Cancel**: `Disconnect` during `connecting` cancels the attempt's context; vendor connectors
  check it between steps and log out if they got that far.
- **Teardown order**: `Display.Detach` first (the server cancels the console binding, every
  WebSocket releases its held keys and mouse buttons, pastes stop, and Detach waits for that),
  then the vendor's `Stop` (background loops stop, virtual media is ejected, the BMC is logged
  out), so no input reaches a closing session and no key stays held on the host.
- **Shutdown**: the main function always calls `Disconnect` on the way out (signal, Quit button,
  idle timeout, server error).

### Browser tabs and idle cleanup

Closing a tab is not a reliable signal (browsers suspend background tabs, crash, or are killed),
so nothing depends on `beforeunload`. Instead `web.Server` tracks activity (authenticated
requests and WebSocket input) and treats an open socket as activity. After `IdleTimeout` with no
socket and no request, it calls `OnIdle`, which disconnects the BMC and quits. When a socket
closes, its held input is released immediately regardless.

## Local server security

See [security/threat-model.md](../security/threat-model.md) T1 and T2. In short: loopback only;
exact Host and Origin; single-use launch code that expires; token in a header (never in URLs or
cookies) and in `sessionStorage`; WebSocket tickets valid once for 30 s; logout rotates the
token and closes sockets; method-restricted routes; 4 KiB JSON bodies; 32 concurrent requests;
8 sockets; strict CSP.

## Browser launching

| Environment | How |
| :--- | :--- |
| Flatpak | OpenURI portal over the session bus, then `xdg-open` (portal-backed in the runtime) |
| Linux / BSD | `xdg-open <url>` |
| macOS | `open <url>` |
| Always | the link is printed; pressing Enter on a terminal prints and opens a fresh one |

The URL is passed as an argument, never through a shell, and contains only our own host, port and
code.

## Configuration storage

| File | Linux | Flatpak | macOS |
| :--- | :--- | :--- | :--- |
| Trust store `known_hosts.json` | `$XDG_CONFIG_HOME/ipmi-connect/` (`~/.config/…`) | `~/.var/app/io.github.a5m0.IPMIConnect/config/ipmi-connect/` | `~/Library/Application Support/ipmi-connect/` |
| Profiles `profiles.json` | same | same | same |
| Media folder | `$XDG_DATA_HOME/ipmi-connect/media` | `~/.var/app/…/data/ipmi-connect/media` | `~/Library/Application Support/ipmi-connect/media` |

Files are written atomically (temporary file, fsync, rename) with mode 0600. Profiles have no
password field; passwords are never stored. A future opt-in credential store should use the
desktop Secret Service through the Secret portal, not a custom vault.

## Compatibility

- `ipmi-connect <host>` behaves as before once the BMC is trusted. **Changed**: an untrusted
  certificate is no longer accepted silently; non-interactive first runs need
  `--trust-fingerprint`, TLS 1.0 BMCs need `--allow-legacy-tls`, and Supermicro needs
  `--allow-insecure-channels`. Decisions are remembered per device.
- `-p` still works, with a deprecation warning.
- The default CLI port is still 8080; desktop mode uses a free port.

## Follow-up work (not implemented)

- Single-instance handoff: a second launch could ask the running instance (over a socket in
  `$XDG_RUNTIME_DIR`) for a fresh link instead of starting another server.
- Secret Service storage for passwords, opt-in.
- Supermicro "KVM SSL" firmware (encrypted RFB) and newer BMC generations.
- A ChromeOS Isolated Web App front end.
