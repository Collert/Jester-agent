# CLAUDE.md

jester-agent runs a Pavilion visual dongle (a Raspberry Pi behind a TV). Read README.md for the flow and the package
map, PROTOCOL.md for the contract with Pavilion (its `devices` app; change both sides together).

- Go, no cgo (cross-compiles with `make pi`). Dependencies are kept few: coder/websocket, modernc.org/sqlite,
  skip2/go-qrcode. Keep `go 1.22` in go.mod working.
- Pages are Go templates rendered on the server (`internal/ui/templates`), with small vanilla JS only for what HTML
  can't do (the TV page's live refresh, clock and spinners, the code boxes). No CSS animation that runs without
  end on the TV: Chromium redraws the whole screen 60 times a second while one does, and a Pi Zero can't spare it.
  The designs are the `Dongle*` and `DonglePortal*` artboards in the "Pavilion POS Register" canvas; treat them as
  a visual spec.
- All state that must survive a restart goes through `internal/store` (SQLite). The agent's run-time state
  (`agent.State`) is in memory and rebuilt on start.
- Everything that touches the system goes through an interface with a fake (`netmgr.Fake`), so the whole flow runs
  on a laptop (`make run`) and in tests (`internal/agent/agent_test.go` uses a fake Pavilion).
- `go vet ./... && go test -race ./...` before committing; add tests with every fix.
