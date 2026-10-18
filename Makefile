#
# Copyright (c) 2026, Kalopa Robotics Limited.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Foudra Print Manager — development tasks. `make help` lists them.
#
BINARY     := print-manager
PKG        := github.com/kalopa/print_manager
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
  -X main.version=$(VERSION) \
  -X main.commit=$(COMMIT) \
  -X main.buildTime=$(BUILD_TIME)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build for this machine into ./bin
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: build-pi
build-pi: ## Build a static linux/arm64 binary for the Raspberry Pi into ./bin/pi
	@mkdir -p bin/pi
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o bin/pi/$(BINARY) ./cmd/$(BINARY)

.PHONY: build-pi32
build-pi32: ## Same, for a 32-bit Raspberry Pi OS (armv7)
	@mkdir -p bin/pi32
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o bin/pi32/$(BINARY) ./cmd/$(BINARY)

# The Pi beside the printers. Override for another: make install PI_HOST=other
PI_HOST ?= printpi

.PHONY: install
install: build-pi ## Build for the Pi, install on $(PI_HOST) and restart the daemon
	scp bin/pi/$(BINARY) $(PI_HOST):/tmp/$(BINARY)
	ssh -t $(PI_HOST) 'sudo install -o root -g root -m 755 /tmp/$(BINARY) /usr/local/bin/$(BINARY) && \
		rm /tmp/$(BINARY) && sudo systemctl restart $(BINARY) && systemctl --no-pager status $(BINARY) | \
		head -5'

.PHONY: test
test: ## Run the test suite
	go test -race -count=1 ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go source
	gofmt -s -w .

.PHONY: check
check: fmt vet test ## Format, vet, test — run before every commit

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin
