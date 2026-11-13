package device

import "testing"

func TestFullAvg10(t *testing.T) {
	text := "some avg10=41.20 avg60=30.02 avg300=12.50 total=123456\nfull avg10=27.85 avg60=19.01 avg300=8.00 total=98765\n"
	if n, ok := fullAvg10(text); !ok || n != 27.85 {
		t.Errorf("fullAvg10 = %v, %v; want 27.85", n, ok)
	}
	if _, ok := fullAvg10("some avg10=1.00 avg60=0.00 avg300=0.00 total=1\n"); ok {
		t.Error("no full line: not ok")
	}
}

func TestParseHealth(t *testing.T) {
	meminfo := "MemTotal:         426132 kB\nMemFree:           11840 kB\nMemAvailable:      61952 kB\nSwapTotal:        255676 kB\nSwapFree:          51900 kB\n"
	pressure := "some avg10=41.20 avg60=30.11 avg300=12.00 total=1\nfull avg10=23.50 avg60=18.02 avg300=7.10 total=1\n"
	h := parseHealth(meminfo, pressure, "3.71 2.10 1.04 5/212 4242\n", "61712\n")
	want := Health{MemoryMB: 416, MemoryAvailableMB: 60, SwapMB: 249, SwapUsedMB: 199, MemoryPressure: 23.5, Load: 3.71, TemperatureC: 61.7}
	if h != want {
		t.Errorf("got %+v, want %+v", h, want)
	}
	if h := parseHealth("", "", "", ""); h != (Health{}) {
		t.Errorf("nothing readable (a laptop) is nothing reported, got %+v", h)
	}
}
