VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build pi test run

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/jester-agent ./cmd/jester-agent

# Raspberry Pi 3/4/5 and Zero 2 W on a 64-bit OS
pi:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o bin/jester-agent-linux-arm64 ./cmd/jester-agent

test:
	go test -race ./...

# Try it on a laptop: fake network, TV page on :8080, setup page at :8080/portal/
run:
	go run ./cmd/jester-agent -fake-network -db ./dev.db -portal "" -cdp "" -listen 127.0.0.1:8080
