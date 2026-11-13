// Package agent is the dongle's brain: one state machine that takes it from fresh out of the box (setup Wi-Fi on,
// "Scan to start" on the TV) through joining the restaurant's Wi-Fi and pairing with Pavilion, to showing whatever
// screen Pavilion picks, and back to setup whenever something goes wrong that someone has to fix (a wrong password,
// a code Pavilion didn't take, the Wi-Fi gone for two minutes, being unpaired).
//
// The TV always shows something that says what's going on: the agent's own pages (internal/ui) for every phase but
// "showing", when the browser is on the Pavilion screen. Browser and network problems are retried by themselves.
package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/collert/jester-agent/internal/browser"
	"github.com/collert/jester-agent/internal/device"
	"github.com/collert/jester-agent/internal/netmgr"
	"github.com/collert/jester-agent/internal/pavilion"
	"github.com/collert/jester-agent/internal/store"
)

// Phase is what the dongle is doing; the TV's page is picked from it.
type Phase string

const (
	PhaseBoot       Phase = "boot"       // starting up
	PhasePair       Phase = "pair"       // not set up: setup Wi-Fi on, "Scan to start"
	PhasePortal     Phase = "info"       // a phone has the setup page open: "Finish on your phone"
	PhaseJoining    Phase = "wifi"       // joining the restaurant's Wi-Fi
	PhaseAddress    Phase = "ip"         // waiting for an IP address
	PhasePairing    Phase = "pavilion"   // sending the code to Pavilion
	PhaseDone       Phase = "done"       // paired: a few seconds of "Paired with …"
	PhaseWifiFail   Phase = "wifi-fail"  // couldn't join: setup Wi-Fi back on
	PhaseCodeFail   Phase = "code-fail"  // Pavilion didn't take the code: setup Wi-Fi back on
	PhaseReachFail  Phase = "reach-fail" // couldn't reach Pavilion at that address: setup Wi-Fi back on
	PhaseConnecting Phase = "connecting" // paired, waiting for the network after a restart
	PhaseIdle       Phase = "idle"       // paired, no screen assigned
	PhaseShowing    Phase = "showing"    // paired, the browser is on the screen
	PhaseRescue     Phase = "rescue"     // paired, but needs a new Wi-Fi: setup Wi-Fi on
)

// Timings. Variables so tests can shorten them.
var (
	LostWifiAfter    = 2 * time.Minute  // no network this long while paired: turn the setup Wi-Fi back on
	RetryWifiEvery   = 5 * time.Minute  // in rescue, try the saved Wi-Fi again this often (when nobody's on the setup page)
	PortalIdleAfter  = 3 * time.Minute  // a phone that hasn't loaded the setup page in this long has gone away
	DoneFor          = 4 * time.Second  // how long "Paired with …" stays up
	StatusEvery      = 30 * time.Second // heartbeat to Pavilion
	PingEvery        = 15 * time.Second // how often the connection to Pavilion is checked for still being there
	PingTimeout      = 10 * time.Second // no answer to that check this long: the connection is gone, open a new one
	NetCheckEvery    = 10 * time.Second
	BrowserCheck     = 15 * time.Second
	BrowserHungAfter = 2 * time.Minute         // no answer from the browser this long (not counting time the Pi is short of memory): restart it
	OpenGrace        = time.Minute             // after sending the browser somewhere, how long the page may take to arrive
	OpenRetry        = 2 * time.Second         // the browser couldn't be sent somewhere new (not up, or too busy): try again this soon
	ScreenRetry      = 30 * time.Second        // after the screen failed to load, show our page this long before trying again
	AwayGrace        = 3 * time.Second         // a browser off the dongle's page this long isn't reloading it: it's somewhere else
	TurnSettle       = 5 * time.Second         // a browser still there this long after its picture was turned took the turn well
	PhoneGrace       = 1500 * time.Millisecond // lets the phone get "Watch the TV" before the setup Wi-Fi goes
)

// State is everything the pages need to know.
type State struct {
	Phase  Phase     `json:"phase"`
	Since  time.Time `json:"since"`
	Detail string    `json:"detail,omitempty"` // what went wrong, in words
	Rescue string    `json:"rescue,omitempty"` // in PhaseRescue: "lost" (the Wi-Fi went away) or "change" (asked from Pavilion)

	Net            netmgr.Status `json:"net"`
	PavilionOnline bool          `json:"pavilion_online"`
	PavilionError  string        `json:"pavilion_error,omitempty"`
	ScreenFailed   time.Time     `json:"screen_failed,omitempty"` // the screen showed the browser's error page
	IdentifyUntil  time.Time     `json:"identify_until,omitempty"`
	PortalSeen     time.Time     `json:"-"`
	Attempt        *SetupRequest `json:"-"` // the setup being tried (for the TV's steps)
}

// SetupRequest is what the phone sent. In rescue only the network is used.
type SetupRequest struct {
	PavilionURL string
	Code        string
	SSID        string
	Password    string
	Hidden      bool
	Wired       bool
}

// Options configure an Agent.
type Options struct {
	Store   *store.Store
	Net     netmgr.Manager
	Browser *browser.Browser
	Rotator interface {
		Rotate(context.Context, string) error
	}
	// KioskRotates: the kiosk turns the picture itself when it starts, asking the agent which way (Rotation), so a
	// new orientation takes a restart of the kiosk (cog straight on the display). Rotator isn't used then.
	KioskRotates bool
	ShellURL     string // the agent's own page in the kiosk browser, e.g. http://127.0.0.1:8080/
	Version      string
	Reboot       string // command line run for Pavilion's Restart ("" = restart the browser only)
	Hardware     string // short ID; "" reads it from the Pi
	// Starved: the system is short of memory right now, so a browser that doesn't answer is waiting, not hung
	// (device.MemoryStarved). nil: never.
	Starved func() bool
}

