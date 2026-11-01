package netmgr

import (
	"context"
	"sync"
	"time"
)

// Fake pretends to be the network, for running the agent on a laptop (-fake-network). A few networks are "in
// range"; joining one works with any password of 8+ characters except "wrongpass", and "Far-Away" is never found.
// The laptop's own network does the real talking to Pavilion.
type Fake struct {
	Delay time.Duration // how long joining and getting an address take

	mu      sync.Mutex
	joined  string
	hotspot bool
	lost    bool
	preset  *Preconfigured
}

// NewFake returns a fake that answers after short delays.
func NewFake() *Fake { return &Fake{Delay: 1500 * time.Millisecond} }

var fakeNets = []Network{
	{SSID: "Juniper-Staff", Signal: 88, Secure: true},
	{SSID: "Juniper-Guest", Signal: 81, Secure: false},
	{SSID: "TELUS2.4-88F1", Signal: 41, Secure: true},
	{SSID: "Bean There Café", Signal: 22, Secure: true},
	{SSID: "Far-Away", Signal: 8, Secure: true},
}

func (f *Fake) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(f.Delay):
		return nil
	}
}

func (f *Fake) Scan(ctx context.Context, rescan bool) ([]Network, error) {
	if rescan {
		if err := f.wait(ctx); err != nil {
			return nil, err
		}
	}
	return append([]Network(nil), fakeNets...), nil
}

func (f *Fake) StartHotspot(ctx context.Context, ssid, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hotspot = true
	return nil
}

func (f *Fake) StopHotspot(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hotspot = false
	return nil
}

func (f *Fake) Join(ctx context.Context, ssid, password string, hidden bool) error {
	_ = f.StopHotspot(ctx)
	if err := f.wait(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ssid == "Far-Away" {
		return ErrNotFound
	}
	secure := true
	for _, n := range fakeNets {
		if n.SSID == ssid {
			secure = n.Secure
		}
	}
	if secure && (len(password) < 8 || password == "wrongpass") {
		return ErrAuth
	}
	f.joined, f.lost = ssid, false
	return nil
}

func (f *Fake) Reconnect(ctx context.Context) error {
	_ = f.StopHotspot(ctx)
	if err := f.wait(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.joined == "" || f.lost {
		return ErrNotFound
	}
	return nil
}

func (f *Fake) WaitForIP(ctx context.Context) (string, error) {
	if err := f.wait(ctx); err != nil {
		return "", err
	}
	return "192.168.1.42", nil
}

func (f *Fake) Status(ctx context.Context) Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := Status{Hotspot: f.hotspot}
	if f.joined != "" && !f.lost && !f.hotspot {
		st.WifiConnected, st.SSID, st.Signal, st.IP = true, f.joined, 88, "192.168.1.42"
	}
	return st
}

func (f *Fake) Forget(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joined = ""
	return nil
}

func (f *Fake) TakePreconfigured(ctx context.Context) (*Preconfigured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.preset
	f.preset = nil
	return p, nil
}

// Preconfigure pretends the network was saved with Raspberry Pi Imager before the first boot.
func (f *Fake) Preconfigure(p Preconfigured) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preset = &p
}

// Remember makes the fake already joined to a network (a dongle that was set up before a restart).
func (f *Fake) Remember(ssid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joined = ssid
}

// LoseWifi makes the network go away (or come back), for trying the "Wi-Fi lost" screens.
func (f *Fake) LoseWifi(lost bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lost = lost
}
