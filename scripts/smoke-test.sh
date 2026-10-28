#!/usr/bin/env bash
# smoke-test.sh — adapter self-tests for mms-interop.
#
# Verifies that each adapter command:
#   1. Starts without error.
#   2. Emits a valid JSON ready event (for server commands).
#   3. Emits valid JSON Lines output (for client commands).
#   4. Exits with the expected code.
#   5. Ready event contains the expected fields: event, address, fixture, adapter, version.
#
# Usage:
#   LIBIEC61850_IMAGE=mms-interop-libiec61850:local \
#   IEC61850BEAN_IMAGE=mms-interop-iec61850bean:local \
#   DSCSYSTEMS_IMAGE=mms-interop-dscsystems:local \
#   ./scripts/smoke-test.sh
#
# Exit code: 0 if all checks pass, 1 if any check fails.

set -euo pipefail

LIBIEC61850_IMAGE="${LIBIEC61850_IMAGE:-mms-interop-libiec61850:local}"
IEC61850BEAN_IMAGE="${IEC61850BEAN_IMAGE:-mms-interop-iec61850bean:local}"
DSCSYSTEMS_IMAGE="${DSCSYSTEMS_IMAGE:-mms-interop-dscsystems:local}"
SCRIPTS="$(cd "$(dirname "$0")" && pwd)"

PASS=0
FAIL=0

# Colours (suppressed when not a tty)
if [ -t 1 ]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; RESET='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; RESET=''
fi

