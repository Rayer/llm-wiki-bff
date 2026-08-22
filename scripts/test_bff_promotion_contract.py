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
        "receipt_schema_version=2\n"
        "component=lwc-bff\n"
        "build_ref=develop\n"
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
            "status": "in_progress",
            "conclusion": None,
            "html_url": "https://github.com/Rayer/llm-wiki-bff/actions/runs/123",
        }))

    def tearDown(self):
        self.tempdir.cleanup()

    def invoke_receipt(self, receipt_path=None, lifecycle="readiness", **extra):
        command = [
            "python3", str(SCRIPT), "validate-dev-receipt",
            "--receipt", str(receipt_path or self.receipt_path),
            "--run-json", str(self.run_path),
            "--expected-sha", SHA,
            "--expected-run-id", str(RUN_ID),
            "--expected-branch", "develop",
            "--expected-event", "workflow_dispatch",
            "--lifecycle", lifecycle,
            "--producer-result", "success",
            "--component", "lwc-bff",
            "--repository", "Rayer/llm-wiki-bff",
            "--ar-repo", AR_REPO,
            "--query-config-revision", "query-dev-2026-08-21.1",
            "--query-config-digest", CONFIG_DIGEST,
            "--output", str(self.output),
        ]
        for key, value in extra.items():
            command.extend([f"--{key.replace('_', '-')}", str(value)])
        return subprocess.run(command, capture_output=True, text=True)

    def invoke_traffic(self, document, path="traffic", recognized=REVISION, mode="artifact", expected=None, compare_path=None):
        source = self.root / "traffic.json"
        source.write_text(json.dumps(document))
        command = [
            "python3", str(SCRIPT), "validate-traffic",
            "--traffic-file", str(source),
            "--traffic-path", path,
            "--traffic-mode", mode,
            "--recognized-revision", recognized,
        ]
        if expected:
            command.extend(["--expected-revision", expected])
        if compare_path:
            command.extend(["--compare-path", compare_path])
        return subprocess.run(command, capture_output=True, text=True)

    def invoke_run_jobs(self, jobs):
        path = self.root / "jobs.json"
        path.write_text(json.dumps(jobs))
        return subprocess.run([
            "python3", str(SCRIPT), "validate-run-jobs",
            "--jobs-json", str(path), "--expected-run-id", str(RUN_ID),
        ], capture_output=True, text=True)

    def test_real_receipt_shape_normalizes_exact_identity(self):
        result = self.invoke_receipt()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.output.read_text()), {
            "schema_version": 1,
            "receipt_schema_version": 2,
            "component": "lwc-bff",
            "build_ref": "develop",
            "result": "ready",
            "source_sha": SHA,
            "dev_run_id": RUN_ID,
            "dev_run_url": "https://github.com/Rayer/llm-wiki-bff/actions/runs/123",
            "image_digest": DIGEST,
            "image_reference": f"{AR_REPO}/llm-wiki-bff@{DIGEST}",
        })

    def test_receipt_lifecycle_is_explicit_and_fail_closed(self):
        self.assertEqual(self.invoke_receipt().returncode, 0)
        run = json.loads(self.run_path.read_text())
        for field, value in (("status", "completed"), ("conclusion", "success")):
            with self.subTest(field=field):
                self.run_path.write_text(json.dumps({**run, field: value}))
                self.assertNotEqual(self.invoke_receipt().returncode, 0)
        self.run_path.write_text(json.dumps({**run, "status": "completed", "conclusion": "success"}))
        self.assertEqual(self.invoke_receipt(lifecycle="production").returncode, 0)

    def test_output_integrity_and_atomic_readiness_receipt(self):
        github_output = self.root / "github-output"
        result = self.invoke_receipt(github_output=github_output)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("dev_run_url=https://github.com/Rayer/llm-wiki-bff/actions/runs/123\n", github_output.read_text())
        self.assertEqual(self.output.read_text().count("\n"), 1)

    def test_production_readiness_must_be_the_exact_normalized_develop_receipt(self):
        self.assertEqual(self.invoke_receipt().returncode, 0)
        command = [
            "python3", str(SCRIPT), "validate-production-readiness",
            "--readiness", str(self.output), "--expected-sha", SHA,
            "--expected-run-id", str(RUN_ID), "--expected-branch", "develop",
            "--component", "lwc-bff", "--repository", "Rayer/llm-wiki-bff",
            "--ar-repo", AR_REPO, "--receipt", str(self.receipt_path),
        ]
        self.assertEqual(subprocess.run(command, capture_output=True, text=True).returncode, 0)
        readiness = json.loads(self.output.read_text())
        readiness["build_ref"] = "main"
        self.output.write_text(json.dumps(readiness))
        self.assertNotEqual(subprocess.run(command, capture_output=True, text=True).returncode, 0)

    def test_receipt_rejects_unknown_missing_trailing_duplicate_ambiguous_and_identity_fields(self):
        cases = {
            "unknown": receipt().replace("component=lwc-bff\n", "unknown=value\n"),
            "missing": receipt().replace(f"dev_run_id={RUN_ID}\n", ""),
            "trailing": receipt() + "trailing=value\n",
            "duplicate": receipt() + f"source_sha={SHA}\n",
            "flattened": receipt().replace("\n", " "),
            "unicode": receipt().replace("component=lwc-bff", "component=lwc-bff☃"),
            "newline": receipt().replace("component=lwc-bff", "component=lwc-bff\nextra=value"),
            "wrong sha": receipt().replace(SHA, "d" * 40),
            "wrong run": receipt().replace(f"dev_run_id={RUN_ID}", "dev_run_id=456"),
            "wrong component": receipt().replace("component=lwc-bff", "component=worker"),
            "tag image": receipt().replace(f"image_reference={AR_REPO}/llm-wiki-bff@{DIGEST}", "repo/lwc-bff:latest"),
            "invalid digest": receipt().replace(DIGEST, "sha256:bad"),
            "schema": receipt().replace("receipt_schema_version=2", "receipt_schema_version=1"),
        }
        for name, value in cases.items():
            with self.subTest(name=name):
                path = self.root / f"{name}.txt"
                path.write_bytes(value.encode())
                self.assertNotEqual(self.invoke_receipt(path).returncode, 0)

    def test_receipt_line_count_diagnostic_names_nine_lines(self):
        path = self.root / "unterminated-receipt.txt"
        path.write_bytes(receipt().rstrip("\n").encode())
        result = self.invoke_receipt(path)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stderr, "promotion contract rejected: receipt must use exactly nine LF-terminated lines\n")

    def test_run_jobs_require_unique_successful_same_run_promotion_evidence(self):
        jobs = [
            {"name": "production-promotion-ready", "run_id": RUN_ID, "status": "completed", "conclusion": "success"},
            {"name": "main-fast-forward-eligible", "run_id": RUN_ID, "status": "completed", "conclusion": "success"},
        ]
        self.assertEqual(self.invoke_run_jobs(jobs).returncode, 0)
        cases = {
            "missing": jobs[:1],
            "skipped": [{**jobs[1], "conclusion": "skipped"}, jobs[0]],
            "failed": [{**jobs[0], "conclusion": "failure"}, jobs[1]],
            "duplicate": jobs + [jobs[0]],
            "wrong run": [{**jobs[0], "run_id": RUN_ID + 1}, jobs[1]],
        }
        for name, value in cases.items():
            with self.subTest(name=name):
                self.assertNotEqual(self.invoke_run_jobs(value).returncode, 0)

    def test_receipt_rejects_noncanonical_run_url_without_writing_output(self):
        original = json.loads(self.run_path.read_text())
        for url in (
            "https://github.com/Rayer/llm-wiki-bff/actions/runs/123?x=1",
            "https://github.com/Rayer/llm-wiki-bff/actions/runs/123\r\nX-Injected: yes",
        ):
            with self.subTest(url=url):
                self.run_path.write_text(json.dumps({**original, "html_url": url}))
                output = self.root / "rejected-output"
                self.assertNotEqual(self.invoke_receipt(github_output=output).returncode, 0)
                self.assertFalse(output.exists())

    def test_receipt_rejects_non_finite_json_constants(self):
        original = self.run_path.read_text()
        for constant in ("NaN", "Infinity", "-Infinity"):
            with self.subTest(constant=constant):
                self.run_path.write_text(original.replace('"id": 123', f'"id": {constant}', 1))
                self.assertNotEqual(self.invoke_receipt().returncode, 0)

    def test_traffic_accepts_pre_mutation_explicit_and_resolved_latest_forms(self):
        explicit = self.invoke_traffic({"traffic": [{"revisionName": REVISION, "percent": 100}]}, mode="provider-pre-mutation")
        explicit_false = self.invoke_traffic({"traffic": [{"latestRevision": False, "revisionName": REVISION, "percent": 100}]}, mode="provider-pre-mutation")
        latest = self.invoke_traffic({"status": {"traffic": [{"latestRevision": True, "revisionName": REVISION, "percent": 100}]}}, path="status.traffic", mode="provider-pre-mutation")
        self.assertEqual(explicit.returncode, 0, explicit.stderr)
        self.assertEqual(explicit_false.returncode, 0, explicit_false.stderr)
        self.assertEqual(latest.returncode, 0, latest.stderr)

    def test_post_rollback_requires_explicit_frozen_revision(self):
        explicit = self.invoke_traffic({"status": {"traffic": [{"revisionName": REVISION, "percent": 100}]}}, path="status.traffic", mode="provider-post-rollback", expected=REVISION)
        false_latest = self.invoke_traffic({"status": {"traffic": [{"latestRevision": False, "revisionName": REVISION, "percent": 100}]}}, path="status.traffic", mode="provider-post-rollback", expected=REVISION)
        latest = self.invoke_traffic({"status": {"traffic": [{"latestRevision": True, "revisionName": REVISION, "percent": 100}]}}, path="status.traffic", mode="provider-post-rollback", expected=REVISION)
        self.assertEqual(explicit.returncode, 0, explicit.stderr)
        self.assertEqual(false_latest.returncode, 0, false_latest.stderr)
        self.assertNotEqual(latest.returncode, 0)

    def test_dev_status_spec_convergence_is_stronger_than_production_preflight(self):
        converged = {
            "status": {"traffic": [{"revisionName": REVISION, "percent": 100}]},
            "spec": {"traffic": [{"revisionName": REVISION, "percent": 100}]},
        }
        latest = {
            "status": {"traffic": [{"latestRevision": True, "revisionName": REVISION, "percent": 100}]},
            "spec": {"traffic": [{"latestRevision": True, "revisionName": REVISION, "percent": 100}]},
        }
        source = self.root / "service.json"
        for document, expected in ((converged, 0), (latest, 1)):
            source.write_text(json.dumps(document))
            result = subprocess.run([
                "python3", str(SCRIPT), "validate-traffic", "--traffic-file", str(source),
                "--traffic-path", "status.traffic", "--compare-path", "spec.traffic",
                "--traffic-mode", "provider-dev-convergence", "--recognized-revision", REVISION,
            ], capture_output=True, text=True)
            self.assertEqual(result.returncode, expected, result.stderr)

    def test_traffic_rejects_split_tagged_mixed_unresolved_and_malformed_forms(self):
        cases = [
            [{"revisionName": REVISION, "percent": 50}, {"revisionName": "llm-wiki-bff-00002-new", "percent": 50}],
            [{"revisionName": REVISION, "percent": 100, "tag": "stable"}],
            [{"revisionName": REVISION, "latestRevision": True, "percent": 100, "revision_name": REVISION}],
            [{"latestRevision": True, "revisionName": "llm-wiki-bff-00002-new", "percent": 100}],
            [{"revisionName": REVISION, "percent": 100, "unexpected": True}],
            [{"revisionName": REVISION, "percent": 99}],
            [{"revisionName": "bff:latest", "percent": 100}],
        ]
        for traffic in cases:
            with self.subTest(traffic=traffic):
                self.assertNotEqual(self.invoke_traffic({"traffic": traffic}, mode="provider-pre-mutation").returncode, 0)

    def test_traffic_rejects_dialect_duplicate_trailing_and_ambiguous_artifacts(self):
        for document in (
            {"traffic": [{"revisionName": REVISION, "percent": 100}]},
            {"traffic": [{"revision_name": REVISION, "percent": 100, "latest_revision": "true"}]},
            {"traffic": [{"revision_name": REVISION, "percent": 100, "tag": "stable"}]},
            {"traffic": [{"revision_name": REVISION, "percent": 100, "extra": False}]},
        ):
            with self.subTest(document=document):
                self.assertNotEqual(self.invoke_traffic(document, mode="artifact").returncode, 0)
        duplicate = self.root / "duplicate.json"
        duplicate.write_text('{"traffic":[{"revisionName":"%s","percent":100}],"traffic":[{"revisionName":"%s","percent":100}]}' % (REVISION, REVISION))
        self.assertNotEqual(self.invoke_traffic_from_path(duplicate, "provider-pre-mutation").returncode, 0)
        trailing = self.root / "trailing.json"
        trailing.write_text('{"traffic":[]} {}')
        self.assertNotEqual(self.invoke_traffic_from_path(trailing, "provider-pre-mutation").returncode, 0)

    def test_traffic_rejects_non_finite_json_constants(self):
        baseline = json.dumps({"traffic": [{"revisionName": REVISION, "percent": 100}]})
        for constant in ("NaN", "Infinity", "-Infinity"):
            with self.subTest(constant=constant):
                source = self.root / "non-finite-traffic.json"
                source.write_text(baseline.replace('"percent": 100', f'"percent": {constant}', 1))
                result = subprocess.run([
                    "python3", str(SCRIPT), "validate-traffic",
                    "--traffic-file", str(source),
                    "--traffic-path", "traffic",
                    "--traffic-mode", "provider-pre-mutation",
                    "--recognized-revision", REVISION,
                ], capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0, result.stderr)

    def invoke_traffic_from_path(self, source, mode):
        return subprocess.run([
            "python3", str(SCRIPT), "validate-traffic", "--traffic-file", str(source),
            "--traffic-path", "traffic", "--traffic-mode", mode,
            "--recognized-revision", REVISION,
        ], capture_output=True, text=True)

    def invoke_canonical_ci_run(self, runs, **extra):
        path = self.root / "ci-runs.json"
        output = self.root / "ci-run-id.txt"
        if isinstance(runs, str):
            path.write_text(runs)
        else:
            path.write_text(json.dumps(runs))
        command = [
            "python3", str(SCRIPT), "validate-canonical-ci-run",
            "--runs-json", str(path),
            "--expected-sha", extra.pop("expected_sha", SHA),
            "--expected-path", extra.pop("expected_path", ".github/workflows/ci.yml"),
            "--expected-event", extra.pop("expected_event", "push"),
            "--expected-ref", extra.pop("expected_ref", "develop"),
            "--output", str(output),
        ]
        result = subprocess.run(command, capture_output=True, text=True)
        return result, output

    def invoke_canonical_ci_jobs(self, jobs, **extra):
        path = self.root / "ci-jobs.json"
        path.write_text(json.dumps(jobs) if not isinstance(jobs, str) else jobs)
        return subprocess.run([
            "python3", str(SCRIPT), "validate-canonical-ci-jobs",
            "--jobs-json", str(path),
            "--expected-run-id", str(extra.get("expected_run_id", RUN_ID)),
            "--required-job", extra.get("required_job", "test"),
        ], capture_output=True, text=True)

    def invoke_fast_forward_compare(self, document):
        path = self.root / "compare.json"
        if isinstance(document, str):
            path.write_text(document)
        else:
            path.write_text(json.dumps(document))
        return subprocess.run([
            "python3", str(SCRIPT), "validate-fast-forward-compare",
            "--compare-json", str(path),
        ], capture_output=True, text=True)

    def canonical_ci_run(self, **overrides):
        run = {
            "id": RUN_ID,
            "path": ".github/workflows/ci.yml",
            "event": "push",
            "head_branch": "develop",
            "head_sha": SHA,
            "status": "completed",
            "conclusion": "success",
        }
        run.update(overrides)
        return run

    def test_canonical_ci_run_accepts_exact_identity(self):
        result, output = self.invoke_canonical_ci_run([self.canonical_ci_run()])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output.read_text(), f"{RUN_ID}\n")

    def test_canonical_ci_run_rejects_wrong_path_event_ref_sha(self):
        cases = {
            "path": self.canonical_ci_run(path=".github/workflows/deploy-bff.yml"),
            "event": self.canonical_ci_run(event="pull_request"),
            "ref": self.canonical_ci_run(head_branch="main"),
            "sha": self.canonical_ci_run(head_sha="d" * 40),
            "dispatch": self.canonical_ci_run(event="workflow_dispatch"),
        }
        for name, run in cases.items():
            with self.subTest(name=name):
                result, output = self.invoke_canonical_ci_run([run])
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())

    def test_canonical_ci_run_rejects_latest_green_failed_skipped_and_duplicates(self):
        latest_green = self.canonical_ci_run(id=999, head_sha="e" * 40)
        failed = self.canonical_ci_run(conclusion="failure")
        skipped = self.canonical_ci_run(conclusion="skipped")
        incomplete = self.canonical_ci_run(status="in_progress", conclusion=None)
        duplicate = [self.canonical_ci_run(), self.canonical_ci_run(id=RUN_ID + 1)]
        cases = {
            "latest green": [latest_green],
            "failed": [failed],
            "skipped": [skipped],
            "incomplete": [incomplete],
            "duplicate success": duplicate,
            "empty": [],
        }
        for name, runs in cases.items():
            with self.subTest(name=name):
                result, output = self.invoke_canonical_ci_run(runs)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())

    def test_canonical_ci_run_accepts_successful_rerun_after_failed_attempt(self):
        failed = self.canonical_ci_run(id=RUN_ID + 1, conclusion="failure")
        result, output = self.invoke_canonical_ci_run([failed, self.canonical_ci_run()])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output.read_text(), f"{RUN_ID}\n")

    def test_canonical_ci_run_rejects_cli_identity_mismatch(self):
        result, output = self.invoke_canonical_ci_run(
            [self.canonical_ci_run()], expected_event="pull_request")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())

    def test_canonical_ci_jobs_require_unique_successful_test_job(self):
        jobs = [{"name": "test", "run_id": RUN_ID, "status": "completed", "conclusion": "success"}]
        self.assertEqual(self.invoke_canonical_ci_jobs(jobs).returncode, 0)
        cases = {
            "missing": [],
            "skipped": [{**jobs[0], "conclusion": "skipped"}],
            "failed": [{**jobs[0], "conclusion": "failure"}],
            "incomplete": [{**jobs[0], "status": "in_progress", "conclusion": None}],
            "duplicate": jobs + jobs,
            "wrong run": [{**jobs[0], "run_id": RUN_ID + 1}],
            "wrong name": [{**jobs[0], "name": "lint"}],
        }
        for name, value in cases.items():
            with self.subTest(name=name):
                self.assertNotEqual(self.invoke_canonical_ci_jobs(value).returncode, 0)

    def test_fast_forward_compare_accepts_ahead_and_identical(self):
        self.assertEqual(self.invoke_fast_forward_compare({"status": "ahead", "ahead_by": 3, "behind_by": 0}).returncode, 0)
        self.assertEqual(self.invoke_fast_forward_compare({"status": "identical", "ahead_by": 0, "behind_by": 0}).returncode, 0)

    def test_fast_forward_compare_rejects_non_ancestor_and_malformed(self):
        cases = {
            "behind": {"status": "behind", "ahead_by": 0, "behind_by": 2},
            "diverged": {"status": "diverged", "ahead_by": 1, "behind_by": 1},
            "ahead behind": {"status": "ahead", "ahead_by": 1, "behind_by": 1},
            "identical ahead": {"status": "identical", "ahead_by": 2, "behind_by": 0},
            "ahead zero": {"status": "ahead", "ahead_by": 0, "behind_by": 0},
            "bool behind": {"status": "ahead", "ahead_by": 1, "behind_by": False},
            "string behind": {"status": "ahead", "ahead_by": 1, "behind_by": "0"},
            "missing": {"status": "ahead", "ahead_by": 1},
            "array": [],
        }
        for name, document in cases.items():
            with self.subTest(name=name):
                self.assertNotEqual(self.invoke_fast_forward_compare(document).returncode, 0)
        malformed = self.root / "malformed-compare.json"
        malformed.write_text("{")
        self.assertNotEqual(self.invoke_fast_forward_compare("{").returncode, 0)
        duplicate = '{"status":"ahead","ahead_by":1,"behind_by":0,"status":"ahead"}'
        self.assertNotEqual(self.invoke_fast_forward_compare(duplicate).returncode, 0)
        self.assertNotEqual(self.invoke_fast_forward_compare('{"status":"ahead","ahead_by":1,"behind_by":NaN}').returncode, 0)


if __name__ == "__main__":
    unittest.main()
