VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := windows/amd64 windows/arm64 darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 linux/arm/6 linux/arm/7

.PHONY: build test test-live lint cross dist

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/meshbridge ./cmd/meshbridge
	CGO_ENABLED=0 go build -o bin/fakenode ./cmd/fakenode

test:
	go test -race ./...

# Runs against the production relay on the bridge-dev-sim gateway with a fake
# node. Uses the parent workspace .envrc through direnv
# (RELAY, SIM_BRIDGE_TOKEN, CLIENT_TOKEN; ADMIN optional).
test-live:
	direnv exec . env MESHBRIDGE_LIVE=1 go test -race -count=1 -v ./e2e/

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...

# Check every release target builds as a static, CGO-free binary.
cross:
	./scripts/cross-check.sh $(TARGETS)

# Release archives into dist/ (CI runs this on v* tags; see .github/workflows/release.yml).
dist:
	sh scripts/dist.sh $(VERSION)
