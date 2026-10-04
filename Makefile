BINARY     := nobackups
PKG        := ./cmd/nobackups
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
GOFLAGS    := -trimpath
export CGO_ENABLED := 0

PLATFORMS  ?= linux/amd64 linux/arm64 linux/arm/7

PREFIX     ?= /usr/local
BINDIR     ?= $(PREFIX)/bin
SYSCONFDIR ?= /etc
UNITDIR    ?= /etc/systemd/system
DESTDIR    ?=

HOST       ?=
ARCH       ?= amd64
SSH        ?= ssh
SCP        ?= scp
SUDO       ?= sudo

CONFIG     ?= ./config.yaml
ARGS       ?= help

DEMO_DIR   := .demo

MINT       ?= npx --yes mint@latest

.DEFAULT_GOAL := help

.PHONY: build
build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

.PHONY: dist
dist:
	@mkdir -p dist
	@set -e; for p in $(PLATFORMS); do \
		os=$$(echo $$p | cut -d/ -f1); arch=$$(echo $$p | cut -d/ -f2); arm=$$(echo $$p | cut -d/ -f3); \
		out=dist/$(BINARY)-$$os-$$arch$${arm:+v$$arm}; \
		echo "  building $$out"; \
		GOOS=$$os GOARCH=$$arch GOARM=$$arm go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out $(PKG); \
	done

.PHONY: checksums
checksums: dist
	cd dist && sha256sum $(BINARY)-* > SHA256SUMS
	@cat dist/SHA256SUMS

.PHONY: version
version:
	@echo $(VERSION)

.PHONY: run
run: build
	./$(BINARY) -c $(CONFIG) $(ARGS)

.PHONY: demo
demo: build
	@rm -rf $(DEMO_DIR) && mkdir -p $(DEMO_DIR)/source/app $(DEMO_DIR)/storage
	@echo "hello from NoBackups" > $(DEMO_DIR)/source/app/hello.txt
	@printf 'state_dir: %s/state\ndestinations:\n  disk: {type: local, path: %s/storage}\njobs:\n  - name: demo\n    sources: [%s/source]\n    destinations: [disk]\n    encryption: {passphrase: demo-passphrase}\n    retention: {keep_last: 3}\n' \
		"$(CURDIR)/$(DEMO_DIR)" "$(CURDIR)/$(DEMO_DIR)" "$(CURDIR)/$(DEMO_DIR)" > $(DEMO_DIR)/config.yaml
	./$(BINARY) -c $(DEMO_DIR)/config.yaml run demo
	./$(BINARY) -c $(DEMO_DIR)/config.yaml snapshots demo
	./$(BINARY) -c $(DEMO_DIR)/config.yaml restore demo --target $(DEMO_DIR)/restored
	@echo; echo "Restored file:"; find $(DEMO_DIR)/restored -name hello.txt -exec cat {} \;
	@echo; echo "Play around with: ./$(BINARY) -c $(DEMO_DIR)/config.yaml <command>"

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint: vet
	@test -z "$$(gofmt -l .)" || { echo "unformatted files (run 'make fmt'):"; gofmt -l .; exit 1; }
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "  (staticcheck not installed, skipping)"; fi

.PHONY: test
test:
	CGO_ENABLED=1 go test -race ./...

.PHONY: test-short
test-short:
	go test -short ./...

.PHONY: cover
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1
	go tool cover -html=coverage.out -o coverage.html
	@echo "open coverage.html"

.PHONY: tidy
tidy:
	go mod tidy
	go mod verify

.PHONY: check
check: lint test

.PHONY: clean
clean:
	rm -rf $(BINARY) dist coverage.out coverage.html $(DEMO_DIR)

.PHONY: docs
docs:
	cd docs && $(MINT) dev

.PHONY: docs-check
docs-check:
	cd docs && $(MINT) validate && $(MINT) broken-links

INSTALL_FLAGS = --bin-dir=$(BINDIR) --config-dir=$(SYSCONFDIR)/nobackups --unit-dir=$(UNITDIR) $(if $(DESTDIR),--root=$(DESTDIR))

.PHONY: install
install: build
	./$(BINARY) install $(INSTALL_FLAGS)

.PHONY: uninstall
uninstall: build
	./$(BINARY) uninstall $(INSTALL_FLAGS)

.PHONY: deploy
deploy:
	@test -n "$(HOST)" || { echo "usage: make deploy HOST=user@server [ARCH=amd64|arm64|armv7]"; exit 1; }
	$(MAKE) --no-print-directory dist PLATFORMS="linux/$(if $(filter armv7,$(ARCH)),arm/7,$(ARCH))"
	$(SCP) dist/$(BINARY)-linux-$(ARCH) $(HOST):/tmp/nobackups-deploy
	$(SSH) -t $(HOST) '$(SUDO) /tmp/nobackups-deploy install; rm -f /tmp/nobackups-deploy'

.PHONY: help
help:
	@echo "Usage: make <target> [VAR=value]"
	@echo
	@grep -E '^[a-z][a-z-]*:' $(MAKEFILE_LIST) | cut -d: -f1 | sort -u | paste -sd' ' | fold -s -w 72 | sed 's/^/  /'
