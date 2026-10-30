.PHONY: help build build-libiec61850 build-iec61850bean build-dscsystems build-iec61850-rust \
        publish publish-libiec61850 publish-iec61850bean publish-dscsystems publish-iec61850-rust \
        smoke smoke-servers goose-crosscheck sv-crosscheck verify-binaries verify-fixtures \
        validate-fixtures genmodel clone-libiec61850 \
        ci pr \
        test \
        clean

PLATFORM            ?= linux/amd64
LIBIEC61850_IMAGE   ?= iec61850-interop-libiec61850:local
IEC61850BEAN_IMAGE  ?= iec61850-interop-iec61850bean:local
DSCSYSTEMS_IMAGE    ?= iec61850-interop-dscsystems:local
IEC61850RUST_IMAGE  ?= iec61850-interop-iec61850-rust:local
ADAPTER_VERSION     ?= dev
FIXTURE             ?= fixtures/mms/interop.json
PORT                ?= 1102
REGISTRY            ?= ghcr.io/otfabric
VERSION             ?= latest

# Scratch directory for locally cloned libiec61850 (used by genmodel target).
LIBIEC61850_CLONE   ?= /tmp/iec61850-interop-libiec61850-src

help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z0-9_-]+:.*?## / {printf "\033[36m%-26s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ---------------------------------------------------------------------------
# Build targets
# ---------------------------------------------------------------------------

build: build-libiec61850 build-iec61850bean build-dscsystems build-iec61850-rust ## Build all adapter images.

build-libiec61850: ## Build the libiec61850 adapter image (server + client).
	@echo "Building the libiec61850 adapter image..."
	docker buildx build \
	  --platform=$(PLATFORM) \
	  --load \
	  --build-arg ADAPTER_VERSION=$(ADAPTER_VERSION) \
	  -f adapters/libiec61850/Dockerfile \
	  -t $(LIBIEC61850_IMAGE) \
	  .

build-iec61850bean: ## Build the iec61850bean adapter image (server + client).
	@echo "Building the iec61850bean adapter image..."
	docker buildx build \
	  --platform=$(PLATFORM) \
	  --load \
	  -f adapters/iec61850bean/Dockerfile \
	  -t $(IEC61850BEAN_IMAGE) \
	  .

build-dscsystems: ## Build the dscsystems/go-iec61850 adapter image (GPL-3.0, see adapters/dscsystems/NOTICE).
	@echo "Building the dscsystems adapter image..."
	docker buildx build \
	  --platform=$(PLATFORM) \
	  --load \
	  --build-arg ADAPTER_VERSION=$(ADAPTER_VERSION) \
	  -f adapters/dscsystems/Dockerfile \
	  -t $(DSCSYSTEMS_IMAGE) \
	  .

build-iec61850-rust: ## Build the csp0924/iec61850-rust adapter image.
	@echo "Building the iec61850-rust adapter image..."
	docker buildx build \
	  --platform=$(PLATFORM) \
	  --load \
	  --build-arg ADAPTER_VERSION=$(ADAPTER_VERSION) \
	  -f adapters/iec61850-rust/Dockerfile \
	  -t $(IEC61850RUST_IMAGE) \
	  .

# ---------------------------------------------------------------------------
# Publish targets
# ---------------------------------------------------------------------------

publish: publish-libiec61850 publish-iec61850bean publish-dscsystems publish-iec61850-rust ## Publish adapter images to $(REGISTRY).

publish-libiec61850: build-libiec61850 ## Tag and push the libiec61850 adapter image.
	docker tag $(LIBIEC61850_IMAGE) $(REGISTRY)/iec61850-interop-libiec61850:$(VERSION)
	docker push $(REGISTRY)/iec61850-interop-libiec61850:$(VERSION)

publish-iec61850bean: build-iec61850bean ## Tag and push the iec61850bean adapter image.
	docker tag $(IEC61850BEAN_IMAGE) $(REGISTRY)/iec61850-interop-iec61850bean:$(VERSION)
	docker push $(REGISTRY)/iec61850-interop-iec61850bean:$(VERSION)

publish-dscsystems: build-dscsystems ## Tag and push the dscsystems adapter image.
	docker tag $(DSCSYSTEMS_IMAGE) $(REGISTRY)/iec61850-interop-dscsystems:$(VERSION)
	docker push $(REGISTRY)/iec61850-interop-dscsystems:$(VERSION)

