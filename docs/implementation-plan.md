# Implementation plan: secure desktop application and Flatpak

This is the working checklist for turning ipmi-connect into a secure desktop application with
Flatpak packaging. It records the baseline, the plan, decisions, security findings and what is
still outstanding. Items marked **[hw]** need physical hardware to validate; see
[testing/hardware-validation.md](testing/hardware-validation.md).

## Phase 0: baseline (recorded 2026-11-18, before any change)

| Check | Command | Result |
| :--- | :--- | :--- |
| Build | `go build ./...` | pass |
| Vet | `go vet ./...` | pass |
| Race tests | `go test -race -count=1 ./...` | pass (11 packages) |
| Lint | `golangci-lint run ./...` (v2.15.0, as pinned in CI) | pass with the CI version; the preinstalled v2.5.0 cannot load a go1.27 module |
| Flatpak | n/a | no packaging existed |

Existing reusable pieces: the vendor protocol clients (`internal/dell`, `internal/supermicro`),
virtual media stack (`internal/vmedia`, `internal/avmp`, `internal/atenvm`), the ASPEED and Hermon
decoders with golden tests, the embedded UI, the launch-code/token exchange in
`internal/web/security.go`, `bmc.FormatError` URL redaction, and the release script.

## Prioritised plan

Dependencies flow top to bottom. Each item lists its acceptance criteria.

1. **TLS trust policy** (`internal/trust`). Replace `InsecureSkipVerify` with a `VerifyConnection`
   policy: CA verification, else a pinned SHA-256 leaf fingerprint, else a typed error carrying
   the certificate details. Legacy TLS (1.0/1.1, CBC/3DES suites) only when authorised for the
   device. Trust store persisted per device, `0600`, atomic writes.
   *Accept:* unit tests for CA-valid, pinned, unknown, changed certificate, legacy refused unless
   authorised, store round trip and replacement.
2. **Destination-scoped endpoint** (`internal/bmc/endpoint.go`). Resolve the BMC once; every dial
   (HTTPS, APCP, RFB, virtual media) goes to that address, on allowed ports only. HTTP clients
   refuse cross-host and HTTPS-to-HTTP redirects and never use a proxy.
   *Accept:* tests for refused foreign hosts/ports and redirects.
3. **Vendor clients on the endpoint.** Dell and Supermicro take a `*bmc.Endpoint`; the APCP TLS
   upgrade uses the same policy. Validate JNLP-supplied ports and token lengths. Bound HTTP body
   reads. *Accept:* existing vendor tests pass against pinned `httptest` TLS servers.
4. **Parser hardening and fuzzing** (parallel workstream): caps, overflow checks and fuzz targets
   for every BMC-controlled parser. *Accept:* fuzz targets run clean; regression tests.
5. **Local server hardening.** Exact Host and Origin checks against the bound origin; launch codes
   expire (2 min) and work once; logout invalidates the token and closes sockets; single-use
   WebSocket tickets instead of the reusable token in the URL; token in `sessionStorage`, not
   `localStorage`; method-restricted routes; limits on concurrent requests and sockets; tighter
   CSP (no `unsafe-inline`). *Accept:* table tests for each defence.
6. **Session manager** (`internal/session`). Lifecycle from the CLI moves into a library:
   idle → connecting → connected → disconnecting, with cancellation, clean BMC logout, input release,
   virtual media cleanup and screen reset. Concurrent connects are refused. Trust and legacy
   consent are checked before credentials are sent. *Accept:* tests with fake vendors for connect,
   disconnect, reconnect to another BMC, failed connect cleanup, concurrent attempts.
7. **Desktop mode and connection manager UI.** No arguments or `--desktop` starts the server on
   an ephemeral port with no BMC; the UI offers address, user, password, vendor, security details,
   fingerprint confirmation, legacy consent, profiles and recents (never passwords), version,
   disconnect, quit, idle timeout. *Accept:* Go API tests and one Playwright smoke test.
8. **Virtual media confinement.** File browsing limited to media roots (home without hidden
   entries, application-private media directory, `--media-dir`, portal selections); validated
   open-and-compare handles; read-only by default; writable images and destructive power
   actions need confirmation. *Accept:* traversal, symlink escape and swap tests.
