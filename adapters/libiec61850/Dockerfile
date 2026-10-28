# Multi-stage Dockerfile for the libIEC61850 MMS server adapter.
#
# Build from repository root:
#   docker buildx build --platform=linux/amd64 \
#       -f adapters/libiec61850/Dockerfile \
#       -t iec61850-interop-libiec61850:local .
#
# The adapter binaries require access to libIEC61850 private headers (Phase 0
# finding: generic MMS server API is only in src/mms/inc_private/). The source
# tree is therefore retained through the adapter-build stage.

ARG DEBIAN_DIGEST=sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818

# ---------------------------------------------------------------------------
# Stage 1: build libIEC61850 from a pinned commit
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim@${DEBIAN_DIGEST} AS libiec61850-build

ARG LIBIEC61850_REPO=https://github.com/mz-automation/libiec61850.git
# mz-automation/libiec61850 HEAD (v1.6.2.1-89-g6e265c54)
ARG LIBIEC61850_SHA=6e265c54af66ea686453b6d31c4dcce1cf7ad2bd

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      cmake \
      default-jre-headless \
      g++ \
      gcc \
      git \
      make \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN git clone "${LIBIEC61850_REPO}" libiec61850 \
    && cd libiec61850 \
    && git checkout --detach "${LIBIEC61850_SHA}" \
    && test "$(git rev-parse HEAD)" = "${LIBIEC61850_SHA}"

# The default profile compiles out the FileRename service; the MMS server
# adapter serves it (fixture files RENAME.ME). Fail if the line moves.
RUN sed -i 's/^#define MMS_RENAME_FILE_SERVICE 0$/#define MMS_RENAME_FILE_SERVICE 1/' \
      libiec61850/config/stack_config.h.cmake \
    && grep -q '^#define MMS_RENAME_FILE_SERVICE 1$' libiec61850/config/stack_config.h.cmake

WORKDIR /src/libiec61850/build
# -fvisibility=default overrides libiec61850's -fvisibility=hidden so that
# LIB61850_INTERNAL symbols remain callable from our adapter code (Fallback B,
# see SPIKE.md).  We build a static library only to avoid runtime DSO issues.
RUN cmake .. \
      -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_INSTALL_PREFIX=/opt/libiec61850 \
      -DBUILD_EXAMPLES=OFF \
      -DBUILD_TESTS=OFF \
      -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
      -DBUILD_SHARED_LIBS=OFF \
      -DCMAKE_C_FLAGS="-fvisibility=default" \
      -DCMAKE_CXX_FLAGS="-fvisibility=default" \
    && make -j"$(nproc)" \
    && make install

# ---------------------------------------------------------------------------
# Generate static IED model from the IEC 61850 fixture (Phase 2A).
# At the pinned SHA the model generator is a Java JAR (genmodel.jar).
# It writes static_model.c and static_model.h into the working directory.
# ---------------------------------------------------------------------------
COPY fixtures/iec61850/interop.icd /tmp/interop.icd
WORKDIR /tmp
RUN GENMODEL=$(find /src/libiec61850 -name 'genmodel.jar' | head -1) \
    && echo "Using genmodel.jar at: ${GENMODEL}" \
    && java -jar "${GENMODEL}" /tmp/interop.icd -ied InteropIED -ap AP1

# ---------------------------------------------------------------------------
# Stage 2: compile the adapter programs
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim@${DEBIAN_DIGEST} AS adapter-build

RUN apt-get update && apt-get install -y --no-install-recommends \
      cmake \
      g++ \
      gcc \
      libjson-c-dev \
      make \
      pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Public installed headers + static library
COPY --from=libiec61850-build /opt/libiec61850 /opt/libiec61850

# Source tree — required for private headers (mms_server_libinternal.h,
# mms_device_model.h) that are not part of the public install (see SPIKE.md).
COPY --from=libiec61850-build /src/libiec61850/src /src/libiec61850/src

# Adapter source
COPY adapters/libiec61850/src /src/adapter

