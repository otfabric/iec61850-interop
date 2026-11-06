# Multi-stage Dockerfile for the dscsystems/go-iec61850 adapter.
#
# dscsystems/go-iec61850 is GPL-3.0. Its code lives only in this image
# and in adapters/dscsystems/ (see NOTICE); it is never linked into the
# MIT-licensed otfabric Go libraries.
#
# Build from repository root:
#   docker buildx build --platform=linux/amd64 \
#       -f adapters/dscsystems/Dockerfile \
#       -t iec61850-interop-dscsystems:local .
#
# The upstream pin is the require line of adapters/dscsystems/go.mod
# (DSCSYSTEMS_SHA below documents the same commit and is checked against it).

ARG GOLANG_DIGEST=sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5
ARG DEBIAN_DIGEST=sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818

# ---------------------------------------------------------------------------
# Stage 1: build the adapter programs (pure Go, no cgo)
# ---------------------------------------------------------------------------
FROM golang:1.27.3-bookworm@${GOLANG_DIGEST} AS build

# dscsystems/go-iec61850 v0.6.6 (commit 4833767). go.mod must require
# DSCSYSTEMS_VERSION (a tag, or the pseudo-version of an untagged commit),
# and the module proxy must say it is commit DSCSYSTEMS_SHA.
ARG DSCSYSTEMS_VERSION=v0.6.6
ARG DSCSYSTEMS_SHA=4833767caf3abbf1f205c4c49cd9432744beaf83

WORKDIR /src
COPY adapters/dscsystems/go.mod adapters/dscsystems/go.sum ./
RUN set -eu; \
    got="$(go list -m -f '{{.Version}}' github.com/dscsystems/go-iec61850)"; \
    test "${got}" = "${DSCSYSTEMS_VERSION}" || { echo "go.mod has go-iec61850 ${got}, want ${DSCSYSTEMS_VERSION}" >&2; exit 1; }; \
    go mod download && go mod verify; \
    origin="$(go mod download -json "github.com/dscsystems/go-iec61850@${DSCSYSTEMS_VERSION}" \
        | sed -nE 's/^[[:space:]]*"Hash": "([0-9a-f]{40})".*/\1/p')"; \
    if [ -n "${origin}" ] && [ "${origin}" != "${DSCSYSTEMS_SHA}" ]; then \
        echo "go-iec61850 ${DSCSYSTEMS_VERSION} is commit ${origin}, not ${DSCSYSTEMS_SHA}" >&2; exit 1; \
    fi
COPY adapters/dscsystems/ ./
ENV CGO_ENABLED=0
RUN go vet ./... \
    && go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim@${DEBIAN_DIGEST} AS runtime

LABEL org.opencontainers.image.source="https://github.com/otfabric/iec61850-interop" \
      org.opencontainers.image.description="iec61850-interop adapter on dscsystems/go-iec61850 (MMS, IEC 61850, GOOSE, Sampled Values)" \
      org.opencontainers.image.licenses="GPL-3.0-only"

COPY --from=build /out/ /usr/local/bin/

# GOOSE and Sampled Values need a raw AF_PACKET socket. File capabilities
# let the non-root adapter user open one when the container has CAP_NET_RAW
# (Docker's default set includes it). Run with --user 0 when no-new-privileges is set.
RUN apt-get update && apt-get install -y --no-install-recommends libcap2-bin \
    && setcap cap_net_raw+ep /usr/local/bin/dscsystems-goose-publisher \
    && setcap cap_net_raw+ep /usr/local/bin/dscsystems-goose-subscriber \
    && setcap cap_net_raw+ep /usr/local/bin/dscsystems-sv-publisher \
    && setcap cap_net_raw+ep /usr/local/bin/dscsystems-sv-subscriber \
    && apt-get purge -y libcap2-bin && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* \
    && addgroup --system --gid 10001 adapter \
    && adduser --system --uid 10001 --ingroup adapter --no-create-home adapter

COPY fixtures/mms/interop.json      /fixtures/mms/interop.json
COPY fixtures/iec61850/interop.icd  /fixtures/iec61850/interop.icd
COPY fixtures/iec61850/values.json  /fixtures/iec61850/values.json
COPY fixtures/goose/                /fixtures/goose/
COPY fixtures/sv/                   /fixtures/sv/
COPY adapters/dscsystems/LICENSE adapters/dscsystems/NOTICE /usr/share/doc/iec61850-interop-dscsystems/

USER adapter

ARG ADAPTER_VERSION=dev
ENV ADAPTER_VERSION=${ADAPTER_VERSION}

EXPOSE 1102

ENTRYPOINT ["dscsystems-mms-server"]
CMD ["--fixture", "/fixtures/mms/interop.json", "--port", "1102"]
