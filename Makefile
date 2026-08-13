BIN ?= $(HOME)/.local/bin/agent-caffeine

.PHONY: build test install uninstall fmt

build:
	go build -o $(BIN) .

test:
	gofmt -l .
	go vet ./...
	go test ./...

install: build
	$(BIN) install

uninstall:
	$(BIN) uninstall

fmt:
	gofmt -w .
