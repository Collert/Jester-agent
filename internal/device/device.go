// Package device reads what the dongle is (its short ID, the Pi model, the TV's resolution) and does the few things
// to the system Pavilion can ask for: turn the picture, restart.
package device

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// idLetters: nothing that reads two ways on a TV (0/O, 1/I).
const idLetters = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// HardwareID is a short, stable ID for this dongle ("7F3K"), from the Pi's serial number (or the machine ID). It
// names the setup Wi-Fi and the dongle until someone renames it in Pavilion.
func HardwareID() string {
	seed := ""
	if raw, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Serial" {
				seed = strings.TrimSpace(v)
			}
		}
	}
	if seed == "" {
		if raw, err := os.ReadFile("/etc/machine-id"); err == nil {
			seed = strings.TrimSpace(string(raw))
		}
	}
	if seed == "" {
		seed, _ = os.Hostname()
	}
	return ShortID(seed)
}

// ShortID turns any string into four letters from idLetters.
func ShortID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	out := make([]byte, 4)
	for i := range out {
		out[i] = idLetters[int(sum[i])%len(idLetters)]
	}
	return string(out)
}

// Model is the board, e.g. "Raspberry Pi 4 Model B Rev 1.5".
func Model() string {
	if raw, err := os.ReadFile("/proc/device-tree/model"); err == nil {
		return strings.TrimRight(strings.TrimSpace(string(raw)), "\x00")
	}
	return "Linux computer"
}

// connector is the first display output with something plugged in: ("HDMI-A-1", "1920x1080").
func connector() (name, mode string) {
	dirs, _ := filepath.Glob("/sys/class/drm/card*-*")
	for _, dir := range dirs {
		status, err := os.ReadFile(filepath.Join(dir, "status"))
		if err != nil || strings.TrimSpace(string(status)) != "connected" {
			continue
		}
		modes, _ := os.ReadFile(filepath.Join(dir, "modes"))
		first := strings.TrimSpace(strings.SplitN(string(modes), "\n", 2)[0])
		base := filepath.Base(dir)
		if i := strings.Index(base, "-"); i >= 0 {
			base = base[i+1:]
		}
		return base, first
	}
	return "", ""
}

// Resolution is the TV's preferred mode, e.g. "1920x1080", or "" with no TV connected.
func Resolution() string {
	_, mode := connector()
	return mode
}

// MemoryStarved: for the last 10 seconds the Pi spent a good part of its time with everything waiting for memory
// (swapping), so a program that doesn't answer is starved, not hung. From the kernel's pressure figures
// (/proc/pressure/memory, "psi=1" on the kernel command line); false where there are none.
func MemoryStarved() bool {
	raw, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return false
	}
	full, ok := fullAvg10(string(raw))
	return ok && full >= 10
}

// fullAvg10 reads the "full avg10=" figure (percent of time) from a pressure file.
func fullAvg10(text string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "full" {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "avg10="); ok {
				n, err := strconv.ParseFloat(v, 64)
				return n, err == nil
			}
		}
	}
	return 0, false
}

// Health is how the Pi is coping, for telling why a dongle is slow: out of memory (little available, a lot swapped,
// pressure high), hot (at 80 °C the Pi slows itself down), or just busy. A figure that can't be read is left out.
type Health struct {
	MemoryMB          int     `json:"memory_mb,omitempty"`           // what Linux has in all
	MemoryAvailableMB int     `json:"memory_available_mb,omitempty"` // what programs could still get without swapping
	SwapUsedMB        int     `json:"swap_used_mb,omitempty"`        // pushed out to swap (compressed memory on a dongle)
	SwapMB            int     `json:"swap_mb,omitempty"`
	MemoryPressure    float64 `json:"memory_pressure,omitempty"` // percent of the last 10 s everything waited for memory
	Load              float64 `json:"load,omitempty"`            // programs wanting a processor, last minute (a Pi Zero 2 W has 4)
	TemperatureC      float64 `json:"temperature_c,omitempty"`
	// What the Pi's firmware has had to do about its power supply or heat, e.g. "under-voltage now". Empty: nothing.
	Trouble string `json:"trouble,omitempty"`
}

// ReadHealth reads Health from /proc and /sys.
func ReadHealth() Health {
	read := func(path string) string {
		raw, _ := os.ReadFile(path)
		return string(raw)
	}
	h := parseHealth(read("/proc/meminfo"), read("/proc/pressure/memory"), read("/proc/loadavg"),
		read("/sys/class/thermal/thermal_zone0/temp"))
	h.Trouble = parseThrottled(read("/sys/devices/platform/soc/soc:firmware/get_throttled"))
	return h
}

