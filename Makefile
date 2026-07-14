.PHONY: all test lint fuzz bench coverage vet tools fixtures fmt clean

GOTEST  := go test -race -timeout 120s
PACKAGES := ./...

all: test lint vet

test:
	$(GOTEST) $(PACKAGES)

coverage:
	$(GOTEST) -coverprofile=coverage.txt -covermode=atomic $(PACKAGES)
	go tool cover -html=coverage.txt -o coverage.html

lint:
	golangci-lint run $(PACKAGES)

fmt:
	gofmt -w .

vet:
	go vet $(PACKAGES)
	govulncheck $(PACKAGES)

fuzz:
	go test -fuzz=FuzzDecodeVarUint    -fuzztime=60s ./encoding/
	go test -fuzz=FuzzApplyUpdateV1    -fuzztime=60s ./crdt/
	go test -fuzz=FuzzApplyUpdateV2    -fuzztime=60s ./crdt/
	go test -fuzz=FuzzApplySyncMessage -fuzztime=60s ./sync/

bench:
	@mkdir -p benchmarks
	go test -bench=. -benchmem -count=3 $(PACKAGES) | tee benchmarks/latest.txt

# Regenerates the deterministic cross-impl conformance fixtures
# (crdt/testdata/*_yjs_fixtures.json) from the pinned yjs. The legacy
# testutil/fixtures/*.bin are generated separately by gen_fixtures.js /
# gen_fixtures_v2.js and are NOT drift-gated (they use a random clientID and
# are not yet deterministic — see #99 follow-up).
fixtures:
	node testutil/gen_conformance_fixtures.js

tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest

clean:
	rm -f coverage.txt coverage.html
	rm -f benchmarks/latest.txt