pass() { echo -e "${GREEN}PASS${RESET} $*"; PASS=$((PASS + 1)); }
fail() { echo -e "${RED}FAIL${RESET} $*"; FAIL=$((FAIL + 1)); }
info() { echo -e "${YELLOW}INFO${RESET} $*"; }

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

    # Run client.
    local output
    output=$(docker run --rm \
        --network "$net" \
        --entrypoint "${cmd_client}" \
        "${image_client}" \
        --host "smoke-server-$$" --port 1102 \
        2>/dev/null || true)

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
    if ! echo "$output" | grep -q '"operation":"associate"'; then
        fail "${label}: missing associate operation"
        return
    fi
    if ! echo "$output" | grep -q '"operation":"conclude"'; then
        fail "${label}: missing conclude operation"
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
# Check: required binary exists inside image.
# ---------------------------------------------------------------------------
check_binary() {
    local image="$1" bin="$2"
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
for bin in dscsystems-mms-server dscsystems-mms-client dscsystems-ied-server \
           dscsystems-ied-client dscsystems-ied-controller dscsystems-ied-reporter \
           dscsystems-goose-publisher dscsystems-goose-subscriber; do
    check_binary "$DSCSYSTEMS_IMAGE" "$bin"
done

info ""
info "=== Capabilities declare IED clients ==="
check_capabilities_command() {
    local image="$1" entrypoint="$2" required="$3"
    local out
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

info ""
info "=== Fixture files ==="
check_fixture_file "$LIBIEC61850_IMAGE" /fixtures/mms/interop.json
check_fixture_file "$LIBIEC61850_IMAGE" /fixtures/iec61850/interop.icd
check_fixture_file "$LIBIEC61850_IMAGE" /fixtures/iec61850/values.json
check_fixture_file "$IEC61850BEAN_IMAGE" /fixtures/iec61850/interop.icd
check_fixture_file "$IEC61850BEAN_IMAGE" /fixtures/iec61850/values.json
for f in /fixtures/goose/goose.json /fixtures/goose/interop-goose.cid; do
    check_fixture_file "$LIBIEC61850_IMAGE" "$f"
    check_fixture_file "$DSCSYSTEMS_IMAGE" "$f"
done
for f in /fixtures/mms/interop.json /fixtures/iec61850/interop.icd /fixtures/iec61850/values.json; do
    check_fixture_file "$DSCSYSTEMS_IMAGE" "$f"
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

output=$(run_client_pair \
    "$LIBIEC61850_IMAGE" libiec61850-ied-server \
    "$LIBIEC61850_IMAGE" libiec61850-ied-reporter)
check_json_lines "libiec61850-ied-reporter" "$output"

output=$(run_client_pair \
    "$IEC61850BEAN_IMAGE" iec61850bean-ied-server \
    "$IEC61850BEAN_IMAGE" iec61850bean-ied-client)
check_json_lines "iec61850bean-ied-client" "$output"
check_ied_client_ops "iec61850bean-ied-client" "$output"

# dscsystems against libiec61850, both directions.
output=$(run_client_pair \
    "$LIBIEC61850_IMAGE" libiec61850-mms-server \
    "$DSCSYSTEMS_IMAGE" dscsystems-mms-client)
check_json_lines "dscsystems-mms-client -> libiec61850-mms-server" "$output"

output=$(run_client_pair \
    "$DSCSYSTEMS_IMAGE" dscsystems-mms-server \
    "$LIBIEC61850_IMAGE" libiec61850-mms-client)
check_json_lines "libiec61850-mms-client -> dscsystems-mms-server" "$output"

output=$(run_client_pair \
    "$LIBIEC61850_IMAGE" libiec61850-ied-server \
    "$DSCSYSTEMS_IMAGE" dscsystems-ied-client)
check_json_lines "dscsystems-ied-client -> libiec61850-ied-server" "$output"
check_ied_client_ops "dscsystems-ied-client -> libiec61850-ied-server" "$output"

output=$(run_client_pair \
    "$DSCSYSTEMS_IMAGE" dscsystems-ied-server \
    "$LIBIEC61850_IMAGE" libiec61850-ied-client)
check_json_lines "libiec61850-ied-client -> dscsystems-ied-server" "$output"
check_ied_client_ops "libiec61850-ied-client -> dscsystems-ied-server" "$output"

output=$(run_client_pair \
    "$LIBIEC61850_IMAGE" libiec61850-ied-server \
    "$DSCSYSTEMS_IMAGE" dscsystems-ied-reporter)
check_json_lines "dscsystems-ied-reporter -> libiec61850-ied-server" "$output"

output=$(run_client_pair \
    "$LIBIEC61850_IMAGE" libiec61850-ied-server \
    "$DSCSYSTEMS_IMAGE" dscsystems-ied-controller)
check_json_lines "dscsystems-ied-controller -> libiec61850-ied-server" "$output"

info ""
info "=== GOOSE publisher -> subscriber (L2, Docker bridge) ==="

goose_check() {
    local out
    if out=$("$SCRIPTS/goose-crosscheck.sh" "$@" 2>&1); then
        pass "${out#PASS }"
    else
        fail "$out"
    fi
}
for pub in "$LIBIEC61850_IMAGE libiec61850" "$DSCSYSTEMS_IMAGE dscsystems"; do
    for sub in "$LIBIEC61850_IMAGE libiec61850" "$DSCSYSTEMS_IMAGE dscsystems"; do
        set -- $pub $sub
        goose_check "$1" "$2-goose-publisher" "$3" "$4-goose-subscriber" gcbAll
    done
done
FLAGS=plain goose_check "$LIBIEC61850_IMAGE" libiec61850-goose-publisher \
    "$DSCSYSTEMS_IMAGE" dscsystems-goose-subscriber gcbBool
FLAGS=plain goose_check "$DSCSYSTEMS_IMAGE" dscsystems-goose-publisher \
    "$LIBIEC61850_IMAGE" libiec61850-goose-subscriber gcbBool
# The host side of the bridge, as a Go test on a Linux host sees it.
HOST_SIDE=1 goose_check "$LIBIEC61850_IMAGE" libiec61850-goose-publisher \
    "$DSCSYSTEMS_IMAGE" dscsystems-goose-subscriber gcbAll
HOST_SIDE=1 goose_check "$DSCSYSTEMS_IMAGE" dscsystems-goose-publisher \
    "$LIBIEC61850_IMAGE" libiec61850-goose-subscriber gcbAll

# ===========================================================================
# Summary
# ===========================================================================

echo ""
echo "Results: ${GREEN}${PASS} passed${RESET}  ${RED}${FAIL} failed${RESET}"

[ $FAIL -eq 0 ]