publish-iec61850-rust: build-iec61850-rust ## Tag and push the iec61850-rust adapter image.
	docker tag $(IEC61850RUST_IMAGE) $(REGISTRY)/iec61850-interop-iec61850-rust:$(VERSION)
	docker push $(REGISTRY)/iec61850-interop-iec61850-rust:$(VERSION)

# ---------------------------------------------------------------------------
# Fixture validation (no Docker required)
# ---------------------------------------------------------------------------

validate-fixtures: ## JSON/XML syntax + cross-reference checks, the notes of the newest version in RELEASE.md, and the generated COVERAGE.md/README matrix (no Docker, no network).
	python3 scripts/validate-fixtures.py
	@python3 scripts/check-versions.py
	@scripts/release-notes.sh > /dev/null && echo "ok   RELEASE.md has the notes of the newest version"
	@scripts/gen-coverage.sh --check
	@python3 -m unittest discover -q -s scripts -p 'test_*.py'

clone-libiec61850: ## Clone libiec61850 at the pinned SHA into $(LIBIEC61850_CLONE).
	$(eval LIBIEC61850_SHA := $(shell grep 'LIBIEC61850_SHA=' adapters/libiec61850/Dockerfile | head -1 | cut -d= -f2))
	@if [ -d "$(LIBIEC61850_CLONE)/.git" ]; then \
	  echo "Already cloned at $(LIBIEC61850_CLONE); checking out $(LIBIEC61850_SHA)"; \
	  git -C "$(LIBIEC61850_CLONE)" fetch --quiet; \
	  git -C "$(LIBIEC61850_CLONE)" checkout --quiet --detach "$(LIBIEC61850_SHA)"; \
	else \
	  echo "Cloning libiec61850 at $(LIBIEC61850_SHA) into $(LIBIEC61850_CLONE)..."; \
	  git clone --quiet https://github.com/mz-automation/libiec61850.git "$(LIBIEC61850_CLONE)"; \
	  git -C "$(LIBIEC61850_CLONE)" checkout --quiet --detach "$(LIBIEC61850_SHA)"; \
	fi
	@echo "libiec61850 ready at $(LIBIEC61850_CLONE)"

genmodel: clone-libiec61850 ## Run genmodel.jar against interop.icd — mirrors validate-fixtures CI step.
	$(eval GENMODEL := $(shell find "$(LIBIEC61850_CLONE)" -name 'genmodel.jar' | head -1))
	@if [ -z "$(GENMODEL)" ]; then \
	  echo "ERROR: genmodel.jar not found in $(LIBIEC61850_CLONE)"; \
	  find "$(LIBIEC61850_CLONE)/tools" -type f | sort; \
	  exit 1; \
	fi
	@echo "Using $(GENMODEL)"
	java -jar "$(GENMODEL)" fixtures/iec61850/interop.icd -ied InteropIED -ap AP1
	@test -s static_model.c && echo "static_model.c: OK" || (echo "static_model.c: MISSING"; exit 1)
	@test -s static_model.h && echo "static_model.h: OK" || (echo "static_model.h: MISSING"; exit 1)

# ---------------------------------------------------------------------------
# Adapter self-tests (require Docker)
# ---------------------------------------------------------------------------

smoke: build ## Run full adapter smoke tests (binaries, fixtures, ready events, JSON Lines).
	LIBIEC61850_IMAGE=$(LIBIEC61850_IMAGE) \
	IEC61850BEAN_IMAGE=$(IEC61850BEAN_IMAGE) \
	DSCSYSTEMS_IMAGE=$(DSCSYSTEMS_IMAGE) \
	IEC61850RUST_IMAGE=$(IEC61850RUST_IMAGE) \
	./scripts/smoke-test.sh

smoke-servers: build ## Verify only the server adapters start and emit valid ready events.
	LIBIEC61850_IMAGE=$(LIBIEC61850_IMAGE) \
	IEC61850BEAN_IMAGE=$(IEC61850BEAN_IMAGE) \
	DSCSYSTEMS_IMAGE=$(DSCSYSTEMS_IMAGE) \
	IEC61850RUST_IMAGE=$(IEC61850RUST_IMAGE) \
	./scripts/smoke-test.sh

goose-crosscheck: build-libiec61850 build-dscsystems build-iec61850-rust ## GOOSE publisher -> subscriber between libiec61850, dscsystems and iec61850-rust, every pair.
	@for pub in "$(LIBIEC61850_IMAGE) libiec61850" "$(DSCSYSTEMS_IMAGE) dscsystems" "$(IEC61850RUST_IMAGE) iec61850-rust"; do \
	  for sub in "$(LIBIEC61850_IMAGE) libiec61850" "$(DSCSYSTEMS_IMAGE) dscsystems" "$(IEC61850RUST_IMAGE) iec61850-rust"; do \
	    set -- $$pub $$sub; \
	    ./scripts/goose-crosscheck.sh $$1 $$2-goose-publisher $$3 $$4-goose-subscriber || exit 1; \
	  done; \
	done

