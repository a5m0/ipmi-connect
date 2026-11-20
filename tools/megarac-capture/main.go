// Command megarac-capture is a logging TLS proxy between AMI's JViewer and a MegaRAC BMC, for
// protocol work: it logs the tunnel services (cookies redacted), IUSB frame headers, IVTP message
// types and sizes, and which side closed each connection first. It never logs tokens, screen
// contents or disk data. See docs/testing/jviewer-capture.md.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "megarac-capture:", err)
		os.Exit(2)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8443", "loopback address:port for JViewer to connect to")
	bmcFlag := flag.String("bmc", "", "the BMC's host[:port] (port 443 by default)")
	fingerprint := flag.String("fingerprint", "", "SHA-256 fingerprint of the BMC's certificate (required)")
	legacy := flag.Bool("legacy-tls", false, "allow obsolete TLS to the BMC (the ASMB8 needs it)")
	logPath := flag.String("log", "", "also write the log to this file")
	flag.Parse()

	if err := checkListen(*listen); err != nil {
		return err
	}
	host, addr, err := parseBMC(*bmcFlag)
	if err != nil {
		return err
	}
	if *fingerprint == "" {
		return fmt.Errorf("-fingerprint is required (openssl s_client -connect %s | openssl x509 -noout -fingerprint -sha256)", addr)
	}
	dial, err := upstreamDialer(host, addr, *fingerprint, *legacy)
	if err != nil {
		return err
	}
	var out io.Writer = os.Stdout
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		out = io.MultiWriter(f, os.Stdout)
	}
	cfg, _, err := listenerConfig()
	if err != nil {
		return err
	}
	ln, err := tls.Listen("tcp", *listen, cfg)
	if err != nil {
		return err
	}
	logf := newLogger(out)
	logf("listening on %s for %s", ln.Addr(), addr)
	p := &proxy{target: addr, dial: dial, logf: logf}
	return p.serve(ln)
}
