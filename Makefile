PKG := github.com/lightninglabs/pool
ESCPKG := github.com\/lightninglabs\/pool

GOACC_PKG := github.com/ory/go-acc
TOOLS_DIR := tools
GOIMPORTS_PKG := github.com/rinchsan/gosimports/cmd/gosimports

GO_BIN := ${GOPATH}/bin
GOACC_BIN := $(GO_BIN)/go-acc
GOIMPORTS_BIN := $(GO_BIN)/gosimports

GOBUILD := go build -v
GOINSTALL := go install -v
GOTEST := go test -v
GOFUZZ := go test -fuzztime 1m -fuzz

GOFILES_NOVENDOR = $(shell find . -type f -name '*.go' -not -path "./vendor/*" -not -name "*pb.go" -not -name "*pb.gw.go" -not -name "*.pb.json.go")
GOLIST := go list -deps $(PKG)/... | grep '$(PKG)'| grep -v '/vendor/'

COMMIT := $(shell git describe --tags --dirty)
LDFLAGS := -X $(PKG).Commit=$(COMMIT)

RM := rm -f
CP := cp
MAKE := make
XARGS := xargs -L 1

include make/testing_flags.mk
include make/release_flags.mk

# For the release, we want to remove the symbol table and debug information (-s)
# and omit the DWARF symbol table (-w). Also we clear the build ID.
RELEASE_LDFLAGS := -s -w -buildid= $(LDFLAGS)

# Linting uses a lot of memory, so keep it under control by limiting the number
# of workers if requested.
ifneq ($(workers),)
LINT_WORKERS = --concurrency=$(workers)
endif

DOCKER_TOOLS = docker run -v $$(pwd):/build pool-tools

GREEN := "\\033[0;32m"
NC := "\\033[0m"
define print
	echo $(GREEN)$1$(NC)
endef

default: scratch

all: scratch check install

# ============
# DEPENDENCIES
# ============

$(GOIMPORTS_BIN):
	@$(call print, "Installing goimports.")
	cd $(TOOLS_DIR); go install -trimpath $(GOIMPORTS_PKG)

$(GOACC_BIN):
	@$(call print, "Fetching go-acc")
	cd $(TOOLS_DIR); go install -trimpath $(GOACC_PKG)

# ============
# INSTALLATION
# ============

build:
	@$(call print, "Building Pool.")
	$(GOBUILD) -ldflags="$(LDFLAGS)" $(PKG)/cmd/pool
	$(GOBUILD) -ldflags="$(LDFLAGS)" $(PKG)/cmd/poold

install:
	@$(call print, "Installing Pool.")
	$(GOINSTALL) -ldflags="$(LDFLAGS)" $(PKG)/cmd/pool
	$(GOINSTALL) -ldflags="$(LDFLAGS)" $(PKG)/cmd/poold

release:
	@$(call print, "Releasing pool and poold binaries.")
	$(VERSION_CHECK)
	./scripts/release.sh build-release "$(VERSION_TAG)" "$(BUILD_SYSTEM)" "" "$(RELEASE_LDFLAGS)"

scratch: build

# =======
# TESTING
# =======

check: unit

unit:
	@$(call print, "Running unit tests.")
	$(UNIT)

unit-cover: $(GOACC_BIN)
	@$(call print, "Running unit coverage tests.")
	$(GOACC_BIN) $(COVER_PKG)

unit-race:
	@$(call print, "Running unit race tests.")
	env CGO_ENABLED=1 GORACE="history_size=7 halt_on_errors=1" $(UNIT_RACE)

fuzz:
	@$(call print, "Running fuzz tests.")
	$(GOFUZZ) FuzzWitnessSpendDetection ./poolscript

# =============
# FLAKE HUNTING
# =============
flake-unit:
	@$(call print, "Flake hunting unit tests.")
	while [ $$? -eq 0 ]; do GOTRACEBACK=all $(UNIT) -count=1; done

flake-race:
	@$(call print, "Flake hunting race tests.")
	while [ $$? -eq 0 ]; do GOTRACEBACK=all CGO_ENABLED=1 GORACE="history_size=7 halt_on_errors=1" $(UNIT_RACE) -count=1; done

# =========
# UTILITIES
# =========

docker-tools:
	@$(call print, "Building tools docker image.")
	docker build -q -t pool-tools $(TOOLS_DIR)

fmt: $(GOIMPORTS_BIN)
	@$(call print, "Fixing imports.")
	gosimports -w $(GOFILES_NOVENDOR)
	@$(call print, "Formatting source.")
	gofmt -l -w -s $(GOFILES_NOVENDOR)

lint: docker-tools
	@$(call print, "Linting source.")
	$(DOCKER_TOOLS) golangci-lint run -v $(LINT_WORKERS)

list:
	@$(call print, "Listing commands.")
	@$(MAKE) -qp | \
		awk -F':' '/^[a-zA-Z0-9][^$$#\/\t=]*:([^=]|$$)/ {split($$1,A,/ /);for(i in A)print A[i]}' | \
		grep -v Makefile | \
		sort

gen: rpc mock

mock:
	@$(call print, "Generating mock packages.")
	cd ./gen; ./gen_mock_docker.sh

mock-check: mock
	@$(call print, "Verifying mocks.")
	if test -n "$$(git status --porcelain '*.go')"; then echo "Mocks not properly generated!"; git status --porcelain '*.go'; exit 1; fi

rpc:
	@$(call print, "Compiling protos.")
	cd ./poolrpc; ./gen_protos_docker.sh

rpc-check: rpc
	@$(call print, "Verifying protos.")
	if test -n "$$(git describe --dirty | grep dirty)"; then echo "Protos not properly formatted or not compiled with correct version!"; git status; git diff; exit 1; fi

rpc-js-compile:
	@$(call print, "Compiling JSON/WASM stubs.")
	GOOS=js GOARCH=wasm $(GOBUILD) $(PKG)/poolrpc

clean:
	@$(call print, "Cleaning source.$(NC)")
	$(RM) ./pool
	$(RM) ./poold
	$(RM) coverage.txt
