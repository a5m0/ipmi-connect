package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/session"
	"github.com/a5m0/ipmi-connect/internal/web"
)

// connectCLI connects to the host on the command line. Trust decisions come from flags or, on a
// terminal, from prompts that show the certificate; nothing is accepted silently.
func connectCLI(ctx context.Context, a *app, opts options) error {
	if opts.Password != "" {
		log.Printf("Warning: -p/--pass is deprecated: the password is visible to other users in the process list. Use $%s or the prompt.", envPassword)
	}
	user, password, err := resolveCredentials(opts, os.Getenv, bmc.ReadPassword)
	if err != nil {
		return fmt.Errorf("reading password: %w", err)
	}
	req := session.Request{
		Address: opts.Host, User: user, Password: password, Vendor: strings.ToLower(opts.Type),
		TrustFingerprints: opts.TrustFingerprints, ReplaceFingerprints: opts.ReplaceFingerprint,
		AllowLegacyTLS: opts.AllowLegacyTLS, AllowInsecureChannels: opts.AllowInsecure,
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	for attempt := 0; attempt < 4; attempt++ {
		_, err := a.manager.Connect(ctx, req)
		var need *session.ConsentRequired
		if !errors.As(err, &need) {
			return err
		}
		if !interactive {
			return fmt.Errorf("%w\n%s", err, consentHelp(need))
		}
		if !askConsent(need, &req) {
			return errors.New("not connecting: the BMC was not trusted")
		}
	}
	return errors.New("not connecting: trust could not be established")
}

// consentHelp explains the flags that would make a non-interactive run succeed.
func consentHelp(need *session.ConsentRequired) string {
	var b strings.Builder
	b.WriteString("Run ipmi-connect in a terminal to review these, or authorise them explicitly:\n")
	for _, c := range need.Certificates {
		if c.Status == "changed" {
			fmt.Fprintf(&b, "  --trust-fingerprint %s --replace-fingerprint   (only if you know why the certificate changed)\n", c.Cert.Fingerprint)
		} else {
			fmt.Fprintf(&b, "  --trust-fingerprint %s   (check it against the BMC first)\n", c.Cert.Fingerprint)
		}
	}
	if len(need.LegacyTLS) > 0 {
		b.WriteString("  --allow-legacy-tls\n")
	}
	if len(need.InsecureChannels) > 0 {
		b.WriteString("  --allow-insecure-channels\n")
	}
	return b.String()
}

// askConsent shows each decision on the terminal and records the answers in req.
func askConsent(need *session.ConsentRequired, req *session.Request) bool {
	in := bufio.NewReader(os.Stdin)
	ask := func(prompt, yes string) bool {
		fmt.Fprint(os.Stderr, prompt)
		line, _ := in.ReadString('\n')
		return strings.EqualFold(strings.TrimSpace(line), yes)
	}
	for _, c := range need.Certificates {
		fmt.Fprintf(os.Stderr, "\n")
		if c.Status == "changed" {
			fmt.Fprintf(os.Stderr, "WARNING: THE CERTIFICATE OF %s HAS CHANGED.\n", need.Device)
			fmt.Fprintf(os.Stderr, "Someone may be intercepting the connection, or the BMC's certificate was regenerated\n")
			fmt.Fprintf(os.Stderr, "(for example by a firmware update). Pinned: %s\n", strings.Join(c.Pinned, ", "))
		} else {
			fmt.Fprintf(os.Stderr, "The certificate of %s is not trusted yet (%s).\n", need.Device, c.Reason)
		}
		printCert(c)
		if c.Status == "changed" {
			if !ask("Type \"replace\" to trust the NEW certificate instead: ", "replace") {
				return false
			}
			req.ReplaceFingerprints = true
		} else if !ask("Check the fingerprint against the BMC. Type \"yes\" to trust it: ", "yes") {
			return false
		}
		req.TrustFingerprints = append(req.TrustFingerprints, c.Cert.Fingerprint)
	}
	if len(need.LegacyTLS) > 0 {
		fmt.Fprintf(os.Stderr, "\n%s only supports obsolete TLS: %s.\n", need.Device, strings.Join(need.LegacyTLS, "; "))
		fmt.Fprintf(os.Stderr, "Obsolete TLS has known weaknesses; use it only on a trusted management network.\n")
		if !ask("Allow obsolete TLS for this BMC? [y/N] ", "y") {
			return false
		}
		req.AllowLegacyTLS = true
	}
	if len(need.InsecureChannels) > 0 {
		fmt.Fprintf(os.Stderr, "\n%s uses channels that are not effectively encrypted:\n", need.Device)
		for _, c := range need.InsecureChannels {
			fmt.Fprintf(os.Stderr, "  - %s\n", c)
		}
		fmt.Fprintf(os.Stderr, "Anyone on the network path can watch the screen and keystrokes. Use a dedicated management network.\n")
		if !ask("Allow these channels for this BMC? [y/N] ", "y") {
			return false
		}
		req.AllowInsecureChannels = true
	}
	return true
}

func printCert(c session.CertDecision) {
	fmt.Fprintf(os.Stderr, "  Endpoint:    %s (%s, %s)\n", c.Endpoint, c.Protocol, c.Cipher)
	fmt.Fprintf(os.Stderr, "  Subject:     %s\n", c.Cert.Subject)
	fmt.Fprintf(os.Stderr, "  Issuer:      %s\n", c.Cert.Issuer)
	fmt.Fprintf(os.Stderr, "  Valid:       %s to %s\n", c.Cert.NotBefore.Format("2006-01-02"), c.Cert.NotAfter.Format("2006-01-02"))
	fmt.Fprintf(os.Stderr, "  Key:         %s\n", c.Cert.Key)
	fmt.Fprintf(os.Stderr, "  SHA-256:     %s\n", c.Cert.Fingerprint)
}

// offerNewLinks mints a fresh launch link whenever Enter is pressed on the terminal, for a
// closed tab or an expired link.
func offerNewLinks(ctx context.Context, server *web.Server, noBrowser bool) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return
	}
	lines := make(chan struct{})
	go func() {
		in := bufio.NewReader(os.Stdin)
		for {
			if _, err := in.ReadString('\n'); err != nil {
				close(lines)
				return
			}
			lines <- struct{}{}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-lines:
			if !ok {
				return
			}
			showLink(server.LaunchURL(), noBrowser)
		}
	}
}

// showLink opens a launch link in the browser, or prints it. The link signs a browser in, so it
// is printed (and so may end up in a log, e.g. the journal for desktop launches) only when it
// has to be copied by hand: with --no-browser, or when no browser could be opened.
func showLink(url string, noBrowser bool) {
	origin, _, _ := strings.Cut(url, "#")
	if !noBrowser && openBrowser(url) {
		log.Printf("Console opened in your browser at %s", origin)
		if term.IsTerminal(int(os.Stdin.Fd())) {
			log.Printf("Closed the tab? Press Enter here to open it again.")
		}
		return
	}
	log.Printf("Console: %s", url)
	log.Printf("Open this link in a browser on this computer. It works once and expires; press Enter for a new one.")
}
