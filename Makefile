.PHONY: build install test test-race fmt lint docker history

# Kept in sync with contextmatrix-agent so CI installs the same linter release.
GOLANGCI_LINT_VERSION ?= v2.14.0

VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null)
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO := github.com/mhersson/contextmatrix-architect/internal/buildinfo
LDFLAGS := -X $(BUILDINFO).version=$(VERSION) -X $(BUILDINFO).commit=$(GIT_COMMIT) -X $(BUILDINFO).buildTime=$(BUILD_TIME)
HISTORY := internal/buildinfo/history/merges.log

# The git arguments equal changelog.GitLogArgs, and
# TestHistoryCommands_MatchGitLogArgs keeps the two in step. A checkout
# without git records no history and still builds.
history:
	git log --no-show-signature --first-parent --merges -n 100 '--format=%cI%x1f%s%x1f%b%x1e' > $(HISTORY) 2>/dev/null || true
build: history
	go build ./...
	go build -trimpath -ldflags="$(LDFLAGS)" -o contextmatrix-architect ./cmd/contextmatrix-architect
install: history
	go install -ldflags="$(LDFLAGS)" ./cmd/contextmatrix-architect
test:
	go test ./...
test-race:
	CGO_ENABLED=1 go test -race ./...
fmt:
	gofumpt -w .
lint:
	golangci-lint run
docker: history
	docker build --build-arg VERSION="$(VERSION)" --build-arg GIT_COMMIT="$(GIT_COMMIT)" --build-arg BUILD_TIME="$(BUILD_TIME)" -t contextmatrix-architect:dev .
