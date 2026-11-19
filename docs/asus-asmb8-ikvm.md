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

1. Open TLS to port 443 (**verified**: TLS 1.2, static RSA with AES-GCM; ipmi-connect gets
   `TLS_RSA_WITH_AES_128_GCM_SHA256`, OpenSSL `AES256-GCM-SHA384`).
2. Send two ASCII requests, back to back **(verified)**:
   ```
   CONNECT <host>:443 HTTPS/1.1\n cookie <webcookie>\r\n\r\n
   JVIEWER VIDEO cookie <webcookie>\r\n\r\n
   ```
   The odd spacing (`\n cookie`) is what the viewer sends. Virtual media uses the same request
   with `CDMEDIA`, `FDMEDIA` or `HDMEDIA` in place of `VIDEO`, one tunnel per drive **(verified for
   CD and floppy)**.
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

### 3.7 Keyboard LEDs (verified)

`KEYBOARD_LEDS` (20) carries one payload byte: bit 0 Num Lock, bit 1 Caps Lock, bit 2 Scroll Lock.
The BMC sends it straight after validation.

---

## 4. Virtual media (IUSB)

Each drive gets its own tunnel (section 2). After the tunnel reply the socket carries IUSB frames:
a 32-byte header, then `u32` (little-endian, at offset 12) bytes. The layout below is from the
viewer's wire behaviour; offsets count from the start of the frame.

| Offset | Size | Field |
| ---: | ---: | :--- |
| 0 | 8 | signature `IUSB    ` (the only field the client should check: the BMC leaves 8..10 zero **(verified)**) |
| 8, 9, 10 | 1 each | major 1, minor 0, header length 32 (as the client sends them) |
| 11 | 1 | checksum: the 32 header bytes sum to zero |
| 12 | 4 | length of what follows the header |
| 17, 18 | 1 each | device type 5, protocol 1 (the client sends these for every drive type) |
| 19 | 1 | direction: `0x80` from the client |
| 22 | 1 | client data: the drive type (`0x80` = image) for a hard disk, else 0 |
| 23 | 1 | drive instance |
| 24 | 4 | sequence number, echoed in replies |
| 41 | 12 | opcode / SCSI CDB (big-endian fields, as in SCSI) |
| 53 | 4 | reply: status (0 good, 1 check condition), sense key, ASC, ASCQ |
| 57 | 4 | reply: data length (little-endian) |
| 61 | n | data (READ replies, WRITE requests) |

- **Auth (verified)**: the client sends opcode `0xF2` at 41 with the KVM session token from 63
  (zero-padded, 160 bytes in all). The BMC answers `0xF1` with a status at 62: 1 connected,
  5 no privilege, 8 no free session, anything else in use (the other client's IP follows).
- **SCSI (verified for CD and floppy)**: the BMC forwards the host's commands; the client
  replies with the request's first 61 bytes echoed (header fields, sequence number and CDB),
  status and sense at 53..56, data length at 57 and data from 61, at most 128 KiB. The host's
  first commands were TEST UNIT READY, READ TOC, READ CAPACITY and READ(10).
- **Private opcodes**: `0xF6` (BMC) stops the drive; `0xF4` (BMC, hard disk only) asks for the
  drive type, which the client appends as one byte after an empty reply.
- **Media state**: the client sends IVTP `MEDIA_STATE` (24) on the KVM channel, `status` 1 when
  its first drive connects and 0 when the last one closes. The BMC does not answer it.
- There is no goodbye packet and no keepalive: the client closes the socket.

**Busy drives (seen on the test board)**: after a drive's tunnel closes, cleanly or not, the
BMC refuses that drive type (status 8) for about two minutes. It then stops answering the auth
at all for a few more minutes; while an auth waits, the web interface stalls too. Sending
`JVIEWER DISCONNECT` on the media tunnel before closing did not change this.

ipmi-connect therefore never closes a tunnel on eject. Each drive type's tunnel is opened on its
first attach and kept until logout. While no image is in it, every SCSI command gets CHECK
CONDITION `02/3A/00` (medium not present); when a new image goes in, the first command other
than INQUIRY gets `06/28/00` (medium may have changed), after which the drive answers normally.
Eject and re-attach are instant; only a re-attach after a logout or a dropped tunnel still
waits out the busy period.
