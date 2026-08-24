.PHONY: build test check

build:
	go build -trimpath -ldflags='-s -w' -o bin/mygpt-cf-tunnel ./cmd/mygpt-cf-tunnel
	go build -trimpath -ldflags='-s -w' -o bin/mygpt-audit ./cmd/mygpt-audit

test:
	go test ./...

check:
	gofmt -w cmd internal
	go vet ./...
	go test ./...
