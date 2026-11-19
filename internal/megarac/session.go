package megarac

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
)

// Status texts shown to the user.
const (
	statusNoSignal = "No video signal from the host"
	statusVideo    = "Video cannot be shown: "
)

const (
	handshakeTimeout = 15 * time.Second
	writeTimeout     = 5 * time.Second
	inputTimeout     = time.Second
	sendQueueLen     = 64
	// pollInterval asks for the power state, which also shows the connection is alive.
	pollInterval = 15 * time.Second
	// idleDead ends a session that has sent nothing for this long, despite the polls.
	idleDead = 3 * pollInterval
	// disconnectWait bounds the wait for STOP_SESSION to be sent.
	disconnectWait = 2 * time.Second
	absoluteMouse  = 2
)

// ErrEncryptionRequired means the BMC requires keyboard and mouse encryption, which is not
// implemented: input would be dropped.
var ErrEncryptionRequired = errors.New("the BMC requires keyboard and mouse encryption, which is not supported; " +
	"turn off keyboard/mouse encryption in the BMC's remote console settings")

// stopReasons explain STOP_SESSION statuses.
var stopReasons = map[uint16]string{
	2:  "the BMC's redirection engine closed the session",
	5:  "the BMC's redirection configuration changed",
	7:  "the web session was logged out",
	8:  "the KVM licence has expired",
	9:  "the KVM session timed out",
	10: "the BMC terminated the session",
}

// powerCodes maps power actions to POWER_CONTROL_REQUEST codes (IPMI chassis control).
var powerCodes = map[bmc.PowerAction]uint16{
	bmc.PowerOff: 0, bmc.PowerOn: 1, bmc.PowerCycle: 2, bmc.PowerReset: 3, bmc.PowerSoftOff: 5,
}

// KeyboardLEDs is the host's keyboard LED state; Known is false until the BMC reports it.
type KeyboardLEDs struct {
	Known, NumLock, CapsLock, ScrollLock bool
}

// ledsFromByte decodes a KEYBOARD_LEDS (20) payload byte: bit 0 Num, 1 Caps, 2 Scroll Lock.
func ledsFromByte(b byte) KeyboardLEDs {
	return KeyboardLEDs{Known: true, NumLock: b&1 != 0, CapsLock: b&2 != 0, ScrollLock: b&4 != 0}
}

func (l KeyboardLEDs) String() string {
	if !l.Known {
		return "unknown"
	}
	onOff := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	return fmt.Sprintf("Num %s, Caps %s, Scroll %s", onOff(l.NumLock), onOff(l.CapsLock), onOff(l.ScrollLock))
}

// Sink receives a session's output.
type Sink interface {
	UpdateScreen(width, height int, pix []byte)
	SetStatus(text string)
	SetFPS(fps int)
}

// credentials are what the session presents to the BMC.
type credentials struct {
	kvmToken, webCookie, user, clientIP string
}

// Session is one tunnelled IVTP session.
type Session struct {
	conn net.Conn
	r    *bufio.Reader
	cred credentials

	out       chan []byte
	done      chan struct{} // closed when Run returns
	validated atomic.Bool
	power     atomic.Value // string
	width     atomic.Int32
	height    atomic.Int32
	leds      atomic.Value // KeyboardLEDs

	inputMu  sync.Mutex // keeps reports in order with the state they describe
	keys     keyboardReport
	hid      hidState
	video    video
	frames   assembler
	decoded  int
	fpsStart time.Time
	status   string
}

func newSession(conn net.Conn, r *bufio.Reader, cred credentials) *Session {
	s := &Session{conn: conn, r: r, cred: cred, out: make(chan []byte, sendQueueLen), done: make(chan struct{})}
	s.power.Store(bmc.PowerStateUnknown)
	s.leds.Store(KeyboardLEDs{})
	return s
}

