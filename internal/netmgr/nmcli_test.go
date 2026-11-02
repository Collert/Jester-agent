package netmgr

import (
	"context"
	"errors"
	"testing"
)

func TestSplitTerse(t *testing.T) {
	got := splitTerse(`My\:Net:72:WPA2`)
	if len(got) != 3 || got[0] != "My:Net" || got[1] != "72" || got[2] != "WPA2" {
		t.Fatalf("splitTerse = %q", got)
	}
}

func TestParseScanKeepsStrongestAndSorts(t *testing.T) {
	out := "Cafe:40:WPA2\nCafe:81:WPA2\n:90:WPA2\nGuest:55:\nOld:20:--\n"
	nets := parseScan(out, "")
	if len(nets) != 3 {
		t.Fatalf("want 3 networks (hidden one dropped), got %+v", nets)
	}
	if nets[0].SSID != "Cafe" || nets[0].Signal != 81 || !nets[0].Secure {
		t.Errorf("strongest first, with its best signal: %+v", nets[0])
	}
	if nets[1].SSID != "Guest" || nets[1].Secure || nets[2].Secure {
		t.Errorf("open networks: %+v", nets)
	}
}

func TestWifiProfilesSkipsOurs(t *testing.T) {
	out := "preconfigured:802-11-wireless\njester-setup:802-11-wireless\njester-wifi:802-11-wireless\nWired connection 1:802-3-ethernet\nlo:loopback\n"
	if got := wifiProfiles(out); len(got) != 1 || got[0] != "preconfigured" {
		t.Fatalf("wifiProfiles = %q", got)
	}
}

func TestHexPSK(t *testing.T) {
	key := "8d4f3c9a0b1e2d3f4a5b6c7d8e9f0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b"
	if !HexPSK(key) {
		t.Error("Imager's 64-hex-digit key")
	}
	for _, pw := range []string{"juniper2026", key[:63], key[:63] + "g"} {
		if HexPSK(pw) {
			t.Errorf("%q is a password", pw)
		}
	}
}

func TestBars(t *testing.T) {
	for signal, want := range map[int]int{95: 4, 70: 4, 55: 3, 35: 2, 5: 1} {
		if got := Bars(signal); got != want {
			t.Errorf("Bars(%d) = %d, want %d", signal, got, want)
		}
	}
}

// A busy Pi's nmcli can time out: that's not the network going away.
func TestStatusKeepsLastWhenNmcliFails(t *testing.T) {
	fail := false
	m := NewNM("wlan0", "")
	m.run = func(ctx context.Context, args ...string) (string, error) {
		if fail {
			return "", errors.New("nmcli device: signal: killed")
		}
		switch args[len(args)-1] {
		case "status":
			return "wlan0:wifi:connected:jester-wifi\n", nil
		case "wlan0":
			return "192.168.1.20/24\n", nil
		}
		return "*:Cafe:70\n", nil
	}
	if st := m.Status(context.Background()); !st.WifiConnected || st.IP != "192.168.1.20" || st.SSID != "Cafe" {
		t.Fatalf("status %+v", st)
	}
	fail = true
	if st := m.Status(context.Background()); !st.WifiConnected || st.IP != "192.168.1.20" {
		t.Errorf("after nmcli timed out: %+v, want the last status", st)
	}
}
