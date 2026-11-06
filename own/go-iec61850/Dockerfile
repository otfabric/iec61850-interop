# SPDX-License-Identifier: MIT
#
# iec61850-interop-otfabric: OT Fabric's own IEC 61850 stack (go-iec61850 and
# the go-mms it is built on) behind the adapter contract, for testing. Not an
# independent implementation: see README.md.
#
# Build from the repository root:
#   make -f own/go-iec61850/Makefile image
#
# The build takes the go-iec61850 release as UPSTREAM_VERSION (the released
# git tag, which is the Go module version) and its commit as UPSTREAM_SHA, and
# the go-mms release as MMS_VERSION and its commit as MMS_SHA (default: the
# version go-iec61850 requires).
# There is no pin in this repository: the workflow passes the latest releases.
# A build never uses main. go.mod and go.sum here are only a starting point
# for local work.

ARG BASE_REGISTRY=docker.io/library
# golang:1.27.3-bookworm and debian:bookworm-slim, multi-arch indexes
# (linux/amd64 and linux/arm64), as versions.yaml base_images.
ARG GOLANG_DIGEST=sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5
ARG DEBIAN_DIGEST=sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818

FROM ${BASE_REGISTRY}/golang:1.27.3-bookworm@${GOLANG_DIGEST} AS build

ARG UPSTREAM_VERSION
ARG UPSTREAM_SHA
ARG MMS_VERSION=
ARG MMS_SHA=
ARG ADAPTER_VERSION=dev
ARG GOPROXY=https://proxy.golang.org

ENV CGO_ENABLED=0 GOPROXY=${GOPROXY} GOSUMDB=sum.golang.org GOTOOLCHAIN=local
WORKDIR /src
COPY own/go-iec61850/go.mod own/go-iec61850/go.sum own/go-iec61850/*.go ./

RUN set -eu; \
    test -n "${UPSTREAM_VERSION}" || { echo "UPSTREAM_VERSION is required" >&2; exit 1; }; \
    echo "${UPSTREAM_SHA}" | grep -Eq '^[0-9a-f]{40}$'; \
    go get "github.com/otfabric/go-iec61850@${UPSTREAM_VERSION}"; \
    if [ -n "${MMS_VERSION}" ]; then go get "github.com/otfabric/go-mms@${MMS_VERSION}"; fi; \
    go mod tidy; \
    built="$(go list -m -f '{{.Version}}' github.com/otfabric/go-iec61850)"; \
    test "${built}" = "${UPSTREAM_VERSION}" || { echo "go.mod has go-iec61850 ${built}, want ${UPSTREAM_VERSION}" >&2; exit 1; }; \
    if [ -n "${MMS_VERSION}" ]; then \
        mms="$(go list -m -f '{{.Version}}' github.com/otfabric/go-mms)"; \
        test "${mms}" = "${MMS_VERSION}" || { echo "go.mod has go-mms ${mms}, want ${MMS_VERSION}" >&2; exit 1; }; \
    fi; \
    go mod download && go mod verify; \
    origin="$(go mod download -json "github.com/otfabric/go-iec61850@${UPSTREAM_VERSION}" \
        | sed -nE 's/^[[:space:]]*"Hash": "([0-9a-f]{40})".*/\1/p')"; \
    if [ -n "${origin}" ] && [ "${origin}" != "${UPSTREAM_SHA}" ]; then \
        echo "go-iec61850 ${UPSTREAM_VERSION} is commit ${origin}, not ${UPSTREAM_SHA}" >&2; exit 1; \
    fi; \
    if [ -n "${MMS_SHA}" ]; then \
        echo "${MMS_SHA}" | grep -Eq '^[0-9a-f]{40}$'; \
        mmsorigin="$(go mod download -json "github.com/otfabric/go-mms@$(go list -m -f '{{.Version}}' github.com/otfabric/go-mms)" \
            | sed -nE 's/^[[:space:]]*"Hash": "([0-9a-f]{40})".*/\1/p')"; \
        if [ -n "${mmsorigin}" ] && [ "${mmsorigin}" != "${MMS_SHA}" ]; then \
            echo "go-mms is commit ${mmsorigin}, not ${MMS_SHA}" >&2; exit 1; \
        fi; \
    fi

