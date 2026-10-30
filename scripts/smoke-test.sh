#!/usr/bin/env bash
# smoke-test.sh — adapter self-tests for iec61850-interop.
#
# Verifies that each adapter command:
#   1. Starts without error.
#   2. Emits a valid JSON ready event (for server commands).
#   3. Emits valid JSON Lines output (for client commands).
#   4. Exits with the expected code.
#   5. Ready event contains the expected fields: event, address, fixture, adapter, version.
#
# Usage:
#   LIBIEC61850_IMAGE=iec61850-interop-libiec61850:local \
#   IEC61850BEAN_IMAGE=iec61850-interop-iec61850bean:local \
#   DSCSYSTEMS_IMAGE=iec61850-interop-dscsystems:local \
#   IEC61850RUST_IMAGE=iec61850-interop-iec61850-rust:local \
#   ./scripts/smoke-test.sh
#
# Exit code: 0 if all checks pass, 1 if any check fails or none passes.

set -euo pipefail

LIBIEC61850_IMAGE="${LIBIEC61850_IMAGE:-iec61850-interop-libiec61850:local}"
IEC61850BEAN_IMAGE="${IEC61850BEAN_IMAGE:-iec61850-interop-iec61850bean:local}"
DSCSYSTEMS_IMAGE="${DSCSYSTEMS_IMAGE:-iec61850-interop-dscsystems:local}"
IEC61850RUST_IMAGE="${IEC61850RUST_IMAGE:-iec61850-interop-iec61850-rust:local}"
SCRIPTS="$(cd "$(dirname "$0")" && pwd)"

# Every check also goes to results.json (interop-result-v1), in REPORT_DIR
# (default reports/). A run with no pass, or a waiver that no longer reproduces, fails.
# shellcheck source=scripts/results.sh
source "$(dirname "${BASH_SOURCE[0]}")/results.sh"
trap results_abort EXIT
results_open smoke "${REPORT_DIR:-$(dirname "${SCRIPTS}")/reports}" "./scripts/smoke-test.sh"
export RESULT_IMAGE="${LIBIEC61850_IMAGE/libiec61850/\{\}}"

PASS=0
FAIL=0

# Colours (suppressed when not a tty)
if [ -t 1 ]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; RESET='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; RESET=''
fi

pass() { echo -e "${GREEN}PASS${RESET} $*"; PASS=$((PASS + 1)); result pass; }
fail() { echo -e "${RED}FAIL${RESET} $*"; FAIL=$((FAIL + 1)); result fail "$*"; }
info() { echo -e "${YELLOW}INFO${RESET} $*"; }

# stack <command>: the implementation of an adapter command, by its prefix.
stack() { echo "$1" | sed -E 's/-(mms|ied|goose|sv)-.*$//'; }
# image_stack <image>: the implementation of an image.
image_stack() {
    case "$1" in
        "$LIBIEC61850_IMAGE") echo libiec61850 ;;
        "$IEC61850BEAN_IMAGE") echo iec61850bean ;;
        "$DSCSYSTEMS_IMAGE") echo dscsystems ;;
        "$IEC61850RUST_IMAGE") echo iec61850-rust ;;
        *) echo "$1" ;;
    esac
}
# slug <text>: the text as a part of a scenario id.
slug() { echo "$1" | tr 'A-Z' 'a-z' | sed -E 's/ -> /./g; s/[^a-z0-9._-]+/-/g; s/^-+//; s/-+$//'; }
# at_label <kind> <label>: the scenario of a check whose label is an adapter
# command, or "<client command> -> <server command>".
at_label() {
    local client="${2%% -> *}" server="${2##* -> }"
    at "$1.$(slug "$2")" "$(stack "${client}")" "$(stack "${server}")"
}