sv-crosscheck: build-libiec61850 build-dscsystems build-iec61850-rust ## Sampled Values publisher -> subscriber between libiec61850, dscsystems and iec61850-rust, every pair (9-2LE stream).
	@for pub in "$(LIBIEC61850_IMAGE) libiec61850" "$(DSCSYSTEMS_IMAGE) dscsystems" "$(IEC61850RUST_IMAGE) iec61850-rust"; do \
	  for sub in "$(LIBIEC61850_IMAGE) libiec61850" "$(DSCSYSTEMS_IMAGE) dscsystems" "$(IEC61850RUST_IMAGE) iec61850-rust"; do \
	    set -- $$pub $$sub; \
	    ./scripts/sv-crosscheck.sh $$1 $$2-sv-publisher $$3 $$4-sv-subscriber MSVCB01 || exit 1; \
	  done; \
	done

verify-binaries: build ## Verify all required binaries are present in the adapter images.
	@echo "--- Checking binaries in $(LIBIEC61850_IMAGE) ---"
	@for bin in libiec61850-mms-server libiec61850-mms-client \
	            libiec61850-ied-server libiec61850-ied-client \
	            libiec61850-ied-controller libiec61850-ied-reporter \
	            libiec61850-goose-publisher libiec61850-goose-subscriber \
	            libiec61850-sv-publisher libiec61850-sv-subscriber; do \
	  docker run --rm --entrypoint sh $(LIBIEC61850_IMAGE) -c "command -v $$bin" \
	    && echo "  OK: $$bin" || echo "  MISSING: $$bin"; \
	done
	@echo "--- Checking binaries in $(IEC61850BEAN_IMAGE) ---"
	@for bin in iec61850bean-ied-server iec61850bean-ied-client \
	            iec61850bean-ied-controller iec61850bean-ied-reporter; do \
	  docker run --rm --entrypoint sh $(IEC61850BEAN_IMAGE) -c "command -v $$bin" \
	    && echo "  OK: $$bin" || echo "  MISSING: $$bin"; \
	done
	@echo "--- Checking binaries in $(IEC61850RUST_IMAGE) ---"
	@for bin in iec61850-rust-mms-server iec61850-rust-mms-client \
	            iec61850-rust-ied-server iec61850-rust-ied-client \
	            iec61850-rust-ied-controller iec61850-rust-ied-reporter \
	            iec61850-rust-goose-publisher iec61850-rust-goose-subscriber \
	            iec61850-rust-sv-publisher iec61850-rust-sv-subscriber; do \
	  docker run --rm --entrypoint sh $(IEC61850RUST_IMAGE) -c "command -v $$bin" \
	    && echo "  OK: $$bin" || echo "  MISSING: $$bin"; \
	done
	@echo "--- Checking binaries in $(DSCSYSTEMS_IMAGE) ---"
	@for bin in dscsystems-mms-server dscsystems-mms-client \
	            dscsystems-ied-server dscsystems-ied-client \
	            dscsystems-ied-controller dscsystems-ied-reporter \
	            dscsystems-goose-publisher dscsystems-goose-subscriber \
	            dscsystems-sv-publisher dscsystems-sv-subscriber; do \
	  docker run --rm --entrypoint sh $(DSCSYSTEMS_IMAGE) -c "command -v $$bin" \
	    && echo "  OK: $$bin" || echo "  MISSING: $$bin"; \
	done

