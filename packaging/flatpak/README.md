# Flatpak packaging

| File | Purpose |
| :--- | :--- |
| `io.github.a5m0.IPMIConnect.yml` | Manifest: GNOME 51 runtime (for WebKitGTK), golang SDK extension, network and display permissions |
| `go-modules.json`, `modules.txt` | Offline Go module sources (module zips from proxy.golang.org, pinned by SHA-256) and the vendor manifest |
| `gen-sources/` | Regenerates the two files above: `go run ./packaging/flatpak/gen-sources` |
| `io.github.a5m0.IPMIConnect.desktop` | Launcher: `ipmi-connect --desktop` |
| `io.github.a5m0.IPMIConnect.metainfo.xml` | AppStream metadata |
| `io.github.a5m0.IPMIConnect.svg` | Icon |
| `smoke-test.sh` | Checks an installed build: sign-in, API, confinement, quit, and the window under Xvfb when available |

Permissions and their limits are explained in [docs/security/sandbox.md](../../docs/security/sandbox.md).

## Build and install locally

```bash
flatpak remote-add --user --if-not-exists flathub https://dl.flathub.org/repo/flathub.flatpakrepo
```

```bash
flatpak install --user flathub org.gnome.Platform//51 org.gnome.Sdk//51 org.freedesktop.Sdk.Extension.golang//26.08
```

```bash
flatpak-builder --user --install --force-clean build-dir packaging/flatpak/io.github.a5m0.IPMIConnect.yml
```

```bash
flatpak run io.github.a5m0.IPMIConnect
```

The build is offline: the Go toolchain comes from the SDK extension (`GOTOOLCHAIN=local`, so it
must be at least the `toolchain` in `go.mod`), modules from `go-modules.json`
(`GOFLAGS=-mod=vendor`, `GOPROXY=off`). After changing Go dependencies, regenerate the sources;
CI fails if they are stale.

## Bundles and a self-hosted repository

A single-file bundle (what releases attach):

```bash
flatpak-builder --repo=repo --force-clean build-dir packaging/flatpak/io.github.a5m0.IPMIConnect.yml
```

```bash
flatpak build-bundle --runtime-repo=https://dl.flathub.org/repo/flathub.flatpakrepo repo ipmi-connect.flatpak io.github.a5m0.IPMIConnect
```

```bash
flatpak install --user ipmi-connect.flatpak
```

To serve updates from your own repository instead, sign it with a GPG key you control and
publish the `repo` folder over HTTPS:

```bash
flatpak build-update-repo --gpg-sign=<key-id> --generate-static-deltas repo
```

Users add it with a `.flatpakrepo` file that carries the public key
(`flatpak remote-add --user ipmi-connect https://example.org/ipmi-connect.flatpakrepo`). No such
repository is set up by this project; releases only attach bundles.

## Tested

In development (2026-11-18, x86_64 container): `flatpak-builder` 1.4.2 built the manifest
offline; `desktop-file-validate` and `appstreamcli validate` passed; the bundle installed; and
`smoke-test.sh` passed. aarch64 was not run in development.

In CI (`.github/workflows/flatpak.yml`, first runs on the draft pull request, 2026-11-18): both
x86_64 (`ubuntu-24.04`) and aarch64 (`ubuntu-24.04-arm`) build offline, install the bundle and pass
`smoke-test.sh`. The first x86_64 run failed when flatpak-github-actions updated the image's
preinstalled runtimes ("Invalid cross-device link"); the workflow now installs the SDK into a
user installation and runs `flatpak-builder` directly.

## Flathub

Not submitted. Before a submission:

- **Application ID.** Flathub derives the repository URL from the last ID component as-is:
  `io.github.a5m0.IPMIConnect` maps to `https://github.com/a5m0/IPMIConnect`, which does not
  exist. Either rename the ID to `io.github.a5m0.ipmi-connect` (a dash is allowed in the last
  component, and it matches the repository) or ask the reviewers for an exception. Changing the
  ID later moves users' configuration (`~/.var/app/<id>`), so decide before the first public
  release.
- **Console software.** Flathub states that console software is not accepted. The Flatpak now
  shows its UI in its own window, which settles that concern.
- Flathub builds from a `git` source pinned to a tag and commit, not the `dir` source used here.
- The `network` permission is required; the metainfo declares it (`<requires><internet>always</internet></requires>`).
