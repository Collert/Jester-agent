package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeDevTools answers like Chromium's DevTools port: the list of pages, the browser's version, and one command
// per connection on /page (the page) and /browser (the browser itself).
func fakeDevTools(t *testing.T, features map[string]string) *httptest.Server {
	t.Helper()
	answer := func(result func(method string) any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &m)
			reply, _ := json.Marshal(map[string]any{"id": m.ID, "result": result(m.Method)})
			_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"method":"Some.event","params":{}}`))
			_ = ws.Write(r.Context(), websocket.MessageText, reply)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"type": "service_worker", "url": "chrome-extension://x/sw.js", "webSocketDebuggerUrl": "ws://" + r.Host + "/sw"},
			{"type": "page", "url": "https://pavilion.test/screens/menu", "webSocketDebuggerUrl": "ws://" + r.Host + "/page"},
		})
	})
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Browser": "Chrome/156.0.8078.79", "webSocketDebuggerUrl": "ws://" + r.Host + "/browser"})
	})
	mux.HandleFunc("/page", answer(func(string) any {
		return map[string]any{"result": map[string]any{"value": "chrome-error://chromewebdata/"}}
	}))
	mux.HandleFunc("/browser", answer(func(method string) any {
		if method != "SystemInfo.getInfo" {
			t.Errorf("the browser was asked %s", method)
		}
		return map[string]any{"gpu": map[string]any{
			"devices":       []map[string]string{{"vendorString": "Broadcom", "deviceString": "VC4 V3D 2.1"}},
			"featureStatus": features,
		}}
	}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestURLIsTheBrowsersAndProbeIsThePages(t *testing.T) {
	server := fakeDevTools(t, nil)
	b := &Browser{CDP: strings.TrimPrefix(server.URL, "http://")}
	if b.Target() != "" {
		t.Error("no target before the browser has answered")
	}
	url, err := b.URL(context.Background())
	if err != nil || url != "https://pavilion.test/screens/menu" {
		t.Errorf("URL = %q, %v: the page among the targets, as the browser lists it", url, err)
	}
	if !strings.HasSuffix(b.Target(), "/page") {
		t.Errorf("Target = %q", b.Target())
	}
	// The page itself says it's the browser's error page: the address it was sent to never loaded.
	href, err := b.Probe(context.Background())
	if err != nil || !IsErrorPage(href) {
		t.Errorf("Probe = %q, %v", href, err)
	}
	if _, err := (&Browser{}).URL(context.Background()); err != ErrNoCDP {
		t.Errorf("without a DevTools port: %v", err)
	}
}

func TestDescribe(t *testing.T) {
	server := fakeDevTools(t, map[string]string{"gpu_compositing": "enabled", "rasterization": "disabled_software", "webgl": "enabled"})
	b := &Browser{CDP: strings.TrimPrefix(server.URL, "http://")}
	info, err := b.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "Chrome/156.0.8078.79" || info.GPU != "Broadcom VC4 V3D 2.1" || !info.Accelerated() {
		t.Errorf("%+v", info)
	}
	if len(info.Graphics) != 2 || info.Graphics["rasterization"] != "disabled_software" {
		t.Errorf("only what says how pages are drawn is kept: %v", info.Graphics)
	}

	server = fakeDevTools(t, map[string]string{"gpu_compositing": "disabled_software"})
	b = &Browser{CDP: strings.TrimPrefix(server.URL, "http://")}
	if info, err := b.Describe(context.Background()); err != nil || info.Accelerated() {
		t.Errorf("drawn by the processor: %+v, %v", info, err)
	}
}

// A browser without a DevTools port (cog) is opened and reloaded with commands.
func TestOpenAndReloadByCommand(t *testing.T) {
	dir := t.TempDir()
	said := filepath.Join(dir, "said")
	script := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ctl := script("ctl", `echo "$@" >> `+said)
	b := &Browser{OpenCmd: ctl + " open", ReloadCmd: ctl + " reload"}
	if err := b.Open(context.Background(), "http://127.0.0.1:8080/"); err != nil {
		t.Fatal(err)
	}
	if err := b.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(said)
	if string(raw) != "open http://127.0.0.1:8080/\nreload\n" {
		t.Errorf("the commands got %q", raw)
	}

	// What the command says when it fails is the error.
	broken := &Browser{OpenCmd: script("broken", "echo no such bus >&2; exit 3")}
	if err := broken.Open(context.Background(), "http://x/"); err == nil || !strings.Contains(err.Error(), "no such bus") {
		t.Errorf("a failing command: %v", err)
	}
	if err := (&Browser{}).Open(context.Background(), "http://x/"); err != ErrNoOpen {
		t.Errorf("no way to open: %v", err)
	}

	// A page that can't be reloaded has its browser restarted instead.
	restarted := filepath.Join(dir, "restarted")
	b = &Browser{ReloadCmd: script("noreload", "exit 1"), RestartCmd: script("restart", "echo again >> "+restarted)}
	if err := b.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(restarted); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(restarted); err != nil || b.Restarts() != 1 {
		t.Errorf("the browser should have been restarted (%v, %d restarts)", err, b.Restarts())
	}
}
