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

# Debian packages. The version comes from the latest vX.Y.Z tag
# (1.2.0, or 1.2.0+3.gabc1234 three commits on); with no tag yet it is
# 0.0~git<date>.<commit>, which sorts before any release.
DEB_ARCHES  ?= arm64 armhf amd64
DEB_VERSION ?= $(or $(shell git describe --tags --match 'v[0-9]*' 2>/dev/null | sed -E 's/^v//; s/-([0-9]+)-g/+\1.g/'),0.0~git$(shell date -u +%Y%m%d).$(COMMIT))
MAINTAINER  ?= Kalopa Robotics Limited <info@kalopa.com>

# Publishing is configured per site, in local.mk (not in git):
#   SIGNING_KEY  = the gpg key that signs the apt repository
#   KUBE_CONTEXT = kubectl context of the cluster serving the repository
#   KUBE_NS      = its namespace
#   KUBE_TARGET  = the workload to exec into, e.g. deploy/download
#   DPKG_DIR     = the repository's directory inside it
-include local.mk
PUBLISH_VARS := SIGNING_KEY KUBE_CONTEXT KUBE_NS KUBE_TARGET DPKG_DIR
export KUBE_CONTEXT KUBE_NS KUBE_TARGET DPKG_DIR

.PHONY: deb
deb: ## Build .deb packages for $(DEB_ARCHES) into ./bin/deb
	@mkdir -p bin/deb && rm -f bin/deb/*.deb bin/deb/SHA256SUMS
	@for a in $(DEB_ARCHES); do \
		case $$a in \
		arm64) env="GOARCH=arm64" ;; \
		armhf) env="GOARCH=arm GOARM=6" ;; \
		amd64) env="GOARCH=amd64" ;; \
		*) echo "unknown arch $$a" >&2; exit 1 ;; \
		esac; \
		env CGO_ENABLED=0 GOOS=linux $$env go build -trimpath -ldflags "$(LDFLAGS) -X main.version=$(DEB_VERSION)" \
			-o bin/deb/$(BINARY)-$$a ./cmd/$(BINARY) || exit 1; \
		packaging/deb/build.sh $$a "$(DEB_VERSION)" "$(MAINTAINER)" bin/deb/$(BINARY)-$$a bin/deb || exit 1; \
		rm bin/deb/$(BINARY)-$$a; \
	done

# Only a clean checkout of a pushed vX.Y.Z tag is published.
.PHONY: publish
publish: ## Release the current tag: GitHub release, plus the apt repository (see local.mk)
	$(foreach v,$(PUBLISH_VARS),$(if $($(v)),,$(error publish: set $(v) in local.mk (see the Makefile))))
	@test -z "$$(git status --porcelain)" || { echo 'publish: the working tree is not clean' >&2; exit 1; }
	@tag=$$(git describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null) || \
		{ echo 'publish: HEAD is not a vX.Y.Z tag' >&2; exit 1; }; \
	git ls-remote --exit-code --tags origin "$$tag" >/dev/null || \
		{ echo "publish: push the tag first: git push origin $$tag" >&2; exit 1; }; \
	$(MAKE) check-tests deb && packaging/apt/publish.sh "$$tag" "$(SIGNING_KEY)"

.PHONY: check-tests
check-tests: ## Vet and test without reformatting (what CI and publish run)
	go vet ./...
	go test -race -count=1 ./...

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
