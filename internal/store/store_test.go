package store

import (
	"path/filepath"
	"testing"
)

func TestConfigSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config().Paired() {
		t.Fatal("a new dongle isn't paired")
	}
	if _, err := s.Update(func(c *Config) {
		c.PavilionURL, c.Token, c.WifiSSID = "https://p.example.com", "4.secret", "Cafe"
		c.Screen = &Screen{Name: "Board", URL: "https://p.example.com/dongles/open/x"}
	}); err != nil {
		t.Fatal(err)
	}
	s.Log("test", "hello")
	if n := s.Counter("starts"); n != 1 {
		t.Errorf("counter = %d", n)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Config()
	if !c.Paired() || c.WifiSSID != "Cafe" || c.Screen == nil || c.Screen.Name != "Board" {
		t.Errorf("config after reopening: %+v", c)
	}
	c.Screen.Name = "changed"
	if s.Config().Screen.Name != "Board" {
		t.Error("Config() must return a copy")
	}
	if ev := s.Events(5); len(ev) != 1 || ev[0].Message != "hello" {
		t.Errorf("events: %+v", ev)
	}
	if n := s.Counter("starts"); n != 2 {
		t.Errorf("counter after reopen = %d", n)
	}
}
