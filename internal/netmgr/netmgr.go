// Package netmgr drives the dongle's network: the setup Wi-Fi (a hotspot the phone joins), scanning, joining the
// restaurant's Wi-Fi and reading what it's connected to. NM talks to NetworkManager through nmcli (Raspberry Pi OS
// Bookworm uses it by default); Fake pretends, for running the agent on a laptop.
package netmgr

import (
	"context"
	"errors"
)

// Network is one Wi-Fi network the dongle can hear.
type Network struct {
	SSID   string `json:"ssid"`
	Signal int    `json:"signal"` // 0-100
	Secure bool   `json:"secure"`
}

// Bars turns the signal into 1-4 bars.
func (n Network) Bars() int { return Bars(n.Signal) }

// Bars turns a 0-100 signal into 1-4 bars.
func Bars(signal int) int {
	switch {
	case signal >= 70:
		return 4
	case signal >= 50:
		return 3
	case signal >= 30:
		return 2
	default:
		return 1
	}
}

// SignalWord is how the TV and Pavilion describe a signal.
func SignalWord(signal int) string {
	return [...]string{"", "Weak", "Fair", "Good", "Strong"}[Bars(signal)]
}

// Status is what the dongle is connected to right now.
type Status struct {
	WifiConnected bool   `json:"wifi_connected"`
	SSID          string `json:"ssid,omitempty"`
	Signal        int    `json:"signal,omitempty"`
	Wired         bool   `json:"wired"` // an Ethernet cable with a connection on it
	IP            string `json:"ip,omitempty"`
	Hotspot       bool   `json:"hotspot"`
}

// Connected: it has a network other than its own setup Wi-Fi, with an address.
func (s Status) Connected() bool { return (s.WifiConnected || s.Wired) && s.IP != "" }

// Why joining failed, so the TV and the phone can say something useful.
var (
	ErrAuth     = errors.New("the Wi-Fi password was not accepted")
	ErrNotFound = errors.New("that network isn't in range")
	ErrNoIP     = errors.New("the router didn't hand out an address")
)

// Manager is what the agent needs from the network.
type Manager interface {
	// Scan lists the networks in range, strongest first. rescan asks the radio to look again (slower; while the
	// hotspot is on, most Pi radios can't, and the last scan comes back).
	Scan(ctx context.Context, rescan bool) ([]Network, error)
	// StartHotspot turns on the setup Wi-Fi at HotspotIP. The Wi-Fi radio can't be on two networks, so this drops
	// the restaurant's Wi-Fi (not Ethernet).
	StartHotspot(ctx context.Context, ssid, password string) error
	StopHotspot(ctx context.Context) error
	// Join connects to a network and saves it so it comes back after a reboot. Returns ErrAuth, ErrNotFound or
	// another error. An empty password means an open network.
	Join(ctx context.Context, ssid, password string, hidden bool) error
	// Reconnect tries the saved network again (after it went away).
	Reconnect(ctx context.Context) error
	// WaitForIP waits for an address on the network it joined.
	WaitForIP(ctx context.Context) (string, error)
	Status(ctx context.Context) Status
	// Forget removes the saved Wi-Fi (when the dongle is unpaired or reset).
	Forget(ctx context.Context) error
}

// HotspotIP is the dongle's address on its own setup Wi-Fi (NetworkManager's default for shared connections).
const HotspotIP = "10.42.0.1"
