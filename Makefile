SHELL := /bin/bash

PLATFORMS ?= linux/amd64 linux/arm64
BUILD_ROOT ?= $(HOME)/.tmp/socialweb-computer-build

OWNED := $(shell jq -r '.assets | to_entries[] | select([.value.fetch[]?.kind] | any(. == "go" or . == "script")) | .key' internal/versions/versions.json)
PLATFORM_FLAGS := $(addprefix --platform ,$(PLATFORMS))
ASSET_FLAGS := $(addprefix --asset ,$(OWNED))

.PHONY: all build pins check-pins dev

all: build

build:
	scripts/build-socialweb-computer.sh --update-pins $(PLATFORM_FLAGS) --build-root "$(BUILD_ROOT)"

pins:
	MESHD_CACHE="$(BUILD_ROOT)/cache/meshd" scripts/stage-assets.sh --update-pins $(PLATFORM_FLAGS) $(ASSET_FLAGS) --cache "$(BUILD_ROOT)/cache/assets" --blob-cache "$(BUILD_ROOT)/cache/blobs"

check-pins: pins
	git diff --exit-code -- internal/versions/versions.json

dev:
	scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc
