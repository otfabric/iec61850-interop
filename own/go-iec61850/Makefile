# The iec61850-interop-otfabric image, from the repository root:
#   make -f own/go-iec61850/Makefile image smoke interop
#
# Apart from the top-level Makefile on purpose: this image is not in ADAPTERS
# and not in a release of the harness.
#
# The references are the release images of VERSION (default: the latest
# harness tag). Against references built from this checkout ("make build",
# tagged iec61850-interop-<adapter>:local):
#   make -f own/go-iec61850/Makefile smoke interop VERSION=

IMAGE ?= ghcr.io/otfabric/iec61850-interop-otfabric:dev
# The harness release of the reference images to test against.
VERSION ?= $(shell git tag -l 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -1)
REFERENCES ?= libiec61850 iec61850bean dscsystems iec61850-rust
BASE_REGISTRY ?= docker.io/library

latest = $(shell git ls-remote --tags --refs https://github.com/otfabric/$(1).git 'v[0-9]*' | sed 's,.*refs/tags/,,' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | sort -V | tail -1)

# The latest go-iec61850 and go-mms releases, as the workflow builds them.
UPSTREAM_VERSION ?= $(call latest,go-iec61850)
UPSTREAM_SHA ?= $(shell git ls-remote --tags https://github.com/otfabric/go-iec61850.git 'refs/tags/$(UPSTREAM_VERSION)^{}' | cut -f1)
MMS_VERSION ?= $(call latest,go-mms)
MMS_SHA ?= $(shell git ls-remote --tags https://github.com/otfabric/go-mms.git 'refs/tags/$(MMS_VERSION)^{}' | cut -f1)

export VERSION
export ADAPTER_IMAGE_otfabric = $(IMAGE)

.PHONY: image smoke interop

image:
	docker build --build-arg BASE_REGISTRY=$(BASE_REGISTRY) --build-arg UPSTREAM_VERSION=$(UPSTREAM_VERSION) --build-arg UPSTREAM_SHA=$(UPSTREAM_SHA) --build-arg MMS_VERSION=$(MMS_VERSION) --build-arg MMS_SHA=$(MMS_SHA) --build-arg ADAPTER_VERSION=$(UPSTREAM_VERSION) --tag $(IMAGE) --file own/go-iec61850/Dockerfile .

smoke:
	scripts/smoke.sh otfabric

interop:
	scripts/interop.sh otfabric $(REFERENCES)
