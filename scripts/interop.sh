#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Cross-stack self-test of the adapter images.
#
# Usage: interop.sh [adapter ...]     default: the Makefile's ADAPTERS
#
# The first adapter is the hub (libiec61850 by default): every other
# adapter's MMS and IED clients run against the hub's servers, and the hub's
# clients against the other adapter's servers, for the roles both declare
# (--capabilities). An IED client's operations and CDC values are checked,
# a controller's operate and the stVal that follows, every other client's
# JSON Lines; a reporter that takes --trigger gi (contract 1.3) runs once
# more with it, and the general interrogation report is checked. GOOSE and Sampled Values run between
# every publisher and every subscriber of the adapters that have them
# (scripts/goose-crosscheck.sh, scripts/sv-crosscheck.sh), and once more
# without flags and from the host side of the bridge for each stack. Known
# limitations (fixtures/capabilities.json) are checked to still fail: when
# one passes it has to be removed.
#
# These results are about the stacks of the images it runs: the references,
# and an image named by ADAPTER_IMAGE_<adapter> (scripts/lib.sh), never a
# consumer's test code. Every check goes to REPORT_DIR/results.json
# (interop-result-v1, default reports/). Exit code: 0 if all checks pass,
# 1 if any fails or none passes.
#
# Environment: VERSION, REGISTRY, ADAPTER_IMAGE_<adapter>, REPORT_DIR.

set -euo pipefail
# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

