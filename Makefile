APP_NAME   := dockershrink
MODULE     := github.com/eltaline/dockershrink
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE       ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS    := -s -w \
	-X '$(MODULE)/cmd.Version=$(VERSION)' \
	-X '$(MODULE)/cmd.Commit=$(COMMIT)' \
	-X '$(MODULE)/cmd.Date=$(DATE)'

.PHONY: all build test vet lint clean

all: lint vet test build

build:
	go build -ldflags "$(LDFLAGS)" -o $(APP_NAME) .

test:
	go test ./... -v

vet:
	go vet ./...

lint:
	golangci-lint run ./...

clean:
	rm -f $(APP_NAME)
