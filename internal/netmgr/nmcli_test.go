package netmgr

import "testing"

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

func TestBars(t *testing.T) {
	for signal, want := range map[int]int{95: 4, 70: 4, 55: 3, 35: 2, 5: 1} {
		if got := Bars(signal); got != want {
			t.Errorf("Bars(%d) = %d, want %d", signal, got, want)
		}
	}
}
