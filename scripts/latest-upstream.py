#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""Compares the upstream pin of one adapter with the latest upstream.

    scripts/latest-upstream.py <adapter>

Prints key=value lines (the format of $GITHUB_OUTPUT):

    kind=library|commit
    pin=<the pin: a version, or a commit with its tag if it has one>
    latest=<the latest upstream, in the same form>
    latest_sha=<the latest commit, for an adapter pinned by commit>
    update=yes|no
    source=<where the latest was looked up>

libiec61850, dscsystems and iec61850-rust are pinned by commit and compared
with the head of the upstream default branch; iec61850bean is compared with
the latest stable release on Maven Central. Nothing is modified.
"""

import re
import subprocess
import sys
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AGENT = "iec61850-interop latest-upstream (https://github.com/otfabric/iec61850-interop)"


def fetch(url):
    request = urllib.request.Request(url, headers={"User-Agent": AGENT})
    with urllib.request.urlopen(request, timeout=30) as response:
        return response.read().decode()


def key(version):
    """Sort key of a stable version; None for anything else (pre-releases)."""
    match = re.fullmatch(r"v?(\d+(?:\.\d+)*)", version)
    return tuple(int(part) for part in match.group(1).split(".")) if match else None


def read_arg(adapter, name):
    dockerfile = (ROOT / "adapters" / adapter / "Dockerfile").read_text()
    match = re.search(rf"^ARG {name}=([0-9a-f]{{40}})$", dockerfile, re.M)
    if not match:
        raise SystemExit(f"adapters/{adapter}/Dockerfile has no ARG {name}=<commit>")
    return match.group(1)


def commit(adapter, arg, repository):
    """Pin (a commit ARG) vs the head of the default branch of repository."""
    pin = read_arg(adapter, arg)
    out = subprocess.check_output(["git", "ls-remote", repository, "HEAD", "refs/tags/*"], text=True)
    head, tags = None, {}
    for line in out.splitlines():
        sha, ref = line.split()
        if ref == "HEAD":
            head = sha
        elif ref.startswith("refs/tags/"):
            tag = ref.removeprefix("refs/tags/")
            if tag.endswith("^{}"):
                tags[sha] = tag[:-3]  # the commit of an annotated tag
            else:
                tags.setdefault(sha, tag)
    if not head:
        raise SystemExit(f"{repository} has no HEAD")

    def label(sha):
        return f"{sha[:12]} ({tags[sha]})" if sha in tags else sha[:12]

    name = repository.removeprefix("https://github.com/").removesuffix(".git")
    return "commit", label(pin), label(head), head, "yes" if head != pin else "no", f"HEAD of {name}"


def libiec61850():
    return commit("libiec61850", "LIBIEC61850_SHA", "https://github.com/mz-automation/libiec61850.git")


def dscsystems():
    return commit("dscsystems", "DSCSYSTEMS_SHA", "https://github.com/dscsystems/go-iec61850.git")


def iec61850_rust():
    return commit("iec61850-rust", "IEC61850_RUST_SHA", "https://github.com/csp0924/iec61850-rust.git")


def iec61850bean():
    pom = (ROOT / "adapters" / "iec61850bean" / "pom.xml").read_text()
    match = re.search(r"<artifactId>iec61850bean</artifactId>\s*<version>([^<]+)</version>", pom)
    if not match:
        raise SystemExit("adapters/iec61850bean/pom.xml has no com.beanit:iec61850bean version")
    pin = match.group(1)
    meta = fetch("https://repo1.maven.org/maven2/com/beanit/iec61850bean/maven-metadata.xml")
    stable = [v for v in re.findall(r"<version>([^<]+)</version>", meta) if key(v)]
    if not stable:
        raise SystemExit("no stable iec61850bean release on Maven Central")
    latest = max(stable, key=key)
    return "library", pin, latest, "", "yes" if key(latest) != key(pin) else "no", "Maven Central"


RESOLVERS = {
    "libiec61850": libiec61850,
    "iec61850bean": iec61850bean,
    "dscsystems": dscsystems,
    "iec61850-rust": iec61850_rust,
}


def main():
    if len(sys.argv) != 2 or sys.argv[1] not in RESOLVERS:
        raise SystemExit(f"usage: latest-upstream.py {'|'.join(RESOLVERS)}")
    kind, pin, latest, latest_sha, update, source = RESOLVERS[sys.argv[1]]()
    print(f"kind={kind}")
    print(f"pin={pin}")
    print(f"latest={latest}")
    print(f"latest_sha={latest_sha}")
    print(f"update={update}")
    print(f"source={source}")


if __name__ == "__main__":
    main()
