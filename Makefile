# Reproducible build for warren binaries.
#
# The flags:
#   -trimpath        removes absolute paths from the binary
#   -buildid=        clears the build ID (otherwise it embeds timestamps)
#   -s -w            strips debug info (smaller binaries, no loss for production)
#
# Verification: build twice, sha256sum both — they must match.

LDFLAGS := -buildid= -s -w
BUILDFLAGS := -trimpath -ldflags="$(LDFLAGS)"

.PHONY: all
all: bin/orchestrator bin/gateway bin/sandboxd

bin/orchestrator: $(shell find cmd/orchestrator internal -name '*.go')
	@mkdir -p bin
	go build $(BUILDFLAGS) -o $@ ./cmd/orchestrator

bin/gateway: $(shell find cmd/gateway internal -name '*.go')
	@mkdir -p bin
	go build $(BUILDFLAGS) -o $@ ./cmd/gateway

bin/sandboxd: $(shell find cmd/sandboxd internal -name '*.go')
	@mkdir -p bin
	go build $(BUILDFLAGS) -o $@ ./cmd/sandboxd

.PHONY: clean
clean:
	rm -rf bin/

.PHONY: verify-reproducible
verify-reproducible:
	@echo "=== verifying reproducible builds ==="
	@mkdir -p /tmp/warren-repro-test
	@go build $(BUILDFLAGS) -o /tmp/warren-repro-test/orch1 ./cmd/orchestrator
	@sleep 1
	@go build $(BUILDFLAGS) -o /tmp/warren-repro-test/orch2 ./cmd/orchestrator
	@if [ "$$(sha256sum /tmp/warren-repro-test/orch1 | cut -d' ' -f1)" = "$$(sha256sum /tmp/warren-repro-test/orch2 | cut -d' ' -f1)" ]; then \
		echo "✓ orchestrator: reproducible"; \
	else \
		echo "✗ orchestrator: NOT reproducible"; exit 1; \
	fi
	@go build $(BUILDFLAGS) -o /tmp/warren-repro-test/gw1 ./cmd/gateway
	@sleep 1
	@go build $(BUILDFLAGS) -o /tmp/warren-repro-test/gw2 ./cmd/gateway
	@if [ "$$(sha256sum /tmp/warren-repro-test/gw1 | cut -d' ' -f1)" = "$$(sha256sum /tmp/warren-repro-test/gw2 | cut -d' ' -f1)" ]; then \
		echo "✓ gateway: reproducible"; \
	else \
		echo "✗ gateway: NOT reproducible"; exit 1; \
	fi
	@go build $(BUILDFLAGS) -o /tmp/warren-repro-test/sandboxd1 ./cmd/sandboxd
	@sleep 1
	@go build $(BUILDFLAGS) -o /tmp/warren-repro-test/sandboxd2 ./cmd/sandboxd
	@if [ "$$(sha256sum /tmp/warren-repro-test/sandboxd1 | cut -d' ' -f1)" = "$$(sha256sum /tmp/warren-repro-test/sandboxd2 | cut -d' ' -f1)" ]; then \
		echo "✓ sandboxd: reproducible"; \
	else \
		echo "✗ sandboxd: NOT reproducible"; exit 1; \
	fi
	@rm -rf /tmp/warren-repro-test
	@echo "=== all binaries reproducible ==="