// parseThrottled puts the Pi firmware's "throttled" flags (what vcgencmd get_throttled prints, in hexadecimal) into
// words: bits 0 to 3 are what's happening now, bits 16 to 19 the same things having happened since it started.
// A power supply that's too weak (a TV's USB socket often is) shows here, and it's what makes a Pi stop dead.
func parseThrottled(raw string) string {
	flags, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(raw), "0x"), 16, 32)
	if err != nil || flags == 0 {
		return ""
	}
	var out []string
	for bit, what := range []string{"under-voltage", "processor speed capped", "slowed down", "at its soft temperature limit"} {
		switch {
		case flags&(1<<bit) != 0:
			out = append(out, what+" now")
		case flags&(1<<(bit+16)) != 0:
			out = append(out, what+" earlier")
		}
	}
	return strings.Join(out, ", ")
}

func parseHealth(meminfo, pressure, loadavg, temp string) Health {
	var h Health
	kb := map[string]int{}
	for _, line := range strings.Split(meminfo, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			if f := strings.Fields(v); len(f) > 0 {
				kb[k], _ = strconv.Atoi(f[0])
			}
		}
	}
	h.MemoryMB, h.MemoryAvailableMB = kb["MemTotal"]/1024, kb["MemAvailable"]/1024
	h.SwapMB, h.SwapUsedMB = kb["SwapTotal"]/1024, (kb["SwapTotal"]-kb["SwapFree"])/1024
	h.MemoryPressure, _ = fullAvg10(pressure)
	if f := strings.Fields(loadavg); len(f) > 0 {
		h.Load, _ = strconv.ParseFloat(f[0], 64)
	}
	if milli, err := strconv.Atoi(strings.TrimSpace(temp)); err == nil {
		h.TemperatureC = float64(milli/100) / 10
	}
	return h
}

// BrowserMemoryMB is what the kiosk browser's processes (cog and WebKit's) hold, in memory and swapped out together.
// The swapped part is the one to watch on a dongle: a page that keeps growing is pushed out to swap, where neither
// "free" nor WebKit's own count shows it as the browser's. 0 when there's no such process.
func BrowserMemoryMB() int { return browserMemoryMB("/proc") }

func browserMemoryMB(proc string) int {
	dirs, _ := filepath.Glob(filepath.Join(proc, "[0-9]*"))
	kb := 0
	for _, dir := range dirs {
		name, err := os.ReadFile(filepath.Join(dir, "comm"))
		if err != nil {
			continue
		}
		// The kernel keeps 15 letters of a name: WPENetworkProcess is "WPENetworkProce".
		if n := strings.TrimSpace(string(name)); n != "cog" && !strings.HasPrefix(n, "WPE") {
			continue
		}
		status, _ := os.ReadFile(filepath.Join(dir, "status"))
		for _, line := range strings.Split(string(status), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && (k == "VmRSS" || k == "VmSwap") {
				if f := strings.Fields(v); len(f) > 0 {
					n, _ := strconv.Atoi(f[0])
					kb += n
				}
			}
		}
	}
	return kb / 1024
}

// Rotator turns the picture with wlr-randr. Env must reach Cage's Wayland socket. Cage forgets the turn whenever it
// starts, so it's asked again for every new browser.
type Rotator struct {
	Env []string
}

// ErrOldCage: the kiosk's compositor doesn't let another program set up the TV's output, so the picture can't be
// turned at all. Cage does from 0.1.5; Debian Bookworm's own package is 0.1.4 (Raspberry Pi's is newer).
var ErrOldCage = errors.New("this Cage can't turn the picture: that takes Cage 0.1.5 or newer (Raspberry Pi's package, not Debian Bookworm's 0.1.4)")

// Rotate sets landscape, portrait (turned right) or portrait_flipped (turned left).
func (r Rotator) Rotate(ctx context.Context, orientation string) error {
	output, _ := connector()
	if output == "" {
		return nil
	}
	transform := map[string]string{"portrait": "90", "portrait_flipped": "270"}[orientation]
	if transform == "" {
		transform = "normal"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wlr-randr", "--output", output, "--transform", transform)
	cmd.Env = append(os.Environ(), r.Env...)
	said, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	err = rotateError(err, string(said))
	if errors.Is(err, ErrOldCage) {
		if version, verr := exec.CommandContext(ctx, "cage", "-v").Output(); verr == nil {
			err = fmt.Errorf("%w; installed: %s", err, strings.TrimSpace(string(version)))
		}
	}
	return err
}

// rotateError is why wlr-randr failed, in its own words (said: what it printed): before Cage is up that's "failed
// to connect to display"; against a Cage without wlr-output-management, ErrOldCage.
func rotateError(err error, said string) error {
	said = strings.TrimSpace(said)
	switch {
	case strings.Contains(said, "wlr-output-management"):
		return ErrOldCage
	case said != "":
		return fmt.Errorf("wlr-randr: %s (%w)", said, err)
	}
	return fmt.Errorf("wlr-randr: %w", err)
}

// Run runs a command line (the reboot command), split on spaces.
func Run(ctx context.Context, line string) error {
	f := strings.Fields(line)
	if len(f) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, f[0], f[1:]...).Run()
}
