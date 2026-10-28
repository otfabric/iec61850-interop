#!/usr/bin/env bash
# sv-crosscheck.sh — Sampled Values publisher -> subscriber check between
# two adapter images on a Docker bridge network.
#
# Usage:
#   scripts/sv-crosscheck.sh PUB_IMAGE PUB_CMD SUB_IMAGE SUB_CMD [STREAM]
#   e.g. scripts/sv-crosscheck.sh \
#          iec61850-interop-libiec61850:local libiec61850-sv-publisher \
#          iec61850-interop-dscsystems:local  dscsystems-sv-subscriber
#
# STREAM is a stream of fixtures/sv/sv.json (default MSVCB01, 9-2LE).
# FLAGS=all adds --simulation and --smp-synch 2 and expects them back
# (default plain: no S bit, smpSynch of the fixture).
# HOST_SIDE=1 runs the subscriber with --network host on the bridge
# device of the network, as a Go test on a Linux host does (docs/goose.md).
# The publisher sends 'values' for ~2 s, gets one {"cmd":"set"} with
# 'update', ~2 s later {"cmd":"stop"}. The subscriber's events must show
# both value sets in that order, the stream's header fields, and at most
# MAX_LOSS_PCT (default 1) % loss or reordering and no duplicates.
#
# Exit code: 0 pass, 1 fail.

set -euo pipefail

PUB_IMAGE="$1"; PUB_CMD="$2"; SUB_IMAGE="$3"; SUB_CMD="$4"; STREAM="${5:-MSVCB01}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FIXTURE="${ROOT}/fixtures/sv/sv.json"
NET="sv-xc-$$"
SUB="sv-xc-sub-$$"
PUB="sv-xc-pub-$$"
OUT="$(mktemp -d)"
FLAGS="${FLAGS:-plain}"

cleanup() {
    docker rm -f "$SUB" "$PUB" "$SUB-keeper" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
    rm -rf "$OUT"
}
trap cleanup EXIT

field() { python3 -c "import json,sys;s=json.load(open('$FIXTURE'))['streams']['$STREAM'];v=s[sys.argv[1]];print(json.dumps(v) if isinstance(v,(list,dict)) else v)" "$1"; }

# wait_ready FILE: block until FILE has a ready event (at most 30 s).
wait_ready() {
    local i
    for i in $(seq 300); do
        grep -q '"event":"ready"' "$1" 2>/dev/null && return 0
        sleep 0.1
    done
}

SMP_SYNCH="$(field smpSynch)"
PUB_FLAGS=()
if [ "$FLAGS" = all ]; then
    SMP_SYNCH=2
    PUB_FLAGS=(--simulation)
fi
# About ten sv events per second per stream.
SAMPLE_EVERY=$(( $(field samplesPerSecond) / 10 ))

BRIDGE="svxc$$"
BRIDGE="${BRIDGE:0:15}"
docker network create -o "com.docker.network.bridge.name=${BRIDGE}" "$NET" >/dev/null

SUB_NET=(--network "$NET"); SUB_IFACE=eth0
if [ "${HOST_SIDE:-0}" = 1 ]; then
    SUB_NET=(--network host); SUB_IFACE="$BRIDGE"
    # See goose-crosscheck.sh: an idle container gives the bridge carrier.
    docker run -d --name "$SUB-keeper" --network "$NET" --entrypoint sleep "$SUB_IMAGE" infinity >/dev/null
    sleep 1
fi

docker run -d --name "$SUB" "${SUB_NET[@]}" --cap-add NET_RAW \
    --entrypoint "$SUB_CMD" "$SUB_IMAGE" \
    --iface "$SUB_IFACE" --app-id "$(field appID)" --sv-id "$(field svID)" --dst "$(field dst)" \
    --sample-every "$SAMPLE_EVERY" >/dev/null

deadline=$((SECONDS + 30))
until docker logs "$SUB" 2>/dev/null | grep -q '"event":"ready"'; do
    [ $SECONDS -lt $deadline ] || { echo "FAIL subscriber not ready"; docker logs "$SUB" || true; exit 1; }
    sleep 0.2
done

update="$(python3 -c "import json;print(json.dumps({'cmd':'set','values':json.load(open('$FIXTURE'))['streams']['$STREAM']['update']}))")"

# The clock starts at the publisher's ready event, not at docker run.
{ wait_ready "$OUT/pub.jsonl"; sleep 2; echo "$update"; sleep 2; echo '{"cmd":"stop"}'; } | \
docker run -i --name "$PUB" --network "$NET" --cap-add NET_RAW \
    --entrypoint "$PUB_CMD" "$PUB_IMAGE" \
    --iface eth0 --app-id "$(field appID)" --dst "$(field dst)" \
    --vlan-id "$(field vlanId)" --vlan-prio "$(field vlanPrio)" \
    --sv-id "$(field svID)" --dataset "$(field datSet)" --conf-rev "$(field confRev)" \
    --smp-rate "$(field smpRate)" --smp-mod "$(field smpMod)" --no-asdu "$(field noASDU)" \
    --smp-synch "$SMP_SYNCH" --profile "$(field profile)" \
    "${PUB_FLAGS[@]+"${PUB_FLAGS[@]}"}" \
    --values "$(field values)" > "$OUT/pub.jsonl" 2> "$OUT/pub.err" || {
        echo "FAIL publisher exited non-zero"; cat "$OUT/pub.err"; exit 1; }

