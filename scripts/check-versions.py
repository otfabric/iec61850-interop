#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""Fails when versions.yaml disagrees with the build inputs.

Checks, with the standard library only:
  - interop_version is the newest version in RELEASE.md;
  - every version/revision of an implementation occurs in its pinned_in files;
  - every sha256 in a Dockerfile or the Makefile is listed in versions.yaml,
    and every base image digest listed there is used.
"""
import pathlib
import re
import sys

root = pathlib.Path(__file__).resolve().parent.parent
text = (root / "versions.yaml").read_text()
errors = []

declared = re.search(r'^interop_version: "([^"]+)"', text, re.M)
newest = re.search(r"^## v(\d+\.\d+\.\d+)\s*$", (root / "RELEASE.md").read_text(), re.M)
if not declared or not newest or declared.group(1) != newest.group(1):
    errors.append(f"interop_version {declared and declared.group(1)} is not the newest RELEASE.md section {newest and newest.group(1)}")

section = text[text.index("implementations:"):text.index("base_images:")]
for block in re.split(r"^  (?=[a-z0-9-]+:\n)", section, flags=re.M)[1:]:
    name = block.split(":", 1)[0]
    files = re.findall(r"^      - (\S+)$", block, re.M)
    if not files:
        errors.append(f"{name}: no pinned_in files")
    sources = "".join((root / f).read_text() for f in files)
    for key in ("version", "revision"):
        for value in re.findall(rf'^    {key}: "([^"]+)"', block, re.M):
            if value not in sources:
                errors.append(f"{name}: {key} {value} is not in {files}")

digests = set(re.findall(r"sha256:[0-9a-f]{64}", text[text.index("base_images:"):]))
used = set()
for path in [*root.glob("**/Dockerfile"), root / "Makefile"]:
    used |= set(re.findall(r"sha256:[0-9a-f]{64}", path.read_text()))
for digest in sorted(used - digests):
    errors.append(f"{digest} is used by a build but not listed in versions.yaml")
for digest in sorted(digests - used):
    errors.append(f"{digest} is listed in versions.yaml but no build uses it")

for error in errors:
    print(f"error: {error}", file=sys.stderr)
if errors:
    sys.exit(1)
print("ok   versions.yaml agrees with the build inputs")
