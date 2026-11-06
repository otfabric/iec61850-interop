#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Shared helpers of scripts/smoke.sh and scripts/interop.sh. Source, do not
# execute. Needs docker, python3 and jq.
#
# An adapter is a stack name (libiec61850, iec61850bean, dscsystems,
# iec61850-rust, ...): its image is image_of <adapter>, its commands are
# <adapter>-<role> (docs/CONTAINER_CONTRACT.md).

SCRIPTS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(dirname "${SCRIPTS}")"
export ROOT
REGISTRY="${REGISTRY:-ghcr.io/otfabric}"
# Empty: the images "make build" tags iec61850-interop-<adapter>:local.
VERSION="${VERSION:-}"
RUN_ID="${RUN_ID:-$$}"

# image_of <adapter>: ADAPTER_IMAGE_<adapter> ('-' as '_') names an image
# outside the release set, which this repository's scripts know nothing
# about; the older LIBIEC61850_IMAGE, IEC61850BEAN_IMAGE, DSCSYSTEMS_IMAGE
# and IEC61850RUST_IMAGE still work. Else the release image of VERSION, or
# the local build.
image_of() {
    local override="ADAPTER_IMAGE_${1//-/_}" legacy
    if [[ -n "${!override:-}" ]]; then echo "${!override}"; return; fi
    legacy="$(echo "${1//-/}" | tr 'a-z' 'A-Z')_IMAGE"
    if [[ -n "${!legacy:-}" ]]; then echo "${!legacy}"; return; fi
    if [[ -n "${VERSION}" ]]; then echo "${REGISTRY}/iec61850-interop-$1:${VERSION}"; else echo "iec61850-interop-$1:local"; fi
}

# shellcheck source=scripts/results.sh
source "${SCRIPTS}/results.sh"
# The images of results.json (scripts/write-results.py takes the
# ADAPTER_IMAGE_<adapter> overrides first, as image_of does).
if [[ -n "${VERSION}" ]]; then RESULT_IMAGE="${REGISTRY}/iec61850-interop-{}:${VERSION}"; else RESULT_IMAGE="iec61850-interop-{}:local"; fi
export RESULT_IMAGE

PASS=0
FAIL=0

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
# slug <text>: the text as a part of a scenario id.
slug() { echo "$1" | tr 'A-Z' 'a-z' | sed -E 's/ -> /./g; s/[^a-z0-9._-]+/-/g; s/^-+//; s/-+$//'; }
# at_label <kind> <label>: the scenario of a check whose label is an adapter
# command, or "<client command> -> <server command>".
at_label() {
    local client="${2%% -> *}" server="${2##* -> }"
    at "$1.$(slug "$2")" "$(stack "${client}")" "$(stack "${server}")"
}

# capabilities <adapter>: the --capabilities event of the image (one line),
# asked of its ied-server, which every stack has.
capabilities() {
    docker run --rm --entrypoint "$1-ied-server" "$(image_of "$1")" --capabilities 2>/dev/null | head -1
}

# has_command <capabilities> <command>
has_command() { jq -e --arg c "$2" '.commands | index($c)' <<<"$1" >/dev/null 2>&1; }

# record_implementation <adapter> <capabilities>: what the image says of
# itself for results.json (an image without "upstream" is a reference of
# versions.yaml).
record_implementation() {
    local file
    file=$(mktemp)
    printf '%s\n' "$2" > "${file}"
    implementation "$1" "${file}"
    rm -f "${file}"
}

# start_server <image> <command> <host port> [extra args...]: start a
# server container, capture its ready line, stop it, print the line.
start_server() {
    local image="$1" cmd="$2" host_port="$3"
    shift 3
    local cid ready="" deadline
    cid=$(docker run -d -p "${host_port}:1102" --entrypoint "${cmd}" "${image}" --port 1102 "$@")
    deadline=$((SECONDS + 30))
    while [ $SECONDS -lt $deadline ]; do
        ready=$(docker logs "$cid" 2>/dev/null | grep '"event":"ready"' | head -1 || true)
        [ -n "$ready" ] && break
        sleep 0.2
    done
    docker stop "$cid" >/dev/null 2>&1
    docker rm "$cid" >/dev/null 2>&1
    printf '%s' "$ready"
}

# run_client_pair <server image> <server command> <client image> <client
# command>: start the server on a private network, run the client against
# it, print the client's stdout. SMOKE_CLIENT_ARGS go to the client.
run_client_pair() {
    local image_server="$1" cmd_server="$2" image_client="$3" cmd_client="$4"
    local net="iec61850-interop-${RUN_ID}-$RANDOM" name sid deadline output try
    name="srv-${net}"
    docker network create "$net" >/dev/null 2>&1
    sid=$(docker run -d --network "$net" --name "${name}" --entrypoint "${cmd_server}" "${image_server}" --port 1102)
    deadline=$((SECONDS + 30))
    while [ $SECONDS -lt $deadline ]; do
        docker logs "$sid" 2>/dev/null | grep -q '"event":"ready"' && break
        sleep 0.2
    done
    # A failed association right after the server's ready event is retried
    # (up to three tries): on a busy runner the new container's name or port
    # can lag the event by a moment. Any other outcome, and a third failed
    # association, is the result.
    for try in 1 2 3; do
        # shellcheck disable=SC2086
        output=$(docker run --rm --network "$net" --entrypoint "${cmd_client}" "${image_client}" \
            --host "${name}" --port 1102 ${SMOKE_CLIENT_ARGS:-} 2>/dev/null || true)
        echo "$output" | grep -q '"operation":"associate","ok":false' || break
        echo "${cmd_client} -> ${cmd_server}: association failed (try ${try}): $(echo "$output" | head -1)" >&2
        sleep 1
    done
    docker stop "$sid" >/dev/null 2>&1
    docker rm "$sid" >/dev/null 2>&1
    docker network rm "$net" >/dev/null 2>&1
    printf '%s' "$output"
}

# check_ready_event <label> <ready line>: valid JSON with event, address,
# fixture, adapter and version.
check_ready_event() {
    local label="$1" ready="$2" missing=() field value
    at_label ready "${label}"
    if [ -z "$ready" ]; then
        fail "${label}: no ready event received within 30 s"
        return
    fi
    if ! echo "$ready" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
        fail "${label}: ready event is not valid JSON: ${ready}"
        return
    fi
    for field in event address fixture adapter version; do
        value=$(echo "$ready" | python3 -c \
            "import sys,json; d=json.load(sys.stdin); print(d.get('${field}',''))" 2>/dev/null)
        [ -z "$value" ] && missing+=("$field")
    done
    if [ ${#missing[@]} -gt 0 ]; then
        fail "${label}: ready event missing fields: ${missing[*]}; got: ${ready}"
    else
        pass "${label}: ready event OK — $(echo "$ready" | python3 -c \
            "import sys,json; d=json.load(sys.stdin); \
             print('adapter={}, fixture={}, version={}'.format(d['adapter'],d['fixture'],d['version']))")"
    fi
}

# check_json_lines <label> <output>: non-empty, every line valid JSON.
check_json_lines() {
    local label="$1" output="$2" bad=0 line
    at_label json-lines "${label}"
    if [ -z "$output" ]; then
        fail "${label}: no JSON Lines output"
        return
    fi
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        if ! echo "$line" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
            bad=$((bad + 1))
            fail "${label}: invalid JSON line: ${line}"
        fi
    done <<< "$output"
    [ $bad -eq 0 ] && pass "${label}: JSON Lines OK ($(echo "$output" | grep -c .) lines)"
}

# check_ied_client_ops <label> <output>: the IED client sequence is complete
# and successful. Same-stack servers historically deny ST writes to
# Mod.stVal (SCL status FC): every non-write operation must succeed, a write
# may be ok:true or access-denied.
check_ied_client_ops() {
    local label="$1" output="$2" bad
    at_label ied-ops "${label}"
    if ! echo "$output" | grep -q '"operation":"associate"'; then
        fail "${label}: missing associate operation"
        return
    fi
    if ! echo "$output" | grep -q '"operation":"conclude"'; then
        fail "${label}: missing conclude operation; output: $(echo "$output" | head -3 | tr '\n' ' ')"
        return
    fi
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

# check_cdc_values <label> <output>: the client read the values.json values
# of GGIO1.DPS1.stVal (Dbpos as its enumeration: 2 = on) and
# MMTR1.TotVAh.actVal (INT32U, 42), which the server seeded.
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

# client_checks <label> <client command> <output>: the checks of a client's
# output, by its role.
client_checks() {
    check_json_lines "$1" "$3"
    case "$2" in
    *-ied-client)
        check_ied_client_ops "$1" "$3"
        check_cdc_values "$1" "$3" ;;
    esac
}
