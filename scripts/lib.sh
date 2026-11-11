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

# check_controller_ops <label> <output>: the default control (SPCSO1,
# direct, ctlVal true) was operated and stVal follows it.
check_controller_ops() {
    local label="$1" output="$2" got
    at_label control "${label}"
    got=$(echo "$output" | python3 -c '
import sys, json
ops = {}
for line in sys.stdin:
    try:
        o = json.loads(line)
    except Exception:
        continue
    ops.setdefault(o.get("operation"), o)
op, st = ops.get("operate"), ops.get("read-stval")
bad = []
if not op or not op.get("ok"):
    bad.append("operate: %s" % (op.get("error") if op else "missing"))
if not st or not st.get("ok") or st.get("value") is not True:
    bad.append("read-stval: %s" % (st.get("value", st.get("error")) if st else "missing"))
print("; ".join(bad))
' 2>/dev/null || echo "parse error")
    if [ -n "$got" ]; then
        fail "${label}: control not carried out: ${got}"
    else
        pass "${label}: operate ok, stVal follows ctlVal"
    fi
}

# reporter_triggers <capabilities>: the reporter's --trigger values
# (contract 1.3), empty for an image without them.
reporter_triggers() { jq -r '(.reporterTriggers // []) | join(" ")' <<<"$1" 2>/dev/null; }

# transports <capabilities>: the GOOSE and SV commands' --transport values
# (contract 1.4), "ethernet" for an image without them.
transports() { jq -r '(.transports // ["ethernet"]) | join(" ")' <<<"$1" 2>/dev/null; }

# report_gi_problems <output>: what is wrong with the GI report of a
# reporter run with --trigger gi (nothing when it is right): both members
# of dsInterop, reason gi, values.json's SPS1.stVal and Mod.stVal.
report_gi_problems() {
    echo "$1" | python3 -c '
import sys, json
r, g = None, None
for line in sys.stdin:
    try:
        o = json.loads(line)
    except Exception:
        continue
    if o.get("operation") == "receive-report":
        r = o
    if o.get("operation") == "trigger-gi":
        g = o
bad = []
if not g or not g.get("ok"):
    bad.append("trigger-gi: %s" % (g.get("error") if g else "missing"))
if not r or not r.get("ok"):
    bad.append("receive-report: %s" % (r.get("error") if r else "missing"))
else:
    if r.get("inclusion") != [True, True]:
        bad.append("inclusion %s" % r.get("inclusion"))
    if r.get("reasons") != ["gi", "gi"]:
        bad.append("reasons %s" % r.get("reasons"))
    if r.get("values") != [False, 1]:
        bad.append("values %s" % r.get("values"))
print("; ".join(bad))
' 2>/dev/null || echo "parse error"
}

# check_report_gi <label> <output>: the reporter run with --trigger gi got
# the general interrogation report of urcb01.
check_report_gi() {
    local label="$1" got
    at_label report-gi "${label}"
    got=$(report_gi_problems "$2")
    if [ -n "$got" ]; then
        fail "${label}: GI report wrong: ${got}"
    else
        pass "${label}: GI report of both members, values [false, 1]"
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
    *-ied-controller)
        check_controller_ops "$1" "$3" ;;
    esac
}

# known_report_gi <client adapter> <server adapter>: "<waiver id> <scope>
# <evidence> <pattern>" when the GI report of the pair fails for a known
# limitation of fixtures/capabilities.json (the pattern is what the failure
# says), nothing otherwise.
known_report_gi() {
    case "$1/$2" in
        # Refuses RptEna unless the client wrote Resv first (no implicit
        # reservation of a URCB); dscsystems' client reserves explicitly.
        dscsystems/iec61850-rust) ;;
        */iec61850-rust)
            echo "iec61850-rust-urcb-implicit-reservation server@iec61850-rust" \
                "https://github.com/csp0924/iec61850-rust/blob/d4e895217f367ed4caebf21611d2c16ee46035c0/crates/iec61850-server/src/reporting/service.rs" \
                "access-denied" ;;
        # Announces data references in OptFlds and sends none.
        cbsigma/iec61850bean)
            echo "iec61850bean-report-data-references server@iec61850bean" \
                "https://github.com/otfabric/iec61850-interop/blob/main/fixtures/capabilities.json" \
                "data reference" ;;
    esac
}

# report_gi_pair <label> <server image> <server command> <client image>
# <client command>: the reporter with --trigger gi against the server, and
# its checks. A pair of known_report_gi has to fail as registered: when it
# passes, the limitation is gone and has to be removed.
report_gi_pair() {
    local output known id scope evidence pattern
    output=$(SMOKE_CLIENT_ARGS="--trigger gi" run_client_pair "$2" "$3" "$4" "$5")
    known=$(known_report_gi "$(stack "$5")" "$(stack "$3")")
    if [ -z "$known" ]; then
        check_json_lines "$1" "$output"
        check_report_gi "$1" "$output"
        return
    fi
    read -r id scope evidence pattern <<<"$known"
    at_label report-gi "$1"
    if [ -z "$(report_gi_problems "$output")" ]; then
        waiver "$id" "$scope" "$evidence" false
        fail "$1: known limitation ${id} no longer reproduces, update capabilities.json and known_report_gi"
    elif echo "$output" | grep -q "$pattern"; then
        waiver "$id" "$scope" "$evidence" true
        echo -e "${YELLOW}KNOWN${RESET} $1: ${id}"
        PASS=$((PASS + 1))
        result known-difference "known limitation reproduced (fixtures/capabilities.json): ${id}" "$id"
    else
        fail "$1: GI report failed otherwise than ${id}: $(report_gi_problems "$output")"
    fi
}
