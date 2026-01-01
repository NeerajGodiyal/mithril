VERSION ?= dev
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -X main.Version=$(VERSION) \
           -X main.GitCommit=$(GIT_COMMIT) \
           -X main.BuildDate=$(BUILD_DATE)

.PHONY: build release clean

build:
	go build -ldflags "$(LDFLAGS)" -o mithril ./cmd/mithril

release:
	go build -ldflags "$(LDFLAGS) -s -w" -o mithril ./cmd/mithril

clean:
	rm -f mithril
