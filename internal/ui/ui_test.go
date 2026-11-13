package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collert/jester-agent/internal/agent"
	"github.com/collert/jester-agent/internal/netmgr"
	"github.com/collert/jester-agent/internal/store"
)

func setup(t *testing.T) (*Server, *agent.Agent, *store.Store) {
	t.Helper()
	return setupWith(t, netmgr.NewFake())
}

func setupWith(t *testing.T, net *netmgr.Fake) (*Server, *agent.Agent, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := agent.New(agent.Options{Store: st, Net: net, ShellURL: "http://127.0.0.1:8080/", Version: "test", Hardware: "TEST"})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for a.State().Phase != agent.PhasePair && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return New(a), a, st
}

func onHotspot(method, target string, form url.Values) *http.Request {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.RemoteAddr = "10.42.0.23:51000"
	return r
}

func TestPortalIsACaptivePortal(t *testing.T) {
	s, _, _ := setup(t)
	h := s.PortalHandler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, onHotspot("GET", "http://connectivitycheck.gstatic.com/generate_204", nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "http://10.42.0.1/" {
		t.Errorf("a phone's captive-portal check goes to setup: %d %s", w.Code, w.Header().Get("Location"))
	}

	r := httptest.NewRequest("GET", "http://10.42.0.1/", nil)
	r.RemoteAddr = "192.168.1.50:5000" // the restaurant's network, not the setup Wi-Fi
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("setup only answers on the setup Wi-Fi, got %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, onHotspot("GET", "http://10.42.0.1/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Pavilion address") {
		t.Errorf("the setup page: %d", w.Code)
	}
}

func TestPortalChecksTheDetails(t *testing.T) {
	s, a, st := setup(t)
	h := s.PortalHandler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, onHotspot("POST", "http://10.42.0.1/", url.Values{"domain": {"pavilion"}, "code": {"12"}}))
	body := w.Body.String()
	if !strings.Contains(body, "doesn’t look like an address") || !strings.Contains(body, "6 digits") {
		t.Error("bad address and code are explained")
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, onHotspot("POST", "http://10.42.0.1/", url.Values{"domain": {"pavilion.juniperlane.ca"}, "code": {"482913"}}))
	if w.Code != http.StatusSeeOther || st.Config().PavilionURL != "https://pavilion.juniperlane.ca" || st.Config().PairCode != "482913" {
		t.Errorf("good details are kept: %d %+v", w.Code, st.Config())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, onHotspot("POST", "http://10.42.0.1/wifi", url.Values{"ssid": {"Juniper-Staff"}, "op": {"start"}}))
	if !strings.Contains(w.Body.String(), "needs a password") {
		t.Error("a secure network needs a password")
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, onHotspot("POST", "http://10.42.0.1/wifi", url.Values{"ssid": {"Juniper-Staff"}, "password": {"juniper2026"}, "op": {"start"}}))
	if !strings.Contains(w.Body.String(), "Watch the TV") {
		t.Error("good Wi-Fi: watch the TV")
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.State().Phase == agent.PhasePair || a.State().Phase == agent.PhasePortal {
		if time.Now().After(deadline) {
			t.Fatal("the agent didn't start joining")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTVPageRenders(t *testing.T) {
	s, _, _ := setup(t)
	w := httptest.NewRecorder()
	s.TVHandler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Scan to start") || !strings.Contains(w.Body.String(), "Pavilion-Setup-TEST") {
		t.Errorf("TV page: %d", w.Code)
	}
}

func TestWifiQR(t *testing.T) {
	if got := wifiQR(`Café;1`, `pa:ss`); got != `WIFI:T:WPA;S:Café\;1;P:pa\:ss;;` {
		t.Error(got)
	}
	if q := makeQR(wifiQR("Pavilion-Setup-TEST", "lane-4821")); q.Size < 21 || !strings.HasPrefix(q.Path, "M0 0h7") {
		t.Errorf("QR: %d %.20s", q.Size, q.Path)
	}
}

// toWifiStep gives the setup page a Pavilion address and code, so it's on the Wi-Fi step.
func toWifiStep(t *testing.T, h http.Handler) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, onHotspot("POST", "http://10.42.0.1/", url.Values{"domain": {"pavilion.juniperlane.ca"}, "code": {"482913"}}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("Pavilion step: %d", w.Code)
	}
}

func TestImagerWifiIsFilledIn(t *testing.T) {
	f := netmgr.NewFake()
	f.Preconfigure(netmgr.Preconfigured{SSID: "Juniper-Staff", PSK: "juniper2026"})
	s, _, _ := setupWith(t, f)
	h := s.PortalHandler()
	toWifiStep(t, h)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, onHotspot("GET", "http://10.42.0.1/wifi", nil))
	body := w.Body.String()
	if !strings.Contains(body, `value="Juniper-Staff" checked`) || !strings.Contains(body, `value="juniper2026"`) {
		t.Error("the network and password saved with Imager are filled in")
	}
	if strings.Contains(body, "data-saved-key") {
		t.Error("a password Imager kept as typed needs no note")
	}
}

func TestImagerKeyIsKeptUnlessChanged(t *testing.T) {
	key := "8d4f3c9a0b1e2d3f4a5b6c7d8e9f0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b"
	for _, typed := range []string{"", "newpass2026"} {
		f := netmgr.NewFake()
		f.Preconfigure(netmgr.Preconfigured{SSID: "Juniper-Staff", PSK: key})
		s, a, st := setupWith(t, f)
		h := s.PortalHandler()
		toWifiStep(t, h)

		w := httptest.NewRecorder()
		h.ServeHTTP(w, onHotspot("GET", "http://10.42.0.1/wifi", nil))
		body := w.Body.String()
		if strings.Contains(body, key) {
			t.Fatal("Imager's key is never sent to the phone")
		}
		if !strings.Contains(body, `value="Juniper-Staff" checked`) || !strings.Contains(body, "set in Raspberry Pi Imager") {
			t.Error("the network is picked, with a note that Imager set its password")
		}

		w = httptest.NewRecorder()
		h.ServeHTTP(w, onHotspot("POST", "http://10.42.0.1/wifi", url.Values{"ssid": {"Juniper-Staff"}, "password": {typed}, "op": {"start"}}))
		if !strings.Contains(w.Body.String(), "Watch the TV") {
			t.Fatalf("password %q: setup starts", typed)
		}
		want := typed
		if typed == "" {
			want = key
		}
		deadline := time.Now().Add(5 * time.Second)
		for a.State().Phase == agent.PhasePair || a.State().Phase == agent.PhasePortal {
			if time.Now().After(deadline) {
				t.Fatal("the agent didn't start joining")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if got := st.Config().WifiPassword; got != want {
			t.Errorf("password %q: joined with %q, want %q", typed, got, want)
		}
	}
}

// While any CSS animation runs, Chromium redraws the TV 60 times a second, which a Pi Zero can't afford next to a
// screen that's loading (measured: the idle page's drift alone was seven times the rest of the page's work). The TV
// page has no animation without an end; its spinners are turned by tv.js, a few steps a second.
func TestTVPageHasNoEndlessAnimation(t *testing.T) {
	css, err := staticFS.ReadFile("static/tv.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(css), "\n") {
		if strings.Contains(line, "animation") && strings.Contains(line, "infinite") {
			t.Errorf("tv.css animates without end: %s", strings.TrimSpace(line))
		}
	}
	s, _, _ := setup(t)
	w := httptest.NewRecorder()
	if err := s.tv.ExecuteTemplate(w, "app", tvView{Kind: "idle", BrandTop: "Window TV"}); err != nil {
		t.Fatal(err)
	}
	if body := w.Body.String(); !strings.Contains(body, "No screen assigned") || strings.Contains(body, "drift") {
		t.Errorf("the idle page sits still: %s", body)
	}
}

// showing is a paired dongle told to show a screen, with the kind of browser that has no DevTools port (cog).
func showing(t *testing.T) (*Server, *agent.Agent, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, _ = st.Update(func(c *store.Config) {
		c.PavilionURL, c.Token, c.WifiSSID, c.Name = "http://127.0.0.1:1", "7.s3cret", "Juniper-Staff", "Window TV"
		c.Screen = &store.Screen{Name: "Order board", URL: "https://pavilion.test/devices/dongles/open/abc"}
	})
	fake := netmgr.NewFake()
	fake.Delay = time.Millisecond
	fake.Remember("Juniper-Staff")
	a := agent.New(agent.Options{Store: st, Net: fake, ShellURL: "http://127.0.0.1:8080/", Version: "test", Hardware: "TEST", KioskRotates: true})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	t.Cleanup(func() { cancel(); <-stopped })
	go func() { a.Run(ctx); close(stopped) }()
	deadline := time.Now().Add(5 * time.Second)
	for a.State().Phase != agent.PhaseShowing && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s := New(a)
	s.NoCDP = true
	return s, a, st
}

func get(h http.Handler, method, target string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// In cog the TV page takes itself to the screen: the page carries where (data-go) while that's where the browser
// should be. When the page reports that the screen's address doesn't answer, it stays and says so.
func TestTVPageCarriesTheScreenToGoTo(t *testing.T) {
	s, a, _ := showing(t)
	h := s.TVHandler()

	body := get(h, "GET", "/?partial=1").Body.String()
	if !strings.Contains(body, `data-go="https://pavilion.test/devices/dongles/open/abc"`) || !strings.Contains(body, "Opening Order board") {
		t.Fatalf("the page should be sent to the screen: %s", body)
	}
	if w := get(h, "GET", "/screen-failed"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("only the page's POST counts, got %d", w.Code)
	}
	if w := get(h, "POST", "/screen-failed"); w.Code != http.StatusNoContent {
		t.Fatalf("POST /screen-failed: %d", w.Code)
	}
	if a.State().ScreenFailed.IsZero() {
		t.Fatal("the failure wasn't noted")
	}
	body = get(h, "GET", "/?partial=1").Body.String()
	if strings.Contains(body, "data-go") || !strings.Contains(body, "Can’t open Order board") {
		t.Errorf("after a failure the page stays and says so: %s", body)
	}
}

func TestKioskAsksWhichWayUp(t *testing.T) {
	s, _, st := showing(t)
	h := s.TVHandler()
	if got := strings.TrimSpace(get(h, "GET", "/kiosk/rotation").Body.String()); got != "0" {
		t.Errorf("landscape: %q", got)
	}
	_, _ = st.Update(func(c *store.Config) { c.Orientation = "portrait_flipped" })
	if got := strings.TrimSpace(get(h, "GET", "/kiosk/rotation").Body.String()); got != "3" {
		t.Errorf("portrait, turned left, is three quarter turns: %q", got)
	}
}

// The browser keeps the page's script and styles but checks them each time, so a kiosk doesn't run an old agent's
// script after an update.
func TestStaticFilesAreCheckedEachTime(t *testing.T) {
	s, _, _ := setup(t)
	h := s.TVHandler()
	w := get(h, "GET", "/static/tv.js")
	tag := w.Header().Get("ETag")
	if w.Code != http.StatusOK || tag == "" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("tv.js: %d, ETag %q, Cache-Control %q", w.Code, tag, w.Header().Get("Cache-Control"))
	}
	if w := get(h, "GET", "/static/tv.js", "If-None-Match", tag); w.Code != http.StatusNotModified {
		t.Errorf("an unchanged file isn't sent again, got %d", w.Code)
	}
	if other := get(h, "GET", "/static/tv.css").Header().Get("ETag"); other == tag || other == "" {
		t.Errorf("each file has its own tag: %q", other)
	}
}
