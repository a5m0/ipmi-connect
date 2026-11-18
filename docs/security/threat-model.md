# Threat model

ipmi-connect processes untrusted network data and holds credentials for privileged
infrastructure: a BMC password can power-cycle a server, and a KVM console is equivalent to
physical access. This document lists what we defend against, how, and what remains.

## Assets

| Asset | Where it lives |
| :--- | :--- |
| BMC password | Typed into the CLI prompt or the connection form; held in process memory for the session (the vendor clients log in again after a web session expires); never written to disk or logs |
| BMC session tokens (Dell ST1/ST2, Supermicro SID, KVM and virtual-media one-time tokens) | Process memory only; redacted from error messages (`bmc.FormatError`) |
| Local console token | Process memory, and the browser tab's `sessionStorage` |
| Trust decisions (pinned fingerprints, consents) | `known_hosts.json` in the configuration folder, mode 0600 |
| Device profiles and recents (address, user, vendor; no password) | `profiles.json`, mode 0600 |
| Disk images served to the host | User-chosen files, read-only unless explicitly made writable |
| The managed server itself | Reachable through power control, keyboard and virtual media |

## Trust boundaries

```text
 browser tab ──HTTP/WS (loopback)── ipmi-connect ──HTTPS / APCP-TLS / RFB / VM── BMC ── host server
   (other sites,                      (Go process,                    (network: management
    other tabs)                        maybe in Flatpak)               VLAN or not)
```

## Threats and mitigations

### T1. A malicious website uses the local API

| Attack | Mitigation | Tested by |
| :--- | :--- | :--- |
| CSRF to `/api/power` etc. | Every API call needs the token in an `Authorization` header, which a cross-site page cannot set without CORS (never granted); state changes also need `application/json`; `Sec-Fetch-Site` other than `same-origin`/`none` is refused | `web/security_test.go` |
| DNS rebinding (evil.example resolving to 127.0.0.1) | `Host` must be exactly `127.0.0.1:<port>`, `localhost:<port>` or `[::1]:<port>` | `TestHostAndOriginMustBeThisServer` |
| Another local web service (http://127.0.0.1:3000) | `Origin` must equal this server's origin, including the port; same-site is refused | `TestSameSiteOtherPortIsRefused` |
| Cross-origin WebSocket hijacking | Socket needs a single-use ticket fetched with the token; Origin checked by the guard and by `websocket.Accept` | `TestWebSocketRejectsCrossSiteOrigin`, `TestWebSocketTicketsAreSingleUseAndExpire` |
| Clickjacking the power buttons | `frame-ancestors 'none'`, `X-Frame-Options: DENY` | `TestSecurityHeaders` |
| Script injection via BMC-supplied strings | Strict CSP (`script-src 'self'`, no inline script or style); all server data inserted with `textContent` | `test/e2e/ui.mjs` (renders `<script>` from the fake server literally) |
| Chrome Local Network Access / Private Network Access | We never send `Access-Control-Allow-Private-Network` or CORS headers, so public sites stay blocked; our own page is same-origin and unaffected | review |

### T2. Another local process or user hijacks the session

| Attack | Mitigation |
| :--- | :--- |
| Reads the launch link from the process list (`xdg-open <url>`) or terminal | The code is in the URL fragment, works once, and expires after 2 minutes (10 with `--no-browser` or on ChromeOS); the page removes it from the address bar |
| Replays a token from browser storage | Token is in `sessionStorage` (per tab, cleared on close), not `localStorage` or cookies; Logout rotates it |
| Connects to the loopback port | Needs the token; the server binds 127.0.0.1 only |
| Reads the trust store or profiles | Files are 0600 in a 0700 folder |

**Residual:** a process running as the same user can read this process's memory, the browser's
memory, or attach a debugger. We do not defend against same-user malware.

### T3. A network attacker between ipmi-connect and the BMC

| Attack | Mitigation |
| :--- | :--- |
| TLS interception to capture the password | Every TLS connection runs `trust.Policy`: CA-verified for the dialled name, or a pinned SHA-256 fingerprint. Unknown or changed certificates fail the handshake before any application data. Credentials are sent only after all TLS endpoints are probed and accepted (`session.Manager`) |
| Silent trust on first use | TOFU needs explicit confirmation of the displayed fingerprint (UI checkbox, typed `yes`, or `--trust-fingerprint`); only fingerprints the probe observed can be pinned |
| Certificate swapped later | `CertificateChangedError`; replacement needs an explicit second confirmation (`replace`) |
| Downgrade to obsolete TLS | TLS 1.2+ with AEAD suites by default; TLS 1.0/1.1 and CBC/3DES only after per-device consent, shown in the security indicator |
| HTTPS to HTTP downgrade | No code path uses http://; redirects to http are refused |
| Redirect or JNLP pointing elsewhere | `bmc.Endpoint` dials only the resolved BMC address on allowed ports; redirects must stay on the same https origin; JNLP ports are range-checked and tokens length-checked |
| DNS answers changing mid-session | The BMC is resolved once per connection; every later dial uses that address |
| Passive eavesdropping on Supermicro KVM (plain RFB) and virtual media (RC4 with a published key), or a Dell APCP channel without TLS | **Not preventable by us.** Disclosed before connecting, requires consent, and shown persistently as "Unencrypted channels". Use a dedicated management VLAN |

### T4. A malicious or compromised BMC

| Attack | Mitigation |
| :--- | :--- |
| Oversized or malformed frames to exhaust memory | Length caps on every parser (rect data, total update size, frame assembly, JNLP, HTTP bodies at 1 MiB), dimension limits (4096), incremental reads for large lengths |
| Malformed packets to crash the process | 13 native fuzz targets over every BMC-facing parser; one panic found and fixed (truncated SCSI CDBs) |
| Writes outside the image via virtual media | `vmedia.Drive` bounds every write; images are read-only unless the user confirms |
| Pointing the client at another host | See T3 (endpoint scoping) |

### T5. Filesystem access through virtual media

| Attack | Mitigation |
| :--- | :--- |
| Listing or reading arbitrary files with a stolen token | `MediaRoots`: only the home folder (outside Flatpak), the private media folder, `--media-dir` folders and files picked in the desktop file chooser; hidden entries below a root are excluded |
| Symlink escape or `..` traversal | Paths are resolved with `EvalSymlinks` before the containment check; links leading out of a root are not listed |
| Swapping the file between add and map (TOCTOU) | The image is opened `O_NOFOLLOW` and must be the same file (device and inode) at map time |
| Overwriting files with Create Image | New images only in allowed folders, never over an existing file, never hidden names |
| Unintended writes to the host's disk image | Read-only by default; writable needs `confirm_writable` and a UI confirmation |

### T6. A compromised ipmi-connect process

Inside the Flatpak the process cannot read the user's files (only portal-picked files and its
private folder), SSH keys, container sockets or devices, and cannot run host commands; see
[sandbox.md](sandbox.md). It **can** reach any host on the network, because Flatpak's network
permission is all or nothing. Outside Flatpak, the process has the user's full privileges.

## Remaining risks

1. Supermicro X8-era KVM and virtual media are unencrypted by design of the firmware.
2. Obsolete TLS (TLS 1.0, CBC, RSA key exchange, no forward secrecy) when consented.
3. TOFU is only as good as the user's fingerprint check on first contact.
4. The BMC password stays in process memory for the session (Go cannot reliably wipe strings).
5. Flatpak does not restrict network destinations; the endpoint scoping is in-process only.
6. CA-verified certificates are accepted even for a device with a pin (decision D2 in the
   implementation plan): an attacker holding a publicly trusted certificate for the BMC's name
   could impersonate it. BMCs on IP addresses or internal names rarely have such certificates.
7. A same-user local attacker (see T2).
8. Browser bugs.

Use a dedicated management network for BMCs regardless of these mitigations.
