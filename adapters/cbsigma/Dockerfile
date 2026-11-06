# SPDX-License-Identifier: MIT
#
# iec61850-interop-cbsigma: cB-Sigma/iec61850 (MMS client, IEC 61850 client
# services, GOOSE) and cB-Sigma/iec61850-sim (the simulated IED, the server
# role) behind the adapter contract. An independent implementation.
#
# Build from the repository root:
#   make build-cbsigma
#
# Both modules are Apache-2.0, as are cB-Sigma's iotsec, iotcrypto and xsd;
# golang.org/x/net, x/sys and x/text are BSD-3-Clause, gopkg.in/yaml.v3 is
# MIT and Apache-2.0. The licence of every linked module is copied to
# /licenses (THIRD_PARTY_NOTICES.md). The pins are below, in go.mod (with
# their checksums in go.sum) and in versions.yaml; scripts/check-versions.py
# fails when they disagree. cB-Sigma publishes tags only, no releases.

ARG BASE_REGISTRY=docker.io/library
# golang:1.27.3-bookworm and debian:bookworm-slim, multi-arch indexes
# (linux/amd64 and linux/arm64), as versions.yaml base_images.
ARG GOLANG_DIGEST=sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5
ARG DEBIAN_DIGEST=sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818

FROM ${BASE_REGISTRY}/golang:1.27.3-bookworm@${GOLANG_DIGEST} AS build

# The release pins. A candidate build (scripts/build-candidate.sh) moves
# them in go.mod and here together.
ARG IEC61850_VERSION=v0.1.0
ARG IEC61850_SIM_VERSION=v0.1.0
ARG ADAPTER_VERSION=dev
ARG GOPROXY=https://proxy.golang.org

ENV CGO_ENABLED=0 GOPROXY=${GOPROXY} GOSUMDB=sum.golang.org GOTOOLCHAIN=local GOFLAGS=-mod=readonly
WORKDIR /src
COPY adapters/cbsigma/go.mod adapters/cbsigma/go.sum ./

# go.mod must name the pins of this file, and every module must match go.sum.
RUN set -eu; \
    lib="$(go list -m -f '{{.Version}}' github.com/cB-Sigma/iec61850)"; \
    simv="$(go list -m -f '{{.Version}}' github.com/cB-Sigma/iec61850-sim)"; \
    test "${lib}" = "${IEC61850_VERSION}" || { echo "go.mod has cB-Sigma/iec61850 ${lib}, want ${IEC61850_VERSION}" >&2; exit 1; }; \
    test "${simv}" = "${IEC61850_SIM_VERSION}" || { echo "go.mod has cB-Sigma/iec61850-sim ${simv}, want ${IEC61850_SIM_VERSION}" >&2; exit 1; }; \
    go mod download && go mod verify

COPY adapters/cbsigma/*.go ./
RUN set -eu; \
    test -z "$(gofmt -l .)"; \
    go vet ./...; \
    go test ./...; \
    go build -trimpath -ldflags "-s -w -X main.adapterVersion=${ADAPTER_VERSION}" -o /out/bin/iec61850-interop .; \
    for c in ied-server ied-client ied-controller ied-reporter goose-publisher goose-subscriber \
             mms-server mms-client sv-publisher sv-subscriber; do \
        ln -s iec61850-interop "/out/bin/cbsigma-${c}"; \
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

COPY --from=build /out/bin/ /usr/local/bin/

# GOOSE needs a raw AF_PACKET socket. The file capability lets the
# non-root user open one when the container has CAP_NET_RAW (Docker's
# default set), as in the other images; with no-new-privileges run with
# --user 0.
RUN apt-get update && apt-get install -y --no-install-recommends libcap2-bin \
    && setcap cap_net_raw+ep /usr/local/bin/iec61850-interop \
    && apt-get purge -y libcap2-bin && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* \
    && addgroup --system --gid 10001 adapter \
    && adduser --system --uid 10001 --ingroup adapter --no-create-home adapter

COPY --from=build /out/licenses /licenses
COPY LICENSE /licenses/iec61850-interop/LICENSE
COPY adapters/cbsigma/NOTICE /licenses/iec61850-interop/NOTICE
COPY fixtures/iec61850/interop.icd  /fixtures/iec61850/interop.icd
COPY fixtures/iec61850/values.json  /fixtures/iec61850/values.json
COPY fixtures/goose/                /fixtures/goose/

LABEL org.opencontainers.image.title="iec61850-interop-cbsigma" \
      org.opencontainers.image.description="cB-Sigma/iec61850 and iec61850-sim behind the iec61850-interop adapter contract: IED server, client, controller, reporter and GOOSE" \
      org.opencontainers.image.source="https://github.com/otfabric/iec61850-interop" \
      org.opencontainers.image.licenses="MIT AND Apache-2.0 AND BSD-3-Clause" \
      org.opencontainers.image.version="${ADAPTER_VERSION}"

USER adapter
ENV ADAPTER_VERSION=${ADAPTER_VERSION}
EXPOSE 1102

ENTRYPOINT ["cbsigma-ied-server"]
CMD ["--port", "1102"]
