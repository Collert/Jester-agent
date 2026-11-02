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
	NetCheckEvery    = 10 * time.Second
	BrowserCheck     = 15 * time.Second
	BrowserHungAfter = 2 * time.Minute         // no answer from the browser this long (not counting time the Pi is short of memory): restart it
	OpenGrace        = time.Minute             // after sending the browser somewhere, how long the page may take to arrive
	ScreenRetry      = 30 * time.Second        // after the screen failed to load, show our page this long before trying again
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
	ShellURL string // the agent's own page in the kiosk browser, e.g. http://127.0.0.1:8080/
	Version  string
	Reboot   string // command line run for Pavilion's Restart ("" = restart the browser only)
	Hardware string // short ID; "" reads it from the Pi
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

	setupCh   chan SetupRequest
	rescueCh  chan string
	browserCh chan struct{}
	conn      *pavilion.Conn
	connMu    sync.Mutex
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

// Subscribe returns a channel nudged on every change (the TV page's live updates) and a function to stop.
func (a *Agent) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	a.mu.Lock()
	a.subs[ch] = struct{}{}
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		delete(a.subs, ch)
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
			a.update(func(s *State) {
				if s.Phase == PhasePortal && time.Since(s.PortalSeen) > PortalIdleAfter {
					s.Phase = PhasePair
				}
			})
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
	cfg := a.Store.Config()
	if a.Rotator != nil && cfg.Orientation != "" {
		_ = a.Rotator.Rotate(ctx, cfg.Orientation)
	}
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
			a.update(func(s *State) { s.Net = st })
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
	if a.Rotator != nil {
		_ = a.Rotator.Rotate(ctx, "landscape")
	}
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
	readErr := make(chan error, 1)
	go func() {
		for {
			m, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			if err := a.handle(ctx, m, send); err != nil {
				readErr <- err
				return
			}
		}
	}()
	tick := time.NewTicker(StatusEvery)
	defer tick.Stop()
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
		if cfg.Orientation != before.Orientation && a.Rotator != nil {
			if err := a.Rotator.Rotate(ctx, cfg.Orientation); err != nil {
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

// steerBrowser keeps the browser on wantURL and restarts it when it hangs.
func (a *Agent) steerBrowser(ctx context.Context) {
	tick := time.NewTicker(BrowserCheck)
	defer tick.Stop()
	var hungSince, sentAt time.Time
	starvedLogged := false
	lastWant := "" // the screen the browser was last sent to (it lands on another address: the link signs in, then redirects)
	open := func(want string) {
		lastWant, sentAt = want, time.Now()
		if err := a.Browser.Navigate(ctx, want); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				// Slow, not failed: the page is most likely still loading. Opening it again would only start it over.
				a.logf("browser", "%s is taking a while to open", want)
				return
			}
			a.logf("browser", "Couldn't open %s: %v", want, err)
			lastWant = ""
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.browserCh:
		case <-tick.C:
		}
		want := a.wantURL()
		current, err := a.Browser.Probe(ctx)
		if err != nil {
			if errors.Is(err, browser.ErrNoCDP) && a.Browser.CDP == "" {
				continue // not steerable: the agent's page navigates by itself
			}
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
			// A busy page doesn't answer, but the browser itself can still take it somewhere new (a screen just
			// picked in Pavilion): don't wait for the old page to calm down first.
			if want != lastWant && !errors.Is(err, browser.ErrNoCDP) {
				open(want)
			}
			continue
		}
		hungSince, starvedLogged = time.Time{}, false
		if browser.IsErrorPage(current) && !a.onShell(current) {
			a.logf("browser", "The screen didn't load; showing the dongle's page and trying again shortly")
			a.update(func(s *State) { s.ScreenFailed = time.Now() })
			time.AfterFunc(ScreenRetry+time.Second, a.forceNavigate)
			want = a.ShellURL
		}
		if want == lastWant && time.Since(sentAt) < OpenGrace && !browser.IsErrorPage(current) {
			continue // sent there a moment ago: until the page arrives the browser still says where it was
		}
		onWant := current == want || (want == a.ShellURL && a.onShell(current)) ||
			(want != a.ShellURL && !a.onShell(current) && want == lastWant && !browser.IsErrorPage(current))
		if onWant {
			continue
		}
		open(want)
	}
}
