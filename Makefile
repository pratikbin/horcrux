VERSION := $(shell echo $(shell git describe --tags) | sed 's/^v//')
SDKVERSION := $(shell go list -m -u -f '{{.Version}}' github.com/cosmos/cosmos-sdk)
TMVERSION := $(shell go list -m -u -f '{{.Version}}' github.com/tendermint/tendermint)
COMMIT  := $(shell git log -1 --format='%H')
NPROCS := 1
OS := $(shell uname -s)

ifeq ($(OS),Linux)
  @echo hi
  NPROCS := $(shell grep -c ^processor /proc/cpuinfo)
endif

ifeq ($(OS),Darwin)
  NPROCS := $(shell sysctl -n hw.ncpu)
endif

all: install

LD_FLAGS = -X github.com/strangelove-ventures/horcrux/cmd/horcrux/cmd.Version=$(VERSION) \
	-X github.com/strangelove-ventures/horcrux/cmd/horcrux/cmd.Commit=$(COMMIT) \
	-X github.com/strangelove-ventures/horcrux/cmd/horcrux/cmd.SDKVersion=$(SDKVERSION) \
	-X github.com/strangelove-ventures/horcrux/cmd/horcrux/cmd.TMVersion=$(TMVERSION)

BUILD_FLAGS := -ldflags '$(LD_FLAGS)'

build:
	@go build -mod readonly $(BUILD_FLAGS) -o build/ ./cmd/horcrux/...

install:
	@go install -mod readonly $(BUILD_FLAGS) ./cmd/horcrux/...

build-linux:
	@GOOS=linux GOARCH=amd64 go build --mod readonly $(BUILD_FLAGS) -o ./build/horcrux ./cmd/horcrux

test:
	@go test -timeout 20m -mod readonly -parallel $(NPROCS) -v ./...

test-short:
	@go test -mod readonly -run TestDownedSigners2of3 -v ./...

test-signer-short:
	@go test -mod readonly -run TestThresholdValidator2of3 -v ./...

clean:
	rm -rf build

build-horcrux-docker:
	docker build -t strangelove-ventures/horcrux:$(VERSION) -f ./docker/horcrux/Dockerfile .

.PHONY: all lint test race msan tools clean build
