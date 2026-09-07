BINARY   = relay
VERSION := $(shell git describe --tags --always --dirty)
GIT_SHA := $(shell git rev-parse --short HEAD)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Empty by default — a local `make build` produces an offline-first binary
# with no baked-in gateway host (mirrors .goreleaser.yaml's default). An
# owner build opts in with `make build RELAY_DEFAULT_GATEWAY_URL=https://...`.
RELAY_DEFAULT_GATEWAY_URL ?=
GOFLAGS  = -trimpath -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(GIT_SHA) -X main.date=$(DATE) -X github.com/patrikmichi/relay/internal/config.DefaultGatewayURL=$(RELAY_DEFAULT_GATEWAY_URL)"

build:
	go build $(GOFLAGS) -o $(BINARY) ./cmd/relay

install: build
	mkdir -p ~/.local/bin
	cp $(BINARY) ~/.local/bin/$(BINARY)
	ln -sf ~/.local/bin/$(BINARY) ~/.local/bin/gw
	@echo "Installed to ~/.local/bin/$(BINARY) (alias: gw)"

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)

.PHONY: build install test vet clean
