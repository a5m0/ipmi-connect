package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/a5m0/ipmi-connect/internal/trust"
)

const (
	dialTimeout    = 10 * time.Second
	requestTimeout = 30 * time.Second // client handshake and tunnel request
	maxBlock       = 8 << 10          // a tunnel request or reply block
	maxIUSBBody    = 1 << 20          // larger means the stream is not IUSB
	maxIVTPBody    = 4 << 20          // larger means the stream is not IVTP
	videoFragment  = 25               // IVTP type of a video fragment
	videoEvery     = 5 * time.Second  // how often video fragment counts are logged
	textPeek       = 96               // bytes of a non-protocol text line that are logged
)

// IUSB offsets, as in internal/megarac/iusb.go.
const (
	iusbHeaderLen = 32
	iusbCDBAt     = 41
	iusbCDBLen    = 12
	iusbAuthStat  = 62
	iusbStatusAt  = 53
	iusbDataLenAt = 57
	iusbDataAt    = 61
	opAuth        = 0xF2
	opAuthReply   = 0xF1
	opPrivate     = 0xF0
)

type logFunc func(format string, a ...any)

// newLogger returns a logFunc, safe for concurrent use, that writes timestamped lines to w.
func newLogger(w io.Writer) logFunc {
	var mu sync.Mutex
	start := time.Now()
	return func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintf(w, "%9.3f "+format+"\n", append([]any{time.Since(start).Seconds()}, a...)...)
	}
}

var cookieRE = regexp.MustCompile(`(?i)(cookie[ =:]+)[^\s;]+`)

// redact blanks cookie values and shows line breaks, so a block logs as one line.
func redact(s string) string {
	s = cookieRE.ReplaceAllString(s, "${1}<redacted>")
	return strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(s)
}

// isText reports whether b is printable ASCII, allowing line breaks and tabs.
func isText(b []byte) bool {
	for _, c := range b {
		if (c < 0x20 || c > 0x7E) && c != '\r' && c != '\n' && c != '\t' {
			return false
		}
	}
	return len(b) > 0
}

// checkListen accepts only a loopback IP address with a port: the proxy hands out the BMC's
// KVM and media to whoever connects.
func checkListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-listen %q: %w", addr, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("-listen %q: use a loopback address such as 127.0.0.1", addr)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("-listen %q: bad port", addr)
	}
	return nil
}

// parseBMC splits -bmc into the host (for the trust store and SNI) and host:port, defaulting
// the port to 443.
func parseBMC(s string) (host, addr string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", errors.New("-bmc is required")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host, port = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"), "443"
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil || host == "" {
		return "", "", fmt.Errorf("-bmc %q: want host or host:port", s)
	}
	return host, net.JoinHostPort(host, port), nil
}

// upstreamDialer returns a dialer for the BMC at addr that accepts only the certificate with
// fingerprint fp, through the same trust policy the app uses.
func upstreamDialer(host, addr, fp string, legacy bool) (func(context.Context) (net.Conn, error), error) {
	fp, err := trust.ParseFingerprint(fp)
	if err != nil {
		return nil, fmt.Errorf("-fingerprint: %w", err)
	}
	store := trust.NewMemoryStore()
	device := trust.DeviceKey(host)
	if err := store.Trust(device, fp, false); err != nil {
		return nil, err
	}
	p := &trust.Policy{Store: store, Device: device, AllowLegacyTLS: legacy}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: dialTimeout}, Config: p.ClientConfig(host, "https")}
	return func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }, nil
}

// listenerConfig returns a server configuration with a fresh self-signed certificate for the
// loopback addresses (JViewer accepts any certificate), and that certificate.
func listenerConfig() (*tls.Config, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "megarac-capture"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, leaf, nil
}

// proxy relays JViewer's connections to the BMC and logs their structure.
type proxy struct {
	target string // BMC host:port, put into tunnel requests
	dial   func(context.Context) (net.Conn, error)
	logf   logFunc
	nconn  atomic.Int32
}

// serve accepts connections until ln fails.
func (p *proxy) serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go p.handle(p.nconn.Add(1), c)
	}
}

