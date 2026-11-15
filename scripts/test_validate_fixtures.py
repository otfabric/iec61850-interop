#!/usr/bin/env python3
# SPDX-License-Identifier: MIT
"""Tests of the capabilities.json check of scripts/validate-fixtures.py."""

import contextlib
import importlib.util
import io
import json
import os
import pathlib
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("validate_fixtures", os.path.join(HERE, "validate-fixtures.py"))
vf = importlib.util.module_from_spec(spec)
spec.loader.exec_module(vf)


class Capabilities(unittest.TestCase):
    def check(self, caps):
        """The number of failures of validate_capabilities on caps, and its output."""
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "capabilities.json"
            path.write_text(json.dumps(caps))
            before = vf.FAIL
            out = io.StringIO()
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
                vf.validate_capabilities(path)
            return vf.FAIL - before, out.getvalue()

    def caps(self):
        return json.loads(vf.CAPABILITIES.read_text())

    def test_the_repository_file_matches(self):
        failures, out = self.check(self.caps())
        self.assertEqual(failures, 0, out)

    def test_a_renamed_key_fails(self):
        caps = self.caps()
        caps["svcbOverMMS"] = caps.pop("svcbOverMms")
        failures, out = self.check(caps)
        self.assertGreater(failures, 0)
        self.assertIn("missing 'svcbOverMms'", out)
        self.assertIn("'svcbOverMMS' is not allowed", out)

    def test_a_renamed_key_of_a_limitation_fails(self):
        caps = self.caps()
        caps["knownLimitations"][0]["reproduced_by"] = caps["knownLimitations"][0].pop("reproducedBy")
        failures, out = self.check(caps)
        self.assertGreater(failures, 0)
        self.assertIn("$.knownLimitations[0]: missing 'reproducedBy'", out)

    def test_a_string_where_an_object_belongs_fails(self):
        caps = self.caps()
        caps["upstreams"]["dscsystems"] = "v0.6.6"
        failures, out = self.check(caps)
        self.assertGreater(failures, 0)
        self.assertIn("$.upstreams.dscsystems: str where the schema wants object", out)

    def test_wrong_values_fail(self):
        for key, value, message in (
            ("schemaVersion", 2, "$.schemaVersion: 2 is not 1"),
            ("schemaVersion", True, "$.schemaVersion: True is not 1"),
            ("adapterVersion", "v1.8.0", "does not match"),
            ("fixtureRevision", "", "does not match"),
        ):
            with self.subTest(key=key, value=value):
                caps = self.caps()
                caps[key] = value
                failures, out = self.check(caps)
                self.assertGreater(failures, 0)
                self.assertIn(message, out)

    def test_a_command_that_is_not_a_role_fails(self):
        caps = self.caps()
        caps["commands"]["libiec61850-ied-sever"] = True
        failures, out = self.check(caps)
        self.assertGreater(failures, 0)
        self.assertIn("libiec61850-ied-sever (name)", out)

    def test_an_unsupported_schema_keyword_is_refused(self):
        errors = vf.schema_errors(1, {"type": "integer", "maximum": 0}, {})
        self.assertEqual(len(errors), 1)
        self.assertIn("not supported", errors[0])


if __name__ == "__main__":
    unittest.main()
