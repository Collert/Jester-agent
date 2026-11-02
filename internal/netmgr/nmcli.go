package netmgr

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Connection names the agent owns in NetworkManager.
const (
	hotspotConn = "jester-setup"
	wifiConn    = "jester-wifi"
)

// NM manages the network with nmcli. It needs root (the agent runs as root under systemd) or a polkit rule.
type NM struct {
	Wifi     string // the Wi-Fi interface, usually wlan0
	Ethernet string // usually eth0; "" to ignore the cable

	mu         sync.Mutex
	lastScan   []Network
	lastScanAt time.Time
}

// scanFresh: how long a list of networks is shown again without asking NetworkManager (each nmcli is a fraction of a
// second of a Pi Zero's time, and the setup page asks on every load).
const scanFresh = 30 * time.Second

// NewNM returns a manager for these interfaces.
func NewNM(wifi, ethernet string) *NM { return &NM{Wifi: wifi, Ethernet: ethernet} }

func (m *NM) nmcli(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nmcli", args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("nmcli %s: %w: %s", args[0], err, msg)
	}
	return out.String(), nil
}

// splitTerse splits one line of `nmcli -t` output, where ':' separates fields and '\:' is a colon in a value.
func splitTerse(line string) []string {
	var fields []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case line[i] == ':':
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(fields, cur.String())
}

// parseScan reads `nmcli -t -f SSID,SIGNAL,SECURITY dev wifi list`: one entry per name, strongest first.
func parseScan(out, skip string) []Network {
	best := map[string]Network{}
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(strings.TrimRight(line, "\r"))
		if len(f) < 3 || f[0] == "" || f[0] == skip {
			continue
		}
		signal, _ := strconv.Atoi(f[1])
		n := Network{SSID: f[0], Signal: signal, Secure: f[2] != "" && f[2] != "--"}
		if old, ok := best[n.SSID]; !ok || n.Signal > old.Signal {
			best[n.SSID] = n
		}
	}
	nets := make([]Network, 0, len(best))
	for _, n := range best {
		nets = append(nets, n)
	}
	sort.Slice(nets, func(i, j int) bool {
		if nets[i].Signal != nets[j].Signal {
			return nets[i].Signal > nets[j].Signal
		}
		return nets[i].SSID < nets[j].SSID
	})
	return nets
}

func (m *NM) Scan(ctx context.Context, rescan bool) ([]Network, error) {
	m.mu.Lock()
	if !rescan && m.lastScan != nil && time.Since(m.lastScanAt) < scanFresh {
		defer m.mu.Unlock()
		return m.lastScan, nil
	}
	m.mu.Unlock()
	flag := "no"
	if rescan {
		flag = "yes"
	}
	out, err := m.nmcli(ctx, 20*time.Second, "-t", "-e", "yes", "-f", "SSID,SIGNAL,SECURITY", "dev", "wifi", "list", "ifname", m.Wifi, "--rescan", flag)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		if m.lastScan != nil {
			return m.lastScan, nil
		}
		return nil, err
	}
	nets := parseScan(out, "")
	// While the hotspot is on, a rescan usually finds only ourselves (or nothing): keep the last good list.
	if len(nets) > 0 || m.lastScan == nil {
		m.lastScan, m.lastScanAt = nets, time.Now()
	}
	return m.lastScan, nil
}

func (m *NM) StartHotspot(ctx context.Context, ssid, password string) error {
	// Look around once before the radio becomes an access point: the phone's list comes from this scan.
	_, _ = m.Scan(ctx, true)
	_, _ = m.nmcli(ctx, 10*time.Second, "connection", "delete", hotspotConn)
	if _, err := m.nmcli(ctx, 10*time.Second, "connection", "add", "type", "wifi", "ifname", m.Wifi,
		"con-name", hotspotConn, "autoconnect", "no", "ssid", ssid,
		"802-11-wireless.mode", "ap", "802-11-wireless.band", "bg",
		"ipv4.method", "shared", "ipv4.addresses", HotspotIP+"/24", "ipv6.method", "disabled",
		"wifi-sec.key-mgmt", "wpa-psk", "wifi-sec.proto", "rsn", "wifi-sec.pairwise", "ccmp", "wifi-sec.group", "ccmp",
		"wifi-sec.psk", password); err != nil {
		return err
	}
	_, err := m.nmcli(ctx, 30*time.Second, "--wait", "25", "connection", "up", hotspotConn)
	return err
}

func (m *NM) StopHotspot(ctx context.Context) error {
	_, err := m.nmcli(ctx, 15*time.Second, "connection", "down", hotspotConn)
	if err != nil && strings.Contains(err.Error(), "not an active connection") {
		return nil
	}
	return err
}

func classifyJoin(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "secrets were required"), strings.Contains(msg, "802-1x supplicant"),
		strings.Contains(msg, "invalid passphrase"), strings.Contains(msg, "psk: property is invalid"):
		return fmt.Errorf("%w (%v)", ErrAuth, err)
	case strings.Contains(msg, "no network with ssid"), strings.Contains(msg, "not found"):
		return fmt.Errorf("%w (%v)", ErrNotFound, err)
	}
	return err
}