# Generated IED static model (from genmodel.py in Stage 1)
COPY --from=libiec61850-build /tmp/static_model.c /src/adapter/
COPY --from=libiec61850-build /tmp/static_model.h /src/adapter/

WORKDIR /src/adapter/build
RUN cmake .. \
      -DCMAKE_BUILD_TYPE=Release \
      -DLIBIEC61850_INCLUDE_DIR=/opt/libiec61850/include/libiec61850 \
      -DLIBIEC61850_PRIVATE_DIR=/src/libiec61850/src \
      -DLIBIEC61850_LIB=/opt/libiec61850/lib/libiec61850.a \
    && make -j"$(nproc)"

# ---------------------------------------------------------------------------
# Stage 3: minimal runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim@${DEBIAN_DIGEST} AS runtime

LABEL org.opencontainers.image.source="https://github.com/otfabric/iec61850-interop" \
      org.opencontainers.image.description="iec61850-interop adapter on libiec61850 (MMS, IEC 61850, GOOSE, Sampled Values)"

RUN apt-get update && apt-get install -y --no-install-recommends \
      libjson-c5 \
    && rm -rf /var/lib/apt/lists/* \
    && addgroup --system --gid 10001 adapter \
    && adduser --system --uid 10001 --ingroup adapter --no-create-home adapter

COPY --from=adapter-build /src/adapter/build/libiec61850-mms-server   /usr/local/bin/
COPY --from=adapter-build /src/adapter/build/libiec61850-mms-client   /usr/local/bin/
COPY --from=adapter-build /src/adapter/build/libiec61850-ied-server   /usr/local/bin/
COPY --from=adapter-build /src/adapter/build/libiec61850-ied-client   /usr/local/bin/
COPY --from=adapter-build /src/adapter/build/libiec61850-ied-reporter    /usr/local/bin/
COPY --from=adapter-build /src/adapter/build/libiec61850-ied-controller /usr/local/bin/
COPY --from=adapter-build /src/adapter/build/libiec61850-goose-publisher  /usr/local/bin/
COPY --from=adapter-build /src/adapter/build/libiec61850-goose-subscriber /usr/local/bin/
COPY --from=adapter-build /src/adapter/build/libiec61850-sv-publisher     /usr/local/bin/
COPY --from=adapter-build /src/adapter/build/libiec61850-sv-subscriber    /usr/local/bin/

# GOOSE and SV need a raw AF_PACKET socket. File capabilities let the non-root
# adapter user open one when the container has CAP_NET_RAW (Docker's
# default set includes it). Run with --user 0 when no-new-privileges is set.
RUN apt-get update && apt-get install -y --no-install-recommends libcap2-bin \
    && setcap cap_net_raw+ep /usr/local/bin/libiec61850-goose-publisher \
    && setcap cap_net_raw+ep /usr/local/bin/libiec61850-goose-subscriber \
    && setcap cap_net_raw+ep /usr/local/bin/libiec61850-sv-publisher \
    && setcap cap_net_raw+ep /usr/local/bin/libiec61850-sv-subscriber \
    && apt-get purge -y libcap2-bin && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

COPY fixtures/mms/interop.json      /fixtures/mms/interop.json
COPY fixtures/iec61850/interop.icd  /fixtures/iec61850/interop.icd
COPY fixtures/iec61850/values.json  /fixtures/iec61850/values.json
COPY fixtures/goose/                /fixtures/goose/
COPY fixtures/sv/                   /fixtures/sv/

USER adapter

# Bake the image version into the container; readable at runtime via $ADAPTER_VERSION.
# Override at build time: docker buildx build --build-arg ADAPTER_VERSION=v0.2.0 ...
ARG ADAPTER_VERSION=dev
ENV ADAPTER_VERSION=${ADAPTER_VERSION}

# Default port 1102 (unprivileged).  Override with --port 102 and
# cap_add: [NET_BIND_SERVICE] when you need the standard MMS port.
EXPOSE 1102

# Default entrypoint; fixture path and port can be overridden at runtime.
ENTRYPOINT ["libiec61850-mms-server"]
CMD ["--fixture", "/fixtures/mms/interop.json", "--port", "1102"]
