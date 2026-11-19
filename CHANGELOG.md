## [0.3.0](https://github.com/a5m0/ipmi-connect/compare/v0.2.0...v0.3.0) (2026-11-19)

### Features

* **config:** XDG paths, device profiles and recents without passwords ([8356f27](https://github.com/a5m0/ipmi-connect/commit/8356f27fd6ac0443fcb612c703a549a0ffaf2145))
* **desktop:** native dialogs, faster drawing, window lockdown, Flatpak window ([5a8d262](https://github.com/a5m0/ipmi-connect/commit/5a8d2622110455cb9ae89031347d8bbcd882c956))
* **flatpak:** io.github.a5m0.IPMIConnect packaging ([7cd4047](https://github.com/a5m0/ipmi-connect/commit/7cd4047eb0d0b920470eec7a1ca26e874ba12b31))
* **portal:** OpenURI and FileChooser over xdg-desktop-portal ([393e08b](https://github.com/a5m0/ipmi-connect/commit/393e08bd870e7d53f5e82bf755cf3fa9c257b501))
* **release:** ship ipmi-connect-desktop in the Linux archives ([1d8fcf8](https://github.com/a5m0/ipmi-connect/commit/1d8fcf87b1d9df872a7ea1b7564e397c0b47f1e2))
* trust-gated BMC sessions, desktop mode and a hardened local server ([077a32e](https://github.com/a5m0/ipmi-connect/commit/077a32ebe1abe2c9372dbad266cd11fb017d410c))
* **trust:** certificate pinning, TOFU trust store and explicit legacy TLS ([72419bb](https://github.com/a5m0/ipmi-connect/commit/72419bbcf845f9c98773b93bb93ea03f254e87e6))
* **ui:** connection manager, consent dialog and security indicator ([e28e67e](https://github.com/a5m0/ipmi-connect/commit/e28e67e66faec0a2c7a16579422d583b870ae177))
* **ui:** docked type-text panel that types on the host ([de9d84e](https://github.com/a5m0/ipmi-connect/commit/de9d84e49b77529e78b13ebbf158f3aa93220bcf))

### Bug Fixes

* address the independent security review ([c395ede](https://github.com/a5m0/ipmi-connect/commit/c395ede9d36730495999b24d6754b0599a48d471))
* **atenvm:** bound reads that Execute returns as one buffer ([6e620f8](https://github.com/a5m0/ipmi-connect/commit/6e620f88274726aef68cd8d999d339da8ea6eeb4))
* **atenvm:** handle truncated READ TOC and MODE SENSE(10) CDBs ([219fa14](https://github.com/a5m0/ipmi-connect/commit/219fa1486d9c87905792f399e3f9baa51f1a00f8))
* **atenvm:** read large frame payloads as their bytes arrive ([7f567f3](https://github.com/a5m0/ipmi-connect/commit/7f567f30db305c0146ce884d658e1bd41604d5b0))
* **ci:** first-run failures of the new workflows ([24d7269](https://github.com/a5m0/ipmi-connect/commit/24d7269be0822b070435935ff96d8391139e3def))
* **dell:** drop video frames of implausible geometry ([ce18ce3](https://github.com/a5m0/ipmi-connect/commit/ce18ce3c8102f762a304b1f028318674ec83c7be))
* **deps:** klauspost/compress v1.20.2 and Go 1.27.4 toolchain ([8d8a9cb](https://github.com/a5m0/ipmi-connect/commit/8d8a9cb17850dd7375de0bd1f6d65fb0ab5378de))
* **flatpak:** compare smoke-test permissions as sets ([6494980](https://github.com/a5m0/ipmi-connect/commit/6494980eee1a28927400d5a8e72dacd0ad3482e4))
* quote the remaining request strings in log lines ([cc6abdd](https://github.com/a5m0/ipmi-connect/commit/cc6abdd5ca33d002ccec0a4e3140925f717d5d8e))
* **release:** conventionalcommits preset 9 for semantic-release 25 ([#5](https://github.com/a5m0/ipmi-connect/issues/5)) ([fb301cc](https://github.com/a5m0/ipmi-connect/commit/fb301cce2cbeda53dc0b26146e0ac61e321b976a))
* **supermicro:** allocate at most one picture per framebuffer update ([827d0f0](https://github.com/a5m0/ipmi-connect/commit/827d0f075757534864579286690ec53c6eba9613))
* **supermicro:** cap total framebuffer update size ([648ed5e](https://github.com/a5m0/ipmi-connect/commit/648ed5e137c1bada5d939fee42e232d8dc441ff5))
* **supermicro:** reject implausibly long JNLP KVM tokens ([afdd641](https://github.com/a5m0/ipmi-connect/commit/afdd64113fd76702c9549861de81c61a2bda128d))
* **ui:** dark selects in WebKit, outlined secondary buttons, header fits 1280 px ([4e9a43a](https://github.com/a5m0/ipmi-connect/commit/4e9a43ab180853ce32ff6cbb21c7ea2ad678c5fa))
* **vmedia:** refuse an image file replaced since it was added ([8b03d98](https://github.com/a5m0/ipmi-connect/commit/8b03d984b0766c60cb7014a7f84286da0e0bbb41))
* **web:** quote the macro name in its log line ([3ac70ce](https://github.com/a5m0/ipmi-connect/commit/3ac70ce971331ba66c68fbb304db770de30e6911)), closes [#11](https://github.com/a5m0/ipmi-connect/issues/11)
* **web:** send a close frame saying why a console socket ended ([eee9bf3](https://github.com/a5m0/ipmi-connect/commit/eee9bf3565b99b24cb1e0af450b1893710e2ef54))
* **web:** tell the page why its console socket ends, then drop it ([407c1d9](https://github.com/a5m0/ipmi-connect/commit/407c1d9a82476980753787a483dfa40568a0a8c8))

## [0.2.0](https://github.com/mhamzahkhan/ipmi-connect/compare/v0.1.0...v0.2.0) (2026-10-18)

### Features

* **cli:** add -v/--version flag ([6fe429a](https://github.com/mhamzahkhan/ipmi-connect/commit/6fe429a3d660911aa4c631cd6ae7a6222f37a2a1))

### Bug Fixes

* **ci:** gate release on CI, harden renovate automerge, and add buildinfo fallback ([08d7034](https://github.com/mhamzahkhan/ipmi-connect/commit/08d7034862a59088de4355c575d2efe479e62df3))
* **ci:** use golangci-lint v2 and fix flaky atenvm detach test ([fdbe112](https://github.com/mhamzahkhan/ipmi-connect/commit/fdbe1120b13a36ca14797858a27bf64c12cc3169))
