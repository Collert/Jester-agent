package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

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
	mux.HandleFunc("/dongles/pair", func(w http.ResponseWriter, r *http.Request) {
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
			"screen": map[string]any{"name": "Specials", "url": "/dongles/open/abc"}})
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
	if c.Screen == nil || c.Screen.URL != server.URL+"/dongles/open/abc" {
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