// handle relays one client connection.
func (p *proxy) handle(id int32, c net.Conn) {
	logf := func(format string, a ...any) { p.logf("c%02d "+format, append([]any{id}, a...)...) }
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(requestTimeout))
	if tc, ok := c.(*tls.Conn); ok {
		if err := tc.Handshake(); err != nil {
			logf("client TLS handshake: %v", err)
			return
		}
	}
	cr := bufio.NewReader(c)
	first, err := cr.Peek(7)
	if err != nil {
		logf("client sent nothing: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	s, err := p.dial(ctx)
	cancel()
	if err != nil {
		logf("dial BMC: %v", err)
		return
	}
	defer s.Close()
	if tc, ok := s.(*tls.Conn); ok {
		st := tc.ConnectionState()
		logf("BMC TLS %s %s", tls.VersionName(st.Version), tls.CipherSuiteName(st.CipherSuite))
	}
	_ = s.SetDeadline(time.Now().Add(requestTimeout))
	sr := bufio.NewReader(s)
	service := "HTTP"
	if string(first) == "CONNECT" {
		if service, err = relayTunnel(cr, c, sr, s, p.target, logf); err != nil {
			logf("tunnel: %v", err)
			return
		}
	} else {
		line, _ := cr.Peek(min(cr.Buffered(), 256))
		l, _, _ := strings.Cut(string(line), "\r\n")
		if i := strings.IndexByte(l, '?'); i >= 0 {
			l = l[:i] + "?…"
		}
		logf("HTTP request: %s", redact(l))
	}
	_ = c.SetDeadline(time.Time{})
	_ = s.SetDeadline(time.Time{})
	logf("service %s", service)

	var once sync.Once
	closeBoth := func(who string, err error) {
		once.Do(func() {
			logf("%s closed first: %s", who, describeClose(err))
			_ = c.Close()
			_ = s.Close()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); closeBoth("client", pump(service, "C>B", cr, s, logf)) }()
	go func() { defer wg.Done(); closeBoth("BMC", pump(service, "B>C", sr, c, logf)) }()
	wg.Wait()
	logf("connection done")
}

// writeError marks an error writing to the other side, as opposed to reading from this one.
type writeError struct{ err error }

func (e writeError) Error() string { return "write to the other side: " + e.err.Error() }
func (e writeError) Unwrap() error { return e.err }

// write writes b to w, marking a failure as a writeError.
func write(w io.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return writeError{err}
	}
	return nil
}

// describeClose says how a direction ended: EOF, reset or another error.
func describeClose(err error) string {
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return "EOF"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "EOF without TLS close_notify"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	}
	return err.Error()
}

// pump forwards one direction of a connection, logging what the service carries.
func pump(service, dir string, r *bufio.Reader, w io.Writer, logf logFunc) error {
	switch service {
	case "CDMEDIA", "FDMEDIA", "HDMEDIA":
		return pumpIUSB(dir, r, w, logf)
	case "VIDEO":
		return pumpIVTP(dir, r, w, logf)
	}
	return rawCopy(dir, r, w, logf)
}

// rawCopy forwards the rest of r and logs only how many bytes went.
func rawCopy(dir string, r io.Reader, w io.Writer, logf logFunc) error {
	n, err := io.Copy(w, r)
	logf("%s raw %d bytes", dir, n)
	if err != nil {
		return err
	}
	return io.EOF
}

// readBlock reads up to and including "\r\n\r\n".
func readBlock(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for !strings.HasSuffix(b.String(), "\r\n\r\n") {
		c, err := r.ReadByte()
		if err != nil {
			return b.String(), err
		}
		b.WriteByte(c)
		if b.Len() > maxBlock {
			return b.String(), errors.New("block too long")
		}
	}
	return b.String(), nil
}

// relayTunnel forwards JViewer's two-block tunnel request ("CONNECT host:port ..." and
// "JVIEWER <service> cookie ...") with the target rewritten to the BMC, then the BMC's reply.
// It logs both, redacted, and returns the service.
func relayTunnel(cr *bufio.Reader, cw io.Writer, br *bufio.Reader, bw io.Writer, target string, logf logFunc) (string, error) {
	connect, err := readBlock(cr)
	if err != nil {
		return "", fmt.Errorf("reading the CONNECT block: %w", err)
	}
	service, err := readBlock(cr)
	if err != nil {
		return "", fmt.Errorf("reading the service block: %w", err)
	}
	logf("tunnel request: %s / %s", redact(connect), redact(service))
	line, rest, _ := strings.Cut(connect, "\n")
	f := strings.Fields(line)
	if len(f) < 3 || f[0] != "CONNECT" {
		return "", fmt.Errorf("unexpected CONNECT line %q", redact(line))
	}
	connect = fmt.Sprintf("CONNECT %s %s\n%s", target, f[2], rest)
	sf := strings.Fields(service)
	if len(sf) < 2 || sf[0] != "JVIEWER" {
		return "", fmt.Errorf("unexpected service block %q", redact(service))
	}
	if _, err := io.WriteString(bw, connect+service); err != nil {
		return "", fmt.Errorf("forwarding the request: %w", err)
	}
	reply, err := readBlock(br)
	logf("BMC tunnel reply: %s", redact(reply))
	if err != nil {
		return "", fmt.Errorf("reading the BMC's reply: %w", err)
	}
	if _, err := io.WriteString(cw, reply); err != nil {
		return "", fmt.Errorf("relaying the reply: %w", err)
	}
	return sf[1], nil
}

