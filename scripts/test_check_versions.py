#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""Tests of scripts/check-versions.py: it runs on a copy of the files it reads,
with one value changed."""

import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent


class CheckVersions(unittest.TestCase):
    def setUp(self):
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        for name in os.listdir(ROOT / "adapters"):
            shutil.copytree(ROOT / "adapters" / name, self.tmp / "adapters" / name,
                            ignore=shutil.ignore_patterns("build", "target", "src", "cmd", "internal"))
        (self.tmp / "own/go-iec61850").mkdir(parents=True)
        shutil.copy(ROOT / "own/go-iec61850/Dockerfile", self.tmp / "own/go-iec61850/Dockerfile")
        (self.tmp / "fixtures").mkdir()
        (self.tmp / "scripts").mkdir()
        for f in ("versions.yaml", "RELEASE.md", "Makefile", "fixtures/capabilities.json", "scripts/check-versions.py"):
            shutil.copy(ROOT / f, self.tmp / f)

    def edit(self, path, old, new):
        p = self.tmp / path
        text = p.read_text()
        self.assertIn(old, text)
        p.write_text(text.replace(old, new, 1))

    def run_check(self):
        return subprocess.run([sys.executable, str(self.tmp / "scripts/check-versions.py")],
                              capture_output=True, text=True)

    def assertFails(self, message):
        r = self.run_check()
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn(message, r.stderr)

    def test_the_repository_passes(self):
        r = self.run_check()
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_a_licence_with_free_text_is_refused(self):
        self.edit("versions.yaml", 'license: "GPL-3.0-only"', 'license: "GPL-3.0-only (or commercial)"')
        self.assertFails("libiec61850: license 'GPL-3.0-only (or commercial)' is not an SPDX expression")

    def test_a_deprecated_identifier_is_refused(self):
        self.edit("versions.yaml", 'revision: "4833767caf3abbf1f205c4c49cd9432744beaf83"\n    license: "GPL-3.0-only"',
                  'revision: "4833767caf3abbf1f205c4c49cd9432744beaf83"\n    license: "GPL-3.0"')
        self.assertFails("dscsystems: license 'GPL-3.0' is not an SPDX expression")

    def test_a_licence_must_be_in_the_image_label(self):
        self.edit("adapters/dscsystems/Dockerfile", 'licenses="GPL-3.0-only"', 'licenses="GPL-3.0-or-later"')
        self.assertFails("dscsystems: license GPL-3.0-only is not in the image label")

    def test_an_expression_is_accepted(self):
        self.edit("versions.yaml", 'mbedtls_license: "Apache-2.0 OR GPL-2.0-or-later"',
                  'mbedtls_license: "(Apache-2.0 OR GPL-2.0-or-later)"')
        self.edit("adapters/libiec61850/Dockerfile", "AND (Apache-2.0 OR GPL-2.0-or-later)",
                  "AND ((Apache-2.0 OR GPL-2.0-or-later))")
        r = self.run_check()
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_adapter_version_is_the_release(self):
        self.edit("fixtures/capabilities.json", '"adapterVersion": "', '"adapterVersion": "0.')
        self.assertFails("capabilities.json: adapterVersion 0.")

    def test_an_upstream_version_is_that_of_versions_yaml(self):
        self.edit("fixtures/capabilities.json", '"version": "1.9.0"\n', '"version": "1.x"\n')
        self.assertFails("capabilities.json: iec61850bean version 1.x is not 1.9.0")

    def test_a_described_version_names_the_revision(self):
        self.edit("fixtures/capabilities.json", '"version": "1.6.2.1-89-g6e265c54"\n', '"version": "1.6.2.1-89-g0123abcd"\n')
        self.assertFails("capabilities.json: libiec61850 version 1.6.2.1-89-g0123abcd does not name revision")

    def test_an_upstream_licence_is_that_of_versions_yaml(self):
        self.edit("fixtures/capabilities.json", '"license": "GPL-3.0-only"', '"license": "GPL-3.0"')
        self.assertFails("capabilities.json: dscsystems license GPL-3.0 is not versions.yaml license GPL-3.0-only")


if __name__ == "__main__":
    unittest.main()