// Run validates the session and serves it until ctx ends, the BMC stops it, or the connection
// fails. It owns the connection.
func (s *Session) Run(ctx context.Context, sink Sink) error {
	defer close(s.done)
	defer func() { _ = s.conn.Close() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The writer outlives ctx so that STOP_SESSION can still go out after a cancel.
	wctx, stopWriter := context.WithCancel(context.Background())
	defer stopWriter()
	writeErr := make(chan error, 1)
	flushed := make(chan struct{})
	go func() { writeErr <- s.writeLoop(wctx, flushed) }()

	msgs := make(chan message)
	readErr := make(chan error, 1)
	go func() {
		for {
			m, err := readMessage(s.r)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	handshake := time.NewTimer(handshakeTimeout)
	defer handshake.Stop()
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	last := time.Now()
	s.fpsStart = time.Now()

	for {
		select {
		case <-ctx.Done():
			s.disconnect(stopWriter, flushed)
			return ctx.Err()
		case err := <-writeErr:
			return fmt.Errorf("MegaRAC KVM write: %w", err)
		case err := <-readErr:
			return fmt.Errorf("MegaRAC KVM connection: %w", err)
		case <-handshake.C:
			if !s.validated.Load() {
				return errors.New("MegaRAC KVM session was not validated in time")
			}
		case <-poll.C:
			if time.Since(last) > idleDead {
				return fmt.Errorf("MegaRAC KVM sent nothing for %v", idleDead.Round(time.Second))
			}
			if s.validated.Load() {
				s.enqueue(header(typePowerStatus, 0, 0))
			}
			s.reportFPS(sink)
		case m := <-msgs:
			last = time.Now()
			if err := s.handle(m, sink); err != nil {
				return err // the BMC ended or refused the session
			}
		}
	}
}

// handle acts on one message from the BMC.
func (s *Session) handle(m message, sink Sink) error {
	switch m.Type {
	case typeSessionAccepted:
		s.enqueue(append(append(header(typeWebToken, uint32(len(s.cred.webCookie)), 0), s.cred.webCookie...),
			validationMessage(s.cred.kvmToken, s.cred.clientIP, s.cred.user)...))
	case typeValidateResponse:
		if len(m.Payload) == 0 || m.Payload[0] != validationOK {
			result := -1
			if len(m.Payload) > 0 {
				result = int(m.Payload[0])
			}
			if result == 2 {
				return errors.New("MegaRAC KVM session rejected: the user has no KVM privilege")
			}
			return fmt.Errorf("MegaRAC KVM session rejected (result %d)", result)
		}
		s.validated.Store(true)
		log.Printf("[MegaRAC] KVM session validated")
		// What JViewer asks for once validated; the BMC expects it.
		s.enqueue(header(typePowerStatus, 0, 0))
		s.enqueue(append(header(typeDisplayLock, 1, 0), 2))
		s.enqueue(header(typeGetUserMacro, 0, 0))
	case typeStopSession:
		reason, ok := stopReasons[m.Status]
		if !ok {
			reason = fmt.Sprintf("the BMC stopped the session (status %d)", m.Status)
		}
		return fmt.Errorf("MegaRAC KVM: %s", reason)
	case typeInitialEncryption:
		return ErrEncryptionRequired
	case typeMouseMode:
		if len(m.Payload) > 0 && m.Payload[0] != absoluteMouse {
			log.Printf("[MegaRAC] BMC mouse mode is %d, not absolute: the pointer may not follow", m.Payload[0])
		}
	case typePowerStatus:
		state := bmc.PowerStateOff
		if m.Status == 1 {
			state = bmc.PowerStateOn
		}
		s.power.Store(state)
	case typePowerResponse:
		s.enqueue(header(typePowerStatus, 0, 0))
	case typeKeyboardLEDs:
		if len(m.Payload) == 0 {
			return nil
		}
		leds := ledsFromByte(m.Payload[0])
		if prev := s.leds.Swap(leds); prev != leds {
			log.Printf("[MegaRAC] Host keyboard LEDs: %s", leds)
		}
	case typeBlankScreen:
		s.setStatus(sink, statusNoSignal)
	case typeVideoFragment:
		frame, err := s.frames.add(m.Payload)
		if err != nil {
			log.Printf("[MegaRAC] Dropping video: %v", err)
			return nil
		}
		if frame != nil {
			s.showFrame(frame, sink)
		}
	}
	return nil
}

func (s *Session) showFrame(frame []byte, sink Sink) {
	w, h, pix, err := s.video.decode(frame)
	if err != nil {
		if errors.Is(err, errRC4Video) {
			s.setStatus(sink, statusVideo+err.Error())
		}
		log.Printf("[MegaRAC] Dropping undecodable frame: %v", err)
		return
	}
	if int(s.width.Load()) != w || int(s.height.Load()) != h {
		log.Printf("[MegaRAC] Host video mode is %dx%d", w, h)
	}
	s.width.Store(int32(w))
	s.height.Store(int32(h))
	s.decoded++
	s.setStatus(sink, "")
	sink.UpdateScreen(w, h, pix)
}

func (s *Session) reportFPS(sink Sink) {
	if el := time.Since(s.fpsStart); el > 0 {
		sink.SetFPS(int(float64(s.decoded)/el.Seconds() + 0.5))
	}
	s.decoded, s.fpsStart = 0, time.Now()
}

func (s *Session) setStatus(sink Sink, text string) {
	if text != s.status {
		s.status = text
		sink.SetStatus(text)
	}
}

// disconnect sends STOP_SESSION, as JViewer does when it exits, waits briefly for it to go out,
// then stops the writer. The BMC closes the socket in answer.
func (s *Session) disconnect(stopWriter context.CancelFunc, flushed <-chan struct{}) {
	select {
	case s.out <- stopSession:
		select {
		case <-flushed:
		case <-time.After(disconnectWait):
		}
	default:
	}
	stopWriter()
}

// stopSession is the STOP_SESSION message the client leaves with.
var stopSession = header(typeStopSession, 0, 0)

// writeLoop sends queued messages until ctx ends; after STOP_SESSION it signals flushed.
func (s *Session) writeLoop(ctx context.Context, flushed chan<- struct{}) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case b := <-s.out:
			_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := s.conn.Write(b); err != nil {
				return err
			}
			if string(b) == string(stopSession) {
				close(flushed)
				<-ctx.Done()
				return nil
			}
		}
	}
}

