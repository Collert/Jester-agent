package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/collert/jester-agent/internal/browser"
	"github.com/collert/jester-agent/internal/device"
	"github.com/collert/jester-agent/internal/netmgr"
	"github.com/collert/jester-agent/internal/store"
)

// fakePavilion answers pairing (code 123456 works) and the WebSocket, and records what the dongle says.
type fakePavilion struct {
	mu       sync.Mutex
	statuses []map[string]any
	acks     []map[string]any
	unpair   chan struct{}
	identify chan struct{}
}

func (f *fakePavilion) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/devices/dongles/pair", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body["code"] != "123456" || body["hardware_id"] != "TEST" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"bad_code","message":"Pavilion doesn’t know that code."}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":"7.s3cret","dongle":{"id":7,"name":"Dongle TEST"},"store":{"name":"Juniper Lane"}}`))
	})
	mux.HandleFunc("/ws/dongle", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer 7.s3cret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		send := func(m any) { raw, _ := json.Marshal(m); _ = ws.Write(ctx, websocket.MessageText, raw) }
		send(map[string]any{"type": "config", "name": "Window TV", "store": "Juniper Lane", "orientation": "landscape",
			"screen": map[string]any{"name": "Specials", "url": "/devices/dongles/open/abc"}})
		go func() {
			select {
			case <-f.identify:
				send(map[string]any{"type": "command", "id": "c1", "command": "identify", "seconds": 1})
			case <-ctx.Done():
				return
			}
			select {
			case <-f.unpair:
				send(map[string]any{"type": "unpaired"})
				_ = ws.Close(4401, "unpaired")
			case <-ctx.Done():
			}
		}()
		for {
			_, raw, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			f.mu.Lock()
			if m["type"] == "status" {
				f.statuses = append(f.statuses, m)
			} else if m["type"] == "ack" {
				f.acks = append(f.acks, m)
			}
			f.mu.Unlock()
		}
	})
	return mux
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSetupPairShowUnpair(t *testing.T) {
	PhoneGrace, DoneFor, NetCheckEvery = 10*time.Millisecond, 20*time.Millisecond, 50*time.Millisecond
	pav := &fakePavilion{unpair: make(chan struct{}), identify: make(chan struct{})}
	server := httptest.NewServer(pav.handler(t))
	defer server.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := netmgr.NewFake()
	fake.Delay = 5 * time.Millisecond
	a := New(Options{Store: st, Net: fake, ShellURL: "http://127.0.0.1:8080/", Version: "test", Hardware: "TEST"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	phase := func(p Phase) func() bool { return func() bool { return a.State().Phase == p } }

	waitFor(t, "the pair screen", phase(PhasePair))
	if !a.HotspotOn() || fake.Status(ctx).Hotspot != true {
		t.Fatal("the setup Wi-Fi should be on")
	}
	a.PortalSeen()
	if a.State().Phase != PhasePortal {
		t.Fatal("a phone on the setup page: Finish on your phone")
	}

	// A wrong password: back to setup, details kept.
	if !a.Submit(SetupRequest{PavilionURL: server.URL, Code: "123456", SSID: "Juniper-Staff", Password: "wrongpass"}) {
		t.Fatal("Submit refused")
	}
	waitFor(t, "wifi-fail", phase(PhaseWifiFail))
	if c := st.Config(); c.PavilionURL != server.URL || c.WifiSSID != "Juniper-Staff" {
		t.Errorf("details should be kept: %+v", c)
	}

	// A code Pavilion doesn't know.
	a.Submit(SetupRequest{PavilionURL: server.URL, Code: "999999", SSID: "Juniper-Staff", Password: "juniper2026"})
	waitFor(t, "code-fail", phase(PhaseCodeFail))
	if st.Config().PairCode != "" {
		t.Error("a refused code is forgotten")
	}

	// The right one: paired, connected, told to show a screen.
	a.Submit(SetupRequest{PavilionURL: server.URL, Code: "123456", SSID: "Juniper-Staff", Password: "juniper2026"})
	waitFor(t, "showing", phase(PhaseShowing))
	c := st.Config()
	if !c.Paired() || c.Token != "7.s3cret" || c.Name != "Window TV" || c.StoreName != "Juniper Lane" {
		t.Errorf("after pairing: %+v", c)
	}
	if c.Screen == nil || c.Screen.URL != server.URL+"/devices/dongles/open/abc" {
		t.Errorf("the screen's path is resolved against Pavilion: %+v", c.Screen)
	}
	if a.wantURL() != c.Screen.URL {
		t.Errorf("the browser should be on the screen, not %s", a.wantURL())
	}
	waitFor(t, "a status message", func() bool { pav.mu.Lock(); defer pav.mu.Unlock(); return len(pav.statuses) > 0 })
	pav.mu.Lock()
	first := pav.statuses[0]
	pav.mu.Unlock()
	if first["hardware_id"] != "TEST" || first["version"] != "test" {
		t.Errorf("status: %v", first)
	}

	// Show its name: the TV page for a second, then the screen again.
	close(pav.identify)
	waitFor(t, "the identify overlay", func() bool { return a.State().IdentifyUntil.After(time.Now()) })
	if a.wantURL() != "http://127.0.0.1:8080/" {
		t.Error("while identifying, the browser shows the dongle's page")
	}
	waitFor(t, "the ack", func() bool {
		pav.mu.Lock()
		defer pav.mu.Unlock()
		return len(pav.acks) == 1 && pav.acks[0]["ok"] == true
	})

	// Unpaired in Pavilion: back to "Scan to start", address and Wi-Fi kept.
	close(pav.unpair)
	waitFor(t, "the pair screen again", func() bool { return a.State().Phase == PhasePair && !st.Config().Paired() })
	if c := st.Config(); c.PavilionURL != server.URL || c.WifiSSID != "Juniper-Staff" || c.Screen != nil {
		t.Errorf("after unpairing: %+v", c)
	}
}

func TestRestartWhilePairedConnectsStraightAway(t *testing.T) {
	NetCheckEvery = 50 * time.Millisecond
	pav := &fakePavilion{unpair: make(chan struct{}), identify: make(chan struct{})}
	server := httptest.NewServer(pav.handler(t))
	defer server.Close()
	st, _ := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	defer st.Close()
	_, _ = st.Update(func(c *store.Config) { c.PavilionURL, c.Token, c.WifiSSID = server.URL, "7.s3cret", "Juniper-Staff" })
	fake := netmgr.NewFake()
	fake.Delay = 5 * time.Millisecond
	fake.Remember("Juniper-Staff")
	a := New(Options{Store: st, Net: fake, ShellURL: "http://127.0.0.1:8080/", Version: "test", Hardware: "TEST"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	waitFor(t, "showing", func() bool { return a.State().Phase == PhaseShowing && a.State().PavilionOnline })
	if a.HotspotOn() {
		t.Error("no setup Wi-Fi while all is well")
	}
}

func TestWifiLostTurnsSetupBackOn(t *testing.T) {
	NetCheckEvery, LostWifiAfter = 20*time.Millisecond, 100*time.Millisecond
	defer func() { LostWifiAfter = 2 * time.Minute }()
	st, _ := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	defer st.Close()
	_, _ = st.Update(func(c *store.Config) {
		c.PavilionURL, c.Token, c.WifiSSID = "http://127.0.0.1:1", "7.s3cret", "Juniper-Staff"
	})
	fake := netmgr.NewFake()
	fake.Delay = time.Millisecond
	fake.Remember("Juniper-Staff")
	a := New(Options{Store: st, Net: fake, ShellURL: "http://127.0.0.1:8080/", Version: "test", Hardware: "TEST"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	waitFor(t, "idle", func() bool { return a.State().Phase == PhaseIdle })
	fake.LoseWifi(true)
	waitFor(t, "rescue with the setup Wi-Fi on", func() bool { return a.State().Phase == PhaseRescue && fake.Status(ctx).Hotspot })
	if !st.Config().Paired() {
		t.Error("still paired")
	}
	// A new network from the phone.
	fake.LoseWifi(false)
	if !a.Submit(SetupRequest{SSID: "Juniper-Guest"}) {
		t.Fatal("Submit refused in rescue")
	}
	waitFor(t, "back to idle", func() bool { return a.State().Phase == PhaseIdle })
	if st.Config().WifiSSID != "Juniper-Guest" || !st.Config().Paired() {
		t.Errorf("after the new Wi-Fi: %+v", st.Config())
	}
}

// fakeCDP is a browser's DevTools port: Page.navigate answers slowly (a Pi Zero opening a screen) and lands on
// another address (the screen's link redirects); Runtime.evaluate says where the page is, or never answers (hung).
type fakeCDP struct {
	mu        sync.Mutex
	href      string
	navigates []string
	hung      bool
	down      bool   // the browser isn't up (yet): nothing answers
	instance  string // which browser this is: a kiosk that's started again is another one
	navDelay  time.Duration
	landOn    string
}

func (f *fakeCDP) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{{"type": "page", "url": f.href, "webSocketDebuggerUrl": "ws://" + r.Host + "/page?browser=" + f.instance}})
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_, raw, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		var m struct {
			ID     int64             `json:"id"`
			Method string            `json:"method"`
			Params map[string]string `json:"params"`
		}
		_ = json.Unmarshal(raw, &m)
		f.mu.Lock()
		var result any = map[string]any{}
		switch m.Method {
		case "Page.navigate":
			f.navigates = append(f.navigates, m.Params["url"])
			f.href = m.Params["url"]
			if f.landOn != "" && !strings.HasPrefix(f.href, "http://127.0.0.1:8080/") {
				f.href = f.landOn
			}
			delay := f.navDelay
			f.mu.Unlock()
			time.Sleep(delay)
		case "Runtime.evaluate":
			hung := f.hung
			result = map[string]any{"result": map[string]any{"value": f.href}}
			f.mu.Unlock()
			if hung {
				<-r.Context().Done()
				return
			}
		default:
			f.mu.Unlock()
		}
		reply, _ := json.Marshal(map[string]any{"id": m.ID, "result": result})
		_ = ws.Write(r.Context(), websocket.MessageText, reply)
	})
	return mux
}

// pairedWithScreen is a paired dongle told to show a screen, steering a browser at cdp.
func pairedWithScreen(t *testing.T, cdp string, restart string, starved ...func() bool) *Agent {
	t.Helper()
	return pairedWith(t, cdp, restart, func(o *Options, _ *store.Config) {
		if len(starved) > 0 {
			o.Starved = starved[0]
		}
	})
}

// pairedWith is pairedWithScreen with something more set up before the agent starts.
func pairedWith(t *testing.T, cdp string, restart string, more func(*Options, *store.Config)) *Agent {
	t.Helper()
	st, _ := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	t.Cleanup(func() { st.Close() })
	fake := netmgr.NewFake()
	fake.Delay = time.Millisecond
	fake.Remember("Juniper-Staff")
	o := Options{Store: st, Net: fake, ShellURL: "http://127.0.0.1:8080/", Version: "test", Hardware: "TEST",
		Browser: &browser.Browser{CDP: cdp, RestartCmd: restart}}
	_, _ = st.Update(func(c *store.Config) {
		c.PavilionURL, c.Token, c.WifiSSID = "http://127.0.0.1:1", "7.s3cret", "Juniper-Staff"
		c.Screen = &store.Screen{Name: "Order board", URL: "https://pavilion.test/devices/dongles/open/abc"}
		more(&o, c)
	})
	a := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	t.Cleanup(func() { cancel(); <-stopped }) // before the test's timings are put back (cleanups run last first)
	go func() { a.Run(ctx); close(stopped) }()
	return a
}

// browserTimings sets the browser check's timings for one test. Call it before pairedWithScreen: they're put back
// only once that agent has stopped.
func browserTimings(t *testing.T, check, hung, call time.Duration) {
	t.Helper()
	oldCheck, oldHung, oldCall := BrowserCheck, BrowserHungAfter, browser.CallTimeout
	BrowserCheck, BrowserHungAfter, browser.CallTimeout = check, hung, call
	t.Cleanup(func() { BrowserCheck, BrowserHungAfter, browser.CallTimeout = oldCheck, oldHung, oldCall })
}

func TestSlowScreenIsOpenedOnce(t *testing.T) {
	browserTimings(t, 20*time.Millisecond, BrowserHungAfter, 50*time.Millisecond)
	cdp := &fakeCDP{href: "http://127.0.0.1:8080/", navDelay: 200 * time.Millisecond, landOn: "https://pavilion.test/screens/order-board"}
	server := httptest.NewServer(cdp.handler(t))
	t.Cleanup(server.Close)
	pairedWithScreen(t, strings.TrimPrefix(server.URL, "http://"), "")

	waitFor(t, "the screen opened", func() bool { cdp.mu.Lock(); defer cdp.mu.Unlock(); return len(cdp.navigates) > 0 })
	time.Sleep(600 * time.Millisecond) // 30 checks
	cdp.mu.Lock()
	defer cdp.mu.Unlock()
	if len(cdp.navigates) != 1 {
		t.Errorf("a screen that's slow to open is opened once, not again on every check: %q", cdp.navigates)
	}
}

func TestBrowserRestartedOnlyWhenHungLong(t *testing.T) {
	browserTimings(t, 20*time.Millisecond, 400*time.Millisecond, 30*time.Millisecond)
	cdp := &fakeCDP{href: "http://127.0.0.1:8080/", hung: true}
	server := httptest.NewServer(cdp.handler(t))
	t.Cleanup(server.Close)
	a := pairedWithScreen(t, strings.TrimPrefix(server.URL, "http://"), "jester-test-no-such-command")

	time.Sleep(250 * time.Millisecond)
	if n := a.Browser.Restarts(); n != 0 {
		t.Fatalf("a browser that's been quiet a short while is left alone, restarted %d times", n)
	}
	waitFor(t, "a restart", func() bool { return a.Browser.Restarts() > 0 })
}

func TestStarvedBrowserIsNotRestarted(t *testing.T) {
	browserTimings(t, 20*time.Millisecond, 200*time.Millisecond, 30*time.Millisecond)
	cdp := &fakeCDP{href: "http://127.0.0.1:8080/", hung: true}
	server := httptest.NewServer(cdp.handler(t))
	t.Cleanup(server.Close)
	var mu sync.Mutex
	starved := true
	a := pairedWithScreen(t, strings.TrimPrefix(server.URL, "http://"), "jester-test-no-such-command",
		func() bool { mu.Lock(); defer mu.Unlock(); return starved })

	time.Sleep(700 * time.Millisecond)
	if n := a.Browser.Restarts(); n != 0 {
		t.Fatalf("a browser waiting for memory is left alone, restarted %d times", n)
	}
	mu.Lock()
	starved = false
	mu.Unlock()
	waitFor(t, "a restart once the Pi has memory and the browser still doesn't answer", func() bool { return a.Browser.Restarts() > 0 })
}

func TestNewScreenOpensWhileThePageIsBusy(t *testing.T) {
	browserTimings(t, 20*time.Millisecond, BrowserHungAfter, 30*time.Millisecond)
	cdp := &fakeCDP{href: "http://127.0.0.1:8080/", hung: true} // the page never answers; the browser does
	server := httptest.NewServer(cdp.handler(t))
	t.Cleanup(server.Close)
	pairedWithScreen(t, strings.TrimPrefix(server.URL, "http://"), "")

	waitFor(t, "the screen opened", func() bool { cdp.mu.Lock(); defer cdp.mu.Unlock(); return len(cdp.navigates) > 0 })
	time.Sleep(300 * time.Millisecond)
	cdp.mu.Lock()
	defer cdp.mu.Unlock()
	if len(cdp.navigates) != 1 {
		t.Errorf("opened once, not on every check: %q", cdp.navigates)
	}
}

// A screen picked in Pavilion while the browser can't be reached (still starting, or too busy to answer in time) is
// opened as soon as the browser answers, not at the next routine check a quarter of a minute later.
func TestNewScreenOpensAsSoonAsTheBrowserAnswers(t *testing.T) {
	browserTimings(t, time.Hour, BrowserHungAfter, 50*time.Millisecond) // no routine check during the test
	oldRetry := OpenRetry
	OpenRetry = 20 * time.Millisecond
	t.Cleanup(func() { OpenRetry = oldRetry })
	cdp := &fakeCDP{href: "http://127.0.0.1:8080/", down: true}
	server := httptest.NewServer(cdp.handler(t))
	t.Cleanup(server.Close)
	pairedWithScreen(t, strings.TrimPrefix(server.URL, "http://"), "")

	time.Sleep(100 * time.Millisecond)
	cdp.mu.Lock()
	if len(cdp.navigates) != 0 {
		t.Fatalf("nothing can be opened while the browser is down: %q", cdp.navigates)
	}
	cdp.down = false
	cdp.mu.Unlock()
	waitFor(t, "the screen opened", func() bool { cdp.mu.Lock(); defer cdp.mu.Unlock(); return len(cdp.navigates) > 0 })
	time.Sleep(150 * time.Millisecond)
	cdp.mu.Lock()
	defer cdp.mu.Unlock()
	if len(cdp.navigates) != 1 || cdp.navigates[0] != "https://pavilion.test/devices/dongles/open/abc" {
		t.Errorf("opened once: %q", cdp.navigates)
	}
}

// fakeRotator is wlr-randr: it can turn the picture only while the kiosk is up (at boot the agent starts first), and
// remembers what it turned it to.
type fakeRotator struct {
	cdp    *fakeCDP
	mu     sync.Mutex
	turned []string
	tries  int    // times it was asked while the kiosk was up
	err    error  // what turning fails with (nil: it works)
	onTurn func() // what a turn does to the kiosk
}

func (f *fakeRotator) Rotate(_ context.Context, orientation string) error {
	f.cdp.mu.Lock()
	down := f.cdp.down
	f.cdp.mu.Unlock()
	if down {
		return errors.New("wlr-randr: failed to connect to display (exit status 1)")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tries++; f.err != nil {
		return f.err
	}
	f.turned = append(f.turned, orientation)
	if f.onTurn != nil {
		f.onTurn()
	}
	return nil
}

func (f *fakeRotator) turns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.turned...)
}

// portraitDongle is a paired dongle set to portrait whose kiosk isn't up yet, as at boot; kioskUp starts it.
func portraitDongle(t *testing.T, rot *fakeRotator) (a *Agent, kioskUp func(instance string)) {
	t.Helper()
	browserTimings(t, 20*time.Millisecond, BrowserHungAfter, 50*time.Millisecond)
	oldSettle := TurnSettle
	TurnSettle = 60 * time.Millisecond
	t.Cleanup(func() { TurnSettle = oldSettle })
	cdp := &fakeCDP{href: "http://127.0.0.1:8080/", down: true}
	rot.cdp = cdp
	server := httptest.NewServer(cdp.handler(t))
	t.Cleanup(server.Close)
	a = pairedWith(t, strings.TrimPrefix(server.URL, "http://"), "", func(o *Options, c *store.Config) {
		o.Rotator, c.Orientation = rot, "portrait"
	})
	// Past the agent's own try at its start, which at boot comes before there's a kiosk.
	waitFor(t, "the agent running", func() bool { return a.State().Phase == PhaseShowing })
	return a, func(instance string) {
		cdp.mu.Lock()
		cdp.down, cdp.instance = false, instance
		cdp.mu.Unlock()
	}
}

func displayLog(a *Agent) []string {
	var out []string
	for _, e := range a.Store.Events(100) {
		if e.Kind == "display" {
			out = append(out, e.Message)
		}
	}
	return out
}

// Cage starts with the picture as the TV is, whatever it was turned to before: the stored orientation is applied
// once the kiosk is up after boot (the agent starts first, when there's nothing to turn yet), and again for every
// kiosk that starts after it (a hung browser restarted, a crash). Not on every check in between.
func TestPictureIsTurnedForEveryBrowserThatStarts(t *testing.T) {
	rot := &fakeRotator{}
	_, kioskUp := portraitDongle(t, rot)

	time.Sleep(150 * time.Millisecond)
	if got := rot.turns(); len(got) != 0 {
		t.Fatalf("nothing can be turned before the kiosk is up: %q", got)
	}
	kioskUp("first")
	waitFor(t, "the picture turned once the browser answers", func() bool { return len(rot.turns()) > 0 })
	time.Sleep(300 * time.Millisecond) // 15 checks
	if got := rot.turns(); len(got) != 1 || got[0] != "portrait" {
		t.Fatalf("turned to the stored orientation once per browser, got %q", got)
	}
	kioskUp("second") // the kiosk was started again: Cage is back in landscape
	waitFor(t, "the picture turned again for the new browser", func() bool { return len(rot.turns()) > 1 })
	time.Sleep(300 * time.Millisecond)
	if got := rot.turns(); len(got) != 2 || got[1] != "portrait" {
		t.Errorf("turned once more for the new browser, got %q", got)
	}
}

// If turning the picture is what stops the kiosk (a Cage that crashes on it), turning it for every browser that
// starts would leave the TV with no browser at all: after two in a row the next one is left as it is, and said so.
func TestPictureIsLeftAloneWhenTurningItStopsTheBrowser(t *testing.T) {
	rot := &fakeRotator{}
	a, kioskUp := portraitDongle(t, rot)
	crashes := true
	rot.onTurn = func() { // called with rot.mu held
		if crashes {
			kioskUp("after-turn-" + string(rune('0'+len(rot.turned))))
		}
	}
	kioskUp("first")
	waitFor(t, "the log to say so", func() bool { return len(displayLog(a)) > 0 })
	time.Sleep(300 * time.Millisecond)
	if got := rot.turns(); len(got) != 2 {
		t.Fatalf("two browsers lost to turning the picture are enough, turned %d times", len(got))
	}
	if log := displayLog(a); len(log) != 1 || !strings.Contains(log[0], "stopped right after the picture was turned") {
		t.Errorf("log: %q", log)
	}
	// A browser that starts later is tried again: the two may have stopped for another reason.
	rot.mu.Lock()
	crashes = false
	rot.mu.Unlock()
	kioskUp("later")
	waitFor(t, "the picture turned for a later browser", func() bool { return len(rot.turns()) == 3 })
}

// A Cage too old to turn the picture is asked once per browser and the log says what's wrong; anything else that
// goes wrong is tried a few times first (the Pi is busy while the kiosk starts).
func TestPictureThatCantBeTurnedIsLogged(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		tries int
		log   string
	}{
		{"old Cage", device.ErrOldCage, 1, "Cage 0.1.5 or newer"},
		{"another failure", errors.New("wlr-randr: exit status 1"), 3, "wlr-randr: exit status 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rot := &fakeRotator{err: c.err}
			a, kioskUp := portraitDongle(t, rot)
			kioskUp("first")
			waitFor(t, "the log to say so", func() bool { return len(displayLog(a)) > 0 })
			time.Sleep(300 * time.Millisecond)
			rot.mu.Lock()
			tries := rot.tries
			rot.mu.Unlock()
			if tries != c.tries {
				t.Errorf("asked %d times, want %d", tries, c.tries)
			}
			if log := displayLog(a); len(log) != 1 || !strings.Contains(log[0], "Couldn't turn the picture") || !strings.Contains(log[0], c.log) {
				t.Errorf("log: %q", log)
			}
		})
	}
}

// What the picture is turned to: Pavilion's orientation while paired, landscape (how setup is read) when not.
func TestTurnPictureFollowsWhatsStored(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	t.Cleanup(func() { st.Close() })
	rot := &fakeRotator{cdp: &fakeCDP{}}
	a := New(Options{Store: st, Net: netmgr.NewFake(), Rotator: rot, Hardware: "TEST"})
	if err := a.turnPicture(context.Background()); err != nil || len(rot.turns()) != 1 || rot.turns()[0] != "landscape" {
		t.Fatalf("a dongle that isn't paired is in landscape: %v %q", err, rot.turns())
	}
	_, _ = st.Update(func(c *store.Config) {
		c.PavilionURL, c.Token, c.Orientation = "http://127.0.0.1:1", "7.s3cret", "portrait_flipped"
	})
	_ = a.turnPicture(context.Background())
	_, _ = st.Update(func(c *store.Config) { c.Token = "" }) // unpaired: the orientation is still stored
	_ = a.turnPicture(context.Background())
	if got := strings.Join(rot.turns(), " "); got != "landscape portrait_flipped landscape" {
		t.Errorf("got %q", got)
	}
}

// A connection to Pavilion that died without a word (the Wi-Fi dropped for a moment) is noticed and replaced:
// otherwise the dongle looks connected while nothing Pavilion sends (a new screen, a command) reaches it.
func TestDeadConnectionToPavilionIsReplaced(t *testing.T) {
	oldEvery, oldTimeout := PingEvery, PingTimeout
	NetCheckEvery, PingEvery, PingTimeout = 50*time.Millisecond, 30*time.Millisecond, 60*time.Millisecond
	var mu sync.Mutex
	connections := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		mu.Lock()
		connections++
		n := connections
		mu.Unlock()
		_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"config","name":"Window TV","store":"Juniper Lane"}`))
		if n == 1 {
			<-r.Context().Done() // the first connection goes dead: open, but nothing reads it or answers its pings
			return
		}
		for { // the next one is alive (reading answers pings)
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	st, _ := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	_, _ = st.Update(func(c *store.Config) { c.PavilionURL, c.Token, c.WifiSSID = server.URL, "7.s3cret", "Juniper-Staff" })
	fake := netmgr.NewFake()
	fake.Delay = time.Millisecond
	fake.Remember("Juniper-Staff")
	a := New(Options{Store: st, Net: fake, ShellURL: "http://127.0.0.1:8080/", Version: "test", Hardware: "TEST"})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { a.Run(ctx); close(stopped) }()
	t.Cleanup(func() {
		cancel()
		<-stopped
		server.CloseClientConnections()
		server.Close()
		st.Close()
		PingEvery, PingTimeout = oldEvery, oldTimeout
	})

	waitFor(t, "a second connection", func() bool { mu.Lock(); defer mu.Unlock(); return connections >= 2 })
	waitFor(t, "connected again", func() bool { return a.State().PavilionOnline })
	time.Sleep(300 * time.Millisecond) // ten pings
	mu.Lock()
	defer mu.Unlock()
	if connections != 2 {
		t.Errorf("a connection that answers is kept, but there were %d", connections)
	}
}

