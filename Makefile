SHELL := /bin/bash

PROJECT := amuxify
VERSION_FILE := VERSION

PREFIX ?= /usr/local

BINDIR := bin
LIBEXECDIR := libexec
INSTALL_LIBEXECDIR := $(PREFIX)/libexec/$(PROJECT)

PUBLIC_COMMANDS := \
	amux-clean \
	amux-remux \
	amux-scan \
	amux-scan-all

LIBEXEC_SCRIPTS := \
	clean-media.sh \
	remux-media.sh \
	scan-media.sh

.PHONY: all help setup wrappers permissions test install uninstall release-check clean

all: test

help:
	@printf '%s\n' \
		'amuxify development targets:' \
		'' \
		'  make setup          Create launchers and set executable permissions' \
		'  make test           Syntax-check scripts and verify repository files' \
		'  make install        Install under PREFIX (default: /usr/local)' \
		'  make uninstall      Remove files installed by make install' \
		'  make release-check  Run release readiness checks' \
		'  make clean          Remove generated bin launchers'

setup: wrappers permissions
	@echo "Setup complete."

wrappers:
	@mkdir -p "$(BINDIR)"
	@printf '%s\n' \
		'#!/usr/bin/env bash' \
		'set -e' \
		'ROOT="$$(cd "$$(dirname "$$0")/.." && pwd -P)"' \
		'exec "$$ROOT/libexec/clean-media.sh" "$$@"' \
		> "$(BINDIR)/amux-clean"
	@printf '%s\n' \
		'#!/usr/bin/env bash' \
		'set -e' \
		'ROOT="$$(cd "$$(dirname "$$0")/.." && pwd -P)"' \
		'exec "$$ROOT/libexec/remux-media.sh" "$$@"' \
		> "$(BINDIR)/amux-remux"
	@printf '%s\n' \
		'#!/usr/bin/env bash' \
		'set -e' \
		'ROOT="$$(cd "$$(dirname "$$0")/.." && pwd -P)"' \
		'exec "$$ROOT/libexec/scan-media.sh" --strict-permissions "$$@"' \
		> "$(BINDIR)/amux-scan"
	@printf '%s\n' \
		'#!/usr/bin/env bash' \
		'set -e' \
		'ROOT="$$(cd "$$(dirname "$$0")/.." && pwd -P)"' \
		'exec "$$ROOT/libexec/scan-media.sh" --strict-permissions --allow-data-tag text "$$@"' \
		> "$(BINDIR)/amux-scan-all"

permissions:
	@chmod +x "$(LIBEXECDIR)/clean-media.sh"
	@chmod +x "$(LIBEXECDIR)/remux-media.sh"
	@chmod +x "$(LIBEXECDIR)/scan-media.sh"
	@chmod +x "$(BINDIR)/amux-clean"
	@chmod +x "$(BINDIR)/amux-remux"
	@chmod +x "$(BINDIR)/amux-scan"
	@chmod +x "$(BINDIR)/amux-scan-all"

test: setup
	@echo "Checking repository files..."
	@test -f README.md
	@test -f LICENSE
	@test -f "$(VERSION_FILE)"
	@echo "Checking Bash syntax..."
	@/bin/bash -n "$(LIBEXECDIR)/clean-media.sh"
	@/bin/bash -n "$(LIBEXECDIR)/remux-media.sh"
	@/bin/bash -n "$(LIBEXECDIR)/scan-media.sh"
	@/bin/bash -n "$(BINDIR)/amux-clean"
	@/bin/bash -n "$(BINDIR)/amux-remux"
	@/bin/bash -n "$(BINDIR)/amux-scan"
	@/bin/bash -n "$(BINDIR)/amux-scan-all"
	@echo "All checks passed."

install: test
	@echo "Installing $(PROJECT) to $(PREFIX)..."
	@mkdir -p "$(PREFIX)/bin"
	@mkdir -p "$(INSTALL_LIBEXECDIR)"
	@install -m 755 "$(LIBEXECDIR)/clean-media.sh" "$(INSTALL_LIBEXECDIR)/clean-media.sh"
	@install -m 755 "$(LIBEXECDIR)/remux-media.sh" "$(INSTALL_LIBEXECDIR)/remux-media.sh"
	@install -m 755 "$(LIBEXECDIR)/scan-media.sh" "$(INSTALL_LIBEXECDIR)/scan-media.sh"
	@printf '%s\n' \
		'#!/usr/bin/env bash' \
		'exec "$(INSTALL_LIBEXECDIR)/clean-media.sh" "$$@"' \
		> "$(PREFIX)/bin/amux-clean"
	@printf '%s\n' \
		'#!/usr/bin/env bash' \
		'exec "$(INSTALL_LIBEXECDIR)/remux-media.sh" "$$@"' \
		> "$(PREFIX)/bin/amux-remux"
	@printf '%s\n' \
		'#!/usr/bin/env bash' \
		'exec "$(INSTALL_LIBEXECDIR)/scan-media.sh" --strict-permissions "$$@"' \
		> "$(PREFIX)/bin/amux-scan"
	@printf '%s\n' \
		'#!/usr/bin/env bash' \
		'exec "$(INSTALL_LIBEXECDIR)/scan-media.sh" --strict-permissions --allow-data-tag text "$$@"' \
		> "$(PREFIX)/bin/amux-scan-all"
	@chmod +x "$(PREFIX)/bin/amux-clean"
	@chmod +x "$(PREFIX)/bin/amux-remux"
	@chmod +x "$(PREFIX)/bin/amux-scan"
	@chmod +x "$(PREFIX)/bin/amux-scan-all"
	@echo "Installed."
	@echo "Try: amux-remux --help"

uninstall:
	@echo "Removing $(PROJECT) from $(PREFIX)..."
	@rm -f "$(PREFIX)/bin/amux-clean"
	@rm -f "$(PREFIX)/bin/amux-remux"
	@rm -f "$(PREFIX)/bin/amux-scan"
	@rm -f "$(PREFIX)/bin/amux-scan-all"
	@rm -rf "$(INSTALL_LIBEXECDIR)"
	@echo "Uninstalled."

release-check: test
	@echo "Checking VERSION..."
	@test -s "$(VERSION_FILE)"
	@grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$' "$(VERSION_FILE)"
	@echo "Version: $$(cat "$(VERSION_FILE)")"
	@echo "Release checks passed."

clean:
	@rm -f "$(BINDIR)/amux-clean"
	@rm -f "$(BINDIR)/amux-remux"
	@rm -f "$(BINDIR)/amux-scan"
	@rm -f "$(BINDIR)/amux-scan-all"
	@echo "Generated launchers removed."