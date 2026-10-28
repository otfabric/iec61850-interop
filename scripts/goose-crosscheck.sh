#!/usr/bin/env bash
# goose-crosscheck.sh — GOOSE publisher -> subscriber check between two
# adapter images on a Docker bridge network.
#
# Usage:
#   scripts/goose-crosscheck.sh PUB_IMAGE PUB_CMD SUB_IMAGE SUB_CMD [CB]
#   e.g. scripts/goose-crosscheck.sh \
#          iec61850-interop-libiec61850:local libiec61850-goose-publisher \
#          iec61850-interop-dscsystems:local  dscsystems-goose-subscriber
#
# CB is a control block of fixtures/goose/goose.json (default gcbAll).
# FLAGS=plain runs the publisher without --test --simulation --nds-com
# and expects all three false (default: all three set and expected true).
# HOST_SIDE=1 runs the subscriber with --network host on the bridge
# device of the network, as a Go test on a Linux host does (docs/goose.md).
# The publisher announces 'values' (stNum 1), gets one {"cmd":"set"}
# with 'update' (stNum 2) and {"cmd":"stop"}. The subscriber's events must
# show both states with the fixture values, the control block's fields,
# the --test/--simulation/--nds-com flags, increasing sqNum, and a
# supervision timeout after the publisher stopped.
#
# Exit code: 0 pass, 1 fail.

set -euo pipefail

PUB_IMAGE="$1"; PUB_CMD="$2"; SUB_IMAGE="$3"; SUB_CMD="$4"; CB="${5:-gcbAll}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FIXTURE="${ROOT}/fixtures/goose/goose.json"
NET="goose-xc-$$"
SUB="goose-xc-sub-$$"
PUB="goose-xc-pub-$$"
OUT="$(mktemp -d)"
FLAGS="${FLAGS:-all}"
PUB_FLAGS=(--test --simulation --nds-com)
[ "$FLAGS" = plain ] && PUB_FLAGS=()

cleanup() {
    docker rm -f "$SUB" "$PUB" "$SUB-keeper" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
    rm -rf "$OUT"
}
trap cleanup EXIT

field() { python3 -c "import json,sys;cb=json.load(open('$FIXTURE'))['controlBlocks']['$CB'];v=cb[sys.argv[1]];print(json.dumps(v) if isinstance(v,(list,dict)) else v)" "$1"; }

# wait_ready FILE: block until FILE has a ready event (at most 30 s).
wait_ready() {
    local i
    for i in $(seq 300); do
        grep -q '"event":"ready"' "$1" 2>/dev/null && return 0
        sleep 0.1
    done
}

BRIDGE="goosexc$$"
BRIDGE="${BRIDGE:0:15}"
docker network create -o "com.docker.network.bridge.name=${BRIDGE}" "$NET" >/dev/null

SUB_NET=(--network "$NET"); SUB_IFACE=eth0
if [ "${HOST_SIDE:-0}" = 1 ]; then
    SUB_NET=(--network host); SUB_IFACE="$BRIDGE"
    # A bridge without ports has no carrier, and the first container to
    # join sees its sends stall while the bridge comes up. A subscriber in
    # a container is that first port; on the host side an idle container
    # takes its place (docs/goose.md).
    docker run -d --name "$SUB-keeper" --network "$NET" --entrypoint sleep "$SUB_IMAGE" infinity >/dev/null
    sleep 1
fi

# Subscriber first, filtered on the control block's APPID and gocbRef.
docker run -d --name "$SUB" "${SUB_NET[@]}" --cap-add NET_RAW \
    --entrypoint "$SUB_CMD" "$SUB_IMAGE" \
    --iface "$SUB_IFACE" --app-id "$(field appID)" --gocb-ref "$(field gocbRef)" --dst "$(field dst)" >/dev/null

deadline=$((SECONDS + 30))
until docker logs "$SUB" 2>/dev/null | grep -q '"event":"ready"'; do
    [ $SECONDS -lt $deadline ] || { echo "FAIL subscriber not ready"; docker logs "$SUB" || true; exit 1; }
    sleep 0.2
done

update="$(python3 -c "import json;print(json.dumps({'cmd':'set','values':json.load(open('$FIXTURE'))['controlBlocks']['$CB']['update']}))")"

# Publisher: state 1 for ~1.5 s (retransmissions), state 2, then stop.
# The clock starts at the publisher's ready event, not at docker run:
# container start-up on a busy host would otherwise eat into state 1.
{ wait_ready "$OUT/pub.jsonl"; sleep 1.5; echo "$update"; sleep 1.5; echo '{"cmd":"stop"}'; } | \
docker run -i --name "$PUB" --network "$NET" --cap-add NET_RAW \
    --entrypoint "$PUB_CMD" "$PUB_IMAGE" \
    --iface eth0 --gocb-ref "$(field gocbRef)" --go-id "$(field goID)" \
    --dataset "$(field datSet)" --app-id "$(field appID)" --dst "$(field dst)" \
    --vlan-id "$(field vlanId)" --vlan-prio "$(field vlanPrio)" \
    --conf-rev "$(field confRev)" --min-time "$(field minTime)" --max-time "$(field maxTime)" \
    "${PUB_FLAGS[@]+"${PUB_FLAGS[@]}"}" \
    --values "$(field values)" > "$OUT/pub.jsonl" 2> "$OUT/pub.err" || {
        echo "FAIL publisher exited non-zero"; cat "$OUT/pub.err"; exit 1; }