// Agent runs the dongle.
type Agent struct {
	Options
	start time.Time

	mu    sync.Mutex
	state State
	subs  map[chan struct{}]struct{}

	browserInfo *browser.Info // what the browser said about itself the last time it was asked
	// The dongle's page in a browser without a DevTools port (steerByPage): the screen it was last told to take
	// itself to, the one it had been told when the last page closed, and when a page last opened and last closed.
	servedGo, heading  string
	pageSeen, pageLeft time.Time
	rotation           int // the turn the kiosk was last told to start with (Rotation); -1: it hasn't asked

	setupCh   chan SetupRequest
	rescueCh  chan string
	browserCh chan struct{}
	conn      *pavilion.Conn
	connMu    sync.Mutex
	turnMu    sync.Mutex // one wlr-randr at a time (turnPicture)
}

// New makes an agent; Run starts it.
func New(o Options) *Agent {
	if o.Hardware == "" {
		o.Hardware = device.HardwareID()
	}
	if o.Browser == nil {
		o.Browser = &browser.Browser{}
	}
	a := &Agent{Options: o, start: time.Now(), subs: map[chan struct{}]struct{}{},
		setupCh: make(chan SetupRequest, 1), rescueCh: make(chan string, 1), browserCh: make(chan struct{}, 1)}
	a.state = State{Phase: PhaseBoot, Since: time.Now()}
	a.rotation = -1
	if o.Browser.Logf == nil {
		o.Browser.Logf = a.logf
	}
	return a
}

func (a *Agent) logf(kind, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[%s] %s", kind, msg)
	a.Store.Log(kind, msg)
}

// ---------- state ----------

// State returns a copy of the current state.
func (a *Agent) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

func (a *Agent) update(change func(*State)) {
	a.mu.Lock()
	before := a.state.Phase
	change(&a.state)
	if a.state.Phase != before {
		a.state.Since = time.Now()
	}
	subs := make([]chan struct{}, 0, len(a.subs))
	for ch := range a.subs {
		subs = append(subs, ch)
	}
	phase := a.state.Phase
	a.mu.Unlock()
	if phase != before {
		log.Printf("[agent] %s → %s", before, phase)
	}
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	select {
	case a.browserCh <- struct{}{}:
	default:
	}
}

func (a *Agent) setPhase(p Phase, detail string) {
	a.update(func(s *State) { s.Phase, s.Detail = p, detail })
}

// Viewers is how many TV pages are open right now (each holds a Subscribe for its live updates). With the kiosk
// browser that's 1 while it shows the dongle's page and 0 while it shows a screen, or isn't running. (Someone
// looking at the TV page from a laptop counts too.)
func (a *Agent) Viewers() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.subs)
}

// PageGo is GoURL for the TV page to act on (its data-go): the agent remembers what the page was told, which is
// where the browser has gone when the page closes.
func (a *Agent) PageGo() string {
	url := a.GoURL()
	a.mu.Lock()
	a.servedGo = url
	a.mu.Unlock()
	return url
}

// page is how many TV pages are open, where the last one to close had been told to go ("" for nowhere, or when
// none has been open yet), and when one last opened and the last one closed (zero: none has yet).
func (a *Agent) page() (viewers int, heading string, seen, left time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.subs), a.heading, a.pageSeen, a.pageLeft
}

// Subscribe returns a channel nudged on every change (the TV page's live updates) and a function to stop.
func (a *Agent) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	a.mu.Lock()
	a.subs[ch] = struct{}{}
	a.pageSeen = time.Now()
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		delete(a.subs, ch)
		if len(a.subs) == 0 {
			a.heading, a.pageLeft = a.servedGo, time.Now()
		}
		a.mu.Unlock()
	}
}

// HotspotSSID is the setup Wi-Fi's name.
func (a *Agent) HotspotSSID() string { return "Pavilion-Setup-" + a.Hardware }

// HotspotPassword is the setup Wi-Fi's password, made the first time it's needed.
func (a *Agent) HotspotPassword() string {
	cfg := a.Store.Config()
	if cfg.HotspotPassword != "" {
		return cfg.HotspotPassword
	}
	words := []string{"lane", "oven", "plum", "fern", "mint", "sage", "dill", "pear", "lime", "kale", "corn", "rye"}
	w, _ := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	n, _ := rand.Int(rand.Reader, big.NewInt(9000))
	pw := fmt.Sprintf("%s-%04d", words[w.Int64()], n.Int64()+1000)
	cfg, _ = a.Store.Update(func(c *store.Config) {
		if c.HotspotPassword == "" {
			c.HotspotPassword = pw
		}
	})
	return cfg.HotspotPassword
}

// HotspotOn: the setup Wi-Fi is up, so the setup page may answer.
func (a *Agent) HotspotOn() bool {
	switch a.State().Phase {
	case PhasePair, PhasePortal, PhaseWifiFail, PhaseCodeFail, PhaseReachFail, PhaseRescue:
		return true
	}
	return false
}

