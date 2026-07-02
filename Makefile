BINARY  := commandcode-proxy
PKG     := ./cmd/commandcode-proxy
DIST    := dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# Cross-compile targets for `make release`.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build test vet run clean release

build: ## Build a static binary for the host platform
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

test: vet ## Run the test suite (vet first)
	go test ./...

vet: ## Run go vet
	go vet ./...

run: ## Run from source
	go run $(PKG)

clean:
	rm -rf $(BINARY) $(DIST)

release: clean ## Cross-compile static binaries into ./dist
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=$(DIST)/$(BINARY)-$$os-$$arch; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' -o $$out $(PKG) || exit 1; \
	done
	@ls -lh $(DIST)
