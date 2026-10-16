.PHONY: build install test test-race fmt lint docker

build:
	go build ./...
	go build -trimpath -o architect ./cmd/architect
install:
	go install ./cmd/architect
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
