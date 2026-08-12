#!/usr/bin/env python3
import difflib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "postprocess_swagger.py"

JSON_FIXTURE = '''{
    "definitions": {
        "unrelated": {"type": "string"},
        "v1.renameProjectRequest": {
            "type": "object",
            "required": ["name"],
            "properties": {"name": {"type": "string"}}
        }
    },
    "paths": {}
}
'''
YAML_FIXTURE = '''swagger: "2.0"
definitions:
  unrelated:
    type: string
  v1.renameProjectRequest:
    properties:
      name:
        type: string
    required:
    - name
    type: object
paths: {}
'''
GO_FIXTURE = '''package docs

const docTemplate = `''' + JSON_FIXTURE + '''`

// SwaggerInfo holds exported Swagger Info
'''


class PostprocessSwaggerTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.docs = Path(self.tempdir.name)
        (self.docs / "swagger.json").write_text(JSON_FIXTURE)
        (self.docs / "swagger.yaml").write_text(YAML_FIXTURE)
        (self.docs / "docs.go").write_text(GO_FIXTURE)

    def tearDown(self):
        self.tempdir.cleanup()

    def invoke(self):
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--docs-dir", str(self.docs)],
            capture_output=True,
            text=True,
        )

    def test_idempotence_and_no_unrelated_drift(self):
        before = {path.name: path.read_text() for path in self.docs.iterdir()}
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        after = {path.name: path.read_text() for path in self.docs.iterdir()}

        document = json.loads(before["swagger.json"])
        document["definitions"]["v1.renameProjectRequest"]["additionalProperties"] = False
        self.assertEqual(json.loads(after["swagger.json"]), document)
        self.assertEqual(
            after["swagger.yaml"],
            before["swagger.yaml"].replace(
                "  v1.renameProjectRequest:\n",
                "  v1.renameProjectRequest:\n    additionalProperties: false\n",
                1,
            ),
        )
        self.assertEqual(
            after["docs.go"],
            before["docs.go"].replace(
                '"v1.renameProjectRequest": {\n            "type":',
                '"v1.renameProjectRequest": {\n            "additionalProperties": false,\n            "type":',
                1,
            ),
        )
        first = after
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual({path.name: path.read_text() for path in self.docs.iterdir()}, first)

    def test_missing_definition_fails_without_writing(self):
        path = self.docs / "swagger.json"
        path.write_text(JSON_FIXTURE.replace(
            '        "unrelated": {"type": "string"},\n        "v1.renameProjectRequest": {\n            "type": "object",\n            "required": ["name"],\n            "properties": {"name": {"type": "string"}}\n        }\n',
            '        "unrelated": {"type": "string"}\n',
        ))
        before = {path.name: path.read_bytes() for path in self.docs.iterdir()}
        result = self.invoke()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing v1.renameProjectRequest", result.stderr)
        self.assertEqual({path.name: path.read_bytes() for path in self.docs.iterdir()}, before)

    def test_diff_is_limited_to_target_flag(self):
        before = {path.name: path.read_text().splitlines() for path in self.docs.iterdir()}
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        for path in self.docs.iterdir():
            diff = list(difflib.unified_diff(before[path.name], path.read_text().splitlines()))
            changed = [line for line in diff if line.startswith(("+", "-")) and not line.startswith(("+++", "---"))]
            self.assertEqual(len(changed), 1, (path.name, diff))
            self.assertIn("additionalProperties", changed[0])
            self.assertIn("false", changed[0])


if __name__ == "__main__":
    unittest.main()
