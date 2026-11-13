package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/collert/jester-agent/internal/agent"
	"github.com/collert/jester-agent/internal/netmgr"
	"github.com/collert/jester-agent/internal/pavilion"
	"github.com/collert/jester-agent/internal/store"
)

// Icon paths (24×24, stroked), as in the canvas's Dongle artboard.
const (
	iconWifi    = "M2 9a15 15 0 0 1 20 0 M5 12.5a10 10 0 0 1 14 0 M8.5 16a5 5 0 0 1 7 0 M12 19.5h.01"
	iconChip    = "M7 7h10v10H7z M10 3v4 M14 3v4 M10 17v4 M14 17v4 M3 10h4 M3 14h4 M17 10h4 M17 14h4"
	iconPhone   = "M7 3h10v18H7z M11 18h2"
	iconCheck   = "m5 12.5 4.5 4.5L19 7.5"
	iconWarn    = "M12 8v5 M12 16.5h.01 M10.3 3.9 2.4 18a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"
	iconWifiOff = "M2 9a15 15 0 0 1 20 0 M5 12.5a10 10 0 0 1 14 0 M8.5 16a5 5 0 0 1 7 0 M12 19.5h.01 M3 3l18 18"
	iconCable   = "M9 3v4 M15 3v4 M7 7h10v5a5 5 0 0 1-10 0z M12 17v4"
)

type step struct {
	N                     int
	Title, Sub            string
	Done, Now, Fail, Todo bool
	Last                  bool
}

type foot struct{ Icon, Text string }

// tvView is everything tv.html shows.
type tvView struct {
	Phase    string
	Kind     string // pair | rescue | setup | idle | center | boot
	BrandTop string
	BrandSub string
	Version  string
	Clock    string

	// pair / rescue / retry card
	Kicker, Title, Text string
	SSID, Password      string
	PortalURL           string
	QR                  qrSVG

	// setup
	HeroIcon, HeroTone string // tone: plain | fail | done
	HeroSpin           bool
	Steps              []step
	ShowRetryQR        bool
	ShowCountdown      bool
	CountdownMS        int64

	// idle / center
	StatusOK   bool
	StatusText string
	Spinner    bool

	Identify     bool
	IdentifyName string
	IdentifySub  string

	Foot []foot
	Go   string // where the page should take itself when the browser can't be steered
}

func clockText(t time.Time) string { return t.Format("3:04") }

func mask(code string) string {
	if len(code) == 6 {
		return code[:2] + "••" + code[4:]
	}
	return code
}

