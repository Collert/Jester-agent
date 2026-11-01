// Package store keeps the dongle's state in a SQLite database: its configuration (the Pavilion it pairs with, the
// Wi-Fi it uses, its pairing token, what Pavilion told it to show) and a short log of what happened (joins, failures,
// crashes), so a restart or a power cut picks up where it left off.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go: cross-compiles for the Pi without cgo
)

// Screen is what Pavilion asked the dongle to show.
type Screen struct {
	Name string `json:"name"`
	URL  string `json:"url"` // a launch link: opening it signs the browser in as this dongle, then shows the screen
}

// Config is everything the dongle remembers. The zero value is a dongle fresh out of the box.
type Config struct {
	// What the person typed on their phone. Kept after a failure so they don't type it again.
	PavilionURL  string `json:"pavilion_url"` // e.g. https://pavilion.juniperlane.ca
	PairCode     string `json:"pair_code"`    // cleared once Pavilion accepts it
	WifiSSID     string `json:"wifi_ssid"`
	WifiPassword string `json:"wifi_password"` // or the 64-hex-digit key Raspberry Pi Imager saved (netmgr.HexPSK)
	WifiHidden   bool   `json:"wifi_hidden"`   // typed in as "Other network…"
	Wired        bool   `json:"wired"`         // uses the Ethernet cable, no Wi-Fi

	// What pairing gave it.
	DongleID  int       `json:"dongle_id"`
	Token     string    `json:"token"` // "<id>.<secret>": the WebSocket's credentials
	Name      string    `json:"name"`  // e.g. "Front counter TV"
	StoreName string    `json:"store_name"`
	PairedAt  time.Time `json:"paired_at"`

	// What Pavilion last said to show (kept so the TV shows it again after a reboot, even before Pavilion answers).
	Screen      *Screen `json:"screen"`
	Orientation string  `json:"orientation"` // landscape | portrait | portrait_flipped

	// The setup Wi-Fi's password, made once so a sticker on the dongle stays right.
	HotspotPassword string `json:"hotspot_password"`
}

// Paired: Pavilion accepted a code and gave it a token.
func (c Config) Paired() bool { return c.Token != "" && c.PavilionURL != "" }

// HasNetwork: it knows which network to use.
func (c Config) HasNetwork() bool { return c.Wired || c.WifiSSID != "" }

// Event is one line of the dongle's log.
type Event struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}

// Store is the database. Safe for concurrent use.
type Store struct {
	db  *sql.DB
	mu  sync.Mutex
	cfg Config
}

const keepEvents = 500

// Open opens (or makes) the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %w", err)
		}
	}
	_ = os.Chmod(path, 0o600) // holds the Wi-Fi password and the pairing token
	s := &Store{db: db}
	var raw string
	err = db.QueryRow(`SELECT value FROM kv WHERE key = 'config'`).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		db.Close()
		return nil, err
	default:
		if err := json.Unmarshal([]byte(raw), &s.cfg); err != nil {
			// A broken row shouldn't brick the dongle: start fresh, but say so.
			s.cfg = Config{}
			s.Log("store", "Config was unreadable and was reset: "+err.Error())
		}
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Config returns a copy of the configuration.
func (s *Store) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	if c.Screen != nil {
		sc := *c.Screen
		c.Screen = &sc
	}
	return c
}

// Update changes the configuration and saves it before returning.
func (s *Store) Update(change func(*Config)) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg
	change(&next)
	raw, err := json.Marshal(next)
	if err != nil {
		return s.cfg, err
	}
	if _, err := s.db.Exec(`INSERT INTO kv (key, value, updated_at) VALUES ('config', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, string(raw), time.Now().Unix()); err != nil {
		return s.cfg, err
	}
	s.cfg = next
	return next, nil
}

// Log adds a line to the log (and drops the oldest past keepEvents).
func (s *Store) Log(kind, message string) {
	if _, err := s.db.Exec(`INSERT INTO events (at, kind, message) VALUES (?, ?, ?)`, time.Now().UnixMilli(), kind, message); err != nil {
		return
	}
	_, _ = s.db.Exec(`DELETE FROM events WHERE id <= (SELECT MAX(id) FROM events) - ?`, keepEvents)
}

// Events returns the newest n log lines, newest first.
func (s *Store) Events(n int) []Event {
	rows, err := s.db.Query(`SELECT at, kind, message FROM events ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var at int64
		var e Event
		if rows.Scan(&at, &e.Kind, &e.Message) == nil {
			e.At = time.UnixMilli(at)
			out = append(out, e)
		}
	}
	return out
}

// Counter bumps a named counter (e.g. browser crashes) and returns the new value.
func (s *Store) Counter(name string) int {
	var n int
	_ = s.db.QueryRow(`INSERT INTO kv (key, value, updated_at) VALUES (?, '1', ?)
		ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + 1, updated_at = excluded.updated_at
		RETURNING CAST(value AS INTEGER)`, "count:"+name, time.Now().Unix()).Scan(&n)
	return n
}
