# ASUS ASMB8-iKVM Deep Dive

## System Profile

- **Device**: ASUS ASMB8-iKVM management module
- **BMC Hardware**: ASPEED AST2400
- **Firmware Vendor**: American Megatrends (AMI MegaRAC SP, classic `/rpc/*.asp` web interface)
- **Remote console**: AMI JViewer (`JViewer.jar` + `JViewer-SOC.jar`), single-port mode
- **Video**: ASPEED video engine, the same compression as the Dell C6220 (see [dell-c6220.md](dell-c6220.md))

Sources: the JViewer jars served by the BMC (`/Java/release/`), read with a decompiler for
interoperability, and a read-only probe of a real board. Each fact below is marked
**(verified)** when it was observed on hardware, otherwise it comes from the viewer's code.
No code was copied.

---

## 1. Web Interface

All requests go to HTTPS 443 (HTTP 80 also works and is not used). The certificate is
self-signed, and the server offers only RSA key exchange (no forward secrecy), so ipmi-connect
asks for the obsolete-TLS consent **(verified)**. It sends no `Server` header; `/` redirects to
`/index.html`, a page titled `Megarac SP` that is gzip-encoded even when compression was not
requested **(verified)**. Auto detection uses that title.

| Step | Request | Notes |
| :--- | :--- | :--- |
| Login | `POST /rpc/WEBSES/create.asp`, form `WEBVAR_USERNAME`, `WEBVAR_PASSWORD` | **(verified)** |
| Session | reply carries `'SESSION_COOKIE'` and `'CSRFTOKEN'`; later requests send `Cookie: SessionCookie=<cookie>` and a `CSRFTOKEN: <token>` header | **(verified)** |
| Failure | `SESSION_COOKIE` starts with `Failure_` (still `HAPI_STATUS:0`) | |
| Power status | `GET /rpc/hoststatus.asp`, `'JF_STATE' : 1` on, `0` off | **(verified)** |
| KVM launch | `GET /Java/jviewer.jnlp?EXTRNIP=<host>&JNLPSTR=JViewer` | **(verified)** |
| KVM token only | `GET /rpc/getsessiontoken.asp`, `'SESSION_TOKEN'` | used by the standalone viewer |
| Logout | `GET /rpc/WEBSES/logout.asp` | **(verified)** |

Replies are JavaScript fragments ending in `HAPI_STATUS:<n>`, 0 on success.

### 1.1 JNLP arguments

Requested over HTTPS, the JNLP launches `JViewer` with named arguments **(verified)**:

| Argument | Value seen | Meaning |
| :--- | :--- | :--- |
| `-kvmtoken` | 16 characters | one-time KVM session token |
| `-webcookie` | 35 characters | the web `SESSION_COOKIE` itself |
| `-kvmport` | `443` | tunnel port (80 when the JNLP is fetched over HTTP) |
| `-kvmsecure` | `1` | tunnel over TLS (0 over HTTP) |
| `-websecureport` | `443` | web port named in the tunnel request |
| `-singleportenabled` | `1` | KVM and media tunnelled through the web port |
| `-cdnum`/`-fdnum`/`-hdnum` | 1 each | virtual media instances |

---

## 2. Single-Port Tunnel

1. Open TLS to port 443 (**verified**: TLS 1.2, `AES256-GCM-SHA384`).
2. Send two ASCII requests, back to back **(verified)**:
   ```
   CONNECT <host>:443 HTTPS/1.1\n cookie <webcookie>\r\n\r\n
   JVIEWER VIDEO cookie <webcookie>\r\n\r\n
   ```
   The odd spacing (`\n cookie`) is what the viewer sends.
3. The BMC answers `HTTP/1.0 200 OK\r\n\r\n` **(verified)**; a reply containing `ERROR` names an
   error code. From then on the socket carries IVTP.
4. To leave, send `JVIEWER DISCONNECT Cookie <webcookie>\r\n\r\n`.

---

## 3. IVTP

Every message starts with an 8-byte little-endian header: `u16 type`, `u32 payload size`,
`u16 status`. Some messages carry their argument in `status` with no payload.

### 3.1 Session validation

| Step | Direction | Message |
| :--- | :--- | :--- |
| 1 | BMC → | `SESSION_ACCEPTED` (23), empty **(verified)** |
| 2 | → BMC | `GET_WEB_TOKEN` (21), payload = webcookie bytes **(verified)** |
| 3 | → BMC | `VALIDATE_VIDEO_SESSION` (18), payload below, then at once `RESUME_REDIRECTION` (6) **(verified)** |
| 4 | BMC → | `VALIDATE_VIDEO_SESSION_RESPONSE` (19): byte 0 result (1 valid, 0 invalid, 2 no KVM privilege), byte 1 session id **(verified: 1, 0)** |
| 5 | → BMC | `POWER_STATUS` (34), `DISPLAY_LOCK_SET` (51) with one byte `2`, `GET_USER_MACRO` (40) **(verified)** |