func (s *Server) tvView() tvView {
	st := s.Agent.State()
	cfg := s.Agent.Store.Config()
	a := s.Agent
	v := tvView{Phase: string(st.Phase), Version: "v" + strings.TrimPrefix(a.Version, "v"), Clock: clockText(time.Now()),
		SSID: a.HotspotSSID(), Password: a.HotspotPassword(), PortalURL: "http://" + netmgr.HotspotIP,
		BrandTop: "Pavilion", BrandSub: "Visual dongle · " + a.Hardware}
	v.QR = makeQR(wifiQR(v.SSID, v.Password))
	host := pavilion.Host(cfg.PavilionURL)
	storeName := cfg.StoreName
	if storeName == "" {
		storeName = host
	}
	name := cfg.Name
	if name == "" {
		name = "Dongle " + a.Hardware
	}
	netName := cfg.WifiSSID
	if cfg.Wired {
		netName = "the cable"
	}
	if st.Attempt != nil && st.Attempt.SSID != "" {
		netName = st.Attempt.SSID
	}
	paired := cfg.Paired()

	switch st.Phase {
	case agent.PhaseBoot:
		v.Kind, v.Title, v.Spinner = "center", "Starting up", true
		v.Foot = []foot{{iconChip, "Dongle " + a.Hardware}}

	case agent.PhasePair:
		v.Kind = "pair"
		v.Foot = []foot{{iconWifi, "Setup Wi-Fi on"}}

	case agent.PhaseRescue:
		v.Kind = "rescue"
		v.BrandTop, v.BrandSub = name, storeName
		v.Kicker = name + " · stays paired"
		if st.Rescue == "change" {
			v.Title = "Pick a new Wi-Fi"
			v.Text = "Scan the code with your phone and choose the network this screen should use."
		} else {
			v.Title = "Wi-Fi lost"
			v.Text = fmt.Sprintf("This screen couldn’t reach %s for a while. Scan the code to give it a new network, or wait: it tries %s again every few minutes.", netName, netName)
		}
		v.Foot = []foot{{iconWifi, "Setup Wi-Fi on"}, {iconChip, "Dongle " + a.Hardware}}

	case agent.PhasePortal, agent.PhaseJoining, agent.PhaseAddress, agent.PhasePairing, agent.PhaseDone,
		agent.PhaseWifiFail, agent.PhaseCodeFail, agent.PhaseReachFail:
		v.Kind = "setup"
		v.Foot = []foot{{iconChip, "Dongle " + a.Hardware}}
		s.setupSteps(&v, st, cfg, netName, host, storeName, paired)

	case agent.PhaseConnecting:
		v.Kind, v.Spinner = "center", true
		v.BrandTop, v.BrandSub = name, storeName
		if cfg.Wired {
			v.Title, v.Text = "Connecting", "Waiting for the network cable."
		} else {
			v.Title, v.Text = "Connecting to "+netName, "If it can’t join in two minutes, the setup Wi-Fi comes on so you can pick another network."
		}
		v.Foot = []foot{{iconWifi, netName}, {iconChip, v.Version}}

	case agent.PhaseIdle, agent.PhaseShowing:
		v.BrandTop, v.BrandSub = name, storeName
		v.StatusOK = st.PavilionOnline
		if st.PavilionOnline {
			v.StatusText = "Paired with " + storeName + " · picks up changes right away"
		} else {
			v.StatusText = "Can’t reach Pavilion right now · trying again"
		}
		if st.Phase == agent.PhaseIdle || cfg.Screen == nil {
			v.Kind = "idle"
		} else {
			v.Kind, v.Spinner = "center", true
			v.Title = "Opening " + cfg.Screen.Name
			if time.Since(st.ScreenFailed) < agent.ScreenRetry {
				v.Spinner = false
				v.Title = "Can’t open " + cfg.Screen.Name
				v.Text = "Pavilion didn’t answer. Trying again in a moment."
			}
		}
		ip := st.Net.IP
		wifi := st.Net.SSID
		if wifi == "" && st.Net.Wired {
			wifi = "Ethernet"
		}
		v.Foot = []foot{{iconWifi, wifi}, {iconChip, ip}, {iconChip, v.Version}}
	}

	if st.IdentifyUntil.After(time.Now()) {
		v.Identify, v.IdentifyName, v.IdentifySub = true, name, storeName+" · Dongle "+a.Hardware
	}
	if s.NoCDP {
		v.Go = a.PageGo() // "" unless the browser should be on the screen right now
	}
	// Drop empty footer items (no IP yet, …).
	keep := v.Foot[:0]
	for _, f := range v.Foot {
		if f.Text != "" {
			keep = append(keep, f)
		}
	}
	v.Foot = keep
	return v
}

