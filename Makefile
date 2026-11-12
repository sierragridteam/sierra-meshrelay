VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := windows/amd64 windows/arm64 darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 linux/arm/6 linux/arm/7

.PHONY: build test test-live lint cross dist dynamo-up dynamo-down test-dynamo

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/meshbridge ./cmd/meshbridge
	CGO_ENABLED=0 go build -o bin/fakenode ./cmd/fakenode

test:
	go test -race ./...

# The relay's DynamoDB store (relay/dynamostore) runs its tests against
# DynamoDB Local in Docker. They skip without MESHRELAY_TEST_DYNAMO_ENDPOINT,
# so plain `make test` stays offline. In-memory: nothing persists.
DYNAMO_IMAGE ?= amazon/dynamodb-local:3.3.2
DYNAMO_PORT ?= 8000
DYNAMO_ENDPOINT ?= http://127.0.0.1:$(DYNAMO_PORT)

dynamo-up:
	@docker inspect meshrelay-dynamo >/dev/null 2>&1 || \
		docker run -d --rm --name meshrelay-dynamo -p 127.0.0.1:$(DYNAMO_PORT):8000 $(DYNAMO_IMAGE) \
			-jar DynamoDBLocal.jar -inMemory -sharedDb >/dev/null
	@for i in $$(seq 1 40); do curl -s -o /dev/null $(DYNAMO_ENDPOINT) && exit 0; sleep 0.25; done; \
		echo "DynamoDB Local did not come up on $(DYNAMO_ENDPOINT)"; exit 1

dynamo-down:
	-docker rm -f meshrelay-dynamo >/dev/null 2>&1

test-dynamo: dynamo-up
	MESHRELAY_TEST_DYNAMO_ENDPOINT=$(DYNAMO_ENDPOINT) go test -race -count=1 ./relay/dynamostore/

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
