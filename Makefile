BINARY  := snapshot-cleaner
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= /usr/local
LDFLAGS := -s -w -X main.Version=$(VERSION)
GO      ?= go

# CGO is off so the result is a single static binary that runs on any Linux
# with a btrfs filesystem, regardless of libc.
export CGO_ENABLED := 0

# The debug build defaults its log level to "trace", so a run records
# everything it did to a file that can be reviewed afterwards. Release builds
# log nothing unless asked with --debug/--log-level.
DEBUG_LDFLAGS := -X main.Version=$(VERSION)-debug -X main.defaultLogLevel=trace

.PHONY: all build debug test vet fmt install clean release integration

all: build

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) .

debug:
	$(GO) build -trimpath -ldflags '$(DEBUG_LDFLAGS)' -o $(BINARY)-debug .
	@echo
	@echo "Built ./$(BINARY)-debug - logs everything to a file by default."
	@echo "Run it, then send the log path it prints on exit."

test:
	$(GO) test ./...

# The integration tests need root, but `sudo go test` would build as root and
# scatter root-owned files through the Go cache. Compile the test binary as the
# normal user first, then run only that one binary under sudo.
integration: $(BINARY)-integration.test
	sudo env SNAPSHOT_CLEANER_INTEGRATION=1 SNAPSHOT_CLEANER_LOG=$(CURDIR)/integration.log \
		./$(BINARY)-integration.test -test.v -test.run Integration 2>&1 | tee integration-test.log
	@echo
	@echo "Full log: $(CURDIR)/integration.log"

$(BINARY)-integration.test:
	$(GO) test -c -o $@ .

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

install: build
	install -Dm755 $(BINARY) $(DESTDIR)$(PREFIX)/sbin/$(BINARY)

release:
	@for arch in amd64 arm64; do \
		echo "building linux/$$arch"; \
		GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/$(BINARY)-linux-$$arch . ; \
	done

clean:
	rm -f $(BINARY) $(BINARY)-debug
	rm -f integration.log integration-test.log $(BINARY)-integration.test
	rm -rf dist
