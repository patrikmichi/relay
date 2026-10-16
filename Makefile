BINARY   = zuano
VERSION := $(shell git describe --tags --always --dirty)
GIT_SHA := $(shell git rev-parse --short HEAD)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GOFLAGS  = -trimpath -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(GIT_SHA) -X main.date=$(DATE)"

build:
	go build $(GOFLAGS) -o $(BINARY) ./cmd/zuano

install: build
	mkdir -p ~/.local/bin
	cp $(BINARY) ~/.local/bin/$(BINARY)
	@echo "Installed to ~/.local/bin/$(BINARY)"

test:
	go test -race ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)

.PHONY: build install test vet clean
