import json
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "validate_auth_promotion_contract.py"
AR_REPO = "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images"
SHA = "c" * 40
DIGEST = "sha256:" + "b" * 64
RUN_ID = 123


def receipt():
    return (
        "receipt_schema_version=1\n"
        "component=lwc-auth\n"
        "build_ref=develop\n"
        f"source_sha={SHA}\n"
        f"dev_run_id={RUN_ID}\n"
        f"image_digest={DIGEST}\n"
        f"image_reference={AR_REPO}/llm-wiki-auth@{DIGEST}\n"
    )


class AuthPromotionContractTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = Path(self.tempdir.name)
        self.receipt_path = self.root / "receipt.txt"
        self.run_path = self.root / "run.json"
        self.output = self.root / "normalized.json"
        self.receipt_path.write_text(receipt())
        self.run_path.write_text(json.dumps({
            "id": RUN_ID,
            "path": ".github/workflows/deploy-auth.yml",
            "event": "workflow_dispatch",
            "head_branch": "develop",
            "head_sha": SHA,
            "status": "completed",
            "conclusion": "success",
            "html_url": "https://github.com/Rayer/llm-wiki-bff/actions/runs/123",
        }))

    def tearDown(self):
        self.tempdir.cleanup()

    def invoke(self, receipt_path=None, **extra):
        command = [
            "python3", str(SCRIPT),
            "--receipt", str(receipt_path or self.receipt_path),
            "--run-json", str(self.run_path),
            "--expected-sha", SHA,
            "--expected-run-id", str(RUN_ID),
            "--repository", "Rayer/llm-wiki-bff",
            "--ar-repo", AR_REPO,
            "--output", str(self.output),
        ]
        for key, value in extra.items():
            command.extend([f"--{key.replace('_', '-')}", str(value)])
        return subprocess.run(command, capture_output=True, text=True)

    def test_exact_receipt_and_run_normalize(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.output.read_text())["image_reference"], f"{AR_REPO}/llm-wiki-auth@{DIGEST}")

    def test_exact_receipt_exports_validated_provenance_only(self):
        github_output = self.root / "github-output"
        result = self.invoke(github_output=github_output)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            github_output.read_text().splitlines(),
            [
                f"source_sha={SHA}",
                f"dev_run_id={RUN_ID}",
                "dev_run_url=https://github.com/Rayer/llm-wiki-bff/actions/runs/123",
                "dev_workflow=.github/workflows/deploy-auth.yml",
                "dev_event=workflow_dispatch",
                "dev_head_branch=develop",
                f"dev_head_sha={SHA}",
                "dev_conclusion=success",
                f"digest={DIGEST}",
                f"image={AR_REPO}/llm-wiki-auth@{DIGEST}",
            ],
        )

    def test_exact_binding_is_sensitive_to_sha_run_and_digest(self):
        cases = {
            "sha": receipt().replace(SHA, "d" * 40),
            "run": receipt().replace(f"dev_run_id={RUN_ID}", "dev_run_id=456"),
            "digest": receipt().replace(f"image_digest={DIGEST}", "image_digest=sha256:" + "a" * 64),
        }
        for name, value in cases.items():
            with self.subTest(name=name):
                path = self.root / f"{name}.txt"
                path.write_text(value)
                self.assertNotEqual(self.invoke(path).returncode, 0)

    def test_rejects_mutable_tag_image_reference(self):
        tagged = receipt().replace(
            f"image_reference={AR_REPO}/llm-wiki-auth@{DIGEST}",
            f"image_reference={AR_REPO}/llm-wiki-auth:latest",
        )
        path = self.root / "tagged.txt"
        path.write_text(tagged)
        self.assertNotEqual(self.invoke(path).returncode, 0)

    def test_producer_run_must_be_completed_successful_canonical_auth_run(self):
        original = json.loads(self.run_path.read_text())
        for field, value in (("path", ".github/workflows/deploy-bff.yml"), ("head_branch", "main"), ("head_sha", "d" * 40), ("conclusion", "failure")):
            with self.subTest(field=field):
                self.run_path.write_text(json.dumps({**original, field: value}))
                self.assertNotEqual(self.invoke().returncode, 0)


if __name__ == "__main__":
    unittest.main()
