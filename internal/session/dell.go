package session

import (
	"context"
	"log"
	"net"
	"sync"
	"time"

	"github.com/mhamzahkhan/ipmi-connect/internal/aspeed"
	"github.com/mhamzahkhan/ipmi-connect/internal/bmc"
	"github.com/mhamzahkhan/ipmi-connect/internal/dell"
	"github.com/mhamzahkhan/ipmi-connect/internal/trust"
)

const (
	dellKVMPort             = 7578
	dellSessionPollInterval = 15 * time.Second
	// dellIdleFrameWait is how long to wait for the first packet of a new frame.
	dellIdleFrameWait = 3 * time.Second
	// dellChunkGap ends a frame: no continuation chunk within this gap means it is complete.
	dellChunkGap = 40 * time.Millisecond
	// dellStaticRefreshAfter forces a full frame before the BMC's 60s idle-video disconnect.
	dellStaticRefreshAfter = 15 * time.Second
	dellReconnectDelay     = 2 * time.Second
	maxReconnectDelay      = time.Minute
)

func builtinVendors() map[bmc.TargetType]*Vendor {
	return map[bmc.TargetType]*Vendor{
		bmc.TargetDell:       dellVendor(),
		bmc.TargetSupermicro: supermicroVendor(),
	}
}

// nextBackoff doubles the reconnect delay, capped at maxReconnectDelay.
func nextBackoff(d time.Duration) time.Duration {
	return min(2*d, maxReconnectDelay)
}

func dellVendor() *Vendor {
	return &Vendor{
		Name:       string(bmc.TargetDell),
		DetectPort: dellKVMPort,
		Probe: func(ctx context.Context, ep *bmc.Endpoint) ([]trust.ProbeResult, []string, error) {
			res, tlsOffered, err := dell.ProbeKVM(ctx, ep, dellKVMPort)
			if err != nil {
				return nil, nil, err
			}
			if !tlsOffered {
				return nil, []string{"KVM, video and virtual media (the BMC offers no TLS on its KVM port)"}, nil
			}
			return []trust.ProbeResult{res}, nil, nil
		},
		Connect: connectDell,
	}
}

// connectDell logs in, opens both APCP channels, and starts the keepalive and video loops.
func connectDell(ctx context.Context, ep *bmc.Endpoint, user, password string, d Display) (*Live, error) {
	client := dell.NewClient(ep, user, password)
	if err := client.Login(); err != nil {
		return nil, err
	}
	// The BMC allows a single GUI session: any failure from here must log out, or the next
	// login is refused until the BMC times the session out.
	fail := func(err error) (*Live, error) {
		client.Logout()
		return nil, err
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"KVM session tokens", client.RefreshKVMTokens},
		{"control channel", client.ConnectControl},
		{"video channel", client.ConnectVideo},
	}
	for _, s := range steps {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		log.Printf("[Dell] Connecting the %s...", s.name)
		if err := s.run(); err != nil {
			return fail(err)
		}
	}
	if err := client.EnableVideo(); err != nil {
		log.Printf("[Dell] Notice: EnableVideo: %v", err)
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}

	runCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pollDellSession(runCtx, d, client) }()
	go func() { defer wg.Done(); streamDellVideo(runCtx, d, client) }()
	var once sync.Once
	return &Live{
		Console:  client,
		Model:    "Dell PowerEdge C6220 II (AST2300)",
		Firmware: "Avocent 2.62.5",
		Stop: func() {
			once.Do(func() {
				cancel()
				client.Logout() // closes the channels, which ends the video loop's read
				wg.Wait()
			})
		},
		Channels: func() []bmc.ChannelSecurity {
			conns := ep.Policy.Connections()
			web, ok := conns["https"]
			out := []bmc.ChannelSecurity{tlsChannel("Web interface (login, power)", web, ok)}
			if client.PlainChannels() {
				return append(out, bmc.ChannelSecurity{
					Channel: "KVM, video and virtual media", Encryption: bmc.EncNone, Auth: bmc.AuthNone,
					Note: "the BMC offered no TLS; screen, keystrokes and disk data are in clear text",
				})
			}
			kvm, ok := conns["kvm"]
			return append(out, tlsChannel("KVM, video and virtual media", kvm, ok))
		},
	}, nil
}

