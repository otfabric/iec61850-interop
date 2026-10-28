# Multi-stage Dockerfile for the csp0924/iec61850-rust adapter.
#
# csp0924/iec61850-rust is MIT OR Apache-2.0 (LICENSE-MIT, LICENSE-APACHE,
# NOTICE); the adapter code in adapters/iec61850-rust/ is MIT.
#
# Build from repository root:
#   docker buildx build --platform=linux/amd64 \
#       -f adapters/iec61850-rust/Dockerfile \
#       -t mms-interop-iec61850-rust:local .
#
# The upstream pin is the `rev` of the iec61850-* git dependencies in
# adapters/iec61850-rust/Cargo.toml, locked in Cargo.lock
# (IEC61850_RUST_SHA below documents the same commit and is checked
# against both).

ARG RUST_DIGEST=sha256:6258907abe69656e41cd992e0b705cdcfabcbbe3db374f92ed2d47121282d4a1
ARG DEBIAN_DIGEST=sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818

# ---------------------------------------------------------------------------
# Stage 1: build the adapter programs
# ---------------------------------------------------------------------------
FROM rust:1.95.0-bookworm@${RUST_DIGEST} AS build

# csp0924/iec61850-rust v0.2.0 (v0.2.0-0-gd4e8952)
ARG IEC61850_RUST_SHA=d4e895217f367ed4caebf21611d2c16ee46035c0

WORKDIR /src
COPY adapters/iec61850-rust/Cargo.toml adapters/iec61850-rust/Cargo.lock ./
RUN n_dep=$(grep -c 'git = "https://github.com/csp0924/iec61850-rust"' Cargo.toml) \
    && n_rev=$(grep -c "git = \"https://github.com/csp0924/iec61850-rust\", rev = \"${IEC61850_RUST_SHA}\"" Cargo.toml) \
    && [ "$n_dep" -gt 0 ] && [ "$n_dep" = "$n_rev" ] \
    && ! grep 'source = "git+https://github.com/csp0924/iec61850-rust' Cargo.lock | grep -qv "#${IEC61850_RUST_SHA}\"" \
    || { echo "Cargo.toml/Cargo.lock do not pin csp0924/iec61850-rust to ${IEC61850_RUST_SHA}" >&2; exit 1; }
COPY adapters/iec61850-rust/src ./src
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    cargo build --release --locked \
    && mkdir -p /out \
    && for b in mms-server mms-client ied-server ied-client ied-controller ied-reporter \
                goose-publisher goose-subscriber; do \
         cp "target/release/iec61850-rust-$b" /out/; \
       done

# ---------------------------------------------------------------------------
# Stage 2: minimal runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim@${DEBIAN_DIGEST} AS runtime

LABEL org.opencontainers.image.source="https://github.com/otfabric/mms-interop" \
      org.opencontainers.image.description="mms-interop adapter on csp0924/iec61850-rust (MMS, IEC 61850, GOOSE)" \
      org.opencontainers.image.licenses="MIT AND (MIT OR Apache-2.0)"

COPY --from=build /out/ /usr/local/bin/

# GOOSE needs a raw AF_PACKET socket. File capabilities let the non-root
# adapter user open one when the container has CAP_NET_RAW (Docker's
# default set includes it). Run with --user 0 when no-new-privileges is set.
RUN apt-get update && apt-get install -y --no-install-recommends libcap2-bin \
    && setcap cap_net_raw+ep /usr/local/bin/iec61850-rust-goose-publisher \
    && setcap cap_net_raw+ep /usr/local/bin/iec61850-rust-goose-subscriber \
    && apt-get purge -y libcap2-bin && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* \
    && addgroup --system --gid 10001 adapter \
    && adduser --system --uid 10001 --ingroup adapter --no-create-home adapter

COPY fixtures/mms/interop.json      /fixtures/mms/interop.json
COPY fixtures/iec61850/interop.icd  /fixtures/iec61850/interop.icd
COPY fixtures/iec61850/values.json  /fixtures/iec61850/values.json
COPY fixtures/goose/                /fixtures/goose/
COPY adapters/iec61850-rust/LICENSE-MIT adapters/iec61850-rust/LICENSE-APACHE adapters/iec61850-rust/NOTICE \
     /usr/share/doc/mms-interop-iec61850-rust/

USER adapter

ARG ADAPTER_VERSION=dev
ENV ADAPTER_VERSION=${ADAPTER_VERSION}

EXPOSE 1102

ENTRYPOINT ["iec61850-rust-mms-server"]
CMD ["--fixture", "/fixtures/mms/interop.json", "--port", "1102"]
