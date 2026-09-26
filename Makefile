SHELL := /bin/bash
PROJECT := amuxify
VERSION := $(shell cat VERSION)
GO ?= go
PREFIX ?= /usr/local
LDFLAGS := -s -w -X github.com/amuxify/amuxify/internal/cli.Version=$(VERSION)
FIXTURES ?= testdata/out

.PHONY: all build test vet fmt lint fixtures install uninstall clean docker release-check help

all: build

help:
	@printf '%s\n' \
		'amuxify development targets:' \
		'' \
		'  make build          Build ./bin/amuxify for this machine' \
		'  make test           go vet + go test' \
		'  make fixtures       Generate the fixture corpus into $(FIXTURES)' \
		'  make install        Install the binary under PREFIX (default /usr/local)' \
		'  make docker         Build the container image locally' \
		'  make release-check  Everything CI runs before a tag'

build:
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(PROJECT) ./cmd/$(PROJECT)

vet:
	$(GO) vet ./...

fmt:
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; echo "run gofmt -w"; exit 1; }

test: vet
	$(GO) test ./...

fixtures:
	@rm -rf "$(FIXTURES)"
	testdata/gen-fixtures.sh "$(FIXTURES)"

install: build
	@mkdir -p "$(PREFIX)/bin"
	install -m 755 bin/$(PROJECT) "$(PREFIX)/bin/$(PROJECT)"
	@echo "Installed $(PROJECT) $(VERSION) to $(PREFIX)/bin"

uninstall:
	rm -f "$(PREFIX)/bin/$(PROJECT)"

docker:
	docker build --build-arg VERSION=$(VERSION) -t ghcr.io/amuxify/$(PROJECT):$(VERSION) .

release-check: fmt test build
	@grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' VERSION
	@grep -q "^## $(VERSION)" CHANGELOG.md || { echo "CHANGELOG.md has no entry for $(VERSION)"; exit 1; }
	@./bin/$(PROJECT) version | grep -q "$(VERSION)"
	@echo "Release checks passed for $(VERSION)."

clean:
	rm -rf bin/$(PROJECT) dist "$(FIXTURES)"