9. **Portals.** OpenURI for the browser and FileChooser for images when available, over the
   session bus without broad D-Bus permissions. *Accept:* builds; fallback paths tested; **[platform]**
   dialogs validated on a desktop.
10. **Flatpak** (`packaging/flatpak/`). Manifest, desktop entry, icon, AppStream, offline Go module
    sources, minimal permissions. *Accept:* `flatpak-builder` succeeds where available; CI builds
    x86_64 and aarch64.
11. **CI/release.** SHA-pinned actions, least privilege, lint/vet/race/govulncheck/licence/CodeQL,
    fuzz smoke, Flatpak builds, SBOM, checksums, provenance; manual release only.
12. **Documentation.** README, SECURITY.md, threat model, sandbox, desktop design, hardware
    validation checklist, Flatpak README.

## Decisions

- **D1: fingerprints are a per-device set.** A certificate matching any fingerprint pinned for a
  device is accepted on any of that device's TLS endpoints (HTTPS 443 and APCP 7578). Whether the
  Dell APCP channel presents the same certificate as the web server is unverified **[hw]**; the set
  handles both cases. Both endpoints are probed before credentials are sent.
- **D2 (revised after review): pins take precedence.** A device with a pinned certificate must
  present it; a different certificate is a change to confirm, even if CA-signed. Devices without
  pins are accepted on normal CA verification.
- **D3: legacy TLS and insecure channels need consent per device**, stored with the trust entry
  and shown in the UI, revocable by forgetting the device.
- **D4: the CLI now refuses unknown certificates.** Interactive runs prompt with the fingerprint;
  non-interactive runs need `--trust-fingerprint` or a prior trust entry. Legacy TLS needs
  `--allow-legacy-tls` (or the prompt). This deliberately breaks unattended first runs against
  self-signed BMCs.
- **D5: the session token lives in `sessionStorage`**, so a reload keeps the tab signed in but
  closing the tab ends it. `localStorage` (persistent) is no longer used for it.
- **D6: the file picker stays server-side** (browsers do not reveal real paths, and writes to `.img`
  files need them) but is confined to media roots. Paths outside them are refused by every endpoint.
- **D7: one new dependency, `github.com/godbus/dbus/v5`** (BSD-2-Clause, no transitive deps), for
  portals. Hand-writing a D-Bus marshaller is a worse risk than a well-reviewed library.
- **D8: releases are manual** (`workflow_dispatch`). The previous workflow published on every
  green `main`; publishing now needs an explicit run.
- **D9: Crostini uses a fixed port.** ChromeOS forwards only configured container ports to
  the browser, so desktop mode on Crostini binds 8080 (or `--web-port`) instead of an ephemeral port.
- **D10: the Flatpak ID stays `io.github.a5m0.IPMIConnect`** as specified, but Flathub derives
  the repository URL from the last component (`github.com/a5m0/IPMIConnect`, which does not
  exist): a Flathub submission needs either `io.github.a5m0.ipmi-connect` or a reviewer
  exception. Decide before the first public Flatpak release; see packaging/flatpak/README.md.
- **D11: Go 1.27.4 toolchain.** `go.mod` keeps `go 1.27.2` as the minimum and adds
  `toolchain go1.27.4` for the net/http and crypto/x509 fixes govulncheck reported.
- **D12: the launch link stays in the browser's command line.** A private-file handoff would
  hide it from other local users, but Ubuntu's snap Firefox cannot read `/run/user` or hidden
  folders. The link is no longer logged on desktop launches; the residual risk is documented.

## Checklist