// PortalSeen: a phone loaded the setup page.
func (a *Agent) PortalSeen() {
	a.mu.Lock()
	quiet := a.state.Phase != PhasePair // nothing on the TV changes: don't nudge it (or the browser check) for every page
	if quiet {
		a.state.PortalSeen = time.Now()
	}
	a.mu.Unlock()
	if quiet {
		return
	}
	a.update(func(s *State) {
		s.PortalSeen = time.Now()
		if s.Phase == PhasePair {
			s.Phase = PhasePortal
		}
	})
}

// Submit hands the phone's details to the agent. It returns false when the dongle isn't taking setup right now.
func (a *Agent) Submit(req SetupRequest) bool {
	if !a.HotspotOn() {
		return false
	}
	select {
	case a.setupCh <- req:
		return true
	default:
		return false // one is being tried already
	}
}

// Scan lists the networks for the setup page.
func (a *Agent) Scan(ctx context.Context, rescan bool) []netmgr.Network {
	nets, err := a.Net.Scan(ctx, rescan)
	if err != nil {
		a.logf("wifi", "Scan failed: %v", err)
	}
	out := nets[:0:0]
	for _, n := range nets {
		if n.SSID != a.HotspotSSID() {
			out = append(out, n)
		}
	}
	return out
}

// Uptime is how long the agent has been running.
func (a *Agent) Uptime() time.Duration { return time.Since(a.start) }

// ---------- running ----------

// Run runs the dongle until ctx ends, and returns once the browser's supervisor and steering have stopped too.
func (a *Agent) Run(ctx context.Context) {
	a.logf("agent", "Starting, version %s, dongle %s", a.Version, a.Hardware)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Browser.Run(ctx) }()
	go func() { defer wg.Done(); a.steerBrowser(ctx) }()
	defer wg.Wait()
	for ctx.Err() == nil {
		if a.Store.Config().Paired() {
			a.runPaired(ctx)
		} else {
			a.runSetup(ctx)
		}
	}
}

func (a *Agent) hotspotUp(ctx context.Context) {
	if err := a.Net.StartHotspot(ctx, a.HotspotSSID(), a.HotspotPassword()); err != nil {
		a.logf("wifi", "Couldn't turn on the setup Wi-Fi: %v", err)
	}
}

// runSetup: not paired. The setup Wi-Fi stays on until a phone gives it details that work.
func (a *Agent) runSetup(ctx context.Context) {
	a.adoptPreconfigured(ctx) // before the setup page can answer
	a.hotspotUp(ctx)          // before "Scan to start": the QR code is for this Wi-Fi
	a.setPhase(PhasePair, "")
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.rescueCh: // nothing to rescue: already in setup
		case <-tick.C:
			// Only when there's something to change: every update has the TV page and the browser check look again.
			if st := a.State(); st.Phase == PhasePortal && time.Since(st.PortalSeen) > PortalIdleAfter {
				a.update(func(s *State) {
					if s.Phase == PhasePortal && time.Since(s.PortalSeen) > PortalIdleAfter {
						s.Phase = PhasePair
					}
				})
			}
		case req := <-a.setupCh:
			if a.trySetup(ctx, req) {
				return
			}
			a.hotspotUp(ctx)
		}
	}
}

// adoptPreconfigured keeps a Wi-Fi saved with Raspberry Pi Imager as the network the setup page starts with. It
// isn't joined behind the phone's back: setup still goes through the phone, with the network filled in.
func (a *Agent) adoptPreconfigured(ctx context.Context) {
	p, err := a.Net.TakePreconfigured(ctx)
	if err != nil {
		a.logf("wifi", "Couldn't read the Wi-Fi saved with Raspberry Pi Imager: %v", err)
	}
	if p == nil {
		return
	}
	a.logf("wifi", "Found %s, saved with Raspberry Pi Imager: the setup page starts with it", p.SSID)
	_, _ = a.Store.Update(func(c *store.Config) {
		c.WifiSSID, c.WifiPassword, c.WifiHidden, c.Wired = p.SSID, p.PSK, p.Hidden, false
	})
}

// trySetup joins the network, then pairs. On failure the phase says why (the caller turns the hotspot back on).
func (a *Agent) trySetup(ctx context.Context, req SetupRequest) bool {
	a.update(func(s *State) { s.Attempt = &req })
	if !a.joinNetwork(ctx, req) {
		return false
	}
	a.setPhase(PhasePairing, "")
	cfg := a.Store.Config()
	st := a.Net.Status(ctx)
	res, err := pavilion.Pair(ctx, cfg.PavilionURL, cfg.PairCode, a.deviceInfo(st))
	if err != nil {
		var codeErr *pavilion.CodeError
		if errors.As(err, &codeErr) {
			a.logf("pair", "Pavilion didn't take the code: %s", codeErr.Message)
			_, _ = a.Store.Update(func(c *store.Config) { c.PairCode = "" })
			a.setPhase(PhaseCodeFail, codeErr.Message)
		} else {
			a.logf("pair", "Couldn't reach Pavilion: %v", err)
			a.setPhase(PhaseReachFail, err.Error())
		}
		return false
	}
	_, _ = a.Store.Update(func(c *store.Config) {
		c.Token, c.DongleID, c.Name, c.StoreName = res.Token, res.Dongle.ID, res.Dongle.Name, res.Store.Name
		c.PairCode, c.PairedAt, c.Screen = "", time.Now(), nil
	})
	a.logf("pair", "Paired with %s as %s", res.Store.Name, res.Dongle.Name)
	a.setPhase(PhaseDone, "")
	select {
	case <-ctx.Done():
	case <-time.After(DoneFor):
	}
	return true
}

