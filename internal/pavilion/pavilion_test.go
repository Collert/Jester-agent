package pavilion

import "testing"

func TestNormalizeBase(t *testing.T) {
	good := map[string]string{
		"pavilion.juniperlane.ca":         "https://pavilion.juniperlane.ca",
		" HTTPS://Pavilion.Example.com/ ": "https://pavilion.example.com",
		"pavilion.example.com/admin/":     "https://pavilion.example.com",
		"http://192.168.1.5:8000":         "http://192.168.1.5:8000",
		"pavilion.example.com:8443":       "https://pavilion.example.com:8443",
		"192.168.30.151:8000":             "http://192.168.30.151:8000",
		"10.0.0.5":                        "http://10.0.0.5",
		"localhost:8000":                  "http://localhost:8000",
		"https://192.168.1.5":             "https://192.168.1.5",
	}
	for in, want := range good {
		got, err := NormalizeBase(in)
		if err != nil || got != want {
			t.Errorf("NormalizeBase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "pavilion", "not an address", "https://", "a..b"} {
		if got, err := NormalizeBase(in); err == nil {
			t.Errorf("NormalizeBase(%q) = %q, want an error", in, got)
		}
	}
}

func TestSocketURLAndHost(t *testing.T) {
	if got := SocketURL("https://p.example.com"); got != "wss://p.example.com/ws/dongle" {
		t.Error(got)
	}
	if got := SocketURL("http://10.0.0.5:8000"); got != "ws://10.0.0.5:8000/ws/dongle" {
		t.Error(got)
	}
	if Host("https://p.example.com") != "p.example.com" || Host("http://10.0.0.5:8000") != "http://10.0.0.5:8000" {
		t.Error("Host")
	}
}