- [x] 1 TLS trust policy (`internal/trust`)
- [x] 2 Endpoint and dial guard (`internal/bmc/endpoint.go`)
- [x] 3 Vendor clients on the endpoint (Dell HTTPS + APCP TLS, Supermicro HTTPS)
- [x] 4 Parser hardening and fuzzing (13 fuzz targets; parallel workstream)
- [x] 5 Local server hardening
- [x] 6 Session manager (`internal/session`)
- [x] 7 Desktop mode and UI (connection manager, consent dialog, security indicator)
- [x] 8 Virtual media confinement (media roots, open-and-compare, read-only default)
- [x] 9 Portals (OpenURI, FileChooser) — **[platform]** dialogs not exercised on a desktop
- [x] 10 Flatpak (built and smoke-tested on x86_64; aarch64 in CI only)
- [x] 11 CI/release (CI green on GitHub except CodeQL's two by-design alerts; the manual release workflow has not been run)
- [x] 12 Documentation
- [x] Independent security review and fixes

## Security findings

### Baseline audit

| ID | Area | Finding | Severity | Status |
| :--- | :--- | :--- | :--- | :--- |
| F1 | `bmc/legacytls.go` | Every TLS connection used `InsecureSkipVerify` with no pinning: a MITM receives the web password and KVM tokens | High | Fixed: `trust.Policy` on every handshake; credentials only after all endpoints are trusted |
| F2 | `web/files.go`, `web/media.go` | File listing and image/create paths anywhere on disk | High (with a stolen token) | Fixed: `MediaRoots` |
| F3 | `vmedia/images.go` | Stat at add, reopen by path at map (swap/symlink TOCTOU) | Medium | Fixed: `O_NOFOLLOW` + same-file check |
| F4 | `static/app.js` | Token in `localStorage` and in the WebSocket URL | Medium | Fixed: `sessionStorage`, single-use tickets |
| F5 | `web/security.go` | Launch code never expired; no logout | Medium | Fixed: 2 min expiry, logout rotates the token |
| F6 | `web/security.go` | Any loopback Host/Origin on any port accepted | Medium | Fixed: exact origin |
| F7 | `dell/channels.go` | JNLP tokens > 96 bytes overran the `cb` layout (> 208 panicked) | Medium | Fixed: length checks |
| F8 | `dell/session.go`, `dell/power.go` | Unbounded `io.ReadAll` of BMC responses | Medium | Fixed: 1 MiB cap |
| F9 | `cmd/frames.go` | Unbounded Dell frame assembly | Medium | Fixed: 16 MiB cap |
| F10 | `dell/session.go` | JNLP ports not range-checked | Low | Fixed |
| F11 | `cmd/detect.go` | Unverified HTTPS request for vendor detection | Low | Fixed: port checks first, banner only over a trusted connection |
| F12 | `web/server.go` | No request/socket limits, no idle cleanup | Low | Fixed |
| F13 | `web/security.go` | CSP allowed inline styles | Low | Fixed, verified in Chromium |
| F14 | CLI | `-p` exposes the password | Low | Deprecated with a warning |
| F15 | dependencies | GO-2026-6865 (klauspost/compress), GO-2026-6859, GO-2026-6809 (stdlib) | Medium | Fixed: compress v1.20.2, Go 1.27.4 |

### Parser hardening (parallel workstream, fuzzing)

| Where | Finding | Status |
| :--- | :--- | :--- |
| `atenvm/target_data.go` | Truncated READ TOC / MODE SENSE(10) CDBs panicked the process (found by fuzzing) | Fixed; crash input kept as a seed |
| `supermicro/messages.go` | 65535 rects × 64 MiB per update | Fixed: 64 MiB per update |
| `supermicro/handshake.go`, `atenvm/framing.go` | Allocation before data arrived | Fixed: incremental reads |
| `supermicro/jnlp.go` | Unbounded tokens | Fixed: 256 bytes |
| `dell/video.go` | Implausible frame geometry | Fixed: 4096 limit |
| `atenvm/target.go` | `Execute` reads up to 4 GiB in one buffer | Fixed: 1 MiB |
| `supermicro/session.go` | A picture allocated per resized rect before checking it | Fixed: once per update, drawable rects only |

### Independent review of the changes

| # | Finding | Severity | Status |
| :--- | :--- | :--- | :--- |
| R1 | Launch code visible in the browser's command line to other local users; logged on desktop launches | Medium | Partly fixed: no longer logged; argv exposure documented (D12) |
| R2 | Two instances overwrite each other's trust-store changes | Medium/low | Fixed: file lock, re-read before write |
| R3 | Create Image from a folder copied dotfiles | Low | Fixed |
| R4 | A previous session's picture could show in the next | Low | Fixed |
| R5 | Quit during a connection attempt could leave the Dell logged in | Low | Fixed: Disconnect waits for the attempt |
| R6 | CA-valid certificate overrode a pin | Low | Fixed (D2 revised) |
| R7 | One stored consent silently accepted a later TLS strip on the Dell KVM port | Low | Fixed: KVM TLS remembered, downgrade re-asks |
| R8 | No probe handshake deadline; portal response sender unchecked; binding WaitGroup misuse | Info | Fixed |
| R8 | Fixed CLI port could be squatted after exit | Info | Documented |
| R8 | Banner-detected vendor may show a second certificate as "changed" | Info | Open (edge case: only when ports did not identify the vendor) |
| R9 | Intermediate-folder symlink race in shared writable media folders | Suspicion | Documented |

## Test results (development environment, 2026-11-18)

| Check | Command | Result |
| :--- | :--- | :--- |
| Format, vet | `gofmt -l .`, `go vet ./...` | clean |
| Lint | `golangci-lint run ./...` (v2.15.0 built with go1.27) | 0 issues |
| Unit and integration | `go test -race -count=1 ./...` | pass (15 packages) |
| Vulnerabilities | `go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...` | no vulnerabilities (after F15) |
| Licences | `go-licenses check ./... --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,MPL-2.0,ISC` | pass |
| Fuzzing | 13 targets, 20-30 s each (`go test -run=^$ -fuzz=...`) | pass after the atenvm fix |
| Browser UI | `node test/e2e/ui.mjs` (Playwright 1.63, Chromium) | pass: sign-in, consent, video over WebSocket, security badge, token handling, replay, disconnect, no CSP violations |
| Flatpak build | `flatpak-builder` 1.4.2, Freedesktop 26.08, golang 1.27.4, offline | pass (x86_64) |
| Flatpak metadata | `desktop-file-validate`, `appstreamcli validate` | pass |
| Flatpak smoke | `packaging/flatpak/smoke-test.sh` under `dbus-run-session` | pass (x86_64) |
| Release script | `scripts/build-release.sh 0.0.0-local` | 4 archives, Flatpak, CycloneDX SBOM, checksums |
| Workflows | `actionlint` v1.7.12; every action pinned to a commit SHA | pass |

Not run anywhere yet: the release workflow (attestations, SBOM upload), the binaries on macOS
(CI only cross-compiles them), any real BMC, portal dialogs on a desktop, ChromeOS.

## CI on GitHub (draft pull request a5m0/ipmi-connect#1)

First run (85d6d5c) and fixes:

| Job | Failure | Fix |
| :--- | :--- | :--- |
| Test (race) | `TestDisconnectWaitsForALateLoginToBeLoggedOut` disconnected while the attempt was still probing TLS | The test waits until the login has started |
| Vulnerabilities | govulncheck ran on Go 1.27.2: `GOTOOLCHAIN=local` blocked the switch to `toolchain go1.27.4` | `GOTOOLCHAIN: auto` |
| Dependency review | `Forbidden` from the dependency-graph API | Owner enabled the dependency graph |
| Flatpak x86_64 | flatpak-github-actions failed updating the image's runtimes (EXDEV) | Install the SDK per user, run flatpak-builder directly |
| CodeQL | 20 alerts | See below |

On cc6abdd all 24 CI jobs passed: lint, race tests, govulncheck and licences, dependency review,
13 fuzz targets, the browser UI test, Linux and macOS cross-builds, and the Flatpak build and
smoke test on x86_64 and aarch64.

CodeQL went from 20 alerts to 2. Fixed: log lines now quote user- and BMC-supplied strings, the
port is bounded at its `uint16` conversion, and `MediaRoots` returns paths rebuilt from the root
and a `filepath.IsLocal` relative path (no behaviour change). Left for the owner, by design: the
pinning `InsecureSkipVerify` (`trust.go`) and the request to the BMC the user chose
(`session/vendor.go`).

## Blockers and open questions

- Physical hardware is not available in this environment; every protocol-facing change is tested
  against fakes only. See [testing/hardware-validation.md](testing/hardware-validation.md).
- The Flathub ID question (D10) is the owner's decision.
- Renovate's `assignees` still names the upstream author (inherited from the fork).

## Follow-up

- Single-instance handoff for a second desktop launch.
- Opt-in password storage via the Secret portal / Secret Service.
- Re-validate the launch-link handoff (R1) if confined browsers gain access to a private
  location, or offer it where the browser is known to be unconfined.
- Supermicro "KVM SSL" firmware; newer BMC generations.
- A ChromeOS Isolated Web App front end.
