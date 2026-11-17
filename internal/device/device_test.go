package device

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestParseThrottled(t *testing.T) {
	for raw, want := range map[string]string{
		"0\n":     "",
		"":        "", // not a Pi
		"50005\n": "under-voltage now, slowed down now",
		"50000\n": "under-voltage earlier, slowed down earlier",
		"0x80008": "at its soft temperature limit now",
	} {
		if got := parseThrottled(raw); got != want {
			t.Errorf("parseThrottled(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestRotateError(t *testing.T) {
	failed := &exec.ExitError{}
	// What wlr-randr 0.3 prints against a compositor without wlr-output-management (Cage before 0.1.5).
	if err := rotateError(failed, "compositor doesn't support wlr-output-management-unstable-v1\n"); !errors.Is(err, ErrOldCage) {
		t.Errorf("a Cage that's too old: got %v", err)
	}
	// Before Cage is up (the agent starts first).
	err := rotateError(failed, "failed to connect to display\n")
	if errors.Is(err, ErrOldCage) || !strings.Contains(err.Error(), "failed to connect to display") {
		t.Errorf("no Cage yet: got %v", err)
	}
	if !errors.Is(err, failed) {
		t.Errorf("the command's own error is kept: %v", err)
	}
	if err := rotateError(exec.ErrNotFound, ""); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("no wlr-randr: got %v", err)
	}
}

func TestBrowserMemoryCountsWhatIsSwappedOut(t *testing.T) {
	proc := t.TempDir()
	process := func(pid, name, status string) {
		dir := filepath.Join(proc, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, "comm"), []byte(name+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644)
	}
	// As on a dongle whose page had grown for two hours: almost all of it swapped out.
	process("708", "WPEWebProcess", "Name:\tWPEWebProcess\nVmRSS:\t   25368 kB\nVmSwap:\t  389120 kB\n")
	process("582", "WPENetworkProce", "VmRSS:\t    3880 kB\nVmSwap:\t   10240 kB\n")
	process("560", "cog", "VmRSS:\t    3072 kB\nVmSwap:\t   20480 kB\n")
	process("505", "jester-agent", "VmRSS:\t    5504 kB\nVmSwap:\t    9000 kB\n")
	process("59", "kswapd0", "Name:\tkswapd0\n") // a kernel thread: no memory of its own
	if got := browserMemoryMB(proc); got != 441 {
		t.Errorf("browser memory = %d MB, want 441 (in memory and swapped, the browser's processes only)", got)
	}
	if got := browserMemoryMB(t.TempDir()); got != 0 {
		t.Errorf("no browser: %d", got)
	}
}
