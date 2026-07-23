#!/usr/bin/env python3
import json
from datetime import datetime, timedelta, timezone
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "render_worker_deployment_evidence.py"
ROLLBACK_FIXTURE = ROOT / "cmd/olw_worker/testdata/lwc179/legacy-prod.json"
OBSERVED_FIXTURE = ROOT / "cmd/olw_worker/testdata/lwc198/observed-prod.json"
MALFORMED_FIXTURE = ROOT / "cmd/olw_worker/testdata/lwc179/malformed-image.json"
IMAGE = "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/olw-pipeline@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
ROLLBACK_ARTIFACT_NAME = "worker-rollback-contract-" + "c" * 40


class WorkerDeploymentEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = Path(self.tempdir.name)
        self.fake_bin = self.root / "bin"
        self.fake_bin.mkdir()
        fake = self.fake_bin / "gcloud"
        fake.write_text(
            textwrap.dedent(
                """
                #!/usr/bin/env python3
                import os
                from pathlib import Path
                import sys

                args = sys.argv[1:]
                if os.environ.get("FAKE_PROVIDER_FAILURE") == "1":
                    print("fake provider failure", file=sys.stderr)
                    raise SystemExit(9)
                if args[:3] == ["run", "jobs", "describe"]:
                    fixtures = os.environ.get("FAKE_JOB_FIXTURES", os.environ["FAKE_JOB_FIXTURE"]).split(",")
                    state = Path(os.environ["FAKE_JOB_STATE"])
                    index = int(state.read_text()) if state.exists() else 0
                    state.write_text(str(index + 1))
                    fixture = fixtures[min(index, len(fixtures) - 1)]
                    print(Path(fixture).read_text(), end="")
                    raise SystemExit(0)
                if args[:4] == ["artifacts", "docker", "images", "describe"]:
                    state = Path(os.environ["FAKE_DIGEST_STATE"])
                    values = os.environ.get("FAKE_DIGESTS", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").split(",")
                    index = int(state.read_text()) if state.exists() else 0
                    state.write_text(str(index + 1))
                    print(values[min(index, len(values) - 1)])
                    raise SystemExit(0)
                print("unexpected fake gcloud command", args, file=sys.stderr)
                raise SystemExit(2)
                """
            ).lstrip()
        )
        fake.chmod(fake.stat().st_mode | stat.S_IXUSR)
        self.env = {
            **os.environ,
            "PATH": f"{self.fake_bin}:{os.environ['PATH']}",
            "FAKE_DIGEST_STATE": str(self.root / "digest-state"),
            "FAKE_JOB_STATE": str(self.root / "job-state"),
            "FAKE_DIGESTS": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa,sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        }

    def tearDown(self):
        self.tempdir.cleanup()

    def metadata(self):
        return {
            "schema_version": 1,
            "project": "llm-wiki-cloud",
            "component": "olw-worker",
            "environment": "production",
            "action": "promote",
            "rollback_artifact_name": ROLLBACK_ARTIFACT_NAME,
            "source": {"commit_sha": "c" * 40, "ref": "refs/heads/main"},
            "dev_provenance": {
                "workflow": "deploy-worker.yml",
                "event": "push",
                "head_branch": "main",
                "head_sha": "c" * 40,
                "conclusion": "success",
                "run_id": 123,
                "run_url": "https://github.com/Rayer/llm-wiki-bff/actions/runs/123",
            },
            "image": {"digest": "sha256:" + "b" * 64, "reference": IMAGE},
            "originating_workflow": {
                "repository": "Rayer/llm-wiki-bff",
                "workflow": "Promote OLW worker to Cloud Run (production)",
                "run_id": 456,
                "run_attempt": 2,
            },
        }

    def prepare_rollback(self, fixture=ROLLBACK_FIXTURE, digests=None, fixtures=None):
        output = self.root / "rollback.json"
        env = {**self.env, "FAKE_JOB_FIXTURE": str(fixture)}
        if fixtures is not None:
            env["FAKE_JOB_FIXTURES"] = ",".join(str(path) for path in fixtures)
        if digests is not None:
            env["FAKE_DIGESTS"] = ",".join(digests)
        result = subprocess.run(
            [
                "python3",
                str(SCRIPT),
                "prepare-rollback",
                "--project",
                "llm-wiki-cloud",
                "--region",
                "asia-east1",
                "--job-name",
                "olw-pipeline",
                "--ar-repo",
                "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images",
                "--artifact-name",
                ROLLBACK_ARTIFACT_NAME,
                "--output",
                str(output),
            ],
            env=env,
            capture_output=True,
            text=True,
        )
        return result, output

    def render(self, metadata, fixture=OBSERVED_FIXTURE):
        metadata_path = self.root / "metadata.json"
        metadata_path.write_text(json.dumps(metadata))
        output = self.root / "deployment-evidence.json"
        failure = self.root / "deployment-evidence-failure.json"
        env = {**self.env, "FAKE_JOB_FIXTURE": str(fixture)}
        result = subprocess.run(
            [
                "python3",
                str(SCRIPT),
                "render-evidence",
                "--project",
                "llm-wiki-cloud",
                "--region",
                "asia-east1",
                "--job-name",
                "olw-pipeline",
                "--ar-repo",
                "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images",
                "--bucket",
                "llm-wiki-data",
                "--expected-runtime-service-account",
                "lwc-worker@llm-wiki-cloud.iam.gserviceaccount.com",
                "--rollback-contract",
                str(self.root / "rollback.json"),
                "--metadata",
                str(metadata_path),
                "--output",
                str(output),
                "--failure-output",
                str(failure),
            ],
            env=env,
            capture_output=True,
            text=True,
        )
        return result, output

    def render_partial(self, metadata=None, output=None):
        metadata = self.metadata() if metadata is None else metadata
        metadata_path = self.root / "metadata.json"
        metadata_path.write_text(json.dumps(metadata))
        output = self.root / "deployment-evidence.json" if output is None else output
        failure = self.root / "deployment-evidence-failure.json"
        env = {**self.env, "FAKE_JOB_FIXTURE": str(OBSERVED_FIXTURE)}
        result = subprocess.run(
            [
                "python3",
                str(SCRIPT),
                "render-partial",
                "--project",
                "llm-wiki-cloud",
                "--region",
                "asia-east1",
                "--job-name",
                "olw-pipeline",
                "--ar-repo",
                "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images",
                "--rollback-contract",
                str(self.root / "rollback.json"),
                "--metadata",
                str(metadata_path),
                "--output",
                str(output),
                "--failure-output",
                str(failure),
            ],
            env=env,
            capture_output=True,
            text=True,
        )
        return result, output

    def test_success_normalizes_evidence_after_provider_readback(self):
        prepared, rollback = self.prepare_rollback()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        self.assertTrue(rollback.exists())

        rendered, evidence = self.render(self.metadata())
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        document = json.loads(evidence.read_text())
        self.assertEqual(document["schema_version"], 1)
        self.assertEqual(document["source"]["commit_sha"], "c" * 40)
        self.assertEqual(document["dev_provenance"]["run_id"], 123)
        self.assertEqual(document["image"], {"digest": "sha256:" + "b" * 64, "reference": IMAGE})
        self.assertEqual(document["observed_job"]["generation"], 42)
        self.assertEqual(document["observed_job"]["image_reference"], IMAGE)
        self.assertEqual(document["observed_job"]["runtime_service_account"], "lwc-worker@llm-wiki-cloud.iam.gserviceaccount.com")
        self.assertEqual(document["provider"]["rollback_artifact_name"], ROLLBACK_ARTIFACT_NAME)
        self.assertEqual(document["config"]["result"], "verified")
        self.assertRegex(document["config"]["fingerprint"], r"^sha256:[0-9a-f]{64}$")
        self.assertEqual(document["provider_verification"]["result"], "verified")
        checked_at = document["provider_verification"]["checked_at"]
        checked = datetime.strptime(checked_at, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
        self.assertLessEqual(datetime.now(timezone.utc) - checked, timedelta(seconds=5))
        self.assertEqual(document["originating_workflow"]["run_attempt"], 2)
        self.assertNotIn("DEEPSEEK", evidence.read_text())

    def test_fail_closed_metadata_and_readback_paths_emit_no_evidence(self):
        cases = {
            "invalid source SHA": lambda m: m["source"].update(commit_sha="bad"),
            "missing source SHA": lambda m: m["source"].pop("commit_sha"),
            "invalid digest": lambda m: m["image"].update(digest="sha256:bad"),
            "missing digest": lambda m: m["image"].pop("digest"),
            "missing provenance run ID": lambda m: m["dev_provenance"].pop("run_id"),
            "invalid provenance run ID": lambda m: m["dev_provenance"].update(run_id=0),
            "unsupported component": lambda m: m.update(component="other-worker"),
            "unsupported environment": lambda m: m.update(environment="staging"),
            "unsupported action": lambda m: m.update(action="rollback"),
            "caller-supplied verification timestamp": lambda m: m.update(
                provider_verification={"result": "verified", "checked_at": "1970-01-01T00:00:00Z"}
            ),
            "secret-like field": lambda m: m.update(credentials={"t" + "oken": "redacted"}),
        }
        for name, mutate in cases.items():
            with self.subTest(name=name):
                prepared, _ = self.prepare_rollback()
                self.assertEqual(prepared.returncode, 0, prepared.stderr)
                metadata = self.metadata()
                mutate(metadata)
                rendered, evidence = self.render(metadata)
                self.assertNotEqual(rendered.returncode, 0, name)
                self.assertFalse(evidence.exists(), name)

        for name, mutate_fixture in {
            "missing generation": lambda d: d["metadata"].pop("generation"),
            "invalid generation": lambda d: d["metadata"].update(generation=0),
            "image mismatch": lambda d: d["spec"]["template"]["spec"]["template"]["spec"]["containers"][0].update(image=IMAGE.replace("b" * 64, "d" * 64)),
        }.items():
            with self.subTest(name=name):
                prepared, _ = self.prepare_rollback()
                self.assertEqual(prepared.returncode, 0, prepared.stderr)
                observed = json.loads(OBSERVED_FIXTURE.read_text())
                mutate_fixture(observed)
                observed_path = self.root / f"{name}.json"
                observed_path.write_text(json.dumps(observed))
                rendered, evidence = self.render(self.metadata(), observed_path)
                self.assertNotEqual(rendered.returncode, 0, name)
                self.assertFalse(evidence.exists(), name)

    def test_fail_closed_rollback_resolution_and_provider_failures_emit_no_rollback(self):
        result, output = self.prepare_rollback(digests=["sha256:" + "a" * 64, "sha256:" + "c" * 64])
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())

        result, output = self.prepare_rollback(fixture=MALFORMED_FIXTURE)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())

        self.env["FAKE_PROVIDER_FAILURE"] = "1"
        result, output = self.prepare_rollback()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())

    def test_partial_fallback_preserves_frozen_rollback_and_unknown_semantics(self):
        prepared, rollback = self.prepare_rollback()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        frozen = json.loads(rollback.read_text())

        rendered, evidence = self.render(self.metadata(), fixture=MALFORMED_FIXTURE)
        self.assertNotEqual(rendered.returncode, 0)
        self.assertFalse(evidence.exists())

        partial, evidence = self.render_partial()
        self.assertEqual(partial.returncode, 0, partial.stderr)
        document = json.loads(evidence.read_text())
        self.assertEqual(document["status"], "UNHEALTHY")
        self.assertEqual(document["provider_verification"]["result"], "failed")
        self.assertEqual(document["provider_verification"]["reason_code"], "observed_shape_unsupported")
        self.assertIsNotNone(document["provider_verification"]["checked_at"])
        self.assertEqual(document["config"], {
            "result": "failed",
            "fingerprint": None,
            "allowlisted": None,
        })
        self.assertEqual(document["rollback"], {
            "image_reference": frozen["image_reference"],
            "config": frozen["config"],
        })
        self.assertNotIn("observed_job", document)
        self.assertIn("independent provider read-back", document["next_action"])
        self.assertIn("retry", document["next_action"])
        self.assertIn("rollback", document["next_action"])

    def test_provider_unavailable_falls_back_to_unknown_without_provider_data(self):
        prepared, _ = self.prepare_rollback()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        self.env["FAKE_PROVIDER_FAILURE"] = "1"

        rendered, evidence = self.render(self.metadata())
        self.assertNotEqual(rendered.returncode, 0)
        self.assertFalse(evidence.exists())
        self.assertNotIn("fake provider failure", rendered.stderr)
        marker = json.loads((self.root / "deployment-evidence-failure.json").read_text())
        self.assertEqual(marker["classification"], "unknown")
        self.assertEqual(marker["reason_code"], "provider_command_failed")
        self.assertNotIn("fake provider failure", (self.root / "deployment-evidence-failure.json").read_text())

        partial, evidence = self.render_partial()
        self.assertEqual(partial.returncode, 0, partial.stderr)
        document = json.loads(evidence.read_text())
        self.assertEqual(document["status"], "PARTIAL")
        self.assertEqual(document["provider_verification"], {
            "result": "unknown",
            "checked_at": None,
            "checks": [],
        })

    def test_image_mismatch_falls_back_to_unhealthy_with_safe_reason(self):
        prepared, _ = self.prepare_rollback()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        observed = json.loads(OBSERVED_FIXTURE.read_text())
        observed["spec"]["template"]["spec"]["template"]["spec"]["containers"][0]["image"] = IMAGE.replace("b" * 64, "d" * 64)
        observed_path = self.root / "image-mismatch.json"
        observed_path.write_text(json.dumps(observed))

        rendered, evidence = self.render(self.metadata(), observed_path)
        self.assertNotEqual(rendered.returncode, 0)
        self.assertFalse(evidence.exists())
        partial, evidence = self.render_partial()
        self.assertEqual(partial.returncode, 0, partial.stderr)
        document = json.loads(evidence.read_text())
        self.assertEqual(document["status"], "UNHEALTHY")
        self.assertEqual(document["config"]["result"], "failed")
        self.assertEqual(document["provider_verification"]["reason_code"], "image_mismatch")
        self.assertIsNotNone(document["provider_verification"]["checked_at"])

    def test_runtime_service_account_mismatch_falls_back_to_unhealthy_with_safe_reason(self):
        prepared, _ = self.prepare_rollback()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        observed = json.loads(OBSERVED_FIXTURE.read_text())
        observed["spec"]["template"]["spec"]["template"]["spec"]["serviceAccountName"] = "unexpected@llm-wiki-cloud.iam.gserviceaccount.com"
        observed_path = self.root / "runtime-service-account-mismatch.json"
        observed_path.write_text(json.dumps(observed))

        rendered, evidence = self.render(self.metadata(), observed_path)
        self.assertNotEqual(rendered.returncode, 0)
        self.assertFalse(evidence.exists())
        marker = json.loads((self.root / "deployment-evidence-failure.json").read_text())
        self.assertEqual(marker["reason_code"], "runtime_service_account_mismatch")
        self.assertEqual(marker["classification"], "failed")
        partial, evidence = self.render_partial()
        self.assertEqual(partial.returncode, 0, partial.stderr)
        document = json.loads(evidence.read_text())
        self.assertEqual(document["status"], "UNHEALTHY")
        self.assertEqual(document["config"]["result"], "failed")
        self.assertEqual(document["provider_verification"]["reason_code"], "runtime_service_account_mismatch")
        self.assertIsNotNone(document["provider_verification"]["checked_at"])

    def test_no_failure_marker_keeps_update_failure_unknown(self):
        prepared, _ = self.prepare_rollback()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        partial, evidence = self.render_partial()
        self.assertEqual(partial.returncode, 0, partial.stderr)
        document = json.loads(evidence.read_text())
        self.assertEqual(document["status"], "PARTIAL")
        self.assertEqual(document["provider_verification"]["result"], "unknown")
        self.assertIsNone(document["provider_verification"]["checked_at"])

    def test_partial_fallback_never_overwrites_existing_success_evidence(self):
        prepared, _ = self.prepare_rollback()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        rendered, evidence = self.render(self.metadata())
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        before = evidence.read_text()

        strict_again, _ = self.render(self.metadata())
        self.assertNotEqual(strict_again.returncode, 0)
        self.assertEqual(evidence.read_text(), before)
        partial, _ = self.render_partial(output=evidence)
        self.assertNotEqual(partial.returncode, 0)
        self.assertEqual(evidence.read_text(), before)

    def test_mutable_rollback_uses_second_validated_job_snapshot(self):
        first = ROOT / "cmd/olw_worker/testdata/lwc198/mutable-rollback-first.json"
        second = ROOT / "cmd/olw_worker/testdata/lwc198/mutable-rollback-second.json"
        result, output = self.prepare_rollback(fixtures=[first, second])
        self.assertEqual(result.returncode, 0, result.stderr)
        rollback = json.loads(output.read_text())
        self.assertEqual(rollback["config"]["env"], [
            {"name": "BUCKET", "value": "llm-wiki-data-second"},
            {"name": "DATA_DIR", "value": "/second"},
        ])
        self.assertEqual(rollback["config"]["volumes"], [
            {
                "name": "second",
                "csi": {
                    "driver": "gcsfuse.run.googleapis.com",
                    "volumeAttributes": {
                        "bucketName": "rollback-bucket",
                        "gcsfuseVersion": "v2",
                    },
                },
            },
        ])
        self.assertEqual(rollback["config"]["volume_mounts"], [
            {"name": "second", "mountPath": "/second"},
        ])

    def test_render_reconstructs_nested_v1_objects_from_allowlisted_fields(self):
        sentinel = "benign-unknown-field-must-not-leak"
        provider = json.loads(ROLLBACK_FIXTURE.read_text())
        provider_container = provider["spec"]["template"]["spec"]["template"]["spec"]["containers"][0]
        provider_container["env"][0]["benign"] = sentinel
        provider_path = self.root / "provider-with-unknown-fields.json"
        provider_path.write_text(json.dumps(provider))

        prepared, rollback = self.prepare_rollback(fixture=provider_path)
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        rollback_document = json.loads(rollback.read_text())
        rollback_document["config"]["benign"] = sentinel
        rollback_document["config"]["env"][0]["benign"] = sentinel
        rollback.write_text(json.dumps(rollback_document))

        metadata = self.metadata()
        metadata["source"]["benign"] = sentinel
        metadata["dev_provenance"]["benign"] = sentinel
        metadata["image"]["benign"] = sentinel
        metadata["originating_workflow"]["benign"] = sentinel

        observed = json.loads(OBSERVED_FIXTURE.read_text())
        observed["spec"]["template"]["spec"]["template"]["spec"]["containers"][0]["env"][0]["benign"] = sentinel
        observed_path = self.root / "observed-with-unknown-fields.json"
        observed_path.write_text(json.dumps(observed))
        rendered, evidence = self.render(metadata, observed_path)

        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        evidence_text = evidence.read_text()
        self.assertNotIn(sentinel, evidence_text)
        document = json.loads(evidence_text)
        clean_metadata = self.metadata()
        self.assertEqual(document["source"], clean_metadata["source"])
        self.assertEqual(document["dev_provenance"], clean_metadata["dev_provenance"])
        self.assertEqual(document["image"], clean_metadata["image"])
        self.assertEqual(document["originating_workflow"], clean_metadata["originating_workflow"])
        self.assertEqual(document["config"]["allowlisted"], {
            "env": [{"name": "BUCKET", "value": "llm-wiki-data"}],
            "bucket": "llm-wiki-data",
            "args": ["run", "[[\"run\",\"--auto-approve\"]]"],
            "volumes": [],
            "volume_mounts": [],
        })
        self.assertEqual(document["rollback"]["config"], {
            "env": [
                {"name": "BUCKET", "value": "llm-wiki-data"},
                {"name": "DATA_DIR", "value": "/data"},
                {"name": "WORKSPACE", "value": "prod"},
                {"name": "VAULT_PATH", "value": "/vault"},
                {"name": "WORKSPACE_DIR", "value": "/workspace"},
            ],
            "bucket": "llm-wiki-data",
            "args": ["run", "[[\"run\",\"--auto-approve\"]]"],
            "volumes": [{
                "name": "gcs",
                "csi": {"driver": "gcsfuse.run.googleapis.com", "volumeAttributes": {}},
            }],
            "volume_mounts": [{"name": "gcs", "mountPath": "/data"}],
        })

    def test_unsupported_volume_and_mount_shapes_fail_closed_before_rollback_write(self):
        for name, mutate in {
            "unsupported volume provider": lambda d: d["spec"]["template"]["spec"]["template"]["spec"]["volumes"][0].update(
                persistentDisk={"diskName": "worker-disk"}
            ),
            "unsupported CSI field": lambda d: d["spec"]["template"]["spec"]["template"]["spec"]["volumes"][0]["csi"].update(
                nodeStageSecretRef={"name": "not-allowed"}
            ),
            "unsupported mount field": lambda d: d["spec"]["template"]["spec"]["template"]["spec"]["containers"][0]["volumeMounts"][0].update(
                readOnly=True
            ),
            "secret-like volume attribute": lambda d: d["spec"]["template"]["spec"]["template"]["spec"]["volumes"][0]["csi"].update(
                volumeAttributes={"accessToken": "redacted"}
            ),
            "keyFile volume attribute": lambda d: d["spec"]["template"]["spec"]["template"]["spec"]["volumes"][0]["csi"].update(
                volumeAttributes={"keyFile": "/var/run/key"}
            ),
            "encryptionKey volume attribute": lambda d: d["spec"]["template"]["spec"]["template"]["spec"]["volumes"][0]["csi"].update(
                volumeAttributes={"encryptionKey": "encrypted"}
            ),
            "accessKey volume attribute": lambda d: d["spec"]["template"]["spec"]["template"]["spec"]["volumes"][0]["csi"].update(
                volumeAttributes={"accessKey": "redacted"}
            ),
            "credential volume attribute": lambda d: d["spec"]["template"]["spec"]["template"]["spec"]["volumes"][0]["csi"].update(
                volumeAttributes={"credential": "redacted"}
            ),
            "secret-like volume attribute value": lambda d: d["spec"]["template"]["spec"]["template"]["spec"]["volumes"][0]["csi"].update(
                volumeAttributes={"mountPath": "/var/lib/secret/data"}
            ),
        }.items():
            with self.subTest(name=name):
                provider = json.loads(ROLLBACK_FIXTURE.read_text())
                mutate(provider)
                fixture = self.root / (name.replace(" ", "-") + ".json")
                fixture.write_text(json.dumps(provider))
                result, output = self.prepare_rollback(fixture=fixture)
                self.assertNotEqual(result.returncode, 0, result.stderr)
                self.assertFalse(output.exists())

    def test_valid_volume_attributes_are_preserved(self):
        provider = json.loads(ROLLBACK_FIXTURE.read_text())
        provider["spec"]["template"]["spec"]["template"]["spec"]["volumes"][0]["csi"]["volumeAttributes"] = {
            "bucketName": "rollback-bucket",
            "mountOptions": "implicit-dirs,uid=1000",
        }
        fixture = self.root / "valid-volume-attributes.json"
        fixture.write_text(json.dumps(provider))

        result, output = self.prepare_rollback(fixture=fixture)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            json.loads(output.read_text())["config"]["volumes"][0]["csi"]["volumeAttributes"],
            provider["spec"]["template"]["spec"]["template"]["spec"]["volumes"][0]["csi"]["volumeAttributes"],
        )


if __name__ == "__main__":
    unittest.main()