# ---------------------------------------------------------------------------
# Helper: start a server container, capture the ready line, return it.
# Usage: ready_line=$(start_server <image> <command> <port> [extra args...])
# The container is stopped and removed automatically on function return.
# ---------------------------------------------------------------------------
start_server() {
    local image="$1" cmd="$2" host_port="$3"
    shift 3
    local extra=("$@")

    local cid
    cid=$(docker run -d \
        -p "${host_port}:1102" \
        --entrypoint "${cmd}" \
        "${image}" \
        --port 1102 \
        "${extra[@]+"${extra[@]}"}" \
    )

    # Wait up to 30 s for the ready event.
    local ready=""
    local deadline=$((SECONDS + 30))
    while [ $SECONDS -lt $deadline ]; do
        ready=$(docker logs "$cid" 2>/dev/null | grep '"event":"ready"' | head -1 || true)
        [ -n "$ready" ] && break
        sleep 0.2
    done

    docker stop "$cid" >/dev/null 2>&1
    docker rm   "$cid" >/dev/null 2>&1

    printf '%s' "$ready"
}

# ---------------------------------------------------------------------------
# Helper: run a client container against a server and capture stdout.
# Usage: output=$(run_client <net> <image> <command> <host> [extra args...])
# ---------------------------------------------------------------------------
run_client_pair() {
    local image_server="$1" cmd_server="$2"
    local image_client="$3" cmd_client="$4"
    local net="smoke-$$"

    docker network create "$net" >/dev/null 2>&1

    # Start server.
    local sid
    sid=$(docker run -d \
        --network "$net" \
        --name "smoke-server-$$" \
        --entrypoint "${cmd_server}" \
        "${image_server}" \
        --port 1102)

    # Wait for server ready.
    local deadline=$((SECONDS + 30))
    while [ $SECONDS -lt $deadline ]; do
        docker logs "$sid" 2>/dev/null | grep -q '"event":"ready"' && break
        sleep 0.2
    done

    # Run client. A failed association right after the server's ready
    # event is retried (up to three tries): on a busy runner the new
    # container's name or port can lag the event by a moment. Any other
    # outcome, and a third failed association, is the result.
    local output try
    for try in 1 2 3; do
        output=$(docker run --rm \
            --network "$net" \
            --entrypoint "${cmd_client}" \
            "${image_client}" \
            --host "smoke-server-$$" --port 1102 \
            2>/dev/null || true)
        echo "$output" | grep -q '"operation":"associate","ok":false' || break
        echo "${cmd_client} -> ${cmd_server}: association failed (try ${try}): $(echo "$output" | head -1)" >&2
        sleep 1
    done

    docker stop "$sid" >/dev/null 2>&1
    docker rm   "$sid" >/dev/null 2>&1
    docker network rm "$net" >/dev/null 2>&1

    printf '%s' "$output"
}

