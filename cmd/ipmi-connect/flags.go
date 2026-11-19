package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// options holds the parsed command line.
type options struct {
	Host        string
	User        string // empty means IPMI_CONNECT_USER, else root
	Password    string // empty means IPMI_CONNECT_PASSWORD, else prompt
	Type        string // auto, dell, supermicro or megarac
	WebPort     int
	WebPortSet  bool // --web-port was given (desktop mode otherwise lets the OS choose)
	NoBrowser   bool
	ShowHelp    bool
	ShowVersion bool
	// PortIgnored records a --port flag: accepted for compatibility with older launchers, but
	// BMC ports come from the protocol.
	PortIgnored bool

	Desktop bool // start the connection manager without a BMC

	// Trust decisions for non-interactive use. Each is an explicit authorisation.
	TrustFingerprints  []string
	ReplaceFingerprint bool
	AllowLegacyTLS     bool
	AllowInsecure      bool

	MediaDirs   []string
	IdleTimeout time.Duration
	IdleSet     bool

	Debug []string // --debug areas, as given (each may be a comma list)
}

const defaultWebPort = 8080

func printUsage() {
	name := os.Args[0]
	p := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format, a...) }
	p("Usage: %s [--desktop] [options]\n       %s <host> [options]\n\n", name, name)
	p("Browser KVM console for legacy Dell (Avocent), Supermicro (ATEN) and AMI MegaRAC BMCs.\n")
	p("Without a host it starts the desktop connection manager in your browser.\n\n")
	p("Options:\n")
	p("  --desktop                 Start the connection manager (the default without a host)\n")
	p("  -u, --user <username>     BMC username (default: $%s, else root)\n", envUser)
	p("  -p, --pass <password>     Deprecated: visible in the process list. Use $%s\n", envPassword)
	p("                            or the prompt.\n")
	p("  --type <auto|dell|supermicro|megarac>  Force BMC vendor type (default: auto)\n")
	p("  --web-port <port>         Local console port (default: 8080 with a host, else any free port)\n")
	p("  --no-browser              Do not open the web browser; print the link\n")
	p("  --idle-timeout <duration> Disconnect and quit after this long with no browser\n")
	p("                            (default: 30m in desktop mode, off with a host; 0 disables)\n")
	p("  --media-dir <folder>      Also allow virtual media images from this folder (repeatable)\n")
	p("  --debug <areas>           Log protocol details for troubleshooting (megarac-media);\n")
	p("                            also $%s\n\n", envDebug)
	p("Trust (asked interactively when a terminal is available):\n")
	p("  --trust-fingerprint <SHA256>  Trust the BMC certificate with this SHA-256 fingerprint\n")
	p("  --replace-fingerprint     Replace a pinned certificate that changed (with --trust-fingerprint)\n")
	p("  --allow-legacy-tls        Allow TLS 1.0/1.1 and CBC/3DES ciphers for this BMC\n")
	p("  --allow-insecure-channels Allow unencrypted KVM/virtual media channels for this BMC\n\n")
	p("  -v, --version             Show version and exit\n")
	p("  -h, --help                Show this help message\n\n")
	p("Examples:\n")
	p("  %s                                  # desktop connection manager\n", name)
	p("  %s 192.0.2.10                       # prompts for the root password\n", name)
	p("  %s 192.0.2.10 -u admin --no-browser\n", name)
	p("  %s=... %s 192.0.2.20 --type supermicro --allow-insecure-channels\n", envPassword, name)
}

// parseArgs accepts single- and double-dash flags, with values either as the next
// argument or after '='. A flag value that starts with '-' is treated as absent.
func parseArgs(args []string) options {
	o := options{Type: "auto", WebPort: defaultWebPort}

	// next returns the following argument as a value, unless it looks like a flag.
	next := func(i *int) (string, bool) {
		if *i+1 < len(args) && !strings.HasPrefix(args[*i+1], "-") {
			*i++
			return args[*i], true
		}
		return "", false
	}
	setPort := func(v string) {
		if p, err := strconv.Atoi(v); err == nil && p >= 0 && p < 65536 {
			o.WebPort, o.WebPortSet = p, true
		}
	}
	setIdle := func(v string) {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			o.IdleTimeout, o.IdleSet = d, true
		} else if v == "0" {
			o.IdleTimeout, o.IdleSet = 0, true
		}
	}
	// value handles "--name value" and "--name=value" for the given spellings.
	value := func(i *int, arg string, set func(string), names ...string) bool {
		for _, n := range names {
			if arg == n {
				if v, ok := next(i); ok {
					set(v)
				}
				return true
			}
			if v, ok := strings.CutPrefix(arg, n+"="); ok {
				set(v)
				return true
			}
		}
		return false
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "-help":
			o.ShowHelp = true
		case arg == "-v" || arg == "--version" || arg == "-version":
			o.ShowVersion = true
		case arg == "--no-browser" || arg == "-no-browser":
			o.NoBrowser = true
		case arg == "--desktop" || arg == "-desktop":
			o.Desktop = true
		case arg == "--replace-fingerprint":
			o.ReplaceFingerprint = true
		case arg == "--allow-legacy-tls":
			o.AllowLegacyTLS = true
		case arg == "--allow-insecure-channels" || arg == "--allow-insecure-kvm":
			o.AllowInsecure = true
		case value(&i, arg, func(v string) { o.User = v }, "-u", "--user", "-user"):
		case value(&i, arg, func(v string) { o.Password = v }, "-p", "--pass", "-pass"):
		case value(&i, arg, func(v string) { o.Type = v }, "--type", "-type"):
		case value(&i, arg, setPort, "--web-port", "-web-port"):
		case value(&i, arg, setIdle, "--idle-timeout"):
		case value(&i, arg, func(v string) { o.TrustFingerprints = append(o.TrustFingerprints, v) }, "--trust-fingerprint"):
		case value(&i, arg, func(v string) { o.MediaDirs = append(o.MediaDirs, v) }, "--media-dir"):
		case value(&i, arg, func(v string) { o.Debug = append(o.Debug, v) }, "--debug"):
		case value(&i, arg, func(string) { o.PortIgnored = true }, "--port", "-port"):
			o.PortIgnored = true
		case !strings.HasPrefix(arg, "-"):
			if o.Host == "" {
				o.Host = arg
			}
		}
	}
	return o
}