// The signal's number changes with every network check; the TV only shows it as a word. Only a change the TV shows
// has the TV page and the browser check look again.
func TestSignalWobbleDoesntNudgeTheTV(t *testing.T) {
	a := New(Options{Store: nil, Net: netmgr.NewFake(), ShellURL: "http://127.0.0.1:8080/", Hardware: "TEST"})
	changes, stop := a.Subscribe()
	defer stop()
	nudged := func() bool {
		select {
		case <-changes:
			<-a.browserCh
			return true
		default:
			return false
		}
	}
	a.noteNet(netmgr.Status{WifiConnected: true, SSID: "Juniper-Staff", Signal: 82, IP: "192.168.1.42"})
	if !nudged() {
		t.Fatal("joining a network is something the TV shows")
	}
	a.noteNet(netmgr.Status{WifiConnected: true, SSID: "Juniper-Staff", Signal: 77, IP: "192.168.1.42"})
	if nudged() {
		t.Error("the TV was nudged for a signal that's still Strong")
	}
	if a.State().Net.Signal != 77 {
		t.Error("the new signal is kept all the same (Pavilion gets the number)")
	}
	a.noteNet(netmgr.Status{WifiConnected: true, SSID: "Juniper-Staff", Signal: 45, IP: "192.168.1.42"})
	if !nudged() {
		t.Error("Strong to Fair is something the TV shows")
	}
}

// Every page the phone loads marks the setup page as in use, but only the first one changes what the TV shows: the
// rest mustn't make the TV (and the browser check) do anything on a Pi Zero that's busy enough already.
func TestPortalPagesDontNudgeTheTV(t *testing.T) {
	a := New(Options{Store: nil, Net: netmgr.NewFake(), ShellURL: "http://127.0.0.1:8080/", Hardware: "TEST"})
	a.state.Phase = PhasePair
	changes, stop := a.Subscribe()
	defer stop()
	a.PortalSeen()
	if a.State().Phase != PhasePortal {
		t.Fatalf("phase %s, want portal", a.State().Phase)
	}
	<-changes
	<-a.browserCh
	first := a.State().PortalSeen
	time.Sleep(10 * time.Millisecond)
	a.PortalSeen()
	if !a.State().PortalSeen.After(first) {
		t.Error("the second page load wasn't noted")
	}
	select {
	case <-changes:
		t.Error("the TV was nudged though nothing on it changed")
	case <-a.browserCh:
		t.Error("the browser check was nudged though nothing changed")
	default:
	}
}
