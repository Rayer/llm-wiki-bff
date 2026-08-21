import json
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "validate_bff_promotion_contract.py"
AR_REPO = "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images"
SHA = "c" * 40
DIGEST = "sha256:" + "b" * 64
CONFIG_DIGEST = "sha256:" + "a" * 64
RUN_ID = 123
REVISION = "llm-wiki-bff-00001-old"


def receipt():
    return (
        "receipt_schema_version=1\n"
        "component=lwc-bff\n"
        f"source_sha={SHA}\n"
        f"dev_run_id={RUN_ID}\n"
        f"image_digest={DIGEST}\n"
        f"image_reference={AR_REPO}/llm-wiki-bff@{DIGEST}\n"
        "query_config_revision=query-dev-2026-08-21.1\n"
        f"query_config_digest={CONFIG_DIGEST}\n"
    )


class BFFPromotionContractTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = Path(self.tempdir.name)
        self.receipt_path = self.root / "receipt.txt"
        self.run_path = self.root / "run.json"
        self.output = self.root / "readiness.json"
        self.receipt_path.write_text(receipt())
        self.run_path.write_text(json.dumps({
            "id": RUN_ID,
            "path": ".github/workflows/deploy-bff.yml",
            "event": "workflow_dispatch",
            "head_branch": "develop",
            "head_sha": SHA,
            "conclusion": "success",
            "html_url": "https://github.com/Rayer/llm-wiki-bff/actions/runs/123",
        }))

    def tearDown(self):
        self.tempdir.cleanup()

    def invoke_receipt(self, receipt_path=None, **extra):
        command = [
            "python3", str(SCRIPT), "validate-dev-receipt",
            "--receipt", str(receipt_path or self.receipt_path),
            "--run-json", str(self.run_path),
            "--expected-sha", SHA,
            "--expected-run-id", str(RUN_ID),
            "--expected-branch", "develop",
            "--expected-event", "workflow_dispatch",
            "--component", "lwc-bff",
            "--ar-repo", AR_REPO,
            "--query-config-revision", "query-dev-2026-08-21.1",
            "--query-config-digest", CONFIG_DIGEST,
            "--output", str(self.output),
        ]
        for key, value in extra.items():
            command.extend([f"--{key.replace('_', '-')}", str(value)])
        return subprocess.run(command, capture_output=True, text=True)

    def invoke_traffic(self, document, path="traffic", recognized=REVISION):
        source = self.root / "traffic.json"
        source.write_text(json.dumps(document))
        return subprocess.run([
            "python3", str(SCRIPT), "validate-traffic",
            "--traffic-file", str(source),
            "--traffic-path", path,
            "--recognized-revision", recognized,
        ], capture_output=True, text=True)

    def test_real_receipt_shape_normalizes_exact_identity(self):
        result = self.invoke_receipt()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.output.read_text()), {
            "schema_version": 1,
            "receipt_schema_version": 1,
            "component": "lwc-bff",
            "result": "ready",
            "source_sha": SHA,
            "dev_run_id": RUN_ID,
            "dev_run_url": "https://github.com/Rayer/llm-wiki-bff/actions/runs/123",
            "image_digest": DIGEST,
            "image_reference": f"{AR_REPO}/llm-wiki-bff@{DIGEST}",
        })

    def test_receipt_rejects_unknown_missing_trailing_duplicate_ambiguous_and_identity_fields(self):
        cases = {
            "unknown": receipt().replace("component=lwc-bff\n", "unknown=value\n"),
            "missing": receipt().replace(f"dev_run_id={RUN_ID}\n", ""),
            "trailing": receipt() + "trailing=value\n",
            "duplicate": receipt() + f"source_sha={SHA}\n",
            "flattened": receipt().replace("\n", " "),
            "wrong sha": receipt().replace(SHA, "d" * 40),
            "wrong run": receipt().replace(f"dev_run_id={RUN_ID}", "dev_run_id=456"),
            "wrong component": receipt().replace("component=lwc-bff", "component=worker"),
            "tag image": receipt().replace(f"image_reference={AR_REPO}/llm-wiki-bff@{DIGEST}", "image_reference=repo/lwc-bff:latest"),
            "invalid digest": receipt().replace(DIGEST, "sha256:bad"),
            "schema": receipt().replace("receipt_schema_version=1", "receipt_schema_version=2"),
        }
        for name, value in cases.items():
            with self.subTest(name=name):
                path = self.root / f"{name}.txt"
                path.write_text(value)
                result = self.invoke_receipt(path)
                self.assertNotEqual(result.returncode, 0)

    def test_traffic_accepts_explicit_and_resolved_latest_forms(self):
        explicit = self.invoke_traffic({"traffic": [{"revision_name": REVISION, "percent": 100}]})
        latest = self.invoke_traffic({"status": {"traffic": [{"latestRevision": True, "revisionName": REVISION, "percent": 100}]}}, path="status.traffic")
        self.assertEqual(explicit.returncode, 0, explicit.stderr)
        self.assertEqual(latest.returncode, 0, latest.stderr)

    def test_traffic_rejects_split_tagged_mixed_unresolved_and_malformed_forms(self):
        cases = [
            [{"revision_name": REVISION, "percent": 50}, {"revision_name": "llm-wiki-bff-00002-new", "percent": 50}],
            [{"revision_name": REVISION, "percent": 100, "tag": "stable"}],
            [{"revision_name": REVISION, "latestRevision": True, "percent": 100}],
            [{"latestRevision": True, "revisionName": "llm-wiki-bff-00002-new", "percent": 100}],
            [{"latestRevision": False, "revisionName": REVISION, "percent": 100}],
            [{"revision_name": REVISION, "percent": 100, "unexpected": True}],
            [{"revision_name": REVISION, "percent": 99}],
            [{"revision_name": "bff:latest", "percent": 100}],
        ]
        for traffic in cases:
            with self.subTest(traffic=traffic):
                result = self.invoke_traffic({"traffic": traffic})
                self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