# TAL of the last frame of state 2 is at most 2 * maxTime.
sleep "$(python3 -c "print(2*$(field maxTime)/1000 + 1)")"
docker logs "$SUB" > "$OUT/sub.jsonl" 2> "$OUT/sub.err"
[ -n "${KEEP:-}" ] && cp "$OUT"/* "$KEEP"/

python3 - "$FIXTURE" "$CB" "$OUT/pub.jsonl" "$OUT/sub.jsonl" "$PUB_CMD" "$SUB_CMD" "$FLAGS" <<'PY'
import json, sys
fixture, cb_name, pub_path, sub_path, pub_cmd, sub_cmd, flags = sys.argv[1:]
flag = flags != "plain"
cb = json.load(open(fixture))["controlBlocks"][cb_name]
label = f"{pub_cmd} -> {sub_cmd} ({cb_name}, flags {flags}{', host side' if __import__('os').environ.get('HOST_SIDE') == '1' else ''})"
fails = []

def lines(p):
    out = []
    for l in open(p):
        l = l.strip()
        if not l:
            continue
        try:
            out.append(json.loads(l))
        except ValueError:
            fails.append(f"{p}: not JSON: {l[:120]}")
    return out

pub, sub = lines(pub_path), lines(sub_path)
published = [e for e in pub if e.get("event") == "published"]
if not any(e.get("event") == "ready" for e in pub):
    fails.append("publisher: no ready event")
if not published or (published[0]["stNum"], published[0]["sqNum"]) != (1, 0):
    fails.append(f"publisher: first frame is not stNum 1 sqNum 0: {published[:1]}")
if not any(e["stNum"] == 2 and e["sqNum"] == 0 for e in published):
    fails.append("publisher: no stNum 2 sqNum 0 after set")
errors = [e for e in pub if e.get("event") == "error"]
if errors:
    fails.append(f"publisher errors: {errors}")

goose = [e for e in sub if e.get("event") == "goose"]
sup = [e for e in sub if e.get("event") == "supervision"]
expect = {
    "gocbRef": cb["gocbRef"], "datSet": cb["datSet"], "goID": cb["goID"],
    "appID": cb["appID"], "dst": cb["dst"], "confRev": cb["confRev"],
    "test": flag, "simulation": flag, "ndsCom": flag,
    "numDatSetEntries": len(cb["values"]),
}
# 1.5 s per state with MinTime 10 ms is 8 frames (0, 10, 30, 70, 150,
# 310, 630, 1270 ms); fewer than 5 means retransmission stalled.
MIN_FRAMES = 5
for state in (1, 2):
    n = sum(1 for e in published if e["stNum"] == state)
    if n < MIN_FRAMES:
        fails.append(f"publisher: {n} frames for stNum {state}, expected >= {MIN_FRAMES}")
for state, values in ((1, cb["values"]), (2, cb["update"])):
    frames = [e for e in goose if e["stNum"] == state]
    if len(frames) < MIN_FRAMES:
        fails.append(f"subscriber: {len(frames)} frames with stNum {state}, expected >= {MIN_FRAMES}")
    if not frames:
        continue
    if frames[0]["sqNum"] != 0:
        fails.append(f"subscriber: stNum {state} first frame sqNum {frames[0]['sqNum']}")
    sq = [e["sqNum"] for e in frames]
    if sq != sorted(sq) or len(set(sq)) != len(sq):
        fails.append(f"subscriber: stNum {state} sqNum not increasing: {sq}")
    for e in frames:
        diff = {k: (e.get(k), v) for k, v in expect.items() if e.get(k) != v}
        if diff:
            fails.append(f"subscriber: stNum {state} sqNum {e['sqNum']}: field mismatch {diff}")
            break
        if e["values"] != values:
            fails.append(f"subscriber: stNum {state} values {e['values']} != fixture {values}")
            break
        if not e.get("t") or not e.get("src") or not e.get("timeAllowedToLive"):
            fails.append(f"subscriber: missing t/src/timeAllowedToLive: {e}")
            break
pub_src = next((e.get("src") for e in pub if e.get("event") == "ready"), None)
if goose and pub_src and goose[0]["src"] != pub_src:
    fails.append(f"subscriber: src {goose[0]['src']} != publisher MAC {pub_src}")
if not any(e.get("state") == "timeout" and e.get("gocbRef") == cb["gocbRef"] for e in sup):
    fails.append(f"subscriber: no supervision timeout after stop: {sup}")

if fails:
    print(f"FAIL {label}")
    for f in fails:
        print(f"  - {f}")
    sys.exit(1)
n1 = sum(1 for e in goose if e["stNum"] == 1)
n2 = sum(1 for e in goose if e["stNum"] == 2)
print(f"PASS {label}: {n1} + {n2} frames, values and fields match, supervision timeout seen")
PY
