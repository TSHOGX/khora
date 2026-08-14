# Reproducible build for khora binaries.
#
# The flags:
#   -trimpath        removes absolute paths from the binary
#   -buildid=        clears the build ID (otherwise it embeds timestamps)
#   -s -w            strips debug info (smaller binaries, no loss for production)
#
# Verification: build twice, sha256sum both — they must match.

# VERSION is derived from git, NOT from a timestamp: `verify-reproducible` builds
# twice and compares sha256, so any input that moves between the two builds breaks
# it. `git describe` is a function of the commit, so two builds of one tree agree
# while two different commits do not. Override with `make VERSION=x` for a build
# from an exported tree with no .git.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Each binary declares its own `version` var (there is no shared version package:
# the gateway imports nothing from internal/, ADR-0007), so the -X target is
# main.version in all four.
LDFLAGS := -buildid= -s -w -X main.version=$(VERSION)
BUILDFLAGS := -trimpath -ldflags="$(LDFLAGS)"

.PHONY: all
all: bin/orchestrator bin/gateway bin/sandboxd bin/khora

bin/orchestrator: $(shell find cmd/orchestrator internal -name '*.go')
	@mkdir -p bin
	go build $(BUILDFLAGS) -o $@ ./cmd/orchestrator

bin/gateway: $(shell find cmd/gateway internal -name '*.go')
	@mkdir -p bin
	go build $(BUILDFLAGS) -o $@ ./cmd/gateway

bin/sandboxd: $(shell find cmd/sandboxd internal -name '*.go')
	@mkdir -p bin
	go build $(BUILDFLAGS) -o $@ ./cmd/sandboxd

bin/khora: $(shell find cmd/khora -name '*.go')
	@mkdir -p bin
	go build $(BUILDFLAGS) -o $@ ./cmd/khora

.PHONY: clean
clean:
	rm -rf bin/

.PHONY: verify-reproducible
verify-reproducible:
	@echo "=== verifying reproducible builds ==="
	@mkdir -p /tmp/khora-repro-test
	@go build $(BUILDFLAGS) -o /tmp/khora-repro-test/orch1 ./cmd/orchestrator
	@sleep 1
	@go build $(BUILDFLAGS) -o /tmp/khora-repro-test/orch2 ./cmd/orchestrator
	@if [ "$$(sha256sum /tmp/khora-repro-test/orch1 | cut -d' ' -f1)" = "$$(sha256sum /tmp/khora-repro-test/orch2 | cut -d' ' -f1)" ]; then \
		echo "✓ orchestrator: reproducible"; \
	else \
		echo "✗ orchestrator: NOT reproducible"; exit 1; \
	fi
	@go build $(BUILDFLAGS) -o /tmp/khora-repro-test/gw1 ./cmd/gateway
	@sleep 1
	@go build $(BUILDFLAGS) -o /tmp/khora-repro-test/gw2 ./cmd/gateway
	@if [ "$$(sha256sum /tmp/khora-repro-test/gw1 | cut -d' ' -f1)" = "$$(sha256sum /tmp/khora-repro-test/gw2 | cut -d' ' -f1)" ]; then \
		echo "✓ gateway: reproducible"; \
	else \
		echo "✗ gateway: NOT reproducible"; exit 1; \
	fi
	@go build $(BUILDFLAGS) -o /tmp/khora-repro-test/sandboxd1 ./cmd/sandboxd
	@sleep 1
	@go build $(BUILDFLAGS) -o /tmp/khora-repro-test/sandboxd2 ./cmd/sandboxd
	@if [ "$$(sha256sum /tmp/khora-repro-test/sandboxd1 | cut -d' ' -f1)" = "$$(sha256sum /tmp/khora-repro-test/sandboxd2 | cut -d' ' -f1)" ]; then \
		echo "✓ sandboxd: reproducible"; \
	else \
		echo "✗ sandboxd: NOT reproducible"; exit 1; \
	fi
	@go build $(BUILDFLAGS) -o /tmp/khora-repro-test/khora1 ./cmd/khora
	@sleep 1
	@go build $(BUILDFLAGS) -o /tmp/khora-repro-test/khora2 ./cmd/khora
	@if [ "$$(sha256sum /tmp/khora-repro-test/khora1 | cut -d' ' -f1)" = "$$(sha256sum /tmp/khora-repro-test/khora2 | cut -d' ' -f1)" ]; then \
		echo "✓ khora: reproducible"; \
	else \
		echo "✗ khora: NOT reproducible"; exit 1; \
	fi
	@rm -rf /tmp/khora-repro-test
	@echo "=== all binaries reproducible ==="
