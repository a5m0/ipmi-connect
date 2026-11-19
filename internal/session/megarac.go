package session

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/a5m0/ipmi-connect/internal/bmc"
	"github.com/a5m0/ipmi-connect/internal/megarac"
	"github.com/a5m0/ipmi-connect/internal/trust"
)

const megaracPowerPollInterval = 15 * time.Second

// megaracVendor is AMI MegaRAC SP (the ASUS ASMB8-iKVM). Its KVM is tunnelled through the web
// server's TLS port, so it has no port of its own to detect and no other channel to probe:
// detection falls to the web banner.
func megaracVendor() *Vendor {
	return &Vendor{
		Name: string(bmc.TargetMegaRAC),
		Probe: func(context.Context, *bmc.Endpoint) ([]trust.ProbeResult, []string, error) {
			return nil, nil, nil
		},
		Connect: connectMegaRAC,
	}
}

// connectMegaRAC logs in, then keeps a tunnelled KVM session up in the background.
func connectMegaRAC(ctx context.Context, ep *bmc.Endpoint, user, password string, d Display) (*Live, error) {
	client := megarac.NewClient(ep, user, password)
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
	go func() { defer wg.Done(); pollPower(runCtx, d, client, megaracPowerPollInterval, "[MegaRAC]") }()
	client.Start(runCtx, d)
	var once sync.Once
	return &Live{
		Console: client,
		Model:   "AMI MegaRAC SP",
		Stop: func() {
			once.Do(func() {
				cancel()
				client.Logout() // waits for the KVM session to close first
				wg.Wait()
			})
		},
		Channels: func() []bmc.ChannelSecurity {
			web, ok := ep.Policy.Connections()["https"]
			return []bmc.ChannelSecurity{
				tlsChannel("Web interface (login, power, tickets)", web, ok),
				tlsChannel("KVM (tunnelled through the web port)", web, ok),
				tlsChannel("Virtual media (tunnelled through the web port)", web, ok),
			}
		},
	}, nil
}

// pollPower refreshes the reported power state; a failed read is UNKNOWN rather than a stale value.
func pollPower(ctx context.Context, d Display, r bmc.PowerReader, every time.Duration, tag string) {
	update := func() {
		state, err := r.PowerState()
		if err != nil {
			log.Printf("%s Power status: %v", tag, err)
			state = bmc.PowerStateUnknown
		}
		d.SetPower(state)
	}
	update()
	ticker := time.NewTicker(every)
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