# Let the subscriber drain and emit a stats line covering the last frame.
sleep 2.5
docker logs "$SUB" > "$OUT/sub.jsonl" 2> "$OUT/sub.err"
[ -n "${KEEP:-}" ] && cp "$OUT"/* "$KEEP"/

python3 - "$FIXTURE" "$STREAM" "$OUT/pub.jsonl" "$OUT/sub.jsonl" "$PUB_CMD" "$SUB_CMD" "$FLAGS" "$SMP_SYNCH" "$SAMPLE_EVERY" <<'PY'
import json, os, sys
fixture, name, pub_path, sub_path, pub_cmd, sub_cmd, flags, smp_synch, every = sys.argv[1:]
s = json.load(open(fixture))["streams"][name]
sim = flags == "all"
every = int(every)
max_loss = float(os.environ.get("MAX_LOSS_PCT", "1"))
label = f"{pub_cmd} -> {sub_cmd} ({name}, flags {flags}{', host side' if os.environ.get('HOST_SIDE') == '1' else ''})"
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
ev = lambda xs, k: [e for e in xs if e.get("event") == k]
if not ev(pub, "ready"):
    fails.append("publisher: no ready event")
published = ev(pub, "published")
reasons = [e.get("reason") for e in published]
if not published or reasons[0] != "start" or published[0].get("smpCnt") != s["noASDU"] - 1:
    fails.append(f"publisher: first published event is not start with smpCnt {s['noASDU'] - 1}: {published[:1]}")
if "set" not in reasons:
    fails.append("publisher: no published event with reason set")
if "periodic" not in reasons:
    fails.append("publisher: no periodic published event")
if ev(pub, "error"):
    fails.append(f"publisher errors: {ev(pub, 'error')}")
stopped = ev(pub, "stopped")
if not stopped:
    fails.append("publisher: no stopped event")
sent = stopped[-1]["samples"] if stopped else 0
# At most 4 s of publishing (timed from its ready event); a publisher
# that runs much faster or slower than its rate is not pacing.
if sent and not (0.8 * 4 * s["samplesPerSecond"] <= sent <= 1.2 * 4 * s["samplesPerSecond"]):
    fails.append(f"publisher: {sent} samples in ~4 s, expected about {4 * s['samplesPerSecond']}")

svs = ev(sub, "sv")
opt = s["optionalFields"]
expect = {
    "appID": s["appID"], "dst": s["dst"], "svID": s["svID"], "confRev": s["confRev"],
    "smpSynch": int(smp_synch), "simulation": sim, "noASDU": s["noASDU"],
    "datSet": s["datSet"] if opt["datSet"] else None,
    "smpRate": s["smpRate"] if opt["smpRate"] else None,
    "smpMod": s["smpMod"] if opt["smpMod"] else None,
}
phase = 0  # 0 = values, 1 = update
counts = [0, 0]
for e in svs:
    diff = {k: (e.get(k), v) for k, v in expect.items() if e.get(k) != v}
    if diff:
        fails.append(f"subscriber: smpCnt {e.get('smpCnt')}: field mismatch {diff}")
        break
    if (e.get("refrTm") is not None) != opt["refrTm"]:
        fails.append(f"subscriber: refrTm {e.get('refrTm')!r}, expected {'present' if opt['refrTm'] else 'null'}")
        break
    if not (0 <= e["smpCnt"] < s["smpCntWrap"]) or e["smpCnt"] % every:
        fails.append(f"subscriber: smpCnt {e['smpCnt']} out of range or not a multiple of {every}")
        break
    if not (0 <= e.get("asduIndex", -1) < s["noASDU"]):
        fails.append(f"subscriber: asduIndex {e.get('asduIndex')}")
        break
    if phase == 0 and e["values"] == s["update"]:
        phase = 1
    want = s["values"] if phase == 0 else s["update"]
    if e["values"] != want:
        fails.append(f"subscriber: smpCnt {e['smpCnt']}: values {e['values']} != {'update' if phase else 'values'} {want}")
        break
    counts[phase] += 1
for i, n in enumerate(counts):
    if n < 5:
        fails.append(f"subscriber: {n} sv events with {'update' if i else 'values'}, expected >= 5")
pub_src = next((e.get("src") for e in pub if e.get("event") == "ready"), None)
if svs and pub_src and svs[0].get("src") != pub_src:
    fails.append(f"subscriber: src {svs[0].get('src')} != publisher MAC {pub_src}")

stats = ev(sub, "stats")
if len(stats) < 4:
    fails.append(f"subscriber: {len(stats)} stats events, expected one per second")
st = stats[-1] if stats else {"received": 0, "lost": 0, "duplicates": 0, "outOfOrder": 0}
missing = max(sent - st["received"], 0)
pct = 100.0 * missing / sent if sent else 100.0
if pct > max_loss:
    fails.append(f"loss: subscriber received {st['received']} of {sent} samples ({pct:.2f} %)")
if sent and 100.0 * st["lost"] / sent > max_loss:
    fails.append(f"loss: subscriber counted {st['lost']} lost")
# A veth pair can reorder a frame when the sending thread moves between
# CPUs; that is the network, not the stacks, so reordering shares the loss
# budget. Duplicates never come from the network.
if sent and 100.0 * st["outOfOrder"] / sent > max_loss:
    fails.append(f"subscriber: outOfOrder {st['outOfOrder']}")
if st["duplicates"]:
    fails.append(f"subscriber: duplicates {st['duplicates']}")

summary = f"sent {sent}, received {st['received']}, lost {st['lost']} ({pct:.2f} %), duplicates {st['duplicates']}, outOfOrder {st['outOfOrder']}"
if fails:
    print(f"FAIL {label}: {summary}")
    for f in fails:
        print(f"  - {f}")
    sys.exit(1)
print(f"PASS {label}: {summary}; {counts[0]} + {counts[1]} sv events match")
PY
