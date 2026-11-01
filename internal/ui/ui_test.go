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
