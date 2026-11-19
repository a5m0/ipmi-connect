// Package session manages the connection to one BMC at a time: trust establishment, login,
// the vendor's background loops, and a clean logout. The CLI and the desktop connection manager
// both drive it.
//
// Connect never sends a credential before every TLS endpoint the session will use has been
// probed and accepted by the trust policy, and every consent the session needs (an unknown
// certificate, obsolete TLS, unencrypted channels) has been given explicitly in the request. A
// missing decision returns *ConsentRequired and nothing is sent or stored.
package session

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/config"
	"github.com/a5m0/ipmi-connect/internal/trust"
)

// Phases of the manager.
const (
	PhaseIdle          = "idle"
	PhaseConnecting    = "connecting"
	PhaseConnected     = "connected"
	PhaseDisconnecting = "disconnecting"
)

// disconnectWait bounds how long Disconnect waits for a cancelled attempt to finish (a login
// in flight times out after about ten seconds per request).
const disconnectWait = 30 * time.Second

// ErrBusy is returned while another connect or disconnect is in progress.
var ErrBusy = errors.New("a connection attempt is already in progress")

// Display is where a session's output goes and what it attaches its console to: the local web
// server. Its methods must not call back into the Manager.
type Display interface {
	UpdateScreen(width, height int, pix []byte)
	SetStatus(text string)
	SetFPS(fps int)
	SetPower(state string)
	// Attach makes console the target of browser input and records the session's description.
	Attach(console bmc.Console, state bmc.SessionState)
	// Detach stops browser input to the console, waits until every browser's held keys and
	// buttons have been released on it, and clears the picture and session state.
	Detach()
}

// Request is one connection attempt. Password is used for this session only: it is never
// logged, stored, or included in errors; String and GoString redact it.
type Request struct {
	Address  string `json:"address"`
	User     string `json:"user"`
	Password string `json:"password"`
	Vendor   string `json:"vendor"` // auto, dell, supermicro or megarac

	// TrustFingerprints are certificates the user has explicitly confirmed. Only fingerprints
	// that the probe actually observed for this device are pinned.
	TrustFingerprints []string `json:"trust_fingerprints"`
	// ReplaceFingerprints confirms replacing a pinned certificate that has changed.
	ReplaceFingerprints bool `json:"replace_fingerprints"`
	// AllowLegacyTLS and AllowInsecureChannels are explicit consents, remembered for the device.
	AllowLegacyTLS        bool `json:"allow_legacy_tls"`
	AllowInsecureChannels bool `json:"allow_insecure_channels"`
}

func (r Request) String() string {
	return fmt.Sprintf("{%s@%s vendor=%s password=[redacted]}", r.User, r.Address, r.Vendor)
}

// GoString redacts the password from %#v.
func (r Request) GoString() string { return r.String() }

