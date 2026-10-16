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

# Copies the policy JSON Schema from a gateway release into the CLI:
# make gen-policy-schema GATEWAY_SCHEMA=/path/to/public/schemas/policy/v1.json
gen-policy-schema:
	@test -n "$(GATEWAY_SCHEMA)" || { echo "set GATEWAY_SCHEMA to the gateway's policy/v1.json" >&2; exit 2; }
	cp "$(GATEWAY_SCHEMA)" internal/policyfile/schema/policy-v1.json

.PHONY: build install test vet clean gen-policy-schema
