# Documentation

Reverse-engineering notes for the BMCs that `ipmi-connect` talks to. Examples use `<bmc>` for the BMC's address.

| Document | Contents |
| :--- | :--- |
| [protocols.md](protocols.md) | Wire formats: Avocent APCP/BEEF packets, Dell AppWeb API, ATEN RFB |
| [dell-c6220.md](dell-c6220.md) | Dell C6220 II deep dive: auth flow, JNLP tokens, video engine, keepalives |
| [supermicro-x8dtl-6f.md](supermicro-x8dtl-6f.md) | Supermicro X8DTL-6F deep dive: CGI quirks, RFB auth, virtual media, power |
| [asus-asmb8-ikvm.md](asus-asmb8-ikvm.md) | ASUS ASMB8-iKVM (AMI MegaRAC) deep dive: RPC login, single-port tunnel, IVTP, video, HID, power |
| [design/desktop.md](design/desktop.md) | Desktop mode, session lifecycle, connection flow, storage |
| [security/threat-model.md](security/threat-model.md) | Threats, mitigations and remaining risks |
| [security/sandbox.md](security/sandbox.md) | Flatpak permissions, portals and network limits |
| [testing/hardware-validation.md](testing/hardware-validation.md) | Manual checks that need real BMCs and desktops |
| [testing/jviewer-capture.md](testing/jviewer-capture.md) | Capturing AMI's JViewer through `tools/megarac-capture` to compare protocols |
| [implementation-plan.md](implementation-plan.md) | Work log: plan, decisions, findings, status |

## Tested hardware & firmware

| | Dell PowerEdge C6220 II | Supermicro X8DTL-6F | ASUS ASMB8-iKVM |
| :--- | :--- | :--- | :--- |
| BMC SoC | ASPEED AST2300 (ASIC type 7) | Winbond/Nuvoton WPCM450 (ARM926EJ-S) | ASPEED AST2400 |
| Firmware | `2.62.5` (May 21 2015) | `0313` / `3.13` (Jan 02 2014), tag `BL_SUPERMICRO_X7SB3_2014-01-02_B` | not recorded |
| Firmware vendor | Avocent MergePoint Embedded | ATEN | AMI MegaRAC SP (classic `/rpc/*.asp` web interface) |
| Host | C6220 II, BIOS 2.10.0 | X8DTL-6F, Intel 5500 / Dual LGA1366 | ASUS server board with the ASMB8-iKVM module |
| Web server | Mbedthis-Appweb 2.4.2 (HTTPS 443) | lighttpd 1.4.23 (HTTP 80, HTTPS 443) | no `Server` header (HTTP 80, HTTPS 443) |
| TLS | TLSv1 only, `DHE-RSA-AES256-SHA` | TLSv1 only, `AES256-SHA` | TLS 1.2, RSA key exchange only (`AES256-GCM-SHA384`) |
| KVM | TCP 7578 (APCP) | TCP 5900 (RFB) | IVTP tunnelled through HTTPS 443 |
| Virtual media | TCP 7578 (AVMP) | TCP 623 (RMCP/USB) or CIFS/HTTP share | IUSB, one tunnel per drive through HTTPS 443 |
| Default user | `root` | `root` | not recorded |
