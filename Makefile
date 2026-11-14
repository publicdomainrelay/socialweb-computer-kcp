SHELL := /bin/bash

PLATFORMS ?= linux/amd64 linux/arm64
BUILD_ROOT ?= $(HOME)/.tmp/socialweb-computer-build

# the assets built here from pinned upstream source: their output trees stay pinned in versions.json. The assets built from this
# repository's own commit (fetch kind go or script) are not pinned: the build records their hashes (scripts/stage-assets.sh) and
# scripts/check-reproducible.sh proves a cold rebuild gives the same bytes.
OWNED := $(shell jq -r '.assets | to_entries[] | select([.value.fetch[]?.kind] | any(. == "gosrc" or . == "cargosrc")) | .key' internal/versions/versions.json)
PLATFORM_FLAGS := $(addprefix --platform ,$(PLATFORMS))
ASSET_FLAGS := $(addprefix --asset ,$(OWNED))

.PHONY: all build pins check-pins reproducible dev

all: build

build:
	scripts/build-socialweb-computer.sh --update-pins $(PLATFORM_FLAGS) --build-root "$(BUILD_ROOT)"

pins:
	MESHD_CACHE="$(BUILD_ROOT)/cache/meshd" GUEST_DOWNLOADS_CACHE="$(BUILD_ROOT)/cache/assets" scripts/stage-assets.sh --update-pins $(PLATFORM_FLAGS) $(ASSET_FLAGS) --cache "$(BUILD_ROOT)/cache/assets" --blob-cache "$(BUILD_ROOT)/cache/blobs"

check-pins: pins
	git diff --exit-code -- internal/versions/versions.json

dev:
	scripts/build-socialweb-computer.sh --dev --publish /var/tmp/swc

reproducible:
	scripts/check-reproducible.sh $(PLATFORM_FLAGS) --build-root "$(BUILD_ROOT)/reproduce"
