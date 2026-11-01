// Package pavilion talks to Pavilion: pairing (one HTTPS request with the code from the admin's Devices page) and
// the WebSocket the dongle keeps open afterwards, which Pavilion uses to tell it what to show and to send commands.
// Both ends are described in PROTOCOL.md.
package pavilion

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Paths on the Pavilion server.
const (
	PairPath   = "/devices/dongles/pair"
	SocketPath = "/ws/dongle"
)

// Close codes Pavilion uses on the WebSocket.
const (
	CloseUnpaired = 4401 // the token isn't (or is no longer) valid: the dongle was unpaired
)

var hostRe = regexp.MustCompile(`^(?i)[a-z0-9-]+(\.[a-z0-9-]+)*(:\d{1,5})?$`)

// NormalizeBase turns what someone typed ("pavilion.yourplace.com", "https://…/", "http://192.168.1.5:8000") into
// a base URL. Without a scheme it's https; plain http only when typed (a Pavilion on the local network in
// development).
func NormalizeBase(input string) (string, error) {
	s := strings.TrimSpace(input)
	scheme := "https"
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "https://"):
		s = s[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		scheme, s = "http", s[len("http://"):]
	}
	s = strings.TrimRight(s, "/")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if !hostRe.MatchString(s) || (!strings.Contains(s, ".") && !strings.HasPrefix(strings.ToLower(s), "localhost")) {
		return "", errors.New("that doesn't look like an address yet, e.g. pavilion.yourplace.com")
	}
	return scheme + "://" + strings.ToLower(s), nil
}

// Host is the base URL without its scheme, for showing people.
func Host(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return base
	}
	if u.Scheme == "http" {
		return "http://" + u.Host
	}
	return u.Host
}

// Device is what the dongle says about itself when it pairs and in every status message.
type Device struct {
	HardwareID string  `json:"hardware_id"`
	Model      string  `json:"model"`
	Version    string  `json:"version"`
	Resolution string  `json:"resolution,omitempty"`
	Network    Network `json:"network"`
}

// Network is the dongle's connection, as Pavilion shows it on the Devices page.
type Network struct {
	SSID   string `json:"ssid,omitempty"`
	Signal int    `json:"signal,omitempty"`
	IP     string `json:"ip,omitempty"`
	Wired  bool   `json:"wired"`
}

// PairResult is what Pavilion gives a dongle that brought a good code.
type PairResult struct {
	Token  string `json:"token"`
	Dongle struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"dongle"`
	Store struct {
		Name string `json:"name"`
	} `json:"store"`
}

// CodeError: Pavilion answered and didn't take the code (wrong, expired, used, or too many tries).
type CodeError struct {
	Kind    string // bad_code | expired | used | throttled | bad_request
	Message string
}

func (e *CodeError) Error() string { return "pairing code not accepted: " + e.Kind + ": " + e.Message }

// ReachError: Pavilion couldn't be reached or didn't answer like Pavilion (wrong address, DNS, TLS, server down).
type ReachError struct{ Err error }

func (e *ReachError) Error() string { return "couldn't reach Pavilion: " + e.Err.Error() }
func (e *ReachError) Unwrap() error { return e.Err }

// HTTPClient is used for pairing; tests swap it.
var HTTPClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	},
}

// Pair sends the code to Pavilion.
func Pair(ctx context.Context, base, code string, device Device) (PairResult, error) {
	var res PairResult
	body, _ := json.Marshal(struct {
		Code string `json:"code"`
		Device
	}{code, device})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+PairPath, bytes.NewReader(body))
	if err != nil {
		return res, &ReachError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "jester-agent/"+device.Version)
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return res, &ReachError{err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &res); err != nil || res.Token == "" {
			return res, &ReachError{fmt.Errorf("unexpected answer from %s", base)}
		}
		return res, nil
	}
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Error == "" {
		// Not Pavilion's JSON: probably the wrong address (or an old Pavilion without dongles).
		return res, &ReachError{fmt.Errorf("%s answered %d", base, resp.StatusCode)}
	}
	return res, &CodeError{Kind: e.Error, Message: e.Message}
}

// SocketURL is the WebSocket address for a base URL.
func SocketURL(base string) string {
	if strings.HasPrefix(base, "http://") {
		return "ws://" + strings.TrimPrefix(base, "http://") + SocketPath
	}
	return "wss://" + strings.TrimPrefix(base, "https://") + SocketPath
}

// Message is anything on the WebSocket. Which fields are set depends on Type.
type Message struct {
	Type string `json:"type"`

	// config (Pavilion → dongle)
	Name        string  `json:"name,omitempty"`
	Store       string  `json:"store,omitempty"`
	Screen      *Screen `json:"screen,omitempty"`
	Orientation string  `json:"orientation,omitempty"`

	// command (Pavilion → dongle), ack (dongle → Pavilion)
	ID      string `json:"id,omitempty"`
	Command string `json:"command,omitempty"` // identify | restart | reload | change_wifi
	Seconds int    `json:"seconds,omitempty"`
	OK      *bool  `json:"ok,omitempty"`
	Error   string `json:"error,omitempty"`

	// status (dongle → Pavilion)
	*Status `json:",omitempty"`
}

// Screen is what to show.
type Screen struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Status is the dongle's heartbeat.
type Status struct {
	Device
	Showing  string `json:"showing"` // screen | idle | setup | identify
	Uptime   int64  `json:"uptime"`  // seconds since the agent started
	Restarts int    `json:"browser_restarts"`
}

// ErrUnpaired: Pavilion closed the socket because the dongle isn't paired any more.
var ErrUnpaired = errors.New("unpaired by Pavilion")

// Conn is an open WebSocket to Pavilion.
type Conn struct{ ws *websocket.Conn }

// Dial opens the WebSocket with the dongle's token. A 401/403 on the upgrade, or close code 4401, is ErrUnpaired.
func Dial(ctx context.Context, base, token, version string) (*Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("User-Agent", "jester-agent/"+version)
	ws, resp, err := websocket.Dial(ctx, SocketURL(base), &websocket.DialOptions{HTTPHeader: h, HTTPClient: HTTPClient})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return nil, ErrUnpaired
		}
		return nil, err
	}
	ws.SetReadLimit(1 << 20)
	return &Conn{ws: ws}, nil
}

// Read waits for the next message. A 4401 close is ErrUnpaired.
func (c *Conn) Read(ctx context.Context) (Message, error) {
	var m Message
	_, raw, err := c.ws.Read(ctx)
	if err != nil {
		if websocket.CloseStatus(err) == CloseUnpaired {
			return m, ErrUnpaired
		}
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return Message{Type: "invalid"}, nil
	}
	return m, nil
}

// Send writes a message.
func (c *Conn) Send(ctx context.Context, m any) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, raw)
}

// Ping checks the connection is alive (WebSocket ping; Daphne and uvicorn answer it).
func (c *Conn) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.ws.Ping(ctx)
}

// Close closes it.
func (c *Conn) Close() { _ = c.ws.Close(websocket.StatusNormalClosure, "") }
