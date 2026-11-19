package session

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/supermicro"
	"github.com/a5m0/ipmi-connect/internal/trust"
)

const (
	supermicroKVMPort   = 5900
	smPowerPollInterval = 15 * time.Second
)

// supermicroInsecure describes the X8-era ATEN channels, which this firmware never encrypts.
var supermicroInsecure = []string{
	"KVM: plain RFB, so the screen and every keystroke cross the network unencrypted",
	"Virtual media: RC4 under a key published in the vendor's client, so disk data is effectively unencrypted",
}

func supermicroVendor() *Vendor {
	return &Vendor{
		Name:       string(bmc.TargetSupermicro),
		DetectPort: supermicroKVMPort,
		Probe: func(context.Context, *bmc.Endpoint) ([]trust.ProbeResult, []string, error) {
			return nil, supermicroInsecure, nil
		},
		Connect: connectSupermicro,
	}
}

// connectSupermicro logs in, then keeps an ATEN iKVM session up in the background.
func connectSupermicro(ctx context.Context, ep *bmc.Endpoint, user, password string, d Display) (*Live, error) {
	client := supermicro.NewClient(ep, user, password)
	if err := client.Login(); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		client.Logout()
		return nil, ctx.Err()
	}
	runCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); pollSupermicroPower(runCtx, d, client) }()
	client.Start(runCtx, d)
	var once sync.Once
	return &Live{
		Console: client,
		Model:   client.Flavor(),
		Stop: func() {
			once.Do(func() {
				cancel()
				client.Logout() // waits for the KVM socket to close first
				wg.Wait()
			})
		},
		Channels: func() []bmc.ChannelSecurity {
			web, ok := ep.Policy.Connections()["https"]
			return []bmc.ChannelSecurity{
				tlsChannel("Web interface (login, power, tickets)", web, ok),
				{Channel: "KVM", Encryption: bmc.EncNone, Auth: bmc.AuthNone, Note: "plain RFB: screen and keystrokes are unencrypted"},
				{Channel: "Virtual media", Encryption: bmc.EncWeak, Auth: bmc.AuthNone, Note: "RC4 under a publicly known key: effectively unencrypted"},
			}
		},
	}, nil
}

// pollSupermicroPower refreshes the reported power state.
func pollSupermicroPower(ctx context.Context, d Display, client *supermicro.Client) {
	update := func() {
		state, err := client.PowerStatus()
		if err != nil {
			log.Printf("[Supermicro] Power status: %v", err)
			state = bmc.PowerStateUnknown // a stale value could hide the real state
		}
		d.SetPower(state)
	}
	update()
	ticker := time.NewTicker(smPowerPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			update()
		}
	}
}
