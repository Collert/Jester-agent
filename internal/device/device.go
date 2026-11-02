// Package device reads what the dongle is (its short ID, the Pi model, the TV's resolution) and does the few things
// to the system Pavilion can ask for: turn the picture, restart.
package device

import (
	"context"
	"crypto/sha256"
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

// Rotator turns the picture with wlr-randr (Cage supports it). Env must reach Cage's Wayland socket.
type Rotator struct {
	Env []string
}

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
	return cmd.Run()
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
