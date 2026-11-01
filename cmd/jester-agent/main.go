// jester-agent runs a Pavilion visual dongle: a Raspberry Pi behind a TV that shows one of Pavilion's screens.
//
// It serves the TV's pages on 127.0.0.1:8080 (the kiosk browser's start page), the setup page on the setup Wi-Fi
// (port 80, a captive portal), keeps the kiosk browser running, and keeps a WebSocket open to the Pavilion it's
// paired with. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/collert/jester-agent/internal/agent"
	"github.com/collert/jester-agent/internal/browser"
	"github.com/collert/jester-agent/internal/device"
	"github.com/collert/jester-agent/internal/netmgr"
	"github.com/collert/jester-agent/internal/store"
	"github.com/collert/jester-agent/internal/ui"
)

// version is set at build time: go build -ldflags "-X main.version=1.0.0"
var version = "0.1.0-dev"

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	var (
		dbPath      = flag.String("db", env("JESTER_DB", "/var/lib/jester/agent.db"), "the SQLite database with the dongle's state")
		listen      = flag.String("listen", env("JESTER_LISTEN", "127.0.0.1:8080"), "where the TV's pages are served (the kiosk browser's start page)")
		portal      = flag.String("portal", env("JESTER_PORTAL", ":80"), "where the setup page is served on the setup Wi-Fi (\"\" to turn off)")
		wifiIf      = flag.String("wifi", env("JESTER_WIFI", "wlan0"), "the Wi-Fi interface")
		ethIf       = flag.String("ethernet", env("JESTER_ETHERNET", "eth0"), "the Ethernet interface (\"\" to ignore it)")
		fake        = flag.Bool("fake-network", os.Getenv("JESTER_FAKE_NETWORK") == "1", "pretend to manage the network (for trying the agent on a laptop)")
		browserCmd  = flag.String("browser", os.Getenv("JESTER_BROWSER"), "the kiosk browser's command line, started and kept running by the agent (\"\" if something else runs it)")
		browserRst  = flag.String("browser-restart", os.Getenv("JESTER_BROWSER_RESTART"), "restarts a browser something else runs, when it hangs (e.g. \"systemctl restart jester-kiosk\")")
		wayland     = flag.String("wayland-runtime", env("XDG_RUNTIME_DIR", "/run/jester"), "the XDG_RUNTIME_DIR holding Cage's Wayland socket (for turning the picture)")
		cdp         = flag.String("cdp", env("JESTER_CDP", "127.0.0.1:9222"), "the browser's DevTools address, for steering it (\"\" if it can't be steered)")
		rebootCmd   = flag.String("reboot", env("JESTER_REBOOT", "systemctl reboot"), "what Pavilion's Restart runs (\"\" restarts the browser only)")
		rotate      = flag.Bool("rotate", os.Getenv("JESTER_ROTATE") != "0", "turn the picture with wlr-randr when Pavilion says portrait")
		hardwareID  = flag.String("id", os.Getenv("JESTER_ID"), "the dongle's short ID (default: from the Pi's serial number)")
		devPortal   = flag.Bool("dev-portal", os.Getenv("JESTER_DEV_PORTAL") == "1", "also serve the setup page at /portal/ on the TV's address (for trying it on a laptop)")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVersion {
		os.Stdout.WriteString(version + "\n")
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("Can't open the database %s: %v", *dbPath, err)
	}
	defer st.Close()
	if n := st.Counter("starts"); n > 1 {
		st.Log("agent", "Started again (start number "+strconv.Itoa(n)+")")
	}

	var nm netmgr.Manager
	if *fake {
		f := netmgr.NewFake()
		if cfg := st.Config(); cfg.WifiSSID != "" {
			f.Remember(cfg.WifiSSID)
		}
		// JESTER_FAKE_IMAGER_WIFI=ssid:password pretends Raspberry Pi Imager saved that Wi-Fi (a 64-hex-digit
		// password is a key, as Imager saves it).
		if ssid, psk, ok := strings.Cut(os.Getenv("JESTER_FAKE_IMAGER_WIFI"), ":"); ok {
			f.Preconfigure(netmgr.Preconfigured{SSID: ssid, PSK: psk})
		}
		nm = f
	} else {
		nm = netmgr.NewNM(*wifiIf, *ethIf)
	}

	// Cage keeps its Wayland socket in XDG_RUNTIME_DIR; wlr-randr needs it too.
	runtimeDir := *wayland
	if *browserCmd != "" {
		_ = os.MkdirAll(runtimeDir, 0o700)
	}
	browserEnv := []string{"XDG_RUNTIME_DIR=" + runtimeDir}
	br := &browser.Browser{Command: strings.Fields(*browserCmd), Env: browserEnv, CDP: *cdp, RestartCmd: *browserRst}

	shell := "http://" + *listen + "/"
	if host, port, err := net.SplitHostPort(*listen); err == nil && (host == "" || host == "0.0.0.0") {
		shell = "http://127.0.0.1:" + port + "/"
	}
	opts := agent.Options{Store: st, Net: nm, Browser: br, ShellURL: shell, Version: version, Reboot: *rebootCmd, Hardware: *hardwareID}
	if *rotate && !*fake {
		opts.Rotator = device.Rotator{Env: append(browserEnv, "WAYLAND_DISPLAY="+env("WAYLAND_DISPLAY", "wayland-0"))}
	}
	if *fake && *rebootCmd == "systemctl reboot" {
		opts.Reboot = ""
	}
	a := agent.New(opts)

	server := ui.New(a)
	server.NoCDP = *cdp == ""
	server.DevPortal = *devPortal || *fake

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serve := func(addr string, h http.Handler, what string) *http.Server {
		srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			for ctx.Err() == nil {
				err := srv.ListenAndServe()
				if errors.Is(err, http.ErrServerClosed) {
					return
				}
				log.Printf("[ui] %s on %s stopped: %v; trying again", what, addr, err)
				time.Sleep(3 * time.Second)
			}
		}()
		return srv
	}
	servers := []*http.Server{serve(*listen, server.TVHandler(), "TV pages")}
	if *portal != "" {
		servers = append(servers, serve(*portal, server.PortalHandler(), "setup page"))
	}
	log.Printf("[agent] TV pages on http://%s/", *listen)
	if server.DevPortal {
		log.Printf("[agent] setup page also on http://%s/portal/", *listen)
	}

	a.Run(ctx)

	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdown)
	}
}
