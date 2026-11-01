// Package ui serves the dongle's pages: the TV's (on 127.0.0.1, what the kiosk browser opens) and the setup page a
// phone gets on the setup Wi-Fi (a captive portal: every address on the hotspot leads to it). Pages are rendered on
// the server; the TV page refreshes itself over Server-Sent Events whenever the agent's state changes.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/collert/jester-agent/internal/agent"
	"github.com/collert/jester-agent/internal/netmgr"
	"github.com/collert/jester-agent/internal/pavilion"
	"github.com/collert/jester-agent/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Server serves both sets of pages.
type Server struct {
	Agent *agent.Agent
	NoCDP bool // the browser can't be steered: the TV page takes itself to the screen
	// DevPortal also serves the setup page on the TV's listener at /portal/ (for trying it on a laptop).
	DevPortal bool

	tv, portal *template.Template
}

// New parses the templates.
func New(a *agent.Agent) *Server {
	funcs := template.FuncMap{
		"bars": func(n int) []bool { return []bool{n >= 1, n >= 2, n >= 3, n >= 4} },
	}
	s := &Server{Agent: a}
	s.tv = template.Must(template.New("tv.html").Funcs(funcs).ParseFS(templateFS, "templates/tv.html"))
	s.portal = template.Must(template.New("portal.html").Funcs(funcs).ParseFS(templateFS, "templates/portal.html"))
	return s
}

func static() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServer(http.FS(sub))
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	}))
}

// ---------- TV ----------

// TVHandler is the kiosk browser's side: /, /events, /state.
func (s *Server) TVHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", static())
	mux.HandleFunc("/events", s.events)
	mux.HandleFunc("/state", s.stateJSON)
	if s.DevPortal {
		mux.Handle("/portal/", http.StripPrefix("/portal", s.portalMux("/portal")))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		name := "tv.html"
		if r.URL.Query().Get("partial") == "1" {
			name = "app"
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.tv.ExecuteTemplate(w, name, s.tvView()); err != nil {
			log.Printf("[ui] tv page: %v", err)
		}
	})
	return mux
}

// events nudges the TV page on every change (and every 15 s, so a dead connection is noticed).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	changes, stop := s.Agent.Subscribe()
	defer stop()
	fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changes:
			fmt.Fprintf(w, "data: %s\n\n", s.Agent.State().Phase)
		case <-ping.C:
			fmt.Fprint(w, "event: ping\ndata: \n\n")
		}
		flusher.Flush()
	}
}

