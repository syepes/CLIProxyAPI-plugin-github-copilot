GO ?= go
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
VERSION ?= 0.1.1
REPOSITORY ?= UNCONFIGURED
HOSTOS := $(shell $(GO) env GOHOSTOS)
HOSTARCH := $(shell $(GO) env GOHOSTARCH)
EXT := $(if $(filter darwin,$(GOOS)),dylib,$(if $(filter windows,$(GOOS)),dll,so))
LIBRARY := dist/$(GOOS)/$(GOARCH)/github-copilot.$(EXT)
LDFLAGS := -s -w -X cliproxyapi-github-copilot/internal/plugin.Version=$(VERSION) -X cliproxyapi-github-copilot/internal/plugin.Repository=$(REPOSITORY)

.PHONY: build check-target freebsd test check integration release-snapshot
build: check-target
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=1 $(GO) build -trimpath -buildvcs=false -buildmode=c-shared -ldflags '$(LDFLAGS)' -o '$(LIBRARY)' ./cmd/plugin
	GOOS=$(HOSTOS) GOARCH=$(HOSTARCH) CGO_ENABLED=0 $(GO) run ./cmd/checklib -path '$(LIBRARY)' -goos '$(GOOS)' -goarch '$(GOARCH)'

test:
	$(GO) test -race -timeout 90s -count=1 ./...

check:
	@for script in scripts/*.sh; do sh -n "$$script"; done
	@test -z "$$(gofmt -l cmd internal integration)" || (echo 'Run gofmt -w cmd internal integration'; exit 1)
	$(GO) vet ./...
	$(GO) run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
	$(GO) test -race -timeout 90s -count=1 ./...

integration: build
	@test -n "$(CPA_BINARY)" || (echo 'Set CPA_BINARY to a plugin-enabled CLIProxyAPI binary; CI tests v8.0.15'; exit 1)
	CPA_REQUIRE_NATIVE=1 CPA_BINARY='$(CPA_BINARY)' CPA_PLUGIN_PATH='$(CURDIR)/$(LIBRARY)' $(GO) test -v -timeout 3m -count=1 ./integration

# Assembles whatever dist/<goos>/<arch>/ libraries already exist into
# store-ready ZIPs and checksums, without publishing anything. A full,
# store-ready release additionally needs the other platform libraries,
# normally produced by CI's per-platform build matrix; see release.yml.
release-snapshot:
	goreleaser release --skip=publish --snapshot --clean
	$(GO) run ./cmd/checkrelease -dir release

# FreeBSD requires cgo plus a target C toolchain; GOOS alone is insufficient.
freebsd:
	VERSION='$(VERSION)' REPOSITORY='$(REPOSITORY)' ./scripts/build-freebsd.sh

check-target:
	@case '$(GOOS)/$(GOARCH)' in linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|freebsd/amd64|windows/amd64) ;; *) echo 'Supported targets: linux/darwin amd64/arm64, freebsd/amd64 and windows/amd64'; exit 1 ;; esac

.PHONY: audit
audit:
	$(GO) mod verify
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