func (s *Server) setupSteps(v *tvView, st agent.State, cfg store.Config, netName, host, storeName string, paired bool) {
	at := map[agent.Phase]int{agent.PhasePortal: 0, agent.PhaseJoining: 1, agent.PhaseAddress: 2, agent.PhasePairing: 3,
		agent.PhaseDone: 4, agent.PhaseWifiFail: 1, agent.PhaseCodeFail: 3, agent.PhaseReachFail: 3}[st.Phase]
	fail := map[agent.Phase]int{agent.PhaseWifiFail: 1, agent.PhaseCodeFail: 3, agent.PhaseReachFail: 3}[st.Phase]
	if fail == 0 {
		fail = -1
	}
	details := "Waiting for your phone"
	if at > 0 {
		details = host + " · code " + mask(cfg.PairCode)
		if paired {
			details = "Paired with " + storeName
		} else if cfg.PairCode == "" {
			details = host
		}
	}
	wifiSub := "The network you pick on your phone"
	switch {
	case fail == 1:
		wifiSub = "Couldn’t join " + netName
	case at > 1 && st.Net.SSID != "":
		wifiSub = st.Net.SSID + " · " + strings.ToLower(netmgr.SignalWord(st.Net.Signal)) + " signal"
	case at > 1:
		wifiSub = netName
	case at == 1:
		wifiSub = "Joining " + netName
	}
	ipSub := "From your router"
	switch {
	case at > 2:
		ipSub = st.Net.IP
	case at == 2:
		ipSub = "Asking the router"
	}
	pavSub := "Checks the pairing code"
	switch {
	case at > 3:
		pavSub = "Paired with " + storeName
	case st.Phase == agent.PhaseCodeFail:
		pavSub = "Code not accepted"
	case st.Phase == agent.PhaseReachFail:
		pavSub = "Couldn’t reach " + host
	case at == 3:
		pavSub = "Checking the pairing code"
	}
	rows := [][2]string{{"Your details", details}, {"Connect to Wi-Fi", wifiSub}, {"Get an IP address", ipSub}, {"Connect to Pavilion", pavSub}}
	if cfg.Wired {
		rows[1][0] = "Connect to the network"
	}
	for i, r := range rows {
		done := i < at && i != fail
		now := i == at && fail < 0
		bad := i == fail
		v.Steps = append(v.Steps, step{N: i + 1, Title: r[0], Sub: r[1], Done: done, Now: now, Fail: bad,
			Todo: !done && !now && !bad, Last: i == len(rows)-1})
	}

	v.HeroTone, v.HeroIcon = "plain", iconWarn
	switch st.Phase {
	case agent.PhasePortal:
		v.Title, v.HeroIcon = "Finish on your phone", iconPhone
		v.Text = "A phone joined the setup Wi-Fi. Enter your Pavilion address, the pairing code and the Wi-Fi this screen should use."
	case agent.PhaseJoining:
		v.Title, v.HeroSpin = "Joining "+netName, true
		v.Text = "Your phone will drop off the setup Wi-Fi now. That’s expected. Everything else happens here."
	case agent.PhaseAddress:
		v.Title, v.HeroSpin = "Getting an address", true
		v.Text = "Connected to " + netName + ". Waiting for the router to hand out an IP address."
	case agent.PhasePairing:
		v.Title, v.HeroSpin = "Connecting to Pavilion", true
		v.Text = "Checking the pairing code with " + host + "."
	case agent.PhaseDone:
		v.HeroTone, v.HeroIcon = "done", iconCheck
		v.Title = "Paired with " + storeName
		v.Text = "This dongle is now Dongle " + s.Agent.Hardware + ". Give it a name and a screen in Settings › Devices."
		v.ShowCountdown, v.CountdownMS = true, agent.DoneFor.Milliseconds()
	case agent.PhaseWifiFail:
		v.HeroTone, v.ShowRetryQR = "fail", true
		v.Title = "Couldn’t join " + netName
		if cfg.Wired {
			v.HeroIcon = iconCable
			v.Title = "No network on the cable"
		}
		v.Text = st.Detail + " Fix it from your phone."
		if st.Detail == "" {
			v.Text = "The password may be wrong, or the network is too far away. Fix it from your phone."
		}
	case agent.PhaseCodeFail:
		v.HeroTone, v.ShowRetryQR = "fail", true
		v.Title = "Pavilion didn’t take the code"
		v.Text = "The code may have expired (they last 15 minutes) or been used already. Make a new one in Settings › Devices, then enter it on your phone."
		if st.Detail != "" {
			v.Text = st.Detail + " Make a new code in Settings › Devices, then enter it on your phone."
		}
	case agent.PhaseReachFail:
		v.HeroTone, v.ShowRetryQR, v.HeroIcon = "fail", true, iconWifiOff
		v.Title = "Couldn’t reach Pavilion"
		v.Text = "Nothing answered at " + host + ". Check the address on your phone: it’s the one you open Pavilion at."
	}
}
