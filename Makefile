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

# Copies the evaluation config schema from a gateway release and pins its hash:
# make gen-evals-schema GATEWAY_EVALS_SCHEMA=/path/to/public/schemas/evals/v1.json
gen-evals-schema:
	@test -n "$(GATEWAY_EVALS_SCHEMA)" || { echo "set GATEWAY_EVALS_SCHEMA to the gateway's evals/v1.json" >&2; exit 2; }
	cp "$(GATEWAY_EVALS_SCHEMA)" internal/evals/schema/evals-v1.json
	python3 -c "import hashlib,sys; sys.stdout.write(hashlib.sha256(open('internal/evals/schema/evals-v1.json','rb').read()).hexdigest())" > internal/evals/schema/evals-v1.sha256

# Copies the grader and statistics conformance vectors from a gateway release
# and records their hashes, which the tests check:
# make gen-evals-fixtures GATEWAY_EVALS_FIXTURES=/path/to/lib/evals/__fixtures__
gen-evals-fixtures:
	@test -n "$(GATEWAY_EVALS_FIXTURES)" || { echo "set GATEWAY_EVALS_FIXTURES to the gateway's evals fixture directory" >&2; exit 2; }
	cp "$(GATEWAY_EVALS_FIXTURES)/stats.json" "$(GATEWAY_EVALS_FIXTURES)/graders.json" internal/evals/testdata/conformance/
	cd internal/evals/testdata/conformance && (sha256sum graders.json stats.json || shasum -a 256 graders.json stats.json) > SHA256SUMS

.PHONY: build install test vet clean gen-policy-schema gen-evals-schema gen-evals-fixtures
