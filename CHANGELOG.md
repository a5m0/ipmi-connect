## [0.4.0](https://github.com/a5m0/ipmi-connect/compare/v0.3.0...v0.4.0) (2026-11-20)

### Features

* --debug / IPMI_CONNECT_DEBUG with a megarac-media trace ([9a6dcff](https://github.com/a5m0/ipmi-connect/commit/9a6dcff64cf98c0a6b1916d10a7a7131dc6865d6))
* **desktop:** read the clipboard natively for paste ([74b4c7f](https://github.com/a5m0/ipmi-connect/commit/74b4c7f302418d8865a2f90986fa3102771b02fc))
* **megarac:** AMI MegaRAC backend for the ASUS ASMB8-iKVM ([7586670](https://github.com/a5m0/ipmi-connect/commit/75866701aef1e88844c3c1b70e0d3f0b741840dc))
* **megarac:** keep media tunnels open across ejects ([73e93e3](https://github.com/a5m0/ipmi-connect/commit/73e93e3681b8138d2ec758846d951a206f5fb81e))
* **megarac:** track the host's keyboard LEDs ([3b63ae3](https://github.com/a5m0/ipmi-connect/commit/3b63ae3ba4495ce781e16c3ca7b439bae81da349))
* **megarac:** virtual media over IUSB tunnels ([2e20b54](https://github.com/a5m0/ipmi-connect/commit/2e20b54b32e342a682e4e44835c533081baaa36f))
* **scripts:** ubuntu-netinstall-iso.sh, a 1 MB unattended Ubuntu netinstall ISO ([fbfcd15](https://github.com/a5m0/ipmi-connect/commit/fbfcd15b2de10aa05299ef44721c9ddea89b330a))
* show the round trip to the BMC as Ping in the stats bar ([363c097](https://github.com/a5m0/ipmi-connect/commit/363c09709ac979733370b10ca89bf0e66abb9fd3))
* **supermicro:** ASUS ASMB8-iKVM support (MegaRAC web login, ASPEED video) ([27f3793](https://github.com/a5m0/ipmi-connect/commit/27f379390754203e673b6ec6527cefb80830e0d0))
* **tools:** megarac-capture, a logging proxy for JViewer captures ([f5bc080](https://github.com/a5m0/ipmi-connect/commit/f5bc080c39caefba101b244363ca6348f6a460ce))
* **trust:** offer static-RSA AES-GCM suites under legacy TLS consent ([d89d1ee](https://github.com/a5m0/ipmi-connect/commit/d89d1ee993d7cc91f907147d58ea9c2dd5a580a1))
* **ui:** drive-centric virtual media panel ([b5cb79f](https://github.com/a5m0/ipmi-connect/commit/b5cb79fe9d94c0fa65e19ed6cf1ad7d58d4306de))

### Bug Fixes

* golangci-lint findings (errcheck, ineffassign, staticcheck) ([8d34672](https://github.com/a5m0/ipmi-connect/commit/8d34672dd1f546973561eb6c1440a34622809040))
* **megarac:** accept the ASMB8's IUSB headers; explain busy drives ([c5f33f7](https://github.com/a5m0/ipmi-connect/commit/c5f33f71f468b6e465fa19aac38598fc9ef5b1fc))
* **megarac:** leave the KVM and media the way JViewer does ([4dc1aef](https://github.com/a5m0/ipmi-connect/commit/4dc1aeff824de336074a5573e5ac27e39d897325))
* **megarac:** one media drive at a time, with the BMC's two-minute wait ([4279bac](https://github.com/a5m0/ipmi-connect/commit/4279bac3ac9d69a5483b3cc1fd9c88b42bc39e76))
* **megarac:** tell a reconnected KVM session that a drive is connected ([8c39991](https://github.com/a5m0/ipmi-connect/commit/8c399910b3ff0b9fde7ade6ecdb0b4ca8152b3b3))
* **supermicro:** ride out host-reboot outages and reclaim keyboard/mouse control ([b78a945](https://github.com/a5m0/ipmi-connect/commit/b78a945ae72aec99d4a96aeae1703a08934d6f7b))

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