// joinNetwork saves the network details and joins (Wi-Fi) or checks the cable, then waits for an address.
func (a *Agent) joinNetwork(ctx context.Context, req SetupRequest) bool {
	_, _ = a.Store.Update(func(c *store.Config) {
		if req.PavilionURL != "" {
			c.PavilionURL, c.PairCode = req.PavilionURL, req.Code
		}
		c.Wired, c.WifiSSID, c.WifiPassword, c.WifiHidden = req.Wired, req.SSID, req.Password, req.Hidden
		if req.Wired {
			c.WifiSSID, c.WifiPassword = "", ""
		}
	})
	a.setPhase(PhaseJoining, "")
	// Let the phone get the "Watch the TV" page before its Wi-Fi goes away.
	select {
	case <-ctx.Done():
		return false
	case <-time.After(PhoneGrace):
	}
	if req.Wired {
		_ = a.Net.StopHotspot(ctx)
	} else if err := a.Net.Join(ctx, req.SSID, req.Password, req.Hidden); err != nil {
		a.logf("wifi", "Couldn't join %s: %v", req.SSID, err)
		detail := "The network didn't let the dongle in."
		switch {
		case errors.Is(err, netmgr.ErrAuth):
			detail = "The password wasn't accepted."
		case errors.Is(err, netmgr.ErrNotFound):
			detail = "The network is too far away, or its name is different."
		}
		a.setPhase(PhaseWifiFail, detail)
		return false
	}
	a.setPhase(PhaseAddress, "")
	if req.Wired {
		deadline := time.Now().Add(30 * time.Second)
		for !a.Net.Status(ctx).Connected() {
			if time.Now().After(deadline) || ctx.Err() != nil {
				a.setPhase(PhaseWifiFail, "The cable isn't connected to a network.")
				return false
			}
			time.Sleep(time.Second)
		}
	} else if _, err := a.Net.WaitForIP(ctx); err != nil {
		a.logf("wifi", "No address from %s: %v", req.SSID, err)
		a.setPhase(PhaseWifiFail, "The router didn't give the dongle an address.")
		return false
	}
	a.update(func(s *State) { s.Net = a.Net.Status(ctx) })
	return true
}

func (a *Agent) deviceInfo(st netmgr.Status) pavilion.Device {
	return pavilion.Device{HardwareID: a.Hardware, Model: device.Model(), Version: a.Version, Resolution: device.Resolution(),
		Network: pavilion.Network{SSID: st.SSID, Signal: st.Signal, IP: st.IP, Wired: st.Wired && !st.WifiConnected}}
}

// screenPhase is idle or showing, from what Pavilion said.
func (a *Agent) screenPhase() Phase {
	if a.Store.Config().Screen != nil {
		return PhaseShowing
	}
	return PhaseIdle
}

// runPaired: keeps the WebSocket to Pavilion open and the network watched, until the dongle is unpaired.
func (a *Agent) runPaired(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// For an agent started again under a kiosk that's up. At boot the kiosk isn't up yet and this can't work:
	// steerBrowser turns the picture once the browser answers.
	_ = a.turnPicture(ctx)
	st := a.Net.Status(ctx)
	if st.Hotspot { // e.g. restarted while in rescue: try the saved Wi-Fi first
		_ = a.Net.Reconnect(ctx)
		st = a.Net.Status(ctx)
	}
	a.update(func(s *State) {
		s.Net = st
		if st.Connected() {
			s.Phase = a.screenPhase()
		} else {
			s.Phase = PhaseConnecting
		}
	})
	unpaired := make(chan struct{})
	go func() {
		if a.socketLoop(ctx) == pavilion.ErrUnpaired {
			close(unpaired)
		}
	}()

	var lostSince, lastTry time.Time
	tick := time.NewTicker(NetCheckEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-unpaired:
			a.unpair(ctx)
			return
		case why := <-a.rescueCh:
			a.enterRescue(ctx, why)
			lastTry = time.Now()
		case req := <-a.setupCh:
			// A new Wi-Fi from the phone, in rescue.
			if a.joinNetwork(ctx, SetupRequest{SSID: req.SSID, Password: req.Password, Hidden: req.Hidden, Wired: req.Wired}) {
				a.logf("wifi", "Joined %s", req.SSID)
				a.update(func(s *State) { s.Phase, s.Rescue, s.Detail = a.screenPhase(), "", "" })
				a.kickSocket()
				lostSince = time.Time{}
			} else {
				a.update(func(s *State) { s.Rescue = "lost" })
				a.hotspotUp(ctx)
			}
		case <-tick.C:
			state := a.State()
			if state.Phase == PhaseRescue || state.Phase == PhaseWifiFail {
				// Every few minutes, when nobody is on the setup page, see if the old Wi-Fi is back.
				cfg := a.Store.Config()
				if cfg.HasNetwork() && time.Since(lastTry) > RetryWifiEvery && time.Since(state.PortalSeen) > PortalIdleAfter && state.Rescue != "change" {
					lastTry = time.Now()
					a.logf("wifi", "Trying %s again", cfg.WifiSSID)
					if a.Net.Reconnect(ctx) == nil {
						if _, err := a.Net.WaitForIP(ctx); err == nil {
							a.logf("wifi", "%s is back", cfg.WifiSSID)
							a.update(func(s *State) { s.Phase, s.Rescue, s.Detail = a.screenPhase(), "", "" })
							a.kickSocket()
							lostSince = time.Time{}
							continue
						}
					}
					a.hotspotUp(ctx)
				}
				continue
			}
			st := a.Net.Status(ctx)
			a.noteNet(st)
			if st.Connected() {
				lostSince = time.Time{}
				if state.Phase == PhaseConnecting {
					a.update(func(s *State) { s.Phase = a.screenPhase() })
				}
				continue
			}
			if lostSince.IsZero() {
				lostSince = time.Now()
				a.logf("wifi", "Lost the network")
			}
			if time.Since(lostSince) >= LostWifiAfter {
				a.enterRescue(ctx, "lost")
				lastTry = time.Now()
			}
		}
	}
}