Validation payload, zero padded fields:

| Offset | Length | Field |
| :--- | :--- | :--- |
| 0 | 1 | token type, 0 = web session token |
| 1 | 129 | KVM token (`-kvmtoken`) |
| 130 | 65 | client IP address (text) |
| 195 | 129 | user name |

**Quirk (verified):** the header declares 332 payload bytes but JViewer writes only 324, followed
at once by the 8-byte `RESUME_REDIRECTION` header. Send exactly that. The BMC does not answer the
324 bytes alone, and replacing the `RESUME_REDIRECTION` header with 8 zero bytes gets the session
accepted but closed within a second.

Without step 2 the BMC ends the session with `STOP_SESSION` (8) status 7 **(verified)**.

### 3.2 After validation (verified)

The BMC sends keyboard LEDs (20), mouse mode (10, `2` = absolute), media licence status (53),
display control status (52), service status (37: web, kvm, cd/fd/hd-media entries), active
clients (39: user name and the client IP from the validation message), the power state in the
status of a type 34 reply (1 = on), user macros (40, 960 bytes), a hardware cursor message (4099,
8 bytes) and video. With the sequence in 3.1 the session stays up; video is sent when the screen
changes.

### 3.3 Stop reasons (`STOP_SESSION`, 8, in `status`)

| Status | Meaning |
| :--- | :--- |
| 2 | Redirection engine asked the viewer to close |
| 5 | Redirection configuration changed; reconnect |
| 7 | Web session logged out |
| 8 | KVM licence expired |
| 9 | KVM session timed out |
| 10 | Terminated by the server |

### 3.4 Video (verified)

`VIDEO_FRAGMENT` (25): payload starts with a `u16` fragment number; bits 0-14 are the index (0
starts a frame), bit 15 marks the last fragment. The rest is appended to the frame.

A frame is an 86-byte little-endian header, then the compressed data:

| Offset | Size | Field | Seen |
| :--- | :--- | :--- | :--- |
| 0 | 2 | engine version | 1 |
| 2 | 2 | header length | 86 |
| 4, 6 | 2, 2 | source width, height | 1024, 768 |
| 13, 15 | 2, 2 | destination width, height | 1024, 768 |
| 42 | 1 | compression mode | 3 |
| 43 | 1 | JPEG scale factor | 16 |
| 44 | 1 | JPEG table selector | 4 |
| 45 | 1 | YUV table mapping (1: chroma uses the luma tables) | 0 |
| 47 | 1 | advance table selector | 7 |
| 49 | 4 | number of macroblocks | |
| 53 | 1 | RC4 enabled | 0 |
| 55 | 1 | YUV 4:2:0 (else 4:4:4) | 0 |
| 69 | 4 | compressed size | |
| 82, 84 | 2, 2 | cursor x, y | |

The compressed data decodes with `internal/aspeed` using luma = chroma = table selector and
advance = advance selector: a captured frame decodes to the host's text console. With RC4
enabled the data is RC4-encrypted under the fixed key `fedcba9876543210` (not seen).

### 3.5 Keyboard and mouse

`HID_PKT` (1) wraps an AMI IUSB packet: a 32-byte header that starts with the signature
`IUSB    ` and holds a sequence number, then the report. Header byte 11 is a checksum that makes
the 32 header bytes sum to zero (mod 256).

- Keyboard: device `0x30`, protocol `0x10`, data length 8, the standard boot report
  (modifiers, 0, six usages); IVTP size 41 **(verified: typing, including type text)**.
- Absolute mouse: device `0x31`, protocol `0x20`, data length 6: buttons, `u16` x and y scaled
  to 0-32767, wheel; IVTP size 39.

If the BMC sends `INITIAL_ENCRYPTION_STATUS` (15), reports must be Blowfish-ECB encrypted under
the KVM token (status 255, size 41). The test board did not ask for it.

### 3.6 Power

`POWER_STATUS` (34) with no payload asks for the state; the reply carries it in `status`.
`POWER_CONTROL_REQUEST` (35) carries the IPMI chassis code in `status`: 0 off, 1 on, 2 cycle,
3 hard reset, 5 soft off; the result comes back as `POWER_CONTROL_RESPONSE` (36).