// CertDecision is a certificate the user must confirm.
type CertDecision struct {
	Endpoint string         `json:"endpoint"`
	Status   string         `json:"status"` // "unknown" or "changed"
	Cert     trust.CertInfo `json:"cert"`
	Pinned   []string       `json:"pinned,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Protocol string         `json:"protocol"`
	Cipher   string         `json:"cipher"`
}

// ConsentRequired lists the decisions the user must make before the connection can proceed.
type ConsentRequired struct {
	Device       string         `json:"device"`
	Vendor       string         `json:"vendor"`
	Certificates []CertDecision `json:"certificates,omitempty"`
	// LegacyTLS lists endpoints that need TLS 1.0/1.1 or CBC/3DES suites.
	LegacyTLS []string `json:"legacy_tls,omitempty"`
	// InsecureChannels describes channels without effective encryption.
	InsecureChannels []string `json:"insecure_channels,omitempty"`
}

func (c *ConsentRequired) Error() string {
	var parts []string
	for _, d := range c.Certificates {
		parts = append(parts, fmt.Sprintf("%s certificate on %s (SHA-256 %s)", d.Status, d.Endpoint, d.Cert.Fingerprint))
	}
	if len(c.LegacyTLS) > 0 {
		parts = append(parts, "obsolete TLS on "+strings.Join(c.LegacyTLS, ", "))
	}
	if len(c.InsecureChannels) > 0 {
		parts = append(parts, "unencrypted channels: "+strings.Join(c.InsecureChannels, "; "))
	}
	return "confirmation needed for " + c.Device + ": " + strings.Join(parts, "; ")
}

// Status is the manager's state for the UI.
type Status struct {
	Phase         string                `json:"phase"`
	Host          string                `json:"host,omitempty"`
	Vendor        string                `json:"vendor,omitempty"`
	Channels      []bmc.ChannelSecurity `json:"channels"`
	SecurityLevel string                `json:"security_level,omitempty"`
	LastError     string                `json:"last_error,omitempty"`
}

// Options configure a Manager.
type Options struct {
	Trust    *trust.Store
	Profiles *config.Store // optional: recents are recorded here
	Display  Display
	// Vendors overrides the built-in vendor implementations (tests).
	Vendors map[bmc.TargetType]*Vendor
	// Roots overrides the system certificate roots (tests).
	Roots *x509.CertPool
}

// Manager runs at most one BMC session.
type Manager struct {
	opts    Options
	vendors map[bmc.TargetType]*Vendor

	mu      sync.Mutex
	phase   string
	cancel  context.CancelFunc
	attempt chan struct{} // closed when the connection attempt in progress has finished
	live    *Live
	ep      *bmc.Endpoint
	vendor  *Vendor
	lastErr string
}

// NewManager returns an idle manager.
func NewManager(opts Options) *Manager {
	if opts.Trust == nil {
		opts.Trust = trust.NewMemoryStore()
	}
	vendors := opts.Vendors
	if vendors == nil {
		vendors = builtinVendors()
	}
	return &Manager{opts: opts, vendors: vendors, phase: PhaseIdle}
}

// Status reports the current phase and connection security.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Phase: m.phase, LastError: m.lastErr, Channels: []bmc.ChannelSecurity{}}
	if m.live != nil && m.ep != nil {
		st.Host, st.Vendor = m.ep.Host, m.vendor.Name
		st.Channels = m.live.Channels()
		st.SecurityLevel = bmc.SecurityLevelOf(st.Channels)
	}
	return st
}

// Connect connects to the BMC in req, first disconnecting any current session. It returns
// *ConsentRequired when the user must decide something; nothing has been sent to the BMC then
// except TLS probes and port checks.
func (m *Manager) Connect(ctx context.Context, req Request) (Status, error) {
	m.mu.Lock()
	if m.phase == PhaseConnecting || m.phase == PhaseDisconnecting {
		m.mu.Unlock()
		return Status{}, ErrBusy
	}
	prev := m.live
	m.live, m.ep, m.vendor = nil, nil, nil
	m.phase = PhaseConnecting
	m.lastErr = ""
	cctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	attempt := make(chan struct{})
	m.attempt = attempt
	m.mu.Unlock()
	defer cancel()
	defer close(attempt) // after the result is recorded and any late session stopped

	if prev != nil {
		log.Printf("[Session] Disconnecting from %q before connecting to %q", prev.Host, req.Address)
		m.teardown(prev)
	}

	live, ep, vendor, err := m.connect(cctx, req)

	if err == nil && cctx.Err() != nil {
		// Cancelled after the login succeeded: end that session before reporting.
		live.Stop()
		live, err = nil, errors.New("connection cancelled")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancel = nil
	if err != nil {
		m.phase = PhaseIdle
		var consent *ConsentRequired
		if !errors.As(err, &consent) {
			m.lastErr = err.Error()
		}
		return Status{Phase: PhaseIdle, LastError: m.lastErr, Channels: []bmc.ChannelSecurity{}}, err
	}
	m.live, m.ep, m.vendor, m.phase = live, ep, vendor, PhaseConnected
	m.opts.Display.Attach(live.Console, bmc.SessionState{
		Host: ep.Host, TargetType: bmc.TargetType(vendor.Name), Model: live.Model, Firmware: live.Firmware,
		Connected: true, Authenticated: true, PowerState: bmc.PowerStateUnknown,
	})
	if m.opts.Profiles != nil {
		if err := m.opts.Profiles.AddRecent(req.Address, req.User, vendor.Name); err != nil {
			log.Printf("[Session] Could not record the recent device: %v", err)
		}
	}
	log.Printf("[Session] Connected to %q (%s)", ep.Host, vendor.Name)
	st := Status{Phase: PhaseConnected, Host: ep.Host, Vendor: vendor.Name, Channels: live.Channels()}
	st.SecurityLevel = bmc.SecurityLevelOf(st.Channels)
	return st, nil
}

// Disconnect cancels a connection attempt, or ends the session: browser input is released, the
// vendor loops stop, virtual media is ejected and the BMC is logged out. It returns once done,
// also for an attempt in progress (which may have to log out of a BMC it had just logged in to).
func (m *Manager) Disconnect() {
	m.mu.Lock()
	switch m.phase {
	case PhaseConnecting:
		if m.cancel != nil {
			m.cancel()
		}
		attempt := m.attempt
		m.mu.Unlock()
		if attempt != nil {
			select {
			case <-attempt:
			case <-time.After(disconnectWait):
				log.Printf("[Session] The connection attempt did not stop within %v", disconnectWait)
			}
		}
		return
	case PhaseConnected:
		live := m.live
		m.live, m.ep, m.vendor = nil, nil, nil
		m.phase = PhaseDisconnecting
		m.mu.Unlock()
		m.teardown(live)
		m.mu.Lock()
		m.phase = PhaseIdle
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
}

func (m *Manager) teardown(live *Live) {
	m.opts.Display.Detach()
	live.Stop()
	log.Printf("[Session] Disconnected from %s", live.Host)
}

// connect runs the trust gate and then the vendor's login.
func (m *Manager) connect(ctx context.Context, req Request) (*Live, *bmc.Endpoint, *Vendor, error) {
	host, _, err := bmc.ParseAddress(req.Address)
	if err != nil {
		return nil, nil, nil, &InputError{err}
	}
	if strings.TrimSpace(req.User) == "" {
		return nil, nil, nil, &InputError{errors.New("enter the BMC user name")}
	}
	if req.Password == "" {
		return nil, nil, nil, &InputError{errors.New("enter the BMC password")}
	}
	device := trust.DeviceKey(host)
	policy := &trust.Policy{Store: m.opts.Trust, Device: device, Roots: m.opts.Roots}
	ep, err := bmc.Resolve(ctx, req.Address, policy)
	if err != nil {
		return nil, nil, nil, err
	}

	vendor, err := m.chooseVendor(ctx, ep, req.Vendor)
	if err != nil {
		return nil, nil, nil, err
	}

	if err := m.establishTrust(ctx, ep, vendor, req); err != nil {
		return nil, nil, nil, err
	}

	if vendor == nil { // ports did not tell; ask the (now trusted) web server
		vendor = m.vendors[detectByBanner(ctx, ep)]
		if vendor == nil {
			return nil, nil, nil, &InputError{errors.New("could not detect the BMC type; choose Dell, Supermicro or AMI MegaRAC")}
		}
		// The vendor's own channels were not covered by the probe: check them now.
		if err := m.establishTrust(ctx, ep, vendor, req); err != nil {
			return nil, nil, nil, err
		}
	}
	if ctx.Err() != nil {
		return nil, nil, nil, ctx.Err()
	}

	log.Printf("[Session] Logging in to %q (%s) as %q", ep.Host, vendor.Name, req.User)
	live, err := vendor.Connect(ctx, ep, req.User, req.Password, m.opts.Display)
	if err != nil {
		return nil, nil, nil, err
	}
	live.Host = ep.Host
	return live, ep, vendor, nil
}

func (m *Manager) chooseVendor(ctx context.Context, ep *bmc.Endpoint, name string) (*Vendor, error) {
	switch strings.ToLower(name) {
	case "", string(bmc.TargetAuto):
		t := detectByPort(ctx, ep, m.vendors)
		if t != "" {
			log.Printf("[Session] %q looks like a %s BMC", ep.Host, t)
		}
		return m.vendors[t], nil // nil: decide after trust from the web banner
	}
	v := m.vendors[bmc.TargetType(strings.ToLower(name))]
	if v == nil {
		return nil, &InputError{fmt.Errorf("unknown BMC type %q", name)}
	}
	return v, nil
}

// establishTrust probes every TLS endpoint the session will use and applies the user's
// decisions from req. It persists only decisions that req carries and the probes justify.
func (m *Manager) establishTrust(ctx context.Context, ep *bmc.Endpoint, vendor *Vendor, req Request) error {
	results, err := m.probe(ctx, ep, vendor)
	if err != nil {
		return err
	}
	need := &ConsentRequired{Device: ep.Policy.Device}
	if vendor != nil {
		need.Vendor = vendor.Name
	}
	confirmed := map[string]bool{}
	for _, fp := range req.TrustFingerprints {
		if c, err := trust.ParseFingerprint(fp); err == nil {
			confirmed[c] = true
		}
	}

	var toTrust, toReplace []string
	for _, r := range results.tls {
		certErr := ep.Policy.CheckCertificate(ep.Host, r)
		var unknown *trust.UnknownCertificateError
		var changed *trust.CertificateChangedError
		switch {
		case certErr == nil:
		case errors.As(certErr, &unknown):
			if confirmed[r.Cert.Fingerprint] {
				toTrust = append(toTrust, r.Cert.Fingerprint)
			} else {
				need.Certificates = append(need.Certificates, CertDecision{
					Endpoint: r.Endpoint, Status: "unknown", Cert: r.Cert, Reason: unknown.Reason, Protocol: r.Version, Cipher: r.Cipher,
				})
			}
		case errors.As(certErr, &changed):
			if req.ReplaceFingerprints && confirmed[r.Cert.Fingerprint] {
				toReplace = append(toReplace, r.Cert.Fingerprint)
			} else {
				need.Certificates = append(need.Certificates, CertDecision{
					Endpoint: r.Endpoint, Status: "changed", Cert: r.Cert, Pinned: changed.Pinned, Protocol: r.Version, Cipher: r.Cipher,
				})
			}
		default:
			return certErr
		}
		if r.Legacy && !ep.Policy.Legacy() && !req.AllowLegacyTLS {
			need.LegacyTLS = append(need.LegacyTLS, fmt.Sprintf("%s (%s, %s)", r.Endpoint, r.Version, r.Cipher))
		}
	}
	dev, _ := m.opts.Trust.Get(ep.Policy.Device)
	kvmTLS := results.kvmTLS
	downgraded := results.kvmProbed && !kvmTLS && dev.KVMTLS
	if len(results.insecure) > 0 && !req.AllowInsecureChannels && (downgraded || !ep.InsecureChannelsAllowed()) {
		need.InsecureChannels = results.insecure
		if downgraded {
			need.InsecureChannels = append([]string{"WARNING: this BMC's KVM port offered TLS before and now does not. " +
				"Someone may be stripping encryption from the connection."}, results.insecure...)
		}
	}
	if len(need.Certificates) > 0 || len(need.LegacyTLS) > 0 || len(need.InsecureChannels) > 0 {
		return need
	}

	// Every decision is made: record them, then connect.
	device := ep.Policy.Device
	if len(toReplace) > 0 {
		keep := slices.Clone(toReplace)
		for _, r := range results.tls {
			if ep.Policy.CheckCertificate(ep.Host, r) == nil {
				keep = append(keep, r.Cert.Fingerprint) // another endpoint's still-valid pin
			}
		}
		if err := m.opts.Trust.Replace(device, append(keep, toTrust...)); err != nil {
			return err
		}
		log.Printf("[Trust] Replaced the pinned certificate of %q with %q", device, strings.Join(toReplace, ", "))
	} else {
		for i, fp := range toTrust {
			if err := m.opts.Trust.Trust(device, fp, i > 0 || hasPins(m.opts.Trust, device)); err != nil {
				return err
			}
			log.Printf("[Trust] Pinned certificate %s for %q at the user's request", fp, device)
		}
	}
	if kvmTLS != dev.KVMTLS && (kvmTLS || downgraded) {
		if err := m.opts.Trust.SetKVMTLS(device, kvmTLS); err != nil {
			return err
		}
	}
	legacyNeeded := slices.ContainsFunc(results.tls, func(r trust.ProbeResult) bool { return r.Legacy })
	if (req.AllowLegacyTLS && legacyNeeded) || (req.AllowInsecureChannels && len(results.insecure) > 0) {
		if err := m.opts.Trust.SetConsent(device, req.AllowLegacyTLS && legacyNeeded, req.AllowInsecureChannels && len(results.insecure) > 0); err != nil {
			return err
		}
	}
	return nil
}

func hasPins(s *trust.Store, device string) bool {
	d, _ := s.Get(device)
	return len(d.Fingerprints) > 0
}

type probeResults struct {
	tls       []trust.ProbeResult
	insecure  []string
	kvmProbed bool // the vendor's channel probe completed (its absence of TLS is meaningful)
	kvmTLS    bool // the vendor's KVM port offered TLS
}

// probe learns the certificates of the web server and the vendor's TLS channels.
func (m *Manager) probe(ctx context.Context, ep *bmc.Endpoint, vendor *Vendor) (probeResults, error) {
	var out probeResults
	dialWeb := func(ctx context.Context) (netConn, error) { return ep.DialPort(ctx, ep.HTTPSPort) }
	web, err := trust.ProbeWithFallback(ctx, dialWeb, ep.Host, "https")
	if err != nil {
		return out, fmt.Errorf("could not reach the BMC's web server at %s: %w", ep.BaseURL(), err)
	}
	out.tls = append(out.tls, web)
	if vendor != nil && vendor.Probe != nil {
		extra, insecure, err := vendor.Probe(ctx, ep)
		if err != nil {
			log.Printf("[Session] Probing the %s channels of %q: %q (checked again when they connect)", vendor.Name, ep.Host, err)
		} else {
			out.kvmProbed = true
		}
		for _, r := range extra {
			if r.Endpoint == "kvm" {
				out.kvmTLS = true
			}
			if !slices.ContainsFunc(out.tls, func(x trust.ProbeResult) bool {
				return x.Cert.Fingerprint == r.Cert.Fingerprint && x.Legacy == r.Legacy
			}) {
				out.tls = append(out.tls, r)
			}
		}
		out.insecure = insecure
	}
	return out, nil
}

// InputError is a problem with the request itself.
type InputError struct{ Err error }

func (e *InputError) Error() string { return e.Err.Error() }
func (e *InputError) Unwrap() error { return e.Err }
