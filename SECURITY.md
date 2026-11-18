# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately, not in public issues: use GitHub's
**Security → Report a vulnerability** form on this repository (private vulnerability
reporting). Include the version (`ipmi-connect --version`), how it is installed (binary, Flatpak,
source), and steps to reproduce. Do not include real BMC credentials or certificates from
production systems.

We aim to acknowledge reports within a week. There is no bug bounty.

## Supported versions

Only the latest release receives security fixes.

## Scope

In scope: the ipmi-connect executable, its local web UI and API, its handling of BMC traffic,
virtual media file access, and the Flatpak packaging.

Out of scope: weaknesses of the BMC firmware itself (for example the Supermicro X8 KVM's lack of
encryption, which ipmi-connect discloses but cannot fix), and attacks that need the user's own
account on the same machine.

## Security model

See [docs/security/threat-model.md](docs/security/threat-model.md) for what is defended and the
remaining risks, and [docs/security/sandbox.md](docs/security/sandbox.md) for the Flatpak
sandbox. Key points:

- BMC certificates are verified, or pinned by SHA-256 after explicit confirmation; changed
  certificates are refused.
- Obsolete TLS and unencrypted BMC channels need per-device consent and are always indicated.
- Passwords are never stored or logged.
- The local console listens on loopback, needs a single-use, expiring link, and refuses
  cross-origin and DNS-rebinding requests.
- Use BMCs only on a dedicated, trusted management network.

## Verifying releases

Release assets come with `sha256sums.txt` and GitHub build-provenance attestations:

```bash
sha256sum --check --ignore-missing sha256sums.txt
gh attestation verify ipmi-connect_<version>_linux_amd64.tar.gz --repo a5m0/ipmi-connect
```
