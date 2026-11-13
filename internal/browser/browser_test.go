package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
