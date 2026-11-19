# Capturing JViewer on a MegaRAC BMC

When ipmi-connect and AMI's JViewer disagree about the protocol, watch what JViewer does.
`tools/megarac-capture` is a logging TLS proxy that sits between JViewer and the BMC. It logs the
structure of the traffic, never its contents:

- each connection's tunnel request and the BMC's reply (section 2 of
  [asus-asmb8-ikvm.md](../asus-asmb8-ikvm.md)), with cookie values blanked;
- IUSB frame headers on media tunnels: length, device, protocol, direction, client data,
  instance, sequence, opcode, CDB, status and sense, and data length. For private opcodes
  (`0xF0` and up) it also logs a data tail of up to 4 bytes. It never logs the auth token or any
  disk data;
- IVTP message types, sizes and statuses on the KVM tunnel, with no payloads. Video fragments
  (type 25) are logged as counts every few seconds;
- which side closed each connection first, and how (EOF, reset or another error).

The proxy listens only on loopback. It accepts the BMC only if its certificate matches
`-fingerprint`, and it uses the same trust code as the app, so it never skips certificate checks.
JViewer, on the other side, accepts the proxy's throwaway self-signed certificate.

## Prerequisites

- A JRE with AWT, not a headless one (JViewer is a Swing application), plus Xvfb and xdotool to
  run and drive it without a desktop. ImageMagick's `import -window root` takes screenshots.
- The BMC's certificate fingerprint:

  ```sh
  openssl s_client -connect <bmc>:443 </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256
  ```

- A dedicated, empty directory for JViewer's files (call it `$J`). Keep the session cookie, CSRF
  token and launch arguments in a `chmod 700` subdirectory, outside the repository.

## Procedure

1. **Start the proxy** (the ASMB8 needs `-legacy-tls`: it negotiates
   `TLS_RSA_WITH_AES_128_GCM_SHA256`):

   ```sh
   go run ./tools/megarac-capture -bmc <bmc> -fingerprint <sha256> -legacy-tls -log "$J/capture.log"
   ```

2. **Log in to the web interface**: POST `WEBVAR_USERNAME` and `WEBVAR_PASSWORD` to
   `/rpc/WEBSES/create.asp`. The reply holds `SESSION_COOKIE` and `CSRFTOKEN`. Pass the password
   on stdin (for example `--data-urlencode WEBVAR_PASSWORD@-`), not on the command line.
3. **Get the launch arguments**: GET `/Java/jviewer.jnlp?EXTRNIP=<bmc>&JNLPSTR=JViewer` with the
   headers `Cookie: SessionCookie=<cookie>` and `CSRFTOKEN: <token>`. The `<argument>` elements,
   in order, are JViewer's command line. They include the KVM token and the web cookie, so treat
   them as secrets.
4. **Point the arguments at the proxy**: set `-hostname` to `127.0.0.1`, and set both `-kvmport`
   and `-websecureport` to the proxy's port (8443 by default). JViewer opens its tunnels to the
   KVM port, so changing only `-websecureport` is not enough.
5. **Download the viewer** with the same two headers: `JViewer.jar`, `JViewer-SOC.jar` and
   `Linux_x86_64.jar` from `/Java/release/`, into `$J`. Extract the native libraries:
   `mkdir "$J/native" && cd "$J/native" && unzip ../Linux_x86_64.jar`.
6. **Run it** on a virtual display, with the arguments read from the file, one per line, so they
   never appear on a command line:

   ```sh
   Xvfb :77 -screen 0 1600x1000x24 &
   cd "$J" && DISPLAY=:77 bash -c 'mapfile -t A < secret/args; exec java -Djava.library.path="$PWD/native" \
     -cp JViewer.jar:JViewer-SOC.jar com.ami.kvm.jviewer.JViewer "${A[@]}"'
   ```

   Drive the menus with `xdotool mousemove X Y click 1` and `xdotool type`, and take a screenshot
   after each step to find the next control.
7. **Clean up**: exit JViewer, kill the Java process, log out of the web session (GET
   `/rpc/WEBSES/logout.asp` with both headers), then stop the proxy and Xvfb.

## Pitfalls

- **curl exits 56** on the BMC's replies: the BMC closes without TLS `close_notify`. The body is
  complete, so treat exit code 56 as success.
- **JViewer's media auth has no timeout.** If the BMC never answers it (see "one drive at a time"
  in [asus-asmb8-ikvm.md](../asus-asmb8-ikvm.md) §4), the Virtual Media dialog freezes. To get
  it back, kill that socket: find JViewer's connection to the proxy with `ss -tnp | grep 8443`,
  then run `sudo ss -K src <addr> sport = :<port>`.
- **After Exit, the Java process lingers.** Kill it before you launch again, or the old one
  keeps its sockets open.
- **Log out of the web session** afterwards. The BMC has few session slots, and a leftover session
  holds one until it times out.
- **The two-minute media rule**: after a media tunnel closes, the ASMB8 keeps its one media slot
  for about two minutes. An attach in that window, from JViewer or ipmi-connect, goes unanswered
  and holds the slot again. Wait two minutes between tests that close a drive.
- **One user at a time**: don't run ipmi-connect, the smoke test and JViewer against the board
  at the same time.

## Logs stay out of the repository

A capture log names the BMC (in the `CONNECT` lines and the listen message) and shows when the
host did what. Keep logs in a scratch directory and never commit them. Record findings in the
docs as layout and behaviour only, as [asus-asmb8-ikvm.md](../asus-asmb8-ikvm.md) does.

Before you quote a log line anywhere, check that no secret got through. For example, grep the
log for the session cookie and the KVM token from the launch arguments. Both searches should
find nothing:

```sh
grep -cF -f "$J/secret/cookie" "$J/capture.log"
```