func (m *NM) Join(ctx context.Context, ssid, password string, hidden bool) error {
	_ = m.StopHotspot(ctx)
	_, _ = m.nmcli(ctx, 10*time.Second, "connection", "delete", wifiConn)
	args := []string{"connection", "add", "type", "wifi", "ifname", m.Wifi, "con-name", wifiConn, "ssid", ssid,
		"autoconnect", "yes", "connection.autoconnect-priority", "10", "connection.autoconnect-retries", "0"}
	if hidden {
		args = append(args, "802-11-wireless.hidden", "yes")
	}
	if password != "" {
		args = append(args, "wifi-sec.key-mgmt", "wpa-psk", "wifi-sec.psk", password)
	}
	if _, err := m.nmcli(ctx, 10*time.Second, args...); err != nil {
		return classifyJoin(err)
	}
	if _, err := m.nmcli(ctx, 50*time.Second, "--wait", "45", "connection", "up", wifiConn); err != nil {
		// Don't leave NetworkManager retrying a network that doesn't work while the hotspot comes back.
		_, _ = m.nmcli(ctx, 10*time.Second, "connection", "delete", wifiConn)
		return classifyJoin(err)
	}
	return nil
}

func (m *NM) Reconnect(ctx context.Context) error {
	_ = m.StopHotspot(ctx)
	_, err := m.nmcli(ctx, 50*time.Second, "--wait", "45", "connection", "up", wifiConn)
	return classifyJoin(err)
}

func (m *NM) WaitForIP(ctx context.Context) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ip := m.ip(ctx, m.Wifi); ip != "" {
			return ip, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", ErrNoIP
}

func (m *NM) ip(ctx context.Context, dev string) string {
	out, err := m.nmcli(ctx, 5*time.Second, "-g", "IP4.ADDRESS", "device", "show", dev)
	if err != nil {
		return ""
	}
	first := strings.TrimSpace(strings.Split(strings.TrimSpace(out), "|")[0])
	ip, _, _ := strings.Cut(first, "/")
	return ip
}

func (m *NM) Status(ctx context.Context) Status {
	var st Status
	out, err := m.nmcli(ctx, 5*time.Second, "-t", "-e", "yes", "-f", "DEVICE,TYPE,STATE,CONNECTION", "device", "status")
	if err != nil {
		return st
	}
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(line)
		if len(f) < 4 || !strings.HasPrefix(f[2], "connected") { // "connected", or "connected (externally)"
			continue
		}
		switch {
		case f[0] == m.Wifi && f[3] == hotspotConn:
			st.Hotspot = true
		case f[0] == m.Wifi:
			st.WifiConnected = true
		case m.Ethernet != "" && f[0] == m.Ethernet:
			st.Wired = true
		}
	}
	if st.WifiConnected {
		st.IP = m.ip(ctx, m.Wifi)
		if out, err := m.nmcli(ctx, 5*time.Second, "-t", "-e", "yes", "-f", "IN-USE,SSID,SIGNAL", "dev", "wifi", "list", "ifname", m.Wifi, "--rescan", "no"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				if f := splitTerse(line); len(f) >= 3 && f[0] == "*" {
					st.SSID = f[1]
					st.Signal, _ = strconv.Atoi(f[2])
				}
			}
		}
	}
	if st.IP == "" && st.Wired {
		st.IP = m.ip(ctx, m.Ethernet)
	}
	return st
}

// wifiProfiles reads `nmcli -t -f NAME,TYPE connection show`: the names of the Wi-Fi connections the agent didn't
// make (Raspberry Pi Imager's is called "preconfigured").
func wifiProfiles(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(strings.TrimRight(line, "\r"))
		if len(f) >= 2 && f[1] == "802-11-wireless" && f[0] != hotspotConn && f[0] != wifiConn {
			names = append(names, f[0])
		}
	}
	return names
}

func (m *NM) TakePreconfigured(ctx context.Context) (*Preconfigured, error) {
	out, err := m.nmcli(ctx, 10*time.Second, "-t", "-e", "yes", "-f", "NAME,TYPE", "connection", "show")
	if err != nil {
		return nil, err
	}
	var found *Preconfigured
	for _, name := range wifiProfiles(out) {
		out, err := m.nmcli(ctx, 5*time.Second, "-e", "yes", "-g", "802-11-wireless.ssid,802-11-wireless.mode,802-11-wireless.hidden", "connection", "show", "id", name)
		if f := splitTerse(strings.TrimSpace(out)); err == nil && found == nil && len(f) >= 3 && f[0] != "" && f[1] != "ap" {
			p := &Preconfigured{SSID: f[0], Hidden: f[2] == "yes"}
			// -s shows the key (root only); an open network has none.
			if psk, err := m.nmcli(ctx, 5*time.Second, "-s", "-g", "802-11-wireless-security.psk", "connection", "show", "id", name); err == nil {
				p.PSK = strings.TrimSpace(psk)
			}
			found = p
		}
		if _, err := m.nmcli(ctx, 10*time.Second, "connection", "delete", "id", name); err != nil {
			return found, err
		}
	}
	return found, nil
}

func (m *NM) Forget(ctx context.Context) error {
	_, err := m.nmcli(ctx, 10*time.Second, "connection", "delete", wifiConn)
	if err != nil && strings.Contains(err.Error(), "unknown connection") {
		return nil
	}
	return err
}
