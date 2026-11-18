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
- **D2: CA-valid certificates are always accepted**, pinned or not. Pins exist for self-signed
  certificates; WebPKI verification is the stronger check when it applies.
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

## Checklist

Status is kept current as work lands; see the end of this file for findings and blockers.

- [ ] 1 TLS trust policy
- [ ] 2 Endpoint and dial guard
- [ ] 3 Vendor clients on the endpoint
- [ ] 4 Parser hardening and fuzzing
- [ ] 5 Local server hardening
- [ ] 6 Session manager
- [ ] 7 Desktop mode and UI
- [ ] 8 Virtual media confinement
- [ ] 9 Portals
- [ ] 10 Flatpak
- [ ] 11 CI/release
- [ ] 12 Documentation
- [ ] Independent security review and fixes

## Security findings (audit of the baseline)

| ID | Area | Finding | Severity | Status |
| :--- | :--- | :--- | :--- | :--- |
| F1 | `bmc/legacytls.go` | Every TLS connection used `InsecureSkipVerify` with no pinning: a MITM receives the web password and KVM tokens. | High | open |
| F2 | `web/files.go` | `/api/files` lists any directory; `/api/media/images` and `/api/media/create` accept any absolute path, including writes. | High (with a stolen token) | open |
| F3 | `vmedia/images.go` | Image is stat'ed at add time and reopened by path at map time: a swapped file or symlink is served (and written). | Medium | open |
| F4 | `web/static/app.js` | Session token kept in `localStorage` (persists across runs) and sent in the WebSocket URL. | Medium | open |
| F5 | `web/security.go` | Launch code never expires; no logout or token invalidation. | Medium | open |
| F6 | `web/security.go` | Host/Origin accept any loopback name and any port, so another local origin can pass the Origin check. | Medium | open |
| F7 | `dell/channels.go` | JNLP tokens longer than 96 bytes overrun the `cb` payload layout (and longer than 208 bytes panic). | Medium (malicious BMC) | open |
| F8 | `dell/session.go`, `dell/power.go` | HTTP bodies from the BMC read with unbounded `io.ReadAll`. | Medium | open |
| F9 | `cmd/frames.go` | Dell frame assembler appends continuation chunks without a cap. | Medium | open |
| F10 | `dell/session.go` | JNLP `kmport`/`vport` accepted without range validation. | Low | open |
| F11 | `cmd/detect.go` | Vendor detection did an unverified HTTPS request. | Low | open |
| F12 | `web/server.go` | No limit on concurrent sockets or requests; no idle cleanup. | Low | open |
| F13 | `web/security.go` | CSP allowed `style-src 'unsafe-inline'`. | Low | open |
| F14 | CLI | `-p` puts the password in the process list (already warned in help). | Low | open |

## Blockers and open questions

- Physical hardware is not available in this environment; every protocol-facing change is tested
  against fakes only. See the **[hw]** items.