ADAPTERS=("$@")
[[ ${#ADAPTERS[@]} -gt 0 ]] || read -r -a ADAPTERS <<<"$(make -s -C "${ROOT}" print-adapters)"
trap results_abort EXIT
results_open interop "${REPORT_DIR:-${ROOT}/reports}" "scripts/interop.sh ${ADAPTERS[*]}"

declare -A CAPS
for a in "${ADAPTERS[@]}"; do
    CAPS[$a]="$(capabilities "$a")"
    [[ -n "${CAPS[$a]}" ]] || { echo "no --capabilities from $(image_of "$a")" >&2; exit 2; }
    record_implementation "$a" "${CAPS[$a]}"
    info "$a: $(image_of "$a"): $(jq -r '.commands | join(" ")' <<<"${CAPS[$a]}")"
done
has() { has_command "${CAPS[$1]}" "$1-$2"; }
# gi <adapter>: its reporter takes --trigger gi (contract 1.3).
gi() { [[ " $(reporter_triggers "${CAPS[$1]}") " == *" gi "* ]]; }

HUB="${ADAPTERS[0]}"
info ""
info "=== MMS and IED clients and servers, against ${HUB} ==="
pairs="mms-client:mms-server ied-client:ied-server ied-reporter:ied-server ied-controller:ied-server"
for a in "${ADAPTERS[@]:1}"; do
    for p in ${pairs}; do
        client="${p%%:*}" server="${p##*:}"
        if has "$a" "$client" && has "${HUB}" "$server"; then
            output=$(run_client_pair "$(image_of "${HUB}")" "${HUB}-${server}" "$(image_of "$a")" "$a-${client}")
            client_checks "$a-${client} -> ${HUB}-${server}" "$a-${client}" "$output"
            if [ "$client" = ied-reporter ] && gi "$a"; then
                report_gi_pair "$a-${client} --trigger gi -> ${HUB}-${server}" \
                    "$(image_of "${HUB}")" "${HUB}-${server}" "$(image_of "$a")" "$a-${client}"
            fi
        fi
        if has "${HUB}" "$client" && has "$a" "$server"; then
            output=$(run_client_pair "$(image_of "$a")" "$a-${server}" "$(image_of "${HUB}")" "${HUB}-${client}")
            client_checks "${HUB}-${client} -> $a-${server}" "${HUB}-${client}" "$output"
            if [ "$client" = ied-reporter ] && gi "${HUB}"; then
                report_gi_pair "${HUB}-${client} --trigger gi -> $a-${server}" \
                    "$(image_of "$a")" "$a-${server}" "$(image_of "${HUB}")" "${HUB}-${client}"
            fi
        fi
    done
done

# at_cross <goose|sv> <publisher command> <subscriber command> <control block>:
# the scenario of a crosscheck. The roles are publisher and subscriber.
at_cross() {
    at "$1.$(slug "$4")$([ "${FLAGS:-}" = plain ] && echo .plain)$([ "${HOST_SIDE:-}" = 1 ] && echo .host-side)" "$(stack "$2")" "$(stack "$3")"
}
goose_check() {
    local out
    at_cross goose "$2" "$4" "$5"
    if out=$("$SCRIPTS/goose-crosscheck.sh" "$@" 2>&1); then pass "${out#PASS }"; else fail "$out"; fi
}
sv_check() {
    local out
    at_cross sv "$2" "$4" "$5"
    if out=$("$SCRIPTS/sv-crosscheck.sh" "$@" 2>&1); then pass "${out#PASS }"; else fail "$out"; fi
}

# l2 <goose|sv>: "<image> <adapter>" of every adapter with both roles.
l2() {
    local a
    for a in "${ADAPTERS[@]}"; do
        if has "$a" "$1-publisher" && has "$a" "$1-subscriber"; then echo "$(image_of "$a") $a"; fi
    done
}

info ""
info "=== GOOSE publisher -> subscriber (L2, Docker bridge) ==="
mapfile -t STACKS < <(l2 goose)
# Every publisher -> every subscriber, all flags set.
for pub in "${STACKS[@]}"; do
    for sub in "${STACKS[@]}"; do
        set -- $pub $sub
        goose_check "$1" "$2-goose-publisher" "$3" "$4-goose-subscriber" gcbAll
    done
done
# No flags, and from the host side of the bridge (a Go test on a Linux
# host), each stack once as publisher and once as subscriber.
n=${#STACKS[@]}
for ((i = 0; i < n && n > 1; i++)); do
    set -- ${STACKS[$i]} ${STACKS[$(( (i + 1) % n ))]}
    FLAGS=plain goose_check "$1" "$2-goose-publisher" "$3" "$4-goose-subscriber" gcbBool
    HOST_SIDE=1 goose_check "$1" "$2-goose-publisher" "$3" "$4-goose-subscriber" gcbAll
done

info ""
info "=== Sampled Values publisher -> subscriber (L2, Docker bridge) ==="
# Pairs that fail for a known limitation (fixtures/capabilities.json): the
# check must fail with nothing received. When one starts to pass, the
# limitation is gone and has to be removed here and in capabilities.json.
SV_SMPMOD_WAIVER=libiec61850-sv-two-octet-smpmod
SV_SMPMOD_EVIDENCE=https://github.com/mz-automation/libiec61850/blob/6e265c54af66ea686453b6d31c4dcce1cf7ad2bd/src/sampled_values/sv_subscriber.c#L525-L528
sv_known() {
    local out
    at_cross sv "$2" "$4" "$5"
    if out=$("$SCRIPTS/sv-crosscheck.sh" "$@" 2>&1); then
        waiver "${SV_SMPMOD_WAIVER}" subscriber@libiec61850 "${SV_SMPMOD_EVIDENCE}" false
        fail "known limitation no longer reproduces, update capabilities.json: ${out#PASS }"
    elif echo "$out" | grep -q "received 0 of"; then
        out="${out%%:*}"
        waiver "${SV_SMPMOD_WAIVER}" subscriber@libiec61850 "${SV_SMPMOD_EVIDENCE}" true
        echo -e "${YELLOW}KNOWN${RESET} known limitation reproduced: ${out#FAIL }"
        PASS=$((PASS + 1))
        result known-difference "known limitation reproduced (fixtures/capabilities.json): ${out#FAIL }" "${SV_SMPMOD_WAIVER}"
    else
        fail "$out"
    fi
}
# libiec61850's subscriber drops an ASDU whose smpMod is two octets, which
# its own publisher, dscsystems' and otfabric's send: the generic stream
# (it carries smpMod) cannot reach it from them.
sv_generic_known() {
    case "$1/$2" in
        libiec61850/libiec61850|dscsystems/libiec61850|otfabric/libiec61850) return 0 ;;
    esac
    return 1
}
mapfile -t STACKS < <(l2 sv)
# Every publisher -> every subscriber: the 9-2LE stream with --simulation
# and smpSynch 2, the generic stream plain.
for pub in "${STACKS[@]}"; do
    for sub in "${STACKS[@]}"; do
        set -- $pub $sub
        FLAGS=all sv_check "$1" "$2-sv-publisher" "$3" "$4-sv-subscriber" MSVCB01
        if sv_generic_known "$2" "$4"; then
            sv_known "$1" "$2-sv-publisher" "$3" "$4-sv-subscriber" MSVCB02
        else
            sv_check "$1" "$2-sv-publisher" "$3" "$4-sv-subscriber" MSVCB02
        fi
    done
done
# From the host side of the bridge, each stack once as publisher and once
# as subscriber.
n=${#STACKS[@]}
for ((i = 0; i < n && n > 1; i++)); do
    set -- ${STACKS[$i]} ${STACKS[$(( (i + 1) % n ))]}
    HOST_SIDE=1 sv_check "$1" "$2-sv-publisher" "$3" "$4-sv-subscriber" MSVCB01
done

echo ""
echo -e "Results: ${GREEN}${PASS} passed${RESET}  ${RED}${FAIL} failed${RESET}"
results_close || FAIL=$((FAIL + 1))
[ $FAIL -eq 0 ]
