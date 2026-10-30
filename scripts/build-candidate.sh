#!/usr/bin/env bash
# build-candidate.sh — build one adapter image at a newer upstream (a candidate).
#
#   scripts/build-candidate.sh <adapter> <commit|version>
#
# libiec61850, dscsystems and iec61850-rust take an upstream commit,
# iec61850bean a Maven version. The pin is moved in the working tree only
# (Dockerfile ARG, go.mod/go.sum, Cargo.toml/Cargo.lock, pom.xml) and the
# image is built with `make build-<adapter>` as iec61850-interop-<adapter>:local.
# Used by the Upstream workflow on a throwaway checkout; locally, `git checkout
# adapters/` undoes the edits. dscsystems needs Go and iec61850-rust Cargo on
# the host to update the lock files.
#
# Prints "[candidate] <adapter> <commit|version> built" when the build succeeds.

set -euo pipefail
cd "$(dirname "$0")/.."

adapter="${1:-}"
want="${2:-}"
if [[ -z "${adapter}" || -z "${want}" ]]; then
  echo "usage: build-candidate.sh libiec61850|iec61850bean|dscsystems|iec61850-rust <commit|version>" >&2
  exit 2
fi

# set_arg FILE NAME VALUE — move ARG NAME=<...> in FILE, fail if it is not there.
set_arg() {
  sed -i.bak -E "s|^ARG $2=[0-9a-f]{40}$|ARG $2=$3|" "$1" && rm -f "$1.bak"
  grep -q "^ARG $2=$3$" "$1" || { echo "build-candidate: no ARG $2 in $1" >&2; exit 1; }
}

case "${adapter}" in
  libiec61850)
    set_arg adapters/libiec61850/Dockerfile LIBIEC61850_SHA "${want}"
    ;;
  iec61850bean)
    python3 - "${want}" <<'PY'
import pathlib, re, sys
path = pathlib.Path("adapters/iec61850bean/pom.xml")
text, n = re.subn(r"(<artifactId>iec61850bean</artifactId>\s*<version>)[^<]+(</version>)",
                  rf"\g<1>{sys.argv[1]}\g<2>", path.read_text(), count=1)
if n != 1:
    raise SystemExit("build-candidate: no com.beanit:iec61850bean version in pom.xml")
path.write_text(text)
PY
    ;;
  dscsystems)
    (cd adapters/dscsystems \
      && go get "github.com/dscsystems/go-iec61850@${want}" \
      && go mod tidy)
    set_arg adapters/dscsystems/Dockerfile DSCSYSTEMS_SHA "${want}"
    # A tagged commit resolves to its tag (v0.6.6), not to a pseudo-version
    # ending in the commit, so check go.mod for the version go get chose.
    version=$(awk '$1 == "require" && $2 == "github.com/dscsystems/go-iec61850" { print $3 }' adapters/dscsystems/go.mod)
    if [[ "${version}" != *-"${want:0:12}" ]]; then
      python3 - "${version}" <<'PY'
import pathlib, sys
path = pathlib.Path("adapters/dscsystems/Dockerfile")
old = 'go-iec61850 v.*-$(echo "${DSCSYSTEMS_SHA}" | cut -c1-12)$"'
text = path.read_text()
if text.count(old) != 1:
    raise SystemExit("build-candidate: the go.mod check in adapters/dscsystems/Dockerfile moved")
path.write_text(text.replace(old, 'go-iec61850 %s$"' % sys.argv[1].replace(".", "\\.")))
PY
    fi
    ;;
  iec61850-rust)
    old=$(sed -nE 's/^ARG IEC61850_RUST_SHA=([0-9a-f]{40})$/\1/p' adapters/iec61850-rust/Dockerfile)
    sed -i.bak "s/${old}/${want}/g" adapters/iec61850-rust/Cargo.toml && rm -f adapters/iec61850-rust/Cargo.toml.bak
    set_arg adapters/iec61850-rust/Dockerfile IEC61850_RUST_SHA "${want}"
    # Re-resolves only the moved git source into Cargo.lock.
    cargo metadata --format-version 1 --manifest-path adapters/iec61850-rust/Cargo.toml > /dev/null
    ;;
  *)
    echo "build-candidate: unknown adapter ${adapter}" >&2
    exit 2
    ;;
esac

git --no-pager diff --stat -- adapters/
make "build-${adapter}" ADAPTER_VERSION="candidate"
echo "[candidate] ${adapter} ${want:0:12} built"
