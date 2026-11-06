#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Contract smoke test of one adapter image.
#
# Usage: smoke.sh <adapter>          e.g. smoke.sh libiec61850
#
# Checks what one image can show of docs/CONTAINER_CONTRACT.md: every
# command its --capabilities lists exists, answers --version and
# --capabilities with the same command list, the fixtures of its roles are
# in the image; each server command emits a valid ready event; each client
# command runs its sequence against the same stack's server (JSON Lines; the
# IED client's operations and CDC values); and ied-client's
# --local-tsel/--remote-tsel against SMOKE_SERVER's ied-server (default
# libiec61850), or the unsupported answer (exit 4) a stack may give.
#
# Every check goes to REPORT_DIR/results.json (interop-result-v1, default
# reports/). Exit code: 0 if all checks pass, 1 if any fails or none passes.
#
# Environment: VERSION, REGISTRY, ADAPTER_IMAGE_<adapter> (scripts/lib.sh),
#              REPORT_DIR, SMOKE_SERVER.

set -euo pipefail
# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

A="${1:?usage: smoke.sh <adapter>}"
IMAGE="$(image_of "$A")"
SMOKE_SERVER="${SMOKE_SERVER:-libiec61850}"
trap results_abort EXIT
results_open "smoke-$A" "${REPORT_DIR:-${ROOT}/reports}" "scripts/smoke.sh $A"
info "${A}: ${IMAGE}"

at "capabilities.$A" "" "$A"
CAPS="$(capabilities "$A")"
if jq -e '.event == "capabilities" and (.commands | type == "array" and length > 0)' <<<"${CAPS}" >/dev/null 2>&1; then
    pass "$A-ied-server --capabilities: $(jq -r '.commands | join(" ")' <<<"${CAPS}")"
else
    fail "$A-ied-server --capabilities: ${CAPS:-no output}"
    exit 1
fi
record_implementation "$A" "${CAPS}"
mapfile -t COMMANDS < <(jq -r '.commands[]' <<<"${CAPS}")
has() { has_command "${CAPS}" "$A-$1"; }

info ""
info "=== Commands ==="
for cmd in "${COMMANDS[@]}"; do
    at "binary.${cmd}" "" "$A"
    if docker run --rm --entrypoint sh "${IMAGE}" -c "command -v ${cmd}" >/dev/null 2>&1; then
        pass "binary ${cmd} in ${IMAGE}"
    else
        fail "binary ${cmd} MISSING in ${IMAGE}"
        continue
    fi
    at "capabilities.$(slug "${cmd}")" "" "$A"
    out=$(docker run --rm --entrypoint "${cmd}" "${IMAGE}" --capabilities 2>/dev/null | head -1 || true)
    if [[ "$(jq -c '.commands | sort' <<<"${out}" 2>/dev/null)" == "$(jq -c '.commands | sort' <<<"${CAPS}")" ]]; then
        pass "${cmd} --capabilities lists the image's commands"
    else
        fail "${cmd} --capabilities: ${out}"
    fi
    at "version.$(slug "${cmd}")" "" "$A"
    out=$(docker run --rm --entrypoint "${cmd}" "${IMAGE}" --version 2>/dev/null | head -1 || true)
    if jq -e '.event == "version" and (.adapterVersion | type == "string" and length > 0)' <<<"${out}" >/dev/null 2>&1; then
        pass "${cmd} --version: $(jq -r .adapterVersion <<<"${out}")"
    else
        fail "${cmd} --version: ${out}"
    fi
done

info ""
info "=== Fixture files ==="
FIXTURES=()
has mms-server && FIXTURES+=(/fixtures/mms/interop.json)
has ied-server && FIXTURES+=(/fixtures/iec61850/interop.icd /fixtures/iec61850/values.json)
if has goose-publisher || has goose-subscriber; then FIXTURES+=(/fixtures/goose/goose.json /fixtures/goose/interop-goose.cid); fi
if has sv-publisher || has sv-subscriber; then FIXTURES+=(/fixtures/sv/sv.json /fixtures/sv/interop-sv.cid); fi
for f in "${FIXTURES[@]}"; do
    at "fixture.$(slug "${f#/fixtures/}")" "" "$A"
    if docker run --rm --entrypoint sh "${IMAGE}" -c "test -f $f" >/dev/null 2>&1; then
        pass "fixture $f in ${IMAGE}"
    else
        fail "fixture $f MISSING in ${IMAGE}"
    fi
done

info ""
info "=== Server ready events ==="
port=$((19000 + RANDOM % 900))
for role in mms-server ied-server; do
    has "$role" || continue
    ready=$(start_server "${IMAGE}" "$A-$role" "${port}")
    check_ready_event "$A-$role" "$ready"
    port=$((port + 1))
done

info ""
info "=== Clients against the same stack ==="
for pair in "mms-client mms-server" "ied-client ied-server" "ied-reporter ied-server" "ied-controller ied-server"; do
    set -- $pair
    has "$1" && has "$2" || continue
    output=$(run_client_pair "${IMAGE}" "$A-$2" "${IMAGE}" "$A-$1")
    client_checks "$A-$1" "$A-$1" "$output"
done

if has ied-client; then
    info ""
    info "=== ied-client --local-tsel / --remote-tsel ==="
    # A client that can set its COTP T-selectors associates with them (the
    # libiec61850 server accepts any selector) and takes an invalid value as
    # an argument error (exit 1); a stack that cannot answers unsupported
    # (exit 4) to both flags.
    SERVER_IMAGE="$(image_of "${SMOKE_SERVER}")"
    code=0
    out=$(docker run --rm --entrypoint "$A-ied-client" "${IMAGE}" --remote-tsel 0001 --host 192.0.2.1 2>/dev/null) || code=$?
    at "tsel.$A-ied-client" "$A" ""
    if [ "$code" = 4 ]; then
        if echo "$out" | grep -q '"code":"unsupported"'; then pass "$A-ied-client: --remote-tsel unsupported (exit 4)"
        else fail "$A-ied-client: --remote-tsel exit 4 without the unsupported event: $out"; fi
    else
        for tsel in "01 01" "0001 00000001"; do
            set -- $tsel
            output=$(SMOKE_CLIENT_ARGS="--local-tsel $1 --remote-tsel $2" \
                run_client_pair "${SERVER_IMAGE}" "${SMOKE_SERVER}-ied-server" "${IMAGE}" "$A-ied-client")
            check_ied_client_ops "$A-ied-client --local-tsel $1 --remote-tsel $2 -> ${SMOKE_SERVER}-ied-server" "$output"
        done
        code=0
        docker run --rm --entrypoint "$A-ied-client" "${IMAGE}" --remote-tsel 0000000001 >/dev/null 2>&1 || code=$?
        at "tsel.$A-ied-client.invalid" "$A" ""
        if [ "$code" = 1 ]; then pass "$A-ied-client: 5-octet --remote-tsel exits 1"
        else fail "$A-ied-client: 5-octet --remote-tsel exit $code, want 1"; fi
    fi
fi

echo ""
echo -e "Results: ${GREEN}${PASS} passed${RESET}  ${RED}${FAIL} failed${RESET}"
results_close || FAIL=$((FAIL + 1))
[ $FAIL -eq 0 ]