// noteNet keeps what the network check found. The TV page and the browser check are told only when something the TV
// shows changed (the network, its address, its signal as a word): the signal's number wobbles all the time, and
// every nudge is work for a browser that has none to spare.
func (a *Agent) noteNet(st netmgr.Status) {
	a.mu.Lock()
	old := a.state.Net
	a.state.Net = st
	a.mu.Unlock()
	old.Signal, st.Signal = netmgr.Bars(old.Signal), netmgr.Bars(st.Signal)
	if old != st {
		a.update(func(*State) {})
	}
}

func (a *Agent) enterRescue(ctx context.Context, why string) {
	a.logf("wifi", "Setup Wi-Fi on (%s)", why)
	a.update(func(s *State) { s.Phase, s.Rescue, s.Detail = PhaseRescue, why, "" })
	a.hotspotUp(ctx)
}

// unpair forgets Pavilion's token and what it showed, keeping the address and Wi-Fi for the next setup.
func (a *Agent) unpair(ctx context.Context) {
	a.logf("pair", "Unpaired by Pavilion")
	_, _ = a.Store.Update(func(c *store.Config) {
		c.Token, c.DongleID, c.Name, c.StoreName, c.Screen, c.PairCode = "", 0, "", "", nil, ""
	})
	if err := a.turnPicture(ctx); err != nil { // not paired any more: back to landscape
		a.logf("display", "Couldn't turn the picture back: %v", err)
	}
}

// turnPicture turns the TV's picture the way Pavilion set it, or to landscape on a dongle that isn't paired (setup
// is read that way). It reads what's stored itself, one caller at a time, so whichever call comes last leaves the
// picture as stored.
func (a *Agent) turnPicture(ctx context.Context) error {
	if a.KioskRotates {
		// The kiosk turns the picture when it starts. One that started with another turn than the one wanted
		// now is started again; one that hasn't started yet will ask.
		a.mu.Lock()
		running := a.rotation
		a.mu.Unlock()
		if want := a.wantRotation(); running >= 0 && running != want {
			a.logf("display", "Turning the picture: starting the kiosk again")
			a.mu.Lock()
			a.rotation = -1 // until the new one asks: a second call mustn't restart it again
			a.mu.Unlock()
			a.Browser.Restart()
		}
		return nil
	}
	if a.Rotator == nil {
		return nil
	}
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	cfg := a.Store.Config()
	orientation := cfg.Orientation
	if !cfg.Paired() {
		orientation = "landscape"
	}
	if orientation == "" {
		return nil
	}
	return a.Rotator.Rotate(ctx, orientation)
}

// wantRotation is the stored orientation as quarter turns (0 to 3, the way cog counts them): landscape 0, portrait
// 1, portrait_flipped 3; landscape on a dongle that isn't paired.
func (a *Agent) wantRotation() int {
	cfg := a.Store.Config()
	if !cfg.Paired() {
		return 0
	}
	return map[string]int{"portrait": 1, "portrait_flipped": 3}[cfg.Orientation]
}

// Rotation is asked by the kiosk each time it starts (KioskRotates): how many quarter turns to show the picture
// with. The agent remembers the answer, so it knows when the kiosk has to start again for a new one.
func (a *Agent) Rotation() int {
	want := a.wantRotation()
	a.mu.Lock()
	a.rotation = want
	a.mu.Unlock()
	return want
}

// pictureTurns is what steerBrowser remembers about turning the picture. Cage starts with the picture as the TV
// is, whatever it was turned to before, so every browser that starts (a new browser.Target) has it turned again.
type pictureTurns struct {
	done    string    // the browser whose picture was last turned (or that was given up on)
	tries   int       // failed tries with the browser that's up now
	at      time.Time // when done's picture was turned, until that browser has been seen to live through it
	stopped int       // browsers in a row that were gone right after their picture was turned
}

// turnFor turns the picture for the browser that just answered, if that hasn't been done for this one. It returns
// when to look at the browser again to see that the turn didn't stop it (nil: no need).
func (a *Agent) turnFor(ctx context.Context, p *pictureTurns, target string) <-chan time.Time {
	if target == p.done {
		if !p.at.IsZero() && time.Since(p.at) >= TurnSettle {
			p.at, p.stopped = time.Time{}, 0
		}
		return nil
	}
	cfg := a.Store.Config()
	if a.Rotator == nil || !cfg.Paired() || cfg.Orientation == "" {
		return nil // nothing to turn it to: Cage starts in landscape, which is how setup is read
	}
	if !p.at.IsZero() { // the last browser was gone before it was seen to live through its turn
		p.at = time.Time{}
		p.stopped++
	}
	if p.stopped >= 2 {
		// Twice in a row is no accident: turning the picture stops this kiosk, and doing it for every browser that
		// starts would leave the TV with none. The next browser after this one is tried again.
		a.logf("display", "The last two browsers stopped right after the picture was turned; leaving this one's as it is")
		p.done, p.tries, p.stopped = target, 0, 0
		return nil
	}
	err := a.turnPicture(ctx)
	if err == nil {
		p.done, p.tries, p.at = target, 0, time.Now()
		return time.After(TurnSettle)
	}
	if p.tries++; p.tries >= 3 || errors.Is(err, device.ErrOldCage) {
		a.logf("display", "Couldn't turn the picture: %v", err)
		p.done, p.tries = target, 0
	}
	return nil
}

// ---------- Pavilion ----------

func (a *Agent) kickSocket() {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	if a.conn != nil {
		a.conn.Close() // the loop reconnects straight away
	}
}

// socketLoop keeps the WebSocket open, reconnecting with growing waits. It returns ErrUnpaired, or ctx's error.
func (a *Agent) socketLoop(ctx context.Context) error {
	wait := time.Second
	for ctx.Err() == nil {
		cfg := a.Store.Config()
		if !a.State().Net.Connected() && a.State().Phase != PhaseShowing && a.State().Phase != PhaseIdle {
			// No network yet: don't spin.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		conn, err := pavilion.Dial(ctx, cfg.PavilionURL, cfg.Token, a.Version)
		if errors.Is(err, pavilion.ErrUnpaired) {
			return err
		}
		if err == nil {
			wait = time.Second
			a.update(func(s *State) { s.PavilionOnline, s.PavilionError = true, "" })
			a.logf("pavilion", "Connected to %s", pavilion.Host(cfg.PavilionURL))
			err = a.session(ctx, conn)
			if errors.Is(err, pavilion.ErrUnpaired) {
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := "lost the connection"
		if err != nil {
			msg = err.Error()
		}
		if a.State().PavilionOnline {
			a.logf("pavilion", "Disconnected: %s", msg)
		}
		a.update(func(s *State) { s.PavilionOnline, s.PavilionError = false, msg })
		jitter, _ := rand.Int(rand.Reader, big.NewInt(int64(wait/2)+1))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait + time.Duration(jitter.Int64())):
		}
		if wait *= 2; wait > time.Minute {
			wait = time.Minute
		}
	}
	return ctx.Err()
}

func (a *Agent) status() pavilion.Status {
	showing := "setup"
	switch st := a.State(); {
	case st.IdentifyUntil.After(time.Now()):
		showing = "identify"
	case st.Phase == PhaseShowing:
		showing = "screen"
	case st.Phase == PhaseIdle:
		showing = "idle"
	}
	return pavilion.Status{Device: a.deviceInfo(a.State().Net), Showing: showing,
		Uptime: int64(a.Uptime().Seconds()), Restarts: a.Browser.Restarts()}
}

// session runs one open WebSocket: status every StatusEvery, Pavilion's messages as they come.
func (a *Agent) session(ctx context.Context, conn *pavilion.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.connMu.Lock()
	a.conn = conn
	a.connMu.Unlock()
	defer func() {
		a.connMu.Lock()
		a.conn = nil
		a.connMu.Unlock()
		conn.Close()
	}()
	send := func(m pavilion.Message) error { return conn.Send(ctx, m) }
	sendStatus := func() error { st := a.status(); return send(pavilion.Message{Type: "status", Status: &st}) }
	if err := sendStatus(); err != nil {
		return err
	}
	// One goroutine only reads, another acts on what was read, in order: acting can take seconds (reloading a busy
	// browser, turning the picture), and the answers to our pings only arrive while something is reading.
	readErr := make(chan error, 2)
	msgs := make(chan pavilion.Message, 16)
	go func() {
		for {
			m, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-msgs:
				if err := a.handle(ctx, m, send); err != nil {
					readErr <- err
					return
				}
			}
		}
	}()
	tick := time.NewTicker(StatusEvery)
	defer tick.Stop()
	ping := time.NewTicker(PingEvery)
	defer ping.Stop()
	for {
		select {
		case err := <-readErr:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := sendStatus(); err != nil {
				return err
			}
		case <-ping.C:
			// A connection that died quietly (the Wi-Fi dropped for a moment, the router restarted) still takes
			// what we send for many minutes, while nothing Pavilion sends arrives: a screen picked there would
			// sit unseen until the connection finally failed. So ask, and open a new one when nothing answers.
			pctx, cancel := context.WithTimeout(ctx, PingTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				return fmt.Errorf("Pavilion stopped answering (%w)", err)
			}
		}
	}
}

func ack(id string, err error) pavilion.Message {
	ok := err == nil
	m := pavilion.Message{Type: "ack", ID: id, OK: &ok}
	if err != nil {
		m.Error = err.Error()
	}
	return m
}