// enqueue queues a message from the session loop; a full queue means the writer is stuck, and
// its error ends the session.
func (s *Session) enqueue(b []byte) {
	select {
	case s.out <- b:
	default:
		log.Printf("[MegaRAC] Send queue full; dropping a type %d message", uint16(b[0])|uint16(b[1])<<8)
	}
}

// send queues user input, waiting briefly rather than dropping it: a lost key-up would leave a
// key held on the host.
func (s *Session) send(b []byte) error {
	select {
	case <-s.done:
		return errors.New("MegaRAC KVM session closed")
	default:
	}
	if !s.validated.Load() {
		return errors.New("MegaRAC KVM session not ready")
	}
	select {
	case s.out <- b:
		return nil
	case <-s.done:
		return errors.New("MegaRAC KVM session closed")
	case <-time.After(inputTimeout):
		return errors.New("MegaRAC KVM input queue is full")
	}
}

// SendKeyEvent sends the keyboard state after a key press or release (usage is a HID usage).
func (s *Session) SendKeyEvent(down bool, usage uint16) error {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	s.keys.set(down, usage)
	return s.send(s.hid.keyboard(s.keys.bytes()))
}

// SendPointerEvent sends an absolute pointer position in screen pixels (buttons: left 1,
// middle 2, right 4).
func (s *Session) SendPointerEvent(buttons byte, x, y uint16) error {
	w, h := int(s.width.Load()), int(s.height.Load())
	if w == 0 || h == 0 {
		return nil // no picture yet: the position means nothing
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	return s.send(s.hid.mouse(hidButtons(buttons), scaleAbsolute(int(x), w), scaleAbsolute(int(y), h)))
}

// RequestFrameRefresh asks for a complete frame.
func (s *Session) RequestFrameRefresh() error {
	return s.send(header(typeFullScreen, 0, 0))
}

// PowerControl sends a power command over the KVM session.
func (s *Session) PowerControl(action bmc.PowerAction) error {
	code, ok := powerCodes[action]
	if !ok {
		return fmt.Errorf("unknown power action: %s", action)
	}
	log.Printf("[MegaRAC] Power %q sent over the KVM session", action)
	return s.send(header(typePowerControl, 0, code))
}

// SetMediaState tells the BMC that media redirection started (on) or stopped, as JViewer does
// when its first drive connects and its last one closes.
func (s *Session) SetMediaState(on bool) error {
	var status uint16
	if on {
		status = 1
	}
	return s.send(header(typeMediaState, 0, status))
}

// KeyboardLEDs returns the host's keyboard LED state as last reported by the BMC.
func (s *Session) KeyboardLEDs() KeyboardLEDs { return s.leds.Load().(KeyboardLEDs) }

// PowerState is the host power state the BMC last reported on this session.
func (s *Session) PowerState() string {
	return s.power.Load().(string)
}