// stateJSON is for looking at a dongle from a laptop on the same network (ssh -L 8080:127.0.0.1:8080): no secrets.
func (s *Server) stateJSON(w http.ResponseWriter, r *http.Request) {
	cfg := s.Agent.Store.Config()
	out := map[string]any{
		"state": s.Agent.State(), "hardware_id": s.Agent.Hardware, "version": s.Agent.Version,
		"uptime_seconds": int(s.Agent.Uptime().Seconds()),
		"config": map[string]any{"pavilion": cfg.PavilionURL, "paired": cfg.Paired(), "dongle_id": cfg.DongleID,
			"name": cfg.Name, "store": cfg.StoreName, "wifi": cfg.WifiSSID, "wired": cfg.Wired, "screen": cfg.Screen,
			"orientation": cfg.Orientation, "paired_at": cfg.PairedAt},
		"log": s.Agent.Store.Events(30),
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// ---------- setup page (captive portal) ----------

// PortalHandler is the setup Wi-Fi's side. Any address that isn't the dongle's (a phone checking for a captive
// portal, or someone typing a website) is sent to http://10.42.0.1/, which is what makes phones open it by
// themselves. It only answers phones on the setup Wi-Fi, and only while that's on.
func (s *Server) PortalHandler() http.Handler {
	inner := s.portalMux("")
	_, hotspot, _ := net.ParseCIDR(netmgr.HotspotIP + "/24")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if remote := net.ParseIP(ip); remote == nil || !hotspot.Contains(remote) {
			http.Error(w, "Setup is only available on the dongle’s setup Wi-Fi.", http.StatusForbidden)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != netmgr.HotspotIP {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "http://"+netmgr.HotspotIP+"/", http.StatusFound)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

type netView struct {
	SSID     string
	Bars     int
	Secure   bool
	On       bool
	SigLabel string
}

type portalView struct {
	Base       string
	Step       string // pav | wifi | sent | off
	Hardware   string
	Hotspot    string
	Paired     bool
	DongleName string
	StoreName  string

	Domain, Code        string
	DomainErr, CodeErr  string
	Banner              string
	Nets                []netView
	Sel                 string // the picked network, "__other__" or "__wired__"
	OtherSSID, Password string
	WifiErr             string
	CanWired            bool
	Summary             [][2]string
	Joining             string // the network it's about to join, on the "Watch the TV" page
}

var codeRe = regexp.MustCompile(`^\d{6}$`)

func (s *Server) portalMux(base string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", static())
	render := func(w http.ResponseWriter, v portalView) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.portal.Execute(w, v); err != nil {
			log.Printf("[ui] portal page: %v", err)
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Redirect(w, r, base+"/", http.StatusFound)
			return
		}
		if !s.Agent.HotspotOn() {
			render(w, s.portalView(base, "off"))
			return
		}
		s.Agent.PortalSeen()
		cfg := s.Agent.Store.Config()
		if r.Method == http.MethodPost { // step 1: the Pavilion address and code
			v := s.portalView(base, "pav")
			v.Domain = strings.TrimSpace(r.FormValue("domain"))
			v.Code = strings.ReplaceAll(strings.TrimSpace(r.FormValue("code")), " ", "")
			baseURL, err := pavilion.NormalizeBase(v.Domain)
			if err != nil {
				v.DomainErr = "That doesn’t look like an address yet, e.g. pavilion.yourplace.com"
			}
			if !codeRe.MatchString(v.Code) {
				v.CodeErr = "The pairing code is 6 digits."
			}
			if v.DomainErr != "" || v.CodeErr != "" {
				v.Banner = ""
				render(w, v)
				return
			}
			_, _ = s.Agent.Store.Update(func(c *store.Config) { c.PavilionURL, c.PairCode = baseURL, v.Code })
			http.Redirect(w, r, base+"/wifi", http.StatusSeeOther)
			return
		}
		if cfg.Paired() || s.Agent.State().Phase == agent.PhaseWifiFail {
			http.Redirect(w, r, base+"/wifi", http.StatusFound)
			return
		}
		render(w, s.portalView(base, "pav"))
	})
	mux.HandleFunc("/pavilion", func(w http.ResponseWriter, r *http.Request) {
		v := s.portalView(base, "pav")
		if !s.Agent.HotspotOn() {
			v.Step = "off"
		}
		s.Agent.PortalSeen()
		render(w, v) // "Back" from the Wi-Fi step, even after a Wi-Fi failure
	})
	mux.HandleFunc("/wifi", func(w http.ResponseWriter, r *http.Request) {
		if !s.Agent.HotspotOn() {
			render(w, s.portalView(base, "off"))
			return
		}
		s.Agent.PortalSeen()
		cfg := s.Agent.Store.Config()
		if !cfg.Paired() && (cfg.PavilionURL == "" || cfg.PairCode == "") {
			http.Redirect(w, r, base+"/", http.StatusFound)
			return
		}
		if r.Method != http.MethodPost {
			render(w, s.portalView(base, "wifi"))
			return
		}
		if r.FormValue("op") == "scan" {
			s.Agent.Scan(r.Context(), true)
			http.Redirect(w, r, base+"/wifi", http.StatusSeeOther)
			return
		}
		v := s.portalView(base, "wifi")
		v.Banner = ""
		v.Sel = r.FormValue("ssid")
		v.Password = r.FormValue("password")
		v.OtherSSID = strings.TrimSpace(r.FormValue("other_ssid"))
		req := agent.SetupRequest{Password: v.Password}
		switch v.Sel {
		case "__wired__":
			req = agent.SetupRequest{Wired: true}
		case "__other__":
			req.SSID, req.Hidden = v.OtherSSID, true
			if req.SSID == "" {
				v.WifiErr = "Type the network’s name."
			}
		case "":
			v.WifiErr = "Pick the Wi-Fi this TV will use."
		default:
			req.SSID = v.Sel
			for _, n := range v.Nets {
				if n.SSID == v.Sel && n.Secure && v.Password == "" {
					v.WifiErr = "This network needs a password."
				}
			}
		}
		if v.WifiErr == "" && !req.Wired && v.Password != "" && (len(v.Password) < 8 || len(v.Password) > 63) {
			v.WifiErr = "Wi-Fi passwords are 8 to 63 characters."
		}
		if v.WifiErr != "" {
			render(w, v)
			return
		}
		if !cfg.Paired() {
			req.PavilionURL, req.Code = cfg.PavilionURL, cfg.PairCode
		}
		if !s.Agent.Submit(req) {
			v.WifiErr = "The dongle is busy trying the last details. Watch the TV."
			render(w, v)
			return
		}
		sent := s.portalView(base, "sent")
		net := req.SSID
		if req.Wired {
			net = "Ethernet cable"
		}
		sent.Summary = [][2]string{{"Pavilion", pavilion.Host(cfg.PavilionURL)}, {"Code", spaced(cfg.PairCode)}, {"Wi-Fi", net}}
		sent.Joining = net
		if cfg.Paired() {
			sent.Summary = [][2]string{{"Screen", cfg.Name}, {"Wi-Fi", net}}
		}
		render(w, sent)
	})
	return mux
}

func spaced(code string) string {
	if len(code) == 6 {
		return code[:3] + " " + code[3:]
	}
	return code
}

func (s *Server) portalView(base, step string) portalView {
	cfg := s.Agent.Store.Config()
	st := s.Agent.State()
	v := portalView{Base: base, Step: step, Hardware: s.Agent.Hardware, Hotspot: s.Agent.HotspotSSID(), Paired: cfg.Paired(),
		DongleName: cfg.Name, StoreName: cfg.StoreName, Code: cfg.PairCode, Sel: cfg.WifiSSID, Password: cfg.WifiPassword}
	if cfg.PavilionURL != "" {
		v.Domain = pavilion.Host(cfg.PavilionURL)
	}
	if cfg.Wired {
		v.Sel = "__wired__"
	}
	if step == "wifi" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		found := false
		for _, n := range s.Agent.Scan(ctx, false) {
			on := n.SSID == v.Sel
			found = found || on
			v.Nets = append(v.Nets, netView{SSID: n.SSID, Bars: n.Bars(), Secure: n.Secure, On: on,
				SigLabel: netmgr.SignalWord(n.Signal) + " signal"})
		}
		if v.Sel != "" && !found && v.Sel != "__wired__" {
			v.OtherSSID, v.Sel = v.Sel, "__other__"
		}
		v.CanWired = s.Agent.Net.Status(ctx).Wired
	}
	switch st.Phase {
	case agent.PhaseWifiFail:
		if step == "wifi" {
			net := cfg.WifiSSID
			if cfg.Wired {
				net = "the cable"
			}
			v.Banner = "Last try couldn’t join " + net + ". " + st.Detail + " Check it, or pick another network."
		}
	case agent.PhaseCodeFail:
		if step == "pav" {
			v.Banner = "Pavilion didn’t take the code. " + st.Detail + " Make a new one in Settings › Devices."
			v.Code = ""
		}
	case agent.PhaseReachFail:
		if step == "pav" {
			v.Banner = "Nothing answered at " + v.Domain + ". Check the address: it’s the one you open Pavilion at."
		}
	}
	return v
}