// handle acts on one message from Pavilion. It returns ErrUnpaired when Pavilion says so.
func (a *Agent) handle(ctx context.Context, m pavilion.Message, send func(pavilion.Message) error) error {
	switch m.Type {
	case "config":
		before := a.Store.Config()
		var screen *store.Screen
		if m.Screen != nil && m.Screen.URL != "" {
			url := m.Screen.URL
			if strings.HasPrefix(url, "/") { // a path on the Pavilion it's paired with
				url = before.PavilionURL + url
			}
			screen = &store.Screen{Name: m.Screen.Name, URL: url}
		}
		cfg, err := a.Store.Update(func(c *store.Config) {
			c.Name, c.StoreName, c.Screen = m.Name, m.Store, screen
			if m.Orientation != "" {
				c.Orientation = m.Orientation
			}
		})
		if err != nil {
			a.logf("store", "Couldn't save Pavilion's settings: %v", err)
		}
		if cfg.Orientation != before.Orientation {
			if err := a.turnPicture(ctx); err != nil {
				a.logf("display", "Couldn't turn the picture: %v", err)
			}
		}
		screenChanged := (before.Screen == nil) != (cfg.Screen == nil) || (cfg.Screen != nil && before.Screen != nil && cfg.Screen.URL != before.Screen.URL)
		a.update(func(s *State) {
			if s.Phase == PhaseIdle || s.Phase == PhaseShowing {
				s.Phase = a.screenPhase()
			}
			if screenChanged {
				s.ScreenFailed = time.Time{}
			}
		})
		if screenChanged {
			a.forceNavigate()
		}
	case "command":
		var err error
		switch m.Command {
		case "identify":
			secs := m.Seconds
			if secs <= 0 || secs > 120 {
				secs = 10
			}
			a.update(func(s *State) { s.IdentifyUntil = time.Now().Add(time.Duration(secs) * time.Second) })
			time.AfterFunc(time.Duration(secs)*time.Second+200*time.Millisecond, func() { a.update(func(*State) {}) })
		case "reload":
			err = a.Browser.Reload(ctx)
		case "restart":
			_ = send(ack(m.ID, nil))
			a.logf("agent", "Restarting (asked by Pavilion)")
			go func() {
				time.Sleep(time.Second)
				if a.Reboot != "" {
					if err := device.Run(context.Background(), a.Reboot); err != nil {
						a.logf("agent", "Restart failed: %v", err)
					}
				} else {
					a.Browser.Restart()
				}
			}()
			return nil
		case "change_wifi":
			select {
			case a.rescueCh <- "change":
			default:
			}
		default:
			err = fmt.Errorf("unknown command %q", m.Command)
		}
		_ = send(ack(m.ID, err))
	case "unpaired":
		return pavilion.ErrUnpaired
	}
	return nil
}

// ---------- browser ----------

func (a *Agent) forceNavigate() {
	select {
	case a.browserCh <- struct{}{}:
	default:
	}
}

// ScreenFailed notes that the screen couldn't be opened (the browser showed its error page, or the dongle's page
// couldn't reach the screen's address before going there): the TV says so on the dongle's page, and the screen is
// tried again after ScreenRetry.
func (a *Agent) ScreenFailed() {
	a.logf("browser", "The screen didn't load; showing the dongle's page and trying again shortly")
	a.update(func(s *State) { s.ScreenFailed = time.Now() })
	time.AfterFunc(ScreenRetry+time.Second, func() { a.update(func(*State) {}) }) // the TV page and the browser look again
}

// GoURL is the screen the browser should be on right now, or "" when it should be on the dongle's page.
func (a *Agent) GoURL() string {
	if want := a.wantURL(); want != a.ShellURL {
		return want
	}
	return ""
}

// wantURL is where the browser should be: the screen while showing it, else the agent's page.
func (a *Agent) wantURL() string {
	st := a.State()
	cfg := a.Store.Config()
	if st.Phase == PhaseShowing && cfg.Screen != nil && st.IdentifyUntil.Before(time.Now()) &&
		time.Since(st.ScreenFailed) > ScreenRetry {
		return cfg.Screen.URL
	}
	return a.ShellURL
}

func (a *Agent) onShell(url string) bool { return strings.HasPrefix(url, a.ShellURL) }

// BrowserInfo is what the browser said about itself (which one, and whether the graphics chip draws its pages), or
// nil when it hasn't been asked yet or can't be.
func (a *Agent) BrowserInfo() *browser.Info {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.browserInfo
}

// describeBrowser asks the browser what it is and notes the answer in the log: how it draws is the first thing to
// know about a dongle that's slow.
func (a *Agent) describeBrowser(ctx context.Context) bool {
	info, err := a.Browser.Describe(ctx)
	if err != nil {
		return false
	}
	a.mu.Lock()
	a.browserInfo = &info
	a.mu.Unlock()
	how := "its pages are drawn by the processor, not the graphics chip"
	if info.Accelerated() {
		how = "its pages are drawn by the graphics chip"
	}
	if status := info.Graphics["gpu_compositing"]; status != "" {
		how += " (" + status + ")"
	}
	a.logf("browser", "The browser is %s; %s", info.Version, how)
	return true
}