# ---------------------------------------------------------------------------
# Check: ready event JSON has required fields with non-empty values.
# ---------------------------------------------------------------------------
check_ready_event() {
    local label="$1" ready="$2"
    at_label ready "${label}"

    if [ -z "$ready" ]; then
        fail "${label}: no ready event received within 30 s"
        return
    fi

    # Validate JSON syntax.
    if ! echo "$ready" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
        fail "${label}: ready event is not valid JSON: ${ready}"
        return
    fi

    # Check required fields.
    local missing=()
    for field in event address fixture adapter version; do
        value=$(echo "$ready" | python3 -c \
            "import sys,json; d=json.load(sys.stdin); print(d.get('${field}',''))" 2>/dev/null)
        [ -z "$value" ] && missing+=("$field")
    done

    if [ ${#missing[@]} -gt 0 ]; then
        fail "${label}: ready event missing fields: ${missing[*]}"
        fail "  got: ${ready}"
    else
        pass "${label}: ready event OK — $(echo "$ready" | python3 -c \
            "import sys,json; d=json.load(sys.stdin); \
             print('adapter={}, fixture={}, version={}'.format(d['adapter'],d['fixture'],d['version']))")"
    fi
}

# ---------------------------------------------------------------------------
# Check: client output is non-empty valid JSON Lines.
# ---------------------------------------------------------------------------
check_json_lines() {
    local label="$1" output="$2"
    at_label json-lines "${label}"

    if [ -z "$output" ]; then
        fail "${label}: no JSON Lines output"
        return
    fi

    local bad=0
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        if ! echo "$line" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
            bad=$((bad + 1))
            fail "${label}: invalid JSON line: ${line}"
        fi
    done <<< "$output"

    [ $bad -eq 0 ] && pass "${label}: JSON Lines OK ($(echo "$output" | grep -c .) lines)"
}

# ---------------------------------------------------------------------------
# Check: IED client sequence is complete and successful for smoke purposes.
#
# Same-stack servers historically deny ST writes to Mod.stVal (SCL status FC);
# reverse tests against go-iec61850 validate that write. Smoke requires every
# non-write operation to succeed, and accepts write ok:true or access-denied.
# ---------------------------------------------------------------------------
check_ied_client_ops() {
    local label="$1" output="$2"
    at_label ied-ops "${label}"
    if ! echo "$output" | grep -q '"operation":"associate"'; then
        fail "${label}: missing associate operation"
        return
    fi
    if ! echo "$output" | grep -q '"operation":"conclude"'; then
        fail "${label}: missing conclude operation; output: $(echo "$output" | head -3 | tr '\n' ' ')"
        return
    fi
    local bad
    bad=$(echo "$output" | python3 -c '
import sys, json
bad = []
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        o = json.loads(line)
    except Exception:
        bad.append("invalid-json")
        continue
    if o.get("ok", True):
        continue
    op = o.get("operation", "?")
    err = str(o.get("error", "")).lower()
    if op == "write" and ("access" in err or "denied" in err or "violation" in err):
        continue
    bad.append("%s:%s" % (op, o.get("error", "")))
if bad:
    print("; ".join(bad))
' 2>/dev/null || true)
    if [ -n "$bad" ]; then
        fail "${label}: unexpected ok:false — ${bad}"
        return
    fi
    pass "${label}: associate/conclude present; ops ok (ST write access-denied allowed)"
}

# ---------------------------------------------------------------------------
# Check: the client read the values.json values of the CDC extras
# GGIO1.DPS1.stVal (Dbpos, reported as its enumeration: 2 = on) and
# MMTR1.TotVAh.actVal (INT32U, 42) — the server seeded them.
# ---------------------------------------------------------------------------
check_cdc_values() {
    local label="$1" output="$2" got
    at_label cdc-values "${label}"
    got=$(echo "$output" | python3 -c '
import sys, json
want = {"GGIO1.DPS1.stVal[ST]": 2, "MMTR1.TotVAh.actVal[ST]": 42}
seen = {}
for line in sys.stdin:
    try:
        o = json.loads(line)
    except Exception:
        continue
    if o.get("operation") != "read":
        continue
    for k in want:
        if str(o.get("target", "")).endswith("/" + k):
            seen[k] = o.get("value") if o.get("ok") else "ok:false"
bad = ["%s=%r" % (k, seen.get(k, "missing")) for k in want if seen.get(k) != want[k]]
print("; ".join(bad))
' 2>/dev/null || echo "parse error")
    if [ -n "$got" ]; then
        fail "${label}: CDC values wrong (want DPS1.stVal=2, TotVAh.actVal=42): ${got}"
    else
        pass "${label}: DPS1.stVal=2, TotVAh.actVal=42"
    fi
}

# ---------------------------------------------------------------------------
# Check: required binary exists inside image.
# ---------------------------------------------------------------------------
check_binary() {
    local image="$1" bin="$2"
    at "binary.${bin}" "" "$(image_stack "${image}")"
    if docker run --rm --entrypoint sh "${image}" -c "command -v ${bin}" >/dev/null 2>&1; then
        pass "binary ${bin} in ${image}"
    else
        fail "binary ${bin} MISSING in ${image}"
    fi
}

# ---------------------------------------------------------------------------
# Check: fixture file exists inside image.
# ---------------------------------------------------------------------------
check_fixture_file() {
    local image="$1" path="$2"
    at "fixture.$(slug "${path#/fixtures/}")" "" "$(image_stack "${image}")"
    if docker run --rm --entrypoint sh "${image}" -c "test -f ${path}" >/dev/null 2>&1; then
        pass "fixture ${path} in ${image}"
    else
        fail "fixture ${path} MISSING in ${image}"
    fi
}

# ===========================================================================
# Tests
# ===========================================================================

info "=== Binary presence ==="
check_binary "$LIBIEC61850_IMAGE" libiec61850-mms-server
check_binary "$LIBIEC61850_IMAGE" libiec61850-mms-client
check_binary "$LIBIEC61850_IMAGE" libiec61850-ied-server
check_binary "$LIBIEC61850_IMAGE" libiec61850-ied-client
check_binary "$LIBIEC61850_IMAGE" libiec61850-ied-reporter
check_binary "$IEC61850BEAN_IMAGE" iec61850bean-ied-server
check_binary "$IEC61850BEAN_IMAGE" iec61850bean-ied-client
check_binary "$LIBIEC61850_IMAGE" libiec61850-goose-publisher
check_binary "$LIBIEC61850_IMAGE" libiec61850-goose-subscriber
check_binary "$LIBIEC61850_IMAGE" libiec61850-sv-publisher
check_binary "$LIBIEC61850_IMAGE" libiec61850-sv-subscriber
for cmd in mms-server mms-client ied-server ied-client ied-controller ied-reporter \
           goose-publisher goose-subscriber sv-publisher sv-subscriber; do
    check_binary "$DSCSYSTEMS_IMAGE" "dscsystems-$cmd"
    check_binary "$IEC61850RUST_IMAGE" "iec61850-rust-$cmd"
done

info ""
info "=== Capabilities declare IED clients ==="
check_capabilities_command() {
    local image="$1" entrypoint="$2" required="$3"
    local out
    at "capabilities.${entrypoint}.${required}" "" "$(image_stack "${image}")"
    out=$(docker run --rm --entrypoint "$entrypoint" "$image" --capabilities 2>/dev/null || true)
    if echo "$out" | grep -q "\"$required\""; then
        pass "${entrypoint} --capabilities includes ${required}"
    else
        fail "${entrypoint} --capabilities missing ${required}: ${out}"
    fi
}
check_capabilities_command "$LIBIEC61850_IMAGE" libiec61850-ied-client libiec61850-ied-client
check_capabilities_command "$IEC61850BEAN_IMAGE" iec61850bean-ied-client iec61850bean-ied-client
check_capabilities_command "$LIBIEC61850_IMAGE" libiec61850-goose-publisher libiec61850-goose-subscriber
check_capabilities_command "$DSCSYSTEMS_IMAGE" dscsystems-ied-client dscsystems-ied-client
check_capabilities_command "$DSCSYSTEMS_IMAGE" dscsystems-goose-subscriber dscsystems-goose-publisher
check_capabilities_command "$IEC61850RUST_IMAGE" iec61850-rust-ied-client iec61850-rust-ied-client
check_capabilities_command "$IEC61850RUST_IMAGE" iec61850-rust-goose-subscriber iec61850-rust-goose-publisher
check_capabilities_command "$LIBIEC61850_IMAGE" libiec61850-sv-publisher libiec61850-sv-subscriber
check_capabilities_command "$DSCSYSTEMS_IMAGE" dscsystems-sv-subscriber dscsystems-sv-publisher
check_capabilities_command "$IEC61850RUST_IMAGE" iec61850-rust-sv-subscriber iec61850-rust-sv-publisher

info ""
info "=== Fixture files ==="
check_fixture_file "$LIBIEC61850_IMAGE" /fixtures/mms/interop.json
check_fixture_file "$LIBIEC61850_IMAGE" /fixtures/iec61850/interop.icd
check_fixture_file "$LIBIEC61850_IMAGE" /fixtures/iec61850/values.json
check_fixture_file "$IEC61850BEAN_IMAGE" /fixtures/iec61850/interop.icd
check_fixture_file "$IEC61850BEAN_IMAGE" /fixtures/iec61850/values.json
for f in /fixtures/goose/goose.json /fixtures/goose/interop-goose.cid \
         /fixtures/sv/sv.json /fixtures/sv/interop-sv.cid; do
    check_fixture_file "$LIBIEC61850_IMAGE" "$f"
done
for f in /fixtures/mms/interop.json /fixtures/iec61850/interop.icd /fixtures/iec61850/values.json \
         /fixtures/goose/goose.json /fixtures/goose/interop-goose.cid \
         /fixtures/sv/sv.json /fixtures/sv/interop-sv.cid; do
    check_fixture_file "$DSCSYSTEMS_IMAGE" "$f"
    check_fixture_file "$IEC61850RUST_IMAGE" "$f"
done

info ""
info "=== Server ready events ==="

ready=$(start_server "$LIBIEC61850_IMAGE" libiec61850-mms-server 19001)
check_ready_event "libiec61850-mms-server" "$ready"

ready=$(start_server "$LIBIEC61850_IMAGE" libiec61850-ied-server 19002 \
    --icd /fixtures/iec61850/interop.icd --values /fixtures/iec61850/values.json)
check_ready_event "libiec61850-ied-server" "$ready"

ready=$(start_server "$IEC61850BEAN_IMAGE" iec61850bean-ied-server 19003)
check_ready_event "iec61850bean-ied-server" "$ready"

ready=$(start_server "$DSCSYSTEMS_IMAGE" dscsystems-mms-server 19004)
check_ready_event "dscsystems-mms-server" "$ready"

ready=$(start_server "$DSCSYSTEMS_IMAGE" dscsystems-ied-server 19005)
check_ready_event "dscsystems-ied-server" "$ready"

ready=$(start_server "$IEC61850RUST_IMAGE" iec61850-rust-mms-server 19006)
check_ready_event "iec61850-rust-mms-server" "$ready"

ready=$(start_server "$IEC61850RUST_IMAGE" iec61850-rust-ied-server 19007)
check_ready_event "iec61850-rust-ied-server" "$ready"

info ""
info "=== Client JSON Lines output ==="

output=$(run_client_pair \
    "$LIBIEC61850_IMAGE" libiec61850-mms-server \
    "$LIBIEC61850_IMAGE" libiec61850-mms-client)
check_json_lines "libiec61850-mms-client" "$output"

output=$(run_client_pair \
    "$LIBIEC61850_IMAGE" libiec61850-ied-server \
    "$LIBIEC61850_IMAGE" libiec61850-ied-client)
check_json_lines "libiec61850-ied-client" "$output"
check_ied_client_ops "libiec61850-ied-client" "$output"
check_cdc_values "libiec61850-ied-client" "$output"

output=$(run_client_pair \
    "$LIBIEC61850_IMAGE" libiec61850-ied-server \
    "$LIBIEC61850_IMAGE" libiec61850-ied-reporter)
check_json_lines "libiec61850-ied-reporter" "$output"

output=$(run_client_pair \
    "$IEC61850BEAN_IMAGE" iec61850bean-ied-server \
    "$IEC61850BEAN_IMAGE" iec61850bean-ied-client)
check_json_lines "iec61850bean-ied-client" "$output"
check_ied_client_ops "iec61850bean-ied-client" "$output"
check_cdc_values "iec61850bean-ied-client" "$output"

# iec61850bean against libiec61850, both directions (CDC values).
output=$(run_client_pair "$LIBIEC61850_IMAGE" libiec61850-ied-server "$IEC61850BEAN_IMAGE" iec61850bean-ied-client)
check_json_lines "iec61850bean-ied-client -> libiec61850-ied-server" "$output"
check_ied_client_ops "iec61850bean-ied-client -> libiec61850-ied-server" "$output"
check_cdc_values "iec61850bean-ied-client -> libiec61850-ied-server" "$output"

output=$(run_client_pair "$IEC61850BEAN_IMAGE" iec61850bean-ied-server "$LIBIEC61850_IMAGE" libiec61850-ied-client)
check_json_lines "libiec61850-ied-client -> iec61850bean-ied-server" "$output"
check_ied_client_ops "libiec61850-ied-client -> iec61850bean-ied-server" "$output"
check_cdc_values "libiec61850-ied-client -> iec61850bean-ied-server" "$output"

# dscsystems and iec61850-rust against libiec61850, both directions.
for stack in "$DSCSYSTEMS_IMAGE dscsystems" "$IEC61850RUST_IMAGE iec61850-rust"; do
    set -- $stack
    img="$1"; p="$2"

    output=$(run_client_pair "$LIBIEC61850_IMAGE" libiec61850-mms-server "$img" "$p-mms-client")
    check_json_lines "$p-mms-client -> libiec61850-mms-server" "$output"

    output=$(run_client_pair "$img" "$p-mms-server" "$LIBIEC61850_IMAGE" libiec61850-mms-client)
    check_json_lines "libiec61850-mms-client -> $p-mms-server" "$output"

    output=$(run_client_pair "$LIBIEC61850_IMAGE" libiec61850-ied-server "$img" "$p-ied-client")
    check_json_lines "$p-ied-client -> libiec61850-ied-server" "$output"
    check_ied_client_ops "$p-ied-client -> libiec61850-ied-server" "$output"
    check_cdc_values "$p-ied-client -> libiec61850-ied-server" "$output"

    output=$(run_client_pair "$img" "$p-ied-server" "$LIBIEC61850_IMAGE" libiec61850-ied-client)
    check_json_lines "libiec61850-ied-client -> $p-ied-server" "$output"
    check_ied_client_ops "libiec61850-ied-client -> $p-ied-server" "$output"
    check_cdc_values "libiec61850-ied-client -> $p-ied-server" "$output"

    output=$(run_client_pair "$LIBIEC61850_IMAGE" libiec61850-ied-server "$img" "$p-ied-reporter")
    check_json_lines "$p-ied-reporter -> libiec61850-ied-server" "$output"

    output=$(run_client_pair "$LIBIEC61850_IMAGE" libiec61850-ied-server "$img" "$p-ied-controller")
    check_json_lines "$p-ied-controller -> libiec61850-ied-server" "$output"
done

info ""
info "=== GOOSE publisher -> subscriber (L2, Docker bridge) ==="

# at_cross <goose|sv> <publisher command> <subscriber command> <control block>:
# the scenario of a crosscheck. The roles are publisher and subscriber.
at_cross() {
    at "$1.$(slug "$4")$([ "${FLAGS:-}" = plain ] && echo .plain)$([ "${HOST_SIDE:-}" = 1 ] && echo .host-side)" "$(stack "$2")" "$(stack "$3")"
}
goose_check() {
    local out
    at_cross goose "$2" "$4" "$5"
    if out=$("$SCRIPTS/goose-crosscheck.sh" "$@" 2>&1); then
        pass "${out#PASS }"
    else
        fail "$out"
    fi
}
STACKS=("$LIBIEC61850_IMAGE libiec61850" "$DSCSYSTEMS_IMAGE dscsystems" "$IEC61850RUST_IMAGE iec61850-rust")
# Every publisher -> every subscriber, all flags set.
for pub in "${STACKS[@]}"; do
    for sub in "${STACKS[@]}"; do
        set -- $pub $sub
        goose_check "$1" "$2-goose-publisher" "$3" "$4-goose-subscriber" gcbAll
    done
done
# No flags, and from the host side of the bridge (a Go test on a Linux
# host), each stack once as publisher and once as subscriber.
for i in 0 1 2; do
    set -- ${STACKS[$i]} ${STACKS[$(( (i + 1) % 3 ))]}
    FLAGS=plain goose_check "$1" "$2-goose-publisher" "$3" "$4-goose-subscriber" gcbBool
    HOST_SIDE=1 goose_check "$1" "$2-goose-publisher" "$3" "$4-goose-subscriber" gcbAll
done

info ""
info "=== Sampled Values publisher -> subscriber (L2, Docker bridge) ==="

sv_check() {
    local out
    at_cross sv "$2" "$4" "$5"
    if out=$("$SCRIPTS/sv-crosscheck.sh" "$@" 2>&1); then
        pass "${out#PASS }"
    else
        fail "$out"
    fi
}
# Pairs that fail for a known limitation (fixtures/capabilities.json): the
# check must fail with nothing received. When one starts to pass, the
# limitation is gone and has to be removed here and in capabilities.json.
SV_SMPMOD_WAIVER=libiec61850-sv-two-octet-smpmod
SV_SMPMOD_EVIDENCE=https://github.com/mz-automation/libiec61850/tree/6e265c54af66ea686453b6d31c4dcce1cf7ad2bd
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
# its own publisher and dscsystems' send: the generic stream (it carries
# smpMod) cannot reach it from them.
sv_generic_known() {
    case "$1/$2" in
        libiec61850/libiec61850|dscsystems/libiec61850) return 0 ;;
    esac
    return 1
}
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
for i in 0 1 2; do
    set -- ${STACKS[$i]} ${STACKS[$(( (i + 1) % 3 ))]}
    HOST_SIDE=1 sv_check "$1" "$2-sv-publisher" "$3" "$4-sv-subscriber" MSVCB01
done

# ===========================================================================
# Summary
# ===========================================================================

echo ""
echo "Results: ${GREEN}${PASS} passed${RESET}  ${RED}${FAIL} failed${RESET}"
results_close || FAIL=$((FAIL + 1))

[ $FAIL -eq 0 ]