verify-fixtures: build ## Verify fixture files are present inside the adapter images.
	@echo "--- Checking fixtures in $(LIBIEC61850_IMAGE) ---"
	@for f in /fixtures/mms/interop.json \
	           /fixtures/iec61850/interop.icd \
	           /fixtures/iec61850/values.json \
	           /fixtures/goose/goose.json \
	           /fixtures/goose/interop-goose.cid \
	           /fixtures/sv/sv.json \
	           /fixtures/sv/interop-sv.cid; do \
	  docker run --rm --entrypoint sh $(LIBIEC61850_IMAGE) -c "test -f $$f" \
	    && echo "  OK: $$f" || echo "  MISSING: $$f"; \
	done
	@echo "--- Checking fixtures in $(IEC61850RUST_IMAGE) ---"
	@for f in /fixtures/mms/interop.json \
	           /fixtures/iec61850/interop.icd \
	           /fixtures/iec61850/values.json \
	           /fixtures/goose/goose.json \
	           /fixtures/goose/interop-goose.cid \
	           /fixtures/sv/sv.json \
	           /fixtures/sv/interop-sv.cid; do \
	  docker run --rm --entrypoint sh $(IEC61850RUST_IMAGE) -c "test -f $$f" \
	    && echo "  OK: $$f" || echo "  MISSING: $$f"; \
	done
	@echo "--- Checking fixtures in $(DSCSYSTEMS_IMAGE) ---"
	@for f in /fixtures/mms/interop.json \
	           /fixtures/iec61850/interop.icd \
	           /fixtures/iec61850/values.json \
	           /fixtures/goose/goose.json \
	           /fixtures/goose/interop-goose.cid \
	           /fixtures/sv/sv.json \
	           /fixtures/sv/interop-sv.cid; do \
	  docker run --rm --entrypoint sh $(DSCSYSTEMS_IMAGE) -c "test -f $$f" \
	    && echo "  OK: $$f" || echo "  MISSING: $$f"; \
	done
	@echo "--- Checking fixtures in $(IEC61850BEAN_IMAGE) ---"
	@for f in /fixtures/iec61850/interop.icd /fixtures/iec61850/values.json; do \
	  docker run --rm --entrypoint sh $(IEC61850BEAN_IMAGE) -c "test -f $$f" \
	    && echo "  OK: $$f" || echo "  MISSING: $$f"; \
	done

# ---------------------------------------------------------------------------
# Local CI mirrors — reproduce the exact jobs without pushing
# ---------------------------------------------------------------------------

# Mirrors pr.yml: validate fixtures + build + verify + smoke.
# Run this before opening a PR.
pr: validate-fixtures build verify-binaries verify-fixtures smoke goose-crosscheck sv-crosscheck ## Mirror pr.yml locally (no publish).
	@echo ""
	@echo "PR checks passed."

# Mirrors ci.yml: validate fixtures + build + smoke + provenance record.
# Run this to confirm a push to main would succeed.
ci: validate-fixtures build verify-binaries verify-fixtures smoke goose-crosscheck sv-crosscheck ## Mirror ci.yml locally (no publish).
	@echo ""
	@echo "--- Provenance ---"
	@echo "iec61850-interop SHA : $$(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
	@echo "libiec61850 SHA : $$(grep 'LIBIEC61850_SHA=' adapters/libiec61850/Dockerfile | head -1 | cut -d= -f2)"
	@echo "iec61850bean    : $$(grep -A1 '<artifactId>iec61850bean</artifactId>' adapters/iec61850bean/pom.xml | sed -nE 's|.*<version>([^<]+)</version>.*|\1|p')"
	@echo "dscsystems      : $$(grep 'ARG DSCSYSTEMS_SHA=' adapters/dscsystems/Dockerfile | cut -d= -f2)"
	@echo "iec61850-rust   : $$(grep 'ARG IEC61850_RUST_SHA=' adapters/iec61850-rust/Dockerfile | cut -d= -f2)"
	@echo "platform        : $(PLATFORM)"
	@echo ""
	@echo "CI checks passed."

test: build smoke ## Build images and run full adapter smoke tests.
	@echo ""
	@echo "Adapter images built and smoke-tested."
	@echo "Run interop tests in go-mms and go-iec61850:"
	@echo "  cd ../go-mms  && LIBIEC61850_IMAGE=$(LIBIEC61850_IMAGE) make interop"
	@echo "  cd ../go-iec61850 && LIBIEC61850_IMAGE=$(LIBIEC61850_IMAGE) IEC61850BEAN_IMAGE=$(IEC61850BEAN_IMAGE) DSCSYSTEMS_IMAGE=$(DSCSYSTEMS_IMAGE) IEC61850RUST_IMAGE=$(IEC61850RUST_IMAGE) make interop"

# ---------------------------------------------------------------------------
# Maintenance
# ---------------------------------------------------------------------------

clean: ## Clean up build artifacts, generated model files and container images.
	rm -rf adapters/libiec61850/build bin static_model.c static_model.h
	@docker rmi $(LIBIEC61850_IMAGE) 2>/dev/null || true
	@docker rmi $(IEC61850BEAN_IMAGE) 2>/dev/null || true
	@docker rmi $(DSCSYSTEMS_IMAGE) 2>/dev/null || true
	@docker rmi $(IEC61850RUST_IMAGE) 2>/dev/null || true