// steerByPage steers a browser that has no DevTools port (cog). The dongle's page does the going: while the
// browser shows it, it holds a connection to the agent open (Viewers), and it takes itself to the screen when the
// agent's state says so (PageGo, the page's data-go). That leaves the agent one thing to do: call the browser back
// to the dongle's page when that's where it should be and isn't (no screen any more, another screen, the dongle's
// name to show, setup). It asks with Browser.Open, and starts the browser again when asking doesn't work or
// brings nothing: a browser that starts, starts on the dongle's page.
//
// What it can't see is a screen that stopped working after it opened. When a page's process dies cog exits, and a
// cog that exits is started again by systemd, on the dongle's page, which sends it to the screen.
func (a *Agent) steerByPage(ctx context.Context) {
	tick := time.NewTicker(BrowserCheck)
	defer tick.Stop()
	started := time.Now()
	a.Browser.Uptime()     // a browser the agent doesn't run counts as started when the agent did
	var calledAt time.Time // when the browser was last called back
	calls := 0             // calls since it was last on the dongle's page
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.browserCh:
		case <-tick.C:
		case <-retry:
		}
		retry = nil
		want := a.wantURL()
		viewers, heading, seen, left := a.page()
		if seen.After(calledAt) {
			calledAt, calls = time.Time{}, 0 // it came: whatever is wrong now is new
		}
		if viewers > 0 {
			continue // on the dongle's page, which goes to the screen by itself
		}
		if want != a.ShellURL && want == heading {
			continue // it left for the screen that's wanted
		}
		// It should be on the dongle's page (to stay, or to be sent on to another screen), and isn't.
		if left.IsZero() {
			left = started // not seen on it since the agent started
		}
		if away := time.Since(left); away < AwayGrace {
			retry = time.After(AwayGrace - away) // perhaps only loading the page, or its connection is coming back
			continue
		}
		if !calledAt.IsZero() && time.Since(calledAt) < OpenGrace {
			continue // called a moment ago: the page takes a while to arrive
		}
		err := errors.New("it was called and didn't come")
		if calls == 0 {
			if err = a.Browser.Open(ctx, a.ShellURL); err == nil {
				calls, calledAt = 1, time.Now()
				continue
			}
		}
		// It can't be called (not up yet on a dongle that's starting, or calling doesn't work here), or it was and
		// nothing came. One that has had its time to start is started again: that brings it to the dongle's page too.
		if a.Browser.CanRestart() && a.Browser.Uptime() > OpenGrace {
			a.logf("browser", "The browser isn't on the dongle's page (%v); starting it again", err)
			a.Browser.Restart()
			calls, calledAt = 0, time.Now()
		} else if !errors.Is(err, browser.ErrNoOpen) {
			retry = time.After(OpenRetry)
		}
	}
}

// steerBrowser keeps the browser on wantURL and restarts it when it hangs.
func (a *Agent) steerBrowser(ctx context.Context) {
	if a.Browser.CDP == "" {
		a.steerByPage(ctx)
		return
	}
	tick := time.NewTicker(BrowserCheck)
	defer tick.Stop()
	var hungSince, sentAt time.Time
	starvedLogged := false
	lastWant := ""                    // the screen the browser was last sent to (it lands on another address: the link signs in, then redirects)
	described, describeTries := "", 0 // the browser (browser.Target) that was last asked what it is
	var turns pictureTurns
	var settle <-chan time.Time
	open := func(want string) bool {
		lastWant, sentAt = want, time.Now()
		if err := a.Browser.Navigate(ctx, want); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				// Slow, not failed: the page is most likely still loading. Opening it again would only start it over.
				a.logf("browser", "%s is taking a while to open", want)
				return true
			}
			a.logf("browser", "Couldn't open %s: %v", want, err)
			lastWant = ""
			return false
		}
		return true
	}
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.browserCh:
		case <-tick.C:
		case <-retry:
		case <-settle:
			settle = nil
		}
		retry = nil
		want := a.wantURL()
		// Somewhere new to go (a screen just picked in Pavilion, or back to the dongle's page): the browser itself
		// says where it is, even while its page is too busy to answer, so it can be sent there straight away. The
		// page is asked (is it hung? did it load?) on the checks after that.
		pending := want != lastWant
		var current string
		var err error
		if pending {
			current, err = a.Browser.URL(ctx)
		} else {
			current, err = a.Browser.Probe(ctx)
		}
		if !errors.Is(err, browser.ErrNoCDP) {
			// The browser answered (even if its page is too busy to), so Cage is up: the picture is turned before
			// anything is opened, not after a screen has loaded sideways.
			if again := a.turnFor(ctx, &turns, a.Browser.Target()); again != nil {
				settle = again
			}
		}
		if err != nil {
			if a.Starved != nil && a.Starved() {
				// Waiting for memory, not hung: a restart would only start the slow load over. The clock starts once
				// the Pi has memory to spare again.
				if !starvedLogged {
					a.logf("browser", "The Pi is short of memory; waiting for the browser instead of restarting it")
					starvedLogged = true
				}
				hungSince = time.Time{}
			} else if hungSince.IsZero() {
				hungSince = time.Now()
			}
			if !hungSince.IsZero() && time.Since(hungSince) >= BrowserHungAfter && a.Browser.CanRestart() && a.Browser.Uptime() > BrowserHungAfter {
				a.logf("browser", "The browser hasn't answered for %s; restarting it", BrowserHungAfter)
				a.Browser.Restart()
				hungSince = time.Time{}
			}
			if pending {
				retry = time.After(OpenRetry) // not at the next check, 15 seconds away: someone is waiting for this screen
			}
			continue
		}
		hungSince, starvedLogged = time.Time{}, false
		if browser.IsErrorPage(current) && !a.onShell(current) {
			a.ScreenFailed()
			want = a.ShellURL
		}
		if want == lastWant && time.Since(sentAt) < OpenGrace && !browser.IsErrorPage(current) {
			continue // sent there a moment ago: until the page arrives the browser still says where it was
		}
		onWant := current == want || (want == a.ShellURL && a.onShell(current)) ||
			(want != a.ShellURL && !a.onShell(current) && want == lastWant && !browser.IsErrorPage(current))
		if !onWant {
			if !open(want) {
				retry = time.After(OpenRetry)
			}
			continue
		}
		lastWant = want
		// Nothing to do and the browser is answering: a good moment to ask a browser that just started what it is.
		if target := a.Browser.Target(); target != described && describeTries < 3 {
			if describeTries++; a.describeBrowser(ctx) {
				described, describeTries = target, 0
			}
		} else if target != described {
			described, describeTries = target, 0 // it won't say: don't keep asking this one
		}
	}
}