// fallBack forwards head and the rest of r unparsed. It logs head and what is buffered after it
// only if that is text (such as "JVIEWER DISCONNECT"), redacted; otherwise just its length.
func fallBack(dir, what string, head []byte, r *bufio.Reader, w io.Writer, logf logFunc) error {
	more, _ := r.Peek(min(r.Buffered(), textPeek))
	peek := append(append([]byte(nil), head...), more...)
	if isText(peek) {
		logf("%s non-%s text: %s", dir, what, redact(string(peek)))
	} else {
		logf("%s non-%s data (%d bytes seen); raw copy from here", dir, what, len(peek))
	}
	if err := write(w, head); err != nil {
		return err
	}
	return rawCopy(dir, r, w, logf)
}

// pumpIUSB forwards IUSB frames, logging header fields: never the auth token or any data.
func pumpIUSB(dir string, r *bufio.Reader, w io.Writer, logf logFunc) error {
	for {
		h := make([]byte, iusbHeaderLen)
		n, err := io.ReadFull(r, h)
		if n == 0 {
			return err
		}
		n32 := binary.LittleEndian.Uint32(h[12:])
		if err != nil || string(h[:8]) != "IUSB    " || n32 > maxIUSBBody {
			return fallBack(dir, "IUSB", h[:n], r, w, logf)
		}
		frame := make([]byte, iusbHeaderLen+int(n32))
		copy(frame, h)
		if _, err := io.ReadFull(r, frame[iusbHeaderLen:]); err != nil {
			return fmt.Errorf("IUSB frame cut short: %w", err)
		}
		logf("%s", describeIUSB(dir, frame))
		if err := write(w, frame); err != nil {
			return err
		}
	}
}

// describeIUSB names a frame's header fields, the SCSI status and the data length.
func describeIUSB(dir string, f []byte) string {
	d := fmt.Sprintf("%s IUSB len=%d dev=%d proto=%d dir=%02x cdata=%02x inst=%d seq=%d", dir,
		len(f)-iusbHeaderLen, f[17], f[18], f[19], f[22], f[23], binary.LittleEndian.Uint32(f[24:]))
	if len(f) <= iusbCDBAt {
		return d
	}
	op := f[iusbCDBAt]
	d += fmt.Sprintf(" op=%02x", op)
	switch op {
	case opAuth:
		return d // the token follows
	case opAuthReply:
		if len(f) > iusbAuthStat {
			d += fmt.Sprintf(" authstat=%d", f[iusbAuthStat])
		}
		return d
	}
	if len(f) >= iusbCDBAt+iusbCDBLen {
		d += fmt.Sprintf(" cdb=% x", f[iusbCDBAt:iusbCDBAt+iusbCDBLen])
	}
	if len(f) >= iusbDataAt {
		d += fmt.Sprintf(" st=%02x/%02x/%02x/%02x dlen=%d", f[iusbStatusAt], f[iusbStatusAt+1],
			f[iusbStatusAt+2], f[iusbStatusAt+3], binary.LittleEndian.Uint32(f[iusbDataLenAt:]))
		if tail := f[iusbDataAt:]; op >= opPrivate && len(tail) > 0 && len(tail) <= 4 {
			d += fmt.Sprintf(" tail=% x", tail)
		}
	}
	return d
}

// pumpIVTP forwards IVTP messages, logging type, size and status, never payloads. Video
// fragments are counted and logged every few seconds.
func pumpIVTP(dir string, r *bufio.Reader, w io.Writer, logf logFunc) error {
	var frags, fragBytes int
	last := time.Now()
	flush := func() {
		if frags > 0 {
			logf("%s IVTP video fragments x%d (%d bytes)", dir, frags, fragBytes)
			frags, fragBytes = 0, 0
		}
		last = time.Now()
	}
	defer flush()
	for {
		peek, err := r.Peek(8)
		if len(peek) == 0 {
			return err
		}
		if strings.HasPrefix("JVIEWER", string(peek[:min(len(peek), 7)])) && isText(peek) {
			flush()
			line, err := readBlock(r)
			logf("%s text: %s", dir, redact(line))
			if werr := write(w, []byte(line)); werr != nil {
				return werr
			}
			if err != nil {
				return err
			}
			continue
		}
		h := make([]byte, 8)
		n, err := io.ReadFull(r, h)
		typ := binary.LittleEndian.Uint16(h)
		size := binary.LittleEndian.Uint32(h[2:])
		status := binary.LittleEndian.Uint16(h[6:])
		if err != nil || size > maxIVTPBody {
			flush()
			logf("%s IVTP desync (type %d size %d); raw copy from here", dir, typ, size)
			if err := write(w, h[:n]); err != nil {
				return err
			}
			return rawCopy(dir, r, w, logf)
		}
		msg := make([]byte, 8+int(size))
		copy(msg, h)
		if _, err := io.ReadFull(r, msg[8:]); err != nil {
			return fmt.Errorf("IVTP message cut short: %w", err)
		}
		if typ == videoFragment {
			frags++
			fragBytes += int(size)
			if time.Since(last) > videoEvery {
				flush()
			}
		} else {
			flush()
			logf("%s IVTP type=%d size=%d status=%d", dir, typ, size, status)
		}
		if err := write(w, msg); err != nil {
			return err
		}
	}
}
