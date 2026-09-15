GO     ?= go
BIN    ?= ./bin/recall
PREFIX ?= $(HOME)/.local

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -s -w -X main.version=$(VERSION)

.PHONY: build test fmt vet tidy ingest stats inject install uninstall setup release clean

build:
	@mkdir -p ./bin
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/recall

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

ingest: build
	$(BIN) ingest

stats: build
	$(BIN) stats

inject: build
	$(BIN) inject

install: build
	@mkdir -p $(PREFIX)/bin
	install -m 755 $(BIN) $(PREFIX)/bin/recall
	@echo "installed to $(PREFIX)/bin/recall"

uninstall:
	rm -f $(PREFIX)/bin/recall

# Render the launchd plist template into ~/Library/LaunchAgents/.
# Run `launchctl load ~/Library/LaunchAgents/ai.fantazm.recall.watch.plist`
# afterward to start it (and `launchctl unload ...` to stop).
watch-install:
	@mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/.local/state/recall
	@sed 's|__HOME__|$(HOME)|g' dist/ai.fantazm.recall.watch.plist.template \
	  > $(HOME)/Library/LaunchAgents/ai.fantazm.recall.watch.plist
	@echo "wrote $(HOME)/Library/LaunchAgents/ai.fantazm.recall.watch.plist"
	@echo "to start: launchctl load $(HOME)/Library/LaunchAgents/ai.fantazm.recall.watch.plist"

watch-uninstall:
	-launchctl unload $(HOME)/Library/LaunchAgents/ai.fantazm.recall.watch.plist 2>/dev/null
	rm -f $(HOME)/Library/LaunchAgents/ai.fantazm.recall.watch.plist

# One-shot bootstrap: build + install, ONNX assets, MCP servers and rules
# for every supported tool found, watch daemon, doctor. Idempotent.
setup:
	./dist/setup.sh

# Build a release tarball matching the GitHub Actions release workflow output.
# Use this for local sanity-checks or airgap distribution. CI publishes the
# same artifact when a `v*` tag is pushed.
release: build
	@mkdir -p ./release
	@NAME="recall-$(VERSION)-darwin-arm64"; \
	  cp $(BIN) ./release/recall; \
	  cd ./release && tar -czf "$$NAME.tar.gz" recall && \
	    shasum -a 256 "$$NAME.tar.gz" > "$$NAME.tar.gz.sha256" && \
	    shasum -a 256 recall > recall.sha256 && \
	    rm -f recall && \
	    echo "wrote release/$$NAME.tar.gz"; \
	    cat "$$NAME.tar.gz.sha256"

clean:
	rm -rf ./bin ./release
