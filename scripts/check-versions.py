#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""Fails when versions.yaml disagrees with the build inputs.

Checks, with the standard library only:
  - interop_version is the newest version in RELEASE.md;
  - every version/revision of an implementation occurs in its pinned_in files;
  - the cbsigma Dockerfile and adapters/cbsigma/go.mod name the same modules;
  - every sha256 in a Dockerfile or the Makefile is listed in versions.yaml,
    and every base image digest listed there is used;
  - every licence of an implementation is an SPDX identifier or expression,
    and occurs in the org.opencontainers.image.licenses label of its image
    where adapters/<name>/Dockerfile sets one;
  - fixtures/capabilities.json: adapterVersion is interop_version, and each
    upstream's version, commit and licence are those of versions.yaml.
"""
import json
import pathlib
import re
import sys

root = pathlib.Path(__file__).resolve().parent.parent
text = (root / "versions.yaml").read_text()
errors = []

# SPDX licence identifiers (https://spdx.org/licenses/) the harness may name;
# add one here when an upstream needs it. The deprecated GNU identifiers
# without -only or -or-later (GPL-3.0, LGPL-2.1, ...) are not SPDX any more.
SPDX_IDS = {
    "0BSD", "AGPL-3.0-only", "AGPL-3.0-or-later", "Apache-2.0", "BSD-2-Clause",
    "BSD-3-Clause", "BSL-1.0", "CC0-1.0", "EPL-1.0", "EPL-2.0", "GPL-2.0-only",
    "GPL-2.0-or-later", "GPL-3.0-only", "GPL-3.0-or-later", "ISC",
    "LGPL-2.1-only", "LGPL-2.1-or-later", "LGPL-3.0-only", "LGPL-3.0-or-later",
    "MIT", "MPL-2.0", "Unlicense", "Zlib",
}


def spdx_problem(expr):
    """None when expr is an SPDX licence expression of SPDX_IDS, else why not."""
    tokens = re.findall(r"\(|\)|[^\s()]+", expr)
    depth, want_operand = 0, True
    for token in tokens:
        if want_operand:
            if token == "(":
                depth += 1
            elif token in SPDX_IDS or token.startswith("LicenseRef-"):
                want_operand = False
            else:
                return f"{token!r} is not an SPDX licence identifier"
        elif token in ("AND", "OR"):
            want_operand = True
        elif token == ")" and depth > 0:
            depth -= 1
        else:
            return f"{token!r} where AND, OR or ) belongs"
    if not tokens or want_operand or depth:
        return "incomplete expression"
    return None


def field(block, key):
    m = re.search(rf'^    {key}: "([^"]+)"', block, re.M)
    return m.group(1) if m else None

declared = re.search(r'^interop_version: "([^"]+)"', text, re.M)
newest = re.search(r"^## v(\d+\.\d+\.\d+)\s*$", (root / "RELEASE.md").read_text(), re.M)
if not declared or not newest or declared.group(1) != newest.group(1):
    errors.append(f"interop_version {declared and declared.group(1)} is not the newest RELEASE.md section {newest and newest.group(1)}")

section = text[text.index("implementations:"):text.index("base_images:")]
blocks = {}
for block in re.split(r"^  (?=[a-z0-9-]+:\n)", section, flags=re.M)[1:]:
    name = block.split(":", 1)[0]
    blocks[name] = block
    files = re.findall(r"^      - (\S+)$", block, re.M)
    if not files:
        errors.append(f"{name}: no pinned_in files")
    sources = "".join((root / f).read_text() for f in files)
    for key in ("version", "revision", "sim_version", "sim_revision", "mbedtls_version", "mbedtls_revision"):
        for value in re.findall(rf'^    {key}: "([^"]+)"', block, re.M):
            if value not in sources:
                errors.append(f"{name}: {key} {value} is not in {files}")
    dockerfile_path = root / "adapters" / name / "Dockerfile"
    label = None
    if dockerfile_path.exists():
        m = re.search(r'org\.opencontainers\.image\.licenses="([^"]+)"', dockerfile_path.read_text())
        label = m.group(1) if m else None
    for key, value in re.findall(r'^    ((?:[a-z0-9]+_)?license): "([^"]*)"', block, re.M):
        problem = spdx_problem(value)
        if problem:
            errors.append(f"{name}: {key} {value!r} is not an SPDX expression: {problem}")
        elif label is not None and not re.search(rf"(?<![\w.+-]){re.escape(value)}(?![\w.+-])", label):
            errors.append(f"{name}: {key} {value} is not in the image label licenses={label!r} of adapters/{name}/Dockerfile")

# fixtures/capabilities.json names the same versions (docs/CONTAINER_CONTRACT.md:
# the pins move in patch or minor releases, recorded there and in manifest.json).
caps = json.loads((root / "fixtures/capabilities.json").read_text())
if declared and caps.get("adapterVersion") != declared.group(1):
    errors.append(f"capabilities.json: adapterVersion {caps.get('adapterVersion')} is not interop_version {declared.group(1)}")
for name, upstream in (caps.get("upstreams") or {}).items():
    block = blocks.get(name)
    if block is None:
        errors.append(f"capabilities.json: upstream {name} is not an implementation of versions.yaml")
        continue
    version, revision = field(block, "version"), field(block, "revision")
    have = upstream.get("version", "")
    if version is not None:
        if have != version:
            errors.append(f"capabilities.json: {name} version {have} is not {version}")
    elif not (revision and re.search(r"-g([0-9a-f]{7,40})$", have) and revision.startswith(have.rsplit("-g", 1)[1])):
        errors.append(f"capabilities.json: {name} version {have} does not name revision {revision}")
    for key, want in (("commit", "revision"), ("simVersion", "sim_version"), ("simCommit", "sim_revision"), ("license", "license")):
        if key in upstream and upstream[key] != field(block, want):
            errors.append(f"capabilities.json: {name} {key} {upstream[key]} is not versions.yaml {want} {field(block, want)}")

# cbsigma: adapters/cbsigma/go.mod is the source of truth of the Go build,
# the Dockerfile checks it at build time; both must name the same tags.
gomod = (root / "adapters/cbsigma/go.mod").read_text()
dockerfile = (root / "adapters/cbsigma/Dockerfile").read_text()
for module, arg in (("github.com/cB-Sigma/iec61850", "IEC61850_VERSION"), ("github.com/cB-Sigma/iec61850-sim", "IEC61850_SIM_VERSION")):
    req = re.search(rf"^\s*(?:require\s+)?{re.escape(module)} (v\S+)", gomod, re.M)
    pin = re.search(rf"^ARG {arg}=(\S+)$", dockerfile, re.M)
    if not req or not pin or req.group(1) != pin.group(1):
        errors.append(f"cbsigma: {module} is {req and req.group(1)} in go.mod and {arg}={pin and pin.group(1)} in the Dockerfile")

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
