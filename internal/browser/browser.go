// Package browser keeps the TV's browser running and steers it. The agent starts the kiosk browser (by default Cage
// running Chromium in kiosk mode) and restarts it whenever it exits or stops answering. It moves the browser between
// the agent's own pages (http://127.0.0.1:8080: setup, "No screen assigned", errors) and the Pavilion screen it was
// told to show through the Chrome DevTools Protocol on a local port.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Browser supervises one browser process.
type Browser struct {
	Command []string // e.g. cage -s -- chromium --kiosk --remote-debugging-port=9222 … http://127.0.0.1:8080/
	Env     []string // extra environment (e.g. XDG_RUNTIME_DIR for Cage)
	CDP     string   // host:port of the DevTools endpoint; "" when the browser can't be steered
	// RestartCmd restarts a browser something else runs (e.g. "systemctl restart jester-kiosk"), when Command is "".
	RestartCmd string
	Logf       func(kind, format string, args ...any)

	mu       sync.Mutex
	proc     *os.Process
	restarts atomic.Int32
	started  atomic.Int64 // unix ms when the current process started
	cdpID    atomic.Int64
}

func (b *Browser) logf(kind, format string, args ...any) {
	if b.Logf != nil {
		b.Logf(kind, format, args...)
	}
}

// Restarts is how many times the browser had to be started again since the agent started.
func (b *Browser) Restarts() int { return int(b.restarts.Load()) }

// Supervised: the agent runs the browser (not just steers one someone else started).
func (b *Browser) Supervised() bool { return len(b.Command) > 0 }

// Run keeps the browser running until ctx ends: when it exits it's started again, waiting longer each time it
// fails quickly (1 s up to 30 s; back to 1 s once it has run for a minute).
func (b *Browser) Run(ctx context.Context) {
	if !b.Supervised() {
		<-ctx.Done()
		return
	}
	wait := time.Second
	for ctx.Err() == nil {
		cmd := exec.Command(b.Command[0], b.Command[1:]...)
		cmd.Env = append(os.Environ(), b.Env...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		start := time.Now()
		if err := cmd.Start(); err != nil {
			b.logf("browser", "Couldn't start the browser: %v", err)
		} else {
			b.mu.Lock()
			b.proc = cmd.Process
			b.mu.Unlock()
			b.started.Store(start.UnixMilli())
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if ctx.Err() == nil {
					b.restarts.Add(1)
					b.logf("browser", "The browser stopped (%v); starting it again", err)
				}
			case <-ctx.Done():
				_ = cmd.Process.Signal(os.Interrupt)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					_ = cmd.Process.Kill()
				}
			}
			b.mu.Lock()
			b.proc = nil
			b.mu.Unlock()
		}
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		if wait *= 2; wait > 30*time.Second {
			wait = 30 * time.Second
		}
	}
}

// CanRestart: the agent can restart the browser (it runs it, or knows the command that does).
func (b *Browser) CanRestart() bool { return b.Supervised() || b.RestartCmd != "" }

// Restart stops the browser; Run (or whatever runs it) starts it again on its start page: the agent's.
func (b *Browser) Restart() {
	if !b.Supervised() {
		if f := strings.Fields(b.RestartCmd); len(f) > 0 {
			b.restarts.Add(1)
			b.started.Store(time.Now().UnixMilli())
			go func() {
				if err := exec.Command(f[0], f[1:]...).Run(); err != nil {
					b.logf("browser", "Restarting the browser failed: %v", err)
				}
			}()
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.proc != nil {
		_ = b.proc.Kill()
	}
}

// Uptime is how long the current browser has been running (for one the agent doesn't run: since the agent started
// or last restarted it).
func (b *Browser) Uptime() time.Duration {
	ms := b.started.Load()
	if ms == 0 {
		b.started.CompareAndSwap(0, time.Now().UnixMilli())
		return 0
	}
	return time.Since(time.UnixMilli(ms))
}

// ErrNoCDP: the browser can't be steered (no DevTools port, or it isn't up yet).
var ErrNoCDP = errors.New("browser: DevTools not available")

type target struct {
	Type string `json:"type"`
	URL  string `json:"url"`
	WS   string `json:"webSocketDebuggerUrl"`
}

func (b *Browser) page(ctx context.Context) (target, error) {
	if b.CDP == "" {
		return target{}, ErrNoCDP
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+b.CDP+"/json/list", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return target{}, fmt.Errorf("%w: %v", ErrNoCDP, err)
	}
	defer resp.Body.Close()
	var targets []target
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return target{}, fmt.Errorf("%w: %v", ErrNoCDP, err)
	}
	for _, t := range targets {
		if t.Type == "page" && t.WS != "" {
			return t, nil
		}
	}
	return target{}, fmt.Errorf("%w: no page", ErrNoCDP)
}

// call sends one DevTools command to the page and waits for its answer.
func (b *Browser) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	t, err := b.page(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, t.WS, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoCDP, err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	ws.SetReadLimit(4 << 20)
	id := b.cdpID.Add(1)
	raw, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := ws.Write(ctx, websocket.MessageText, raw); err != nil {
		return nil, err
	}
	for {
		_, msg, err := ws.Read(ctx)
		if err != nil {
			return nil, err
		}
		var reply struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &reply) != nil || reply.ID != id {
			continue // an event, not our answer
		}
		if reply.Error != nil {
			return nil, errors.New("browser: " + reply.Error.Message)
		}
		return reply.Result, nil
	}
}

// Navigate opens url in the browser.
func (b *Browser) Navigate(ctx context.Context, url string) error {
	_, err := b.call(ctx, "Page.navigate", map[string]any{"url": url})
	return err
}

// Reload reloads the page, or restarts the browser when it can't be steered.
func (b *Browser) Reload(ctx context.Context) error {
	if _, err := b.call(ctx, "Page.reload", map[string]any{"ignoreCache": true}); err != nil {
		if b.CanRestart() {
			b.Restart()
			return nil
		}
		return err
	}
	return nil
}

// Probe asks the page something trivial and returns its address: an error means the browser (or its tab) is hung
// or crashed.
func (b *Browser) Probe(ctx context.Context) (string, error) {
	t, err := b.page(ctx)
	if err != nil {
		return "", err
	}
	res, err := b.call(ctx, "Runtime.evaluate", map[string]any{"expression": "location.href", "returnByValue": true})
	if err != nil {
		return t.URL, err
	}
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	_ = json.Unmarshal(res, &out)
	if out.Result.Value != "" {
		return out.Result.Value, nil
	}
	return t.URL, nil
}

// IsErrorPage: the browser is showing its own "can't reach this page" (or a crashed tab).
func IsErrorPage(url string) bool {
	return strings.HasPrefix(url, "chrome-error://") || strings.HasPrefix(url, "about:neterror") || url == "about:blank#blocked"
}
