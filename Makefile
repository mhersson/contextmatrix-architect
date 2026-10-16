.PHONY: build install test test-race fmt lint docker

# Kept in sync with contextmatrix-agent so CI installs the same linter release.
GOLANGCI_LINT_VERSION ?= v2.14.0

build:
	go build ./...
	go build -trimpath -o contextmatrix-architect ./cmd/contextmatrix-architect
install:
	go install ./cmd/contextmatrix-architect
test:
	go test ./...
test-race:
	CGO_ENABLED=1 go test -race ./...
fmt:
	gofumpt -w .
lint:
	golangci-lint run
docker:
	docker build -t contextmatrix-architect:dev .