RUN set -eu; \
    test -z "$(gofmt -l .)"; \
    go vet ./...; \
    go test ./...; \
    go build -trimpath -ldflags "-s -w \
        -X main.adapterVersion=${ADAPTER_VERSION} \
        -X main.upstreamVersion=${UPSTREAM_VERSION} \
        -X main.upstreamRevision=${UPSTREAM_SHA} \
        -X main.mmsRevision=${MMS_SHA:-unknown}" \
        -o /out/bin/iec61850-interop .; \
    for c in mms-server mms-client ied-server ied-client ied-controller ied-reporter \
             goose-publisher goose-subscriber sv-publisher sv-subscriber; do \
        ln -s iec61850-interop "/out/bin/otfabric-${c}"; \
    done; \
    mkdir -p /out/licenses/go; \
    cp "$(go env GOROOT)/LICENSE" /out/licenses/go/LICENSE; \
    go version -m /out/bin/iec61850-interop | awk '$1 == "dep" { print $2, $3 }' > /out/licenses/BUNDLED-MODULES.txt; \
    test -s /out/licenses/BUNDLED-MODULES.txt; \
    while read -r module modversion; do \
        dir="$(go list -m -f '{{.Dir}}' "${module}")"; \
        mkdir -p "/out/licenses/${module}"; \
        for file in "${dir}"/LICENSE* "${dir}"/LICENCE* "${dir}"/COPYING* "${dir}"/NOTICE*; do \
            if [ -f "${file}" ]; then cp "${file}" "/out/licenses/${module}/"; fi; \
        done; \
        test -n "$(ls "/out/licenses/${module}")" || { echo "no licence file in ${module} ${modversion}" >&2; exit 1; }; \
    done < /out/licenses/BUNDLED-MODULES.txt

FROM ${BASE_REGISTRY}/debian:bookworm-slim@${DEBIAN_DIGEST} AS runtime

ARG ADAPTER_VERSION=dev
ARG UPSTREAM_VERSION

COPY --from=build /out/bin/ /usr/local/bin/

# GOOSE and Sampled Values need a raw AF_PACKET socket. The file capability
# lets the non-root user open one when the container has CAP_NET_RAW
# (Docker's default set), as in the reference images; with
# no-new-privileges run with --user 0.
RUN apt-get update && apt-get install -y --no-install-recommends libcap2-bin \
    && setcap cap_net_raw+ep /usr/local/bin/iec61850-interop \
    && apt-get purge -y libcap2-bin && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* \
    && addgroup --system --gid 10001 adapter \
    && adduser --system --uid 10001 --ingroup adapter --no-create-home adapter

COPY --from=build /out/licenses /licenses
COPY LICENSE /licenses/iec61850-interop/LICENSE
COPY fixtures/mms/interop.json      /fixtures/mms/interop.json
COPY fixtures/iec61850/interop.icd  /fixtures/iec61850/interop.icd
COPY fixtures/iec61850/values.json  /fixtures/iec61850/values.json
COPY fixtures/goose/                /fixtures/goose/
COPY fixtures/sv/                   /fixtures/sv/

LABEL org.opencontainers.image.title="iec61850-interop-otfabric" \
      org.opencontainers.image.description="OT Fabric go-iec61850 and go-mms behind the iec61850-interop adapter contract, for testing. Not an independent implementation." \
      org.opencontainers.image.source="https://github.com/otfabric/iec61850-interop" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${UPSTREAM_VERSION}"

USER adapter
ENV ADAPTER_VERSION=${ADAPTER_VERSION}
EXPOSE 1102

ENTRYPOINT ["otfabric-mms-server"]
CMD ["--fixture", "/fixtures/mms/interop.json", "--port", "1102"]
