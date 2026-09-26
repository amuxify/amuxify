SHELL := /bin/bash
PROJECT := amuxify
VERSION := $(shell cat VERSION)
GO ?= go
PREFIX ?= /usr/local
LDFLAGS := -s -w -X github.com/amuxify/amuxify/internal/cli.Version=$(VERSION)
FIXTURES ?= testdata/out

.PHONY: all build test test-fixtures test-required vet fmt lint fixtures install uninstall clean docker release-check help

all: build

help:
	@printf '%s\n' \
		'amuxify development targets:' \
		'' \
		'  make build          Build ./bin/amuxify for this machine' \
		'  make test           go vet + go test' \
		'  make test-fixtures  Generate fixtures once, then go test' \
		'  make test-required  Same, failing instead of skipping when tools are missing' \
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

test-fixtures: fixtures
	AMUXIFY_FIXTURES="$(abspath $(FIXTURES))" $(GO) test ./...

test-required: fixtures
	AMUXIFY_FIXTURES="$(abspath $(FIXTURES))" AMUXIFY_REQUIRE_TOOLS=1 $(GO) test ./...

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
	@grep -q "ghcr.io/amuxify/$(PROJECT):$(VERSION) " contrib/hooks/Dockerfile.sabnzbd || { echo "contrib/hooks/Dockerfile.sabnzbd does not pull the $(VERSION) image"; exit 1; }
	@echo "Release checks passed for $(VERSION)."


clean:
	rm -rf bin/$(PROJECT) dist "$(FIXTURES)"