// pollDellSession keeps the WebGUI session alive and refreshes the reported power state.
func pollDellSession(ctx context.Context, d Display, client *dell.Client) {
	setDellPower(d, client)
	ticker := time.NewTicker(dellSessionPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := client.KeepAlive(); err != nil {
				log.Printf("[Dell] KeepAlive notice: %v", err)
			}
			setDellPower(d, client)
		}
	}
}

// setDellPower records the host power state; a failed read is UNKNOWN rather than a stale value.
func setDellPower(d Display, client *dell.Client) {
	pwr, err := client.GetPowerState()
	if err != nil {
		pwr = bmc.PowerStateUnknown
	}
	d.SetPower(pwr)
}

// streamDellVideo reassembles ASPEED frames from video packets and forwards them to the browser.
func streamDellVideo(ctx context.Context, d Display, client *dell.Client) {
	log.Printf("[Dell] Starting live Aspeed video streaming loop...")

	var frames frameAssembler
	var width, height uint16
	lastPktTime := time.Now()
	seenSelectors := map[[2]byte]bool{}
	decoder := aspeed.NewDecoder()
	decoded, fpsStart := 0, time.Now()

	// send decodes a whole frame into the picture and hands it to the browsers. Every frame covers
	// the screen from the origin. The decoder keeps state across frames (and reconnects): each
	// frame only updates blocks of the previous picture.
	send := func(f *dell.VideoPacket) {
		if f == nil {
			return
		}
		h := aspeed.Header{
			Width: int(f.Width), Height: int(f.Height), Mode420: f.Mode420,
			LumaSelector: f.LumaSelector, ChromaSelector: f.ChromaSelector,
		}
		if err := decoder.Decode(h, f.Bitstream); err != nil {
			log.Printf("[Dell] Dropping undecodable frame: %v", err)
			return
		}
		decoded++
		d.UpdateScreen(h.Width, h.Height, decoder.Frame())
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if elapsed := time.Since(fpsStart); elapsed >= time.Second {
			d.SetFPS(int(float64(decoded)/elapsed.Seconds() + 0.5))
			decoded, fpsStart = 0, time.Now()
		}

		readTimeout := dellIdleFrameWait
		if frames.inProgress() {
			readTimeout = dellChunkGap
		}

		pkt, err := client.ReadVideoPacketWithTimeout(readTimeout)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				// A pause while accumulating chunks means the frame is complete.
				send(frames.take())

				if time.Since(lastPktTime) > dellStaticRefreshAfter {
					_ = client.RequestFrameRefresh()
					lastPktTime = time.Now()
				}
				continue
			}
			if ctx.Err() != nil {
				return // shutting down: the read failed because Logout closed the channels
			}
			log.Printf("[Dell] Video stream interrupted (%v). Attempting automatic reconnection...", err)
			d.SetStatus("Reconnecting to the KVM…")
			frames.take() // a partial frame would decode as garbage
			if !reconnectDell(ctx, client) {
				return
			}
			d.SetStatus("")
			// Blocks decoded before the drop may be stale or damaged: start from a full frame.
			if err := client.RequestFrameRefresh(); err != nil {
				log.Printf("[Dell] Refresh after reconnect: %v", err)
			}
			lastPktTime = time.Now()
			continue
		}

		if pkt == nil {
			continue
		}
		lastPktTime = time.Now()

		if pkt.Flags&1 != 0 {
			if pkt.Width != width || pkt.Height != height {
				width, height = pkt.Width, pkt.Height
				log.Printf("[Dell] Host video mode is %dx%d", width, height)
			}
			if sel := [2]byte{pkt.LumaSelector, pkt.ChromaSelector}; !seenSelectors[sel] {
				seenSelectors[sel] = true
				log.Printf("[Dell] Video quantisation tables: luma %d, chroma %d", sel[0], sel[1])
			}
		}
		send(frames.add(pkt))
	}
}

// reconnectDell retries with exponential backoff until the channels are back or ctx is
// cancelled (returns false).
func reconnectDell(ctx context.Context, client *dell.Client) bool {
	delay := dellReconnectDelay
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
		if err := client.Reconnect(); err != nil {
			delay = nextBackoff(delay)
			log.Printf("[Dell] Reconnection attempt failed: %v; retrying in %v", err, delay)
			continue
		}
		log.Printf("[Dell] Reconnected successfully to video channel. Resuming video stream.")
		return true
	}
}
