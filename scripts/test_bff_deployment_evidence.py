#!/usr/bin/env python3
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "render_bff_deployment_evidence.py"
DIGEST = "sha256:" + "b" * 64
PRIOR_DIGEST = "sha256:" + "a" * 64
AR_REPO = "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images"
IMAGE = f"{AR_REPO}/llm-wiki-bff@{DIGEST}"
PRIOR_IMAGE = f"{AR_REPO}/llm-wiki-bff@{PRIOR_DIGEST}"
SA = "lwc-bff-prod@llm-wiki-cloud.iam.gserviceaccount.com"
JOB = "olw-pipeline"
SERVICE = "llm-wiki-bff"
ARTIFACT = "bff-rollback-contract-" + "c" * 40
EVIDENCE_ARTIFACT = "bff-deployment-evidence-" + "c" * 40
QUERY_STAGE_CONFIG_PATH = "/app/configs/query/dev/query-dev-2026-08-31.1.json"
PRIOR_QUERY_STAGE_CONFIG_PATH = "/app/configs/query/dev/query-dev-2026-08-21.1.json"
PRIOR_QUERY_STAGE_CONFIG_PATH_2 = "/app/configs/query/dev/query-dev-2026-08-21.2.json"
PRIOR_QUERY_STAGE_CONFIG_PATH_3 = "/app/configs/query/dev/query-dev-2026-08-22.1.json"
LEGACY_QUERY_ENV_NAMES = [
    "QUERY_EXPANSION_MODEL", "QUERY_EXPANSION_REASONING", "ANSWER_SYNTHESIS_MODEL",
    "ANSWER_SYNTHESIS_REASONING", "QUERY_SELECTION_LIMIT", "QUERY_SELECTION_EXPLORATION_SLOTS",
    "QUERY_SELECTION_EVIDENCE_THRESHOLD", "QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT",
    "QUERY_EXPANSION_ATTEMPTS", "QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY",
]


FIXTURES = ROOT / "scripts" / "fixtures"


def fixture(name):
    return json.loads((FIXTURES / name).read_text())


class BFFDeploymentEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = Path(self.tempdir.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        gcloud = self.bin / "gcloud"
        gcloud.write_text(textwrap.dedent(r'''
            #!/usr/bin/env python3
            import json, os, sys
            from pathlib import Path
            args = sys.argv[1:]
            log = Path(os.environ["FAKE_LOG"])
            log.write_text(log.read_text() + "gcloud " + " ".join(args) + "\n" if log.exists() else "gcloud " + " ".join(args) + "\n")
            if os.environ.get("FAKE_PROVIDER_FAILURE") == "1":
                raise SystemExit(9)
            if args[:3] == ["run", "services", "describe"]:
                paths = os.environ["FAKE_SERVICE_FIXTURES"].split(",")
                state = Path(os.environ["FAKE_SERVICE_STATE"])
                i = int(state.read_text()) if state.exists() else 0
                state.write_text(str(i + 1))
                print(Path(paths[min(i, len(paths) - 1)]).read_text(), end="")
            elif args[:3] == ["run", "revisions", "describe"]:
                print(Path(os.environ["FAKE_REVISION_FIXTURE"]).read_text(), end="")
            elif args[:3] == ["run", "services", "get-iam-policy"]:
                print(Path(os.environ["FAKE_SERVICE_IAM"]).read_text(), end="")
            elif args[:3] == ["run", "jobs", "get-iam-policy"]:
                print(Path(os.environ["FAKE_JOB_IAM"]).read_text(), end="")
            else:
                raise SystemExit(2)
        ''').lstrip())
        gcloud.chmod(gcloud.stat().st_mode | stat.S_IXUSR)
        curl = self.bin / "curl"
        curl.write_text(textwrap.dedent(r'''
            #!/usr/bin/env python3
            import json, os, sys
            from pathlib import Path
            args = sys.argv[1:]
            Path(os.environ["FAKE_LOG"]).write_text(Path(os.environ["FAKE_LOG"]).read_text() + "curl " + " ".join(args) + "\n")
            if os.environ.get("FAKE_CURL_FAILURE") == "1":
                raise SystemExit(7)
            headers = args[args.index("-D") + 1]
            body = args[args.index("-o") + 1]
            Path(headers).write_text(os.environ.get("FAKE_HEADERS", "HTTP/2 200\nCache-Control: no-store\nContent-Type: application/json\n"))
            Path(body).write_text(os.environ["FAKE_VERSION_JSON"])
            if os.environ.get("FAKE_CURL_HTTP_ERROR") == "1" and "--fail-with-body" in args:
                raise SystemExit(22)
        ''').lstrip())
        curl.chmod(curl.stat().st_mode | stat.S_IXUSR)
        self.before = self.root / "before.json"
        self.after = self.root / "after.json"
        self.prior_revision_path = self.root / "prior-revision.json"
        self.revision_path = self.root / "revision.json"
        self.service_iam = self.root / "service-iam.json"
        self.job_iam = self.root / "job-iam.json"
        self.before.write_text(json.dumps(fixture("bff-service-before.json")))
        self.after.write_text(json.dumps(fixture("bff-service-after.json")))
        self.prior_revision_path.write_text(json.dumps(fixture("bff-revision-before.json")))
        self.revision_path.write_text(json.dumps(fixture("bff-revision-after.json")))
        self.service_iam.write_text(json.dumps({"bindings": [{"role": "roles/run.invoker", "members": ["allUsers"]}]}))
        self.job_iam.write_text(json.dumps({"bindings": [{"role": "roles/run.jobsExecutorWithOverrides", "members": [f"serviceAccount:{SA}"]}]}))
        self.env = {**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}", "FAKE_LOG": str(self.root / "provider.log"), "FAKE_SERVICE_STATE": str(self.root / "service-state"), "FAKE_SERVICE_FIXTURES": f"{self.before},{self.before}", "FAKE_REVISION_FIXTURE": str(self.prior_revision_path), "FAKE_SERVICE_IAM": str(self.service_iam), "FAKE_JOB_IAM": str(self.job_iam), "FAKE_VERSION_JSON": json.dumps({"product_version": "1.2.3", "commit": "c" * 40, "branch": "develop", "tag": "", "image_tag": "c" * 40, "service": SERVICE, "revision": "llm-wiki-bff-00002-new"})}

    def tearDown(self):
        self.tempdir.cleanup()

    def invoke(self, mode, *extra):
        return subprocess.run(["python3", str(SCRIPT), mode, "--project", "llm-wiki-cloud", "--region", "asia-east1", "--service-name", SERVICE, "--pipeline-job-name", JOB, "--ar-repo", AR_REPO, *extra], env=self.env, capture_output=True, text=True)

    def metadata(self):
        return {"schema_version": 1, "project": "llm-wiki-cloud", "component": "lwc-bff", "environment": "production", "action": "promote", "rollback_artifact_name": ARTIFACT, "build_ref": "develop", "production_source_ref": "main", "source": {"commit_sha": "c" * 40, "ref": "refs/heads/main"}, "dev_provenance": {"workflow": "deploy-bff.yml", "event": "workflow_dispatch", "head_branch": "develop", "head_sha": "c" * 40, "conclusion": "success", "run_id": 123, "run_url": "https://github.com/Rayer/llm-wiki-bff/actions/runs/123"}, "image": {"digest": DIGEST, "reference": IMAGE}, "originating_workflow": {"repository": "Rayer/llm-wiki-bff", "workflow": "Promote BFF to Cloud Run (production)", "run_id": 456, "run_attempt": 2}}

    def prepare(self):
        output = self.root / "rollback.json"
        self.custom_revision = False
        for path in (self.root / "deployment-evidence.json", self.root / "failure.json"):
            path.unlink(missing_ok=True)
        self.after.write_text(json.dumps(fixture("bff-service-after.json")))
        self.revision_path.write_text(self.prior_revision_path.read_text())
        self.env["FAKE_SERVICE_FIXTURES"] = f"{self.before},{self.before}"
        self.env["FAKE_REVISION_FIXTURE"] = str(self.prior_revision_path)
        result = self.invoke("prepare-rollback", "--artifact-name", ARTIFACT, "--output", str(output))
        return result, output

    def render(self, metadata=None, service_fixtures=None, expected_revision=None):
        metadata = self.metadata() if metadata is None else metadata
        metadata_path = self.root / "metadata.json"
        metadata_path.write_text(json.dumps(metadata))
        output = self.root / "deployment-evidence.json"
        failure = self.root / "failure.json"
        self.env["FAKE_SERVICE_STATE"] = str(self.root / "post-state")
        self.env["FAKE_SERVICE_FIXTURES"] = service_fixtures or f"{self.after},{self.after}"
        self.env["FAKE_REVISION_FIXTURE"] = str(self.revision_path)
        if not getattr(self, "custom_revision", False):
            self.revision_path.write_text(json.dumps(fixture("bff-revision-after.json")))
        extra = ("--expected-revision", expected_revision) if expected_revision is not None else ()
        result = self.invoke("render-evidence", "--expected-runtime-service-account", SA, *extra, "--rollback-contract", str(self.root / "rollback.json"), "--metadata", str(metadata_path), "--output", str(output), "--failure-output", str(failure))
        return result, output, failure

    def render_documents(self, observed_service, observed_revision=None):
        self.after.write_text(json.dumps(observed_service))
        if observed_revision is not None:
            self.revision_path.write_text(json.dumps(observed_revision))
            self.custom_revision = True
        return self.render()

    def test_success_is_deterministic_and_has_no_secret_values(self):
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        self.assertEqual(json.loads(rollback.read_text())["ready_revision"], "llm-wiki-bff-00001-old")
        rendered, output, _ = self.render()
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        document = json.loads(output.read_text())
        self.assertEqual(document["status"], "HEALTHY")
        self.assertEqual(document["provider"]["rollback_artifact_name"], ARTIFACT)
        self.assertNotEqual(document["provider"]["rollback_artifact_name"], EVIDENCE_ARTIFACT)
        self.assertEqual(document["source"]["commit_sha"], "c" * 40)
        self.assertEqual(document["build_ref"], "develop")
        self.assertEqual(document["production_source_ref"], "main")
        self.assertEqual(document["image"], {"digest": DIGEST, "reference": IMAGE})
        self.assertEqual(document["observed_service"]["ready_revision"], "llm-wiki-bff-00002-new")
        self.assertEqual(document["observed_service"]["image_reference"], IMAGE)
        self.assertEqual(document["observed_service"]["image_digest"], DIGEST)
        self.assertEqual(document["observed_service"]["traffic"], [{"revision_name": "llm-wiki-bff-00002-new", "percent": 100, "latest_revision": True}])
        self.assertEqual(document["config"]["allowlisted"]["env"][-1], {"name": "QUERY_STAGE_CONFIG_PATH", "value": QUERY_STAGE_CONFIG_PATH})
        self.assertEqual(document["config"]["allowlisted"]["legacy_preserved"], [{"name": "PROJECT_ID", "value": "demo"}, {"name": "USER_ID", "value": "test-user"}])
        self.assertEqual(document["health"]["identity"]["commit"], "c" * 40)
        self.assertNotIn("secret-value", output.read_text())
        self.assertIn('"secret_references"', output.read_text())
        again = self.root / "again.json"
        output.replace(again)
        self.env["FAKE_SERVICE_STATE"] = str(self.root / "post-state-again")
        result = self.invoke("render-evidence", "--expected-runtime-service-account", SA, "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(self.root / "failure-again.json"))
        self.assertEqual(result.returncode, 0, result.stderr)
        second = json.loads(output.read_text())
        first_document = json.loads(again.read_text())
        first_document["health"]["checked_at"] = second["health"]["checked_at"]
        first_document["provider_verification"]["checked_at"] = second["provider_verification"]["checked_at"]
        self.assertEqual(output.read_text(), json.dumps(first_document, sort_keys=True, separators=(",", ":")) + "\n")

    def test_strict_evidence_uses_explicit_serving_revision_when_newer_ready_revision_has_no_traffic(self):
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        serving = fixture("bff-service-after.json")
        serving["status"]["latestReadyRevisionName"] = "llm-wiki-bff-00003-ready-no-traffic"
        serving["status"]["traffic"] = [{"revisionName": "llm-wiki-bff-00002-new", "percent": 100, "latestRevision": False}]
        first = self.root / "serving-first.json"
        second = self.root / "serving-second.json"
        first.write_text(json.dumps(serving))
        serving_second = json.loads(json.dumps(serving))
        serving_second["status"]["latestReadyRevisionName"] = "llm-wiki-bff-00004-ready-no-traffic"
        second.write_text(json.dumps(serving_second))

        rendered, output, _ = self.render(service_fixtures=f"{first},{second}")

        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        document = json.loads(output.read_text())
        self.assertEqual(document["observed_service"]["ready_revision"], "llm-wiki-bff-00002-new")
        self.assertEqual(document["observed_service"]["traffic"], [{"revision_name": "llm-wiki-bff-00002-new", "percent": 100, "latest_revision": False}])

    def test_strict_evidence_rejects_traffic_that_is_not_the_persisted_candidate(self):
        prepared, _ = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        rendered, output, failure = self.render(expected_revision="llm-wiki-bff-00003-other")
        self.assertNotEqual(rendered.returncode, 0)
        self.assertFalse(output.exists())
        self.assertEqual(json.loads(failure.read_text())["reason_code"], "traffic_mismatch")

    def test_strict_evidence_uses_serving_revision_config_when_ready_template_is_untrafficked(self):
        prepared, _ = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        first = fixture("bff-service-after.json")
        first["status"]["latestReadyRevisionName"] = "llm-wiki-bff-00003-ready-no-traffic"
        first["spec"]["template"]["spec"]["containers"][0]["image"] = PRIOR_IMAGE
        next(entry for entry in first["spec"]["template"]["spec"]["containers"][0]["env"] if entry["name"] == "QUERY_STAGE_CONFIG_PATH")["value"] = PRIOR_QUERY_STAGE_CONFIG_PATH
        first["spec"]["template"]["metadata"]["annotations"]["run.googleapis.com/vpc-access-egress"] = "all-traffic"
        second = json.loads(json.dumps(first))
        first_path = self.root / "template-first.json"
        second_path = self.root / "template-second.json"
        first_path.write_text(json.dumps(first))
        second_path.write_text(json.dumps(second))

        rendered, output, _ = self.render(service_fixtures=f"{first_path},{second_path}")

        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        document = json.loads(output.read_text())
        self.assertEqual(document["observed_service"]["image_reference"], IMAGE)
        self.assertEqual(
            next(entry for entry in document["config"]["allowlisted"]["env"] if entry["name"] == "QUERY_STAGE_CONFIG_PATH")["value"],
            QUERY_STAGE_CONFIG_PATH,
        )
        self.assertEqual(document["config"]["allowlisted"]["network"]["vpc_egress"], "private-ranges-only")

    def test_strict_evidence_rejects_service_ingress_change_between_reads(self):
        prepared, _ = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        first = fixture("bff-service-after.json")
        second = json.loads(json.dumps(first))
        second["metadata"]["annotations"]["run.googleapis.com/ingress"] = "internal"
        first_path = self.root / "ingress-first.json"
        second_path = self.root / "ingress-second.json"
        first_path.write_text(json.dumps(first))
        second_path.write_text(json.dumps(second))

        rendered, output, failure = self.render(service_fixtures=f"{first_path},{second_path}")

        self.assertNotEqual(rendered.returncode, 0)
        self.assertFalse(output.exists())
        self.assertEqual(json.loads(failure.read_text())["reason_code"], "config_mismatch")

    def test_iam_bindings_require_unconditional_role_members(self):
        conditional = {"title": "only during maintenance", "expression": "true"}
        cases = (
            (self.service_iam, "roles/run.invoker", "allUsers"),
            (self.job_iam, "roles/run.jobsExecutorWithOverrides", f"serviceAccount:{SA}"),
        )
        for policy_path, role, member in cases:
            with self.subTest(role=role):
                policy_path.write_text(json.dumps({"bindings": [{"role": role, "members": [member], "condition": conditional}]}))
                prepared, _ = self.prepare()
                self.assertEqual(prepared.returncode, 0, prepared.stderr)
                result, output, failure = self.render()
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())
                self.assertEqual(json.loads(failure.read_text())["reason_code"], "iam_binding_missing")
                policy_path.write_text(json.dumps({"bindings": [{"role": role, "members": [member]}]}))

        prepared, _ = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        result, output, _ = self.render()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(output.exists())

    def test_prepare_freezes_effective_serving_revision_when_ready_revision_differs(self):
        first = fixture("bff-service-before.json")
        first["status"]["latestCreatedRevisionName"] = "llm-wiki-bff-00071-zgk"
        first["status"]["latestReadyRevisionName"] = "llm-wiki-bff-00071-zgk"
        first["status"]["traffic"] = [{"revisionName": "llm-wiki-bff-00070-f5c", "percent": 100}]
        second = json.loads(json.dumps(first))
        first_path = self.root / "rollback-first.json"
        second_path = self.root / "rollback-second.json"
        first_path.write_text(json.dumps(first))
        second_path.write_text(json.dumps(second))
        revision = fixture("bff-revision-before.json")
        revision["metadata"]["name"] = "llm-wiki-bff-00070-f5c"
        revision_path = self.root / "serving-revision.json"
        revision_path.write_text(json.dumps(revision))
        self.env["FAKE_SERVICE_FIXTURES"] = f"{first_path},{second_path}"
        self.env["FAKE_REVISION_FIXTURE"] = str(revision_path)
        output = self.root / "rollback-serving.json"

        result = self.invoke("prepare-rollback", "--artifact-name", ARTIFACT, "--output", str(output))

        self.assertEqual(result.returncode, 0, result.stderr)
        rollback = json.loads(output.read_text())
        self.assertEqual(rollback["ready_revision"], "llm-wiki-bff-00070-f5c")
        self.assertEqual(rollback["prior_revision_handle"], "projects/llm-wiki-cloud/locations/asia-east1/revisions/llm-wiki-bff-00070-f5c")
        self.assertEqual(rollback["traffic"], [{"revision_name": "llm-wiki-bff-00070-f5c", "percent": 100}])

    def test_public_identity_retains_metadata_build_ref_during_production_promotion(self):
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        identity = json.loads(self.env["FAKE_VERSION_JSON"])
        identity["branch"] = "develop"
        self.env["FAKE_VERSION_JSON"] = json.dumps(identity)

        rendered, output, _ = self.render()

        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        document = json.loads(output.read_text())
        self.assertEqual(document["build_ref"], "develop")
        self.assertEqual(document["production_source_ref"], "main")
        self.assertEqual(document["source"]["ref"], "refs/heads/main")
        self.assertEqual(document["health"]["identity"]["branch"], "develop")

    def test_public_identity_main_is_rejected_for_develop_build(self):
        prepared, _ = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        identity = json.loads(self.env["FAKE_VERSION_JSON"])
        identity["branch"] = "main"
        self.env["FAKE_VERSION_JSON"] = json.dumps(identity)

        rendered, output, failure = self.render()

        self.assertNotEqual(rendered.returncode, 0)
        self.assertFalse(output.exists())
        self.assertEqual(json.loads(failure.read_text())["reason_code"], "identity_body_mismatch")

    def test_old_v2_secret_shape_and_bare_revision_digest_are_rejected(self):
        before = fixture("bff-revision-before.json")
        before["spec"]["containers"][0]["env"][6] = {"name": "JWT_SECRET", "valueSource": {"secretKeyRef": {"secret": "jwt-secret-prod", "version": "latest"}}}
        before["status"]["imageDigest"] = PRIOR_DIGEST
        self.prior_revision_path.write_text(json.dumps(before))
        result = self.invoke("prepare-rollback", "--artifact-name", ARTIFACT, "--output", str(self.root / "rollback.json"))
        self.assertNotEqual(result.returncode, 0)

    def test_rollback_uses_prior_revision_effective_network_and_legacy_values(self):
        service_before = fixture("bff-service-before.json")
        service_before["spec"]["template"]["metadata"]["annotations"]["run.googleapis.com/vpc-access-egress"] = "all-traffic"
        self.before.write_text(json.dumps(service_before))
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        document = json.loads(rollback.read_text())
        self.assertEqual(document["prior_revision_handle"], "projects/llm-wiki-cloud/locations/asia-east1/revisions/llm-wiki-bff-00001-old")
        self.assertEqual(document["image_reference"], PRIOR_IMAGE)
        self.assertEqual(document["image_digest"], PRIOR_DIGEST)
        self.assertEqual(document["config"]["network"]["vpc_egress"], "private-ranges-only")
        self.assertEqual(document["config"]["legacy_preserved"], [{"name": "PROJECT_ID", "value": "demo"}, {"name": "USER_ID", "value": "test-user"}])

    def test_current_production_sealed_config_upgrade_is_accepted(self):
        service_before = fixture("bff-service-before.json")
        service_before["spec"]["template"]["spec"]["containers"][0]["env"].append({"name": "QUERY_STAGE_CONFIG_PATH", "value": PRIOR_QUERY_STAGE_CONFIG_PATH})
        self.before.write_text(json.dumps(service_before))
        prior_revision = fixture("bff-revision-before.json")
        prior_revision["spec"]["containers"][0]["env"].append({"name": "QUERY_STAGE_CONFIG_PATH", "value": PRIOR_QUERY_STAGE_CONFIG_PATH})
        self.prior_revision_path.write_text(json.dumps(prior_revision))

        result, rollback = self.prepare()

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            next(entry for entry in json.loads(rollback.read_text())["config"]["env"] if entry["name"] == "QUERY_STAGE_CONFIG_PATH")["value"],
            PRIOR_QUERY_STAGE_CONFIG_PATH,
        )

    def test_production_prior_2026_08_21_2_revision_builds_rollback_contract(self):
        self.prior_revision_path.write_text((FIXTURES / "bff-revision-before-2026-08-21.2.json").read_text())
        result, rollback = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        saved = json.loads(rollback.read_text())
        self.assertEqual(saved["config"]["env"][-1], {"name": "QUERY_STAGE_CONFIG_PATH", "value": PRIOR_QUERY_STAGE_CONFIG_PATH_2})

    def test_production_prior_2026_08_22_1_revision_builds_rollback_contract(self):
        prior_revision = fixture("bff-revision-before.json")
        prior_revision["spec"]["containers"][0]["env"].append({"name": "QUERY_STAGE_CONFIG_PATH", "value": PRIOR_QUERY_STAGE_CONFIG_PATH_3})
        self.prior_revision_path.write_text(json.dumps(prior_revision))
        result, rollback = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        saved = json.loads(rollback.read_text())
        self.assertEqual(saved["config"]["env"][-1], {"name": "QUERY_STAGE_CONFIG_PATH", "value": PRIOR_QUERY_STAGE_CONFIG_PATH_3})

    def test_prior_sealed_config_allowlist_and_saved_rollback_are_closed(self):
        for value in (PRIOR_QUERY_STAGE_CONFIG_PATH, PRIOR_QUERY_STAGE_CONFIG_PATH_2, PRIOR_QUERY_STAGE_CONFIG_PATH_3, "/app/configs/query/dev/query-dev-2026-08-21.3.json"):
            with self.subTest(value=value):
                prior_revision = fixture("bff-revision-before.json")
                prior_revision["spec"]["containers"][0]["env"].append({"name": "QUERY_STAGE_CONFIG_PATH", "value": value})
                self.prior_revision_path.write_text(json.dumps(prior_revision))
                result, _ = self.prepare()
                self.assertEqual(result.returncode, 0 if value in (PRIOR_QUERY_STAGE_CONFIG_PATH, PRIOR_QUERY_STAGE_CONFIG_PATH_2, PRIOR_QUERY_STAGE_CONFIG_PATH_3) else 1, result.stderr)
                if value not in (PRIOR_QUERY_STAGE_CONFIG_PATH, PRIOR_QUERY_STAGE_CONFIG_PATH_2, PRIOR_QUERY_STAGE_CONFIG_PATH_3):
                    self.assertIn("config_mismatch", result.stderr)

        prior_revision = fixture("bff-revision-before.json")
        prior_revision["spec"]["containers"][0]["env"].append({"name": "QUERY_STAGE_CONFIG_PATH", "value": PRIOR_QUERY_STAGE_CONFIG_PATH})
        prior_revision["spec"]["containers"][0]["env"].append({"name": "QUERY_STAGE_CONFIG_PATH", "value": PRIOR_QUERY_STAGE_CONFIG_PATH})
        self.prior_revision_path.write_text(json.dumps(prior_revision))
        result, _ = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("config_mismatch", result.stderr)

        prior_revision["spec"]["containers"][0]["env"] = [entry for entry in prior_revision["spec"]["containers"][0]["env"] if entry["name"] != "QUERY_STAGE_CONFIG_PATH"]
        prior_revision["spec"]["containers"][0]["env"].append({"name": "QUERY_STAGE_CONFIG_PATH", "value": PRIOR_QUERY_STAGE_CONFIG_PATH})
        self.prior_revision_path.write_text(json.dumps(prior_revision))
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        saved = json.loads(rollback.read_text())
        next(entry for entry in saved["config"]["env"] if entry["name"] == "QUERY_STAGE_CONFIG_PATH")["value"] = "/app/configs/query/dev/arbitrary.json"
        rollback.write_text(json.dumps(saved))
        (self.root / "metadata.json").write_text(json.dumps(self.metadata()))
        result = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(self.root / "deployment-evidence.json"), "--failure-output", str(self.root / "failure.json"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("config_mismatch", result.stderr)

    def test_service_and_revision_effective_config_must_match(self):
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        observed_revision = fixture("bff-revision-after.json")
        observed_revision["metadata"]["annotations"]["run.googleapis.com/vpc-access-egress"] = "all-traffic"
        result, output, failure = self.render_documents(fixture("bff-service-after.json"), observed_revision)
        self.assertNotEqual(result.returncode, 0)
        marker = json.loads(failure.read_text())
        self.assertEqual(marker["reason_code"], "config_mismatch")
        self.assertFalse(output.exists())

    def test_legacy_values_must_be_preserved_by_new_revision(self):
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        observed_revision = fixture("bff-revision-after.json")
        observed_revision["spec"]["containers"][0]["env"][-1]["value"] = "changed-user"
        result, output, failure = self.render_documents(fixture("bff-service-after.json"), observed_revision)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(json.loads(failure.read_text())["reason_code"], "config_mismatch")
        self.assertFalse(output.exists())

    def test_promoted_serving_revision_requires_exact_query_stage_path(self):
        prepared, _ = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        for value in (None, PRIOR_QUERY_STAGE_CONFIG_PATH, "/app/configs/query/dev/wrong.json", "duplicate"):
            with self.subTest(value=value):
                observed_revision = fixture("bff-revision-after.json")
                target = observed_revision["spec"]["containers"][0]
                path_entry = next(entry for entry in target["env"] if entry["name"] == "QUERY_STAGE_CONFIG_PATH")
                if value is None:
                    target["env"].remove(path_entry)
                elif value == "duplicate":
                    target["env"].append(dict(path_entry))
                else:
                    path_entry["value"] = value
                result, output, failure = self.render_documents(fixture("bff-service-after.json"), observed_revision)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(json.loads(failure.read_text())["reason_code"], "config_mismatch")
                self.assertFalse(output.exists())

    def test_all_legacy_query_envs_are_rejected_from_promoted_evidence(self):
        prepared, _ = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        for name in LEGACY_QUERY_ENV_NAMES:
            with self.subTest(name=name):
                observed_revision = fixture("bff-revision-after.json")
                observed_revision["spec"]["containers"][0]["env"].append({"name": name, "value": "legacy"})
                result, output, failure = self.render_documents(fixture("bff-service-after.json"), observed_revision)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(json.loads(failure.read_text())["reason_code"], "config_mismatch")
                self.assertFalse(output.exists())

    def test_invalid_metadata_and_provider_contracts_fail_closed(self):
        prepared, _ = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        cases = {
            "malformed SHA": lambda m: m["source"].update(commit_sha="bad"),
            "build ref mismatch": lambda m: m.update(build_ref="main"),
            "production source ref mismatch": lambda m: m.update(production_source_ref="develop"),
            "duplicate digest": lambda m: m["image"].update(digest="sha256:" + "a" * 64),
            "provenance mismatch": lambda m: m["dev_provenance"].update(head_sha="d" * 40),
            "origin repository mismatch": lambda m: m["originating_workflow"].update(repository="fork/llm-wiki-bff"),
        }
        for name, mutate in cases.items():
            with self.subTest(name=name):
                metadata = self.metadata()
                mutate(metadata)
                result, output, _ = self.render(metadata)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())

        self.env["FAKE_JOB_IAM"] = str(self.root / "missing-iam.json")
        Path(self.env["FAKE_JOB_IAM"]).write_text(json.dumps({"bindings": []}))
        result, _, _ = self.render()
        self.assertNotEqual(result.returncode, 0)
        self.env["FAKE_JOB_IAM"] = str(self.job_iam)
        self.env["FAKE_SERVICE_IAM"] = str(self.root / "malformed-service-iam.json")
        Path(self.env["FAKE_SERVICE_IAM"]).write_text(json.dumps({"bindings": {}}))
        result, _, _ = self.render()
        self.assertNotEqual(result.returncode, 0)

    def test_provider_unavailable_is_partial_and_mismatch_is_unhealthy_with_rollback(self):
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        frozen = json.loads(rollback.read_text())
        self.env["FAKE_PROVIDER_FAILURE"] = "1"
        result, output, failure = self.render()
        self.assertNotEqual(result.returncode, 0)
        partial = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(failure))
        self.assertEqual(partial.returncode, 0, partial.stderr)
        document = json.loads(output.read_text())
        self.assertEqual(document["status"], "PARTIAL")
        self.assertEqual(document["rollback"], frozen)

        self.env.pop("FAKE_PROVIDER_FAILURE")
        self.env["FAKE_JOB_IAM"] = str(self.job_iam)
        self.env["FAKE_VERSION_JSON"] = self.env["FAKE_VERSION_JSON"].replace('"commit": "' + "c" * 40, '"commit": "' + "d" * 40)
        output.unlink()
        result, output, failure = self.render()
        self.assertNotEqual(result.returncode, 0)
        unhealthy = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(failure))
        self.assertEqual(unhealthy.returncode, 0, unhealthy.stderr)
        document = json.loads(output.read_text())
        self.assertEqual(document["status"], "UNHEALTHY")
        self.assertEqual(document["rollback"], frozen)
        self.assertEqual(document["provider_verification"]["result"], "failed")

    def test_partial_preserves_unknown_marker_and_ignores_forged_classification(self):
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        (self.root / "metadata.json").write_text(json.dumps(self.metadata()))
        marker_path = self.root / "failure.json"
        marker_path.write_text(json.dumps({"classification": "unknown", "reason_code": "identity_unavailable", "checked_at": "2026-07-24T00:00:00Z"}))
        output = self.root / "deployment-evidence.json"
        partial = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(marker_path))
        self.assertEqual(partial.returncode, 0, partial.stderr)
        document = json.loads(output.read_text())
        self.assertEqual(document["status"], "PARTIAL")
        self.assertEqual(document["reason"], "identity_unavailable")
        self.assertEqual(document["provider_verification"], {"result": "unknown", "reason_code": "identity_unavailable", "checked_at": "2026-07-24T00:00:00Z", "checks": []})

        for forged in (
            {"classification": "failed", "reason_code": "identity_unavailable", "checked_at": "2026-07-24T00:00:00Z"},
            {"classification": "unknown", "reason_code": "image_mismatch", "checked_at": "2026-07-24T00:00:00Z"},
        ):
            with self.subTest(forged=forged):
                output.unlink()
                marker_path.write_text(json.dumps(forged))
                partial = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(marker_path))
                self.assertEqual(partial.returncode, 0, partial.stderr)
                document = json.loads(output.read_text())
                self.assertEqual(document["status"], "PARTIAL")
                self.assertEqual(document["reason"], "deployment_outcome_unknown")
                self.assertEqual(document["provider_verification"], {"result": "unknown", "reason_code": None, "checked_at": None, "checks": []})

    def test_render_modes_require_failure_output(self):
        cases = {
            "render-evidence": ("--expected-runtime-service-account", SA, "--rollback-contract", str(self.root / "rollback.json"), "--metadata", str(self.root / "metadata.json"), "--output", str(self.root / "deployment-evidence.json")),
            "render-partial": ("--rollback-contract", str(self.root / "rollback.json"), "--metadata", str(self.root / "metadata.json"), "--output", str(self.root / "deployment-evidence.json")),
        }
        for mode, extra in cases.items():
            with self.subTest(mode=mode):
                result = self.invoke(mode, *extra)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("required", result.stderr)

    def test_saved_canonical_traffic_unknown_keys_are_rejected(self):
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        (self.root / "metadata.json").write_text(json.dumps(self.metadata()))
        saved = json.loads(rollback.read_text())
        saved["traffic"][0]["unexpected"] = "must-not-be-discarded"
        rollback.write_text(json.dumps(saved))
        result = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(self.root / "deployment-evidence.json"), "--failure-output", str(self.root / "failure.json"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("provider_shape_unsupported", result.stderr)

    def test_provider_traffic_is_compared_in_canonical_order_for_one_target(self):
        first = fixture("bff-service-before.json")
        second = fixture("bff-service-before.json")
        first["status"]["traffic"] = [{"revisionName": "llm-wiki-bff-00001-old", "percent": 100, "latestRevision": False}]
        second["status"]["traffic"] = [{"latestRevision": False, "percent": 100, "revisionName": "llm-wiki-bff-00001-old"}]
        first_path = self.root / "traffic-first.json"
        second_path = self.root / "traffic-second.json"
        first_path.write_text(json.dumps(first))
        second_path.write_text(json.dumps(second))
        self.env["FAKE_SERVICE_FIXTURES"] = f"{first_path},{second_path}"
        self.env["FAKE_SERVICE_STATE"] = str(self.root / "traffic-state")
        output = self.root / "traffic-rollback.json"
        result = self.invoke("prepare-rollback", "--artifact-name", ARTIFACT, "--output", str(output))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(output.read_text())["traffic"], [{"revision_name": "llm-wiki-bff-00001-old", "percent": 100, "latest_revision": False}])

    def test_prepare_rejects_ambiguous_or_non_100_percent_traffic(self):
        cases = [
            [{"revisionName": "llm-wiki-bff-00001-old", "percent": 50}, {"revisionName": "llm-wiki-bff-00002-new", "percent": 50}],
            [{"revisionName": "llm-wiki-bff-00001-old", "percent": 100}, {"revisionName": "llm-wiki-bff-00002-new", "percent": 0}],
            [{"revisionName": "llm-wiki-bff-00001-old", "percent": 99}],
            [{"revisionName": "llm-wiki-bff-00001-old", "percent": 100, "tag": "stable"}],
            [{"percent": 100}],
        ]
        for traffic in cases:
            with self.subTest(traffic=traffic):
                first = fixture("bff-service-before.json")
                first["status"]["traffic"] = traffic
                second = json.loads(json.dumps(first))
                first_path = self.root / "invalid-traffic-first.json"
                second_path = self.root / "invalid-traffic-second.json"
                first_path.write_text(json.dumps(first))
                second_path.write_text(json.dumps(second))
                self.env["FAKE_SERVICE_FIXTURES"] = f"{first_path},{second_path}"
                result = self.invoke("prepare-rollback", "--artifact-name", ARTIFACT, "--output", str(self.root / "invalid-traffic.json"))
                self.assertNotEqual(result.returncode, 0)

    def test_provider_and_identity_mismatches_are_classified_failed(self):
        cases = {
            "image": lambda s, r: (s["spec"]["template"]["spec"]["containers"][0].update(image=PRIOR_IMAGE), r["spec"]["containers"][0].update(image=PRIOR_IMAGE), r["status"].update(imageDigest=PRIOR_DIGEST)),
            "service account": lambda s, r: s["spec"].update(serviceAccountName="wrong@llm-wiki-cloud.iam.gserviceaccount.com"),
            "network": lambda s, r: s["spec"]["template"]["metadata"]["annotations"].update({"run.googleapis.com/vpc-access-egress": "all-traffic"}),
            "traffic": lambda s, r: s["status"].update(traffic=[{"revisionName": "llm-wiki-bff-00002-new", "percent": 50}, {"revisionName": "llm-wiki-bff-00001-old", "percent": 50}]),
            "missing secret": lambda s, r: s["spec"]["template"]["spec"]["containers"][0]["env"].pop(),
        }
        for name, mutate in cases.items():
            with self.subTest(name=name):
                prepared, rollback = self.prepare()
                self.assertEqual(prepared.returncode, 0, prepared.stderr)
                observed = json.loads(self.after.read_text())
                observed_revision = json.loads(self.revision_path.read_text())
                mutate(observed, observed_revision)
                result, output, failure = self.render_documents(observed, observed_revision)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())
                partial = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(failure))
                self.assertEqual(partial.returncode, 0, partial.stderr)
                self.assertEqual(json.loads(output.read_text())["status"], "UNHEALTHY")

    def test_observed_revision_image_shapes_are_failed_with_safe_reasons(self):
        cases = {
            "malformed image reference": (lambda r: r["spec"]["containers"][0].update(image="not-an-immutable-image"), "image_mismatch"),
            "malformed container": (lambda r: r["spec"].update(containers=[None]), "provider_shape_unsupported"),
            "malformed full image digest": (lambda r: r["status"].update(imageDigest={"image": IMAGE}), "image_mismatch"),
        }
        for name, (mutate, reason_code) in cases.items():
            with self.subTest(name=name):
                prepared, rollback = self.prepare()
                self.assertEqual(prepared.returncode, 0, prepared.stderr)
                observed_revision = fixture("bff-revision-after.json")
                mutate(observed_revision)
                result, output, failure = self.render_documents(fixture("bff-service-after.json"), observed_revision)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())
                marker = json.loads(failure.read_text())
                self.assertEqual(marker, {"classification": "failed", "reason_code": reason_code, "checked_at": marker["checked_at"]})
                partial = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(failure))
                self.assertEqual(partial.returncode, 0, partial.stderr)
                document = json.loads(output.read_text())
                self.assertEqual(document["status"], "UNHEALTHY")
                self.assertEqual(document["provider_verification"]["reason_code"], reason_code)

    def test_malformed_service_shapes_emit_failed_marker_and_unhealthy_fallback(self):
        cases = {
            "metadata None": lambda s: s.update(metadata=None),
            "metadata list": lambda s: s.update(metadata=[]),
            "metadata annotations malformed": lambda s: s["metadata"].update(annotations=[]),
            "spec None": lambda s: s.update(spec=None),
            "spec list": lambda s: s.update(spec=[]),
            "template None": lambda s: s["spec"].update(template=None),
            "template list": lambda s: s["spec"].update(template=[]),
            "template metadata None": lambda s: s["spec"]["template"].update(metadata=None),
            "template metadata list": lambda s: s["spec"]["template"].update(metadata=[]),
            "template annotations malformed": lambda s: s["spec"]["template"]["metadata"].update(annotations=[]),
        }
        for name, mutate in cases.items():
            with self.subTest(name=name):
                prepared, rollback = self.prepare()
                self.assertEqual(prepared.returncode, 0, prepared.stderr)
                observed_service = fixture("bff-service-after.json")
                mutate(observed_service)
                result, output, failure = self.render_documents(observed_service, fixture("bff-revision-after.json"))
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())
                marker = json.loads(failure.read_text())
                self.assertEqual(marker["classification"], "failed")
                self.assertEqual(marker["reason_code"], "provider_shape_unsupported")
                partial = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(failure))
                self.assertEqual(partial.returncode, 0, partial.stderr)
                document = json.loads(output.read_text())
                self.assertEqual(document["status"], "UNHEALTHY")
                self.assertEqual(document["provider_verification"]["reason_code"], "provider_shape_unsupported")

    def test_identity_sha_cache_header_and_status_gates_fail(self):
        for label, body, headers in [
            ("wrong sha", self.env["FAKE_VERSION_JSON"].replace('"commit": "' + "c" * 40, '"commit": "' + "d" * 40), None),
            ("wrong cache", None, "HTTP/2 200\nCache-Control: public\n"),
            ("wrong status", None, "HTTP/2 503\nCache-Control: no-store\n"),
        ]:
            with self.subTest(label=label):
                prepared, rollback = self.prepare()
                self.assertEqual(prepared.returncode, 0, prepared.stderr)
                self.env.pop("FAKE_HEADERS", None)
                self.env["FAKE_VERSION_JSON"] = body or self.env["FAKE_VERSION_JSON"]
                if headers is None:
                    self.env.pop("FAKE_HEADERS", None)
                else:
                    self.env["FAKE_HEADERS"] = headers
                result, output, failure = self.render()
                self.assertNotEqual(result.returncode, 0)
                partial = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(failure))
                self.assertEqual(partial.returncode, 0, partial.stderr)
                self.assertEqual(json.loads(output.read_text())["status"], "UNHEALTHY")

    def test_identity_transport_failure_is_unknown_but_http_status_is_failed(self):
        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        self.env["FAKE_HEADERS"] = "HTTP/2 503\nCache-Control: no-store\n"
        self.env["FAKE_CURL_HTTP_ERROR"] = "1"
        result, output, failure = self.render()
        self.assertNotEqual(result.returncode, 0)
        marker = json.loads(failure.read_text())
        self.assertEqual(marker["classification"], "failed")
        self.assertEqual(marker["reason_code"], "identity_status_mismatch")
        self.assertNotIn("--fail-with-body", Path(self.env["FAKE_LOG"]).read_text())
        partial = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(failure))
        self.assertEqual(partial.returncode, 0, partial.stderr)
        self.assertEqual(json.loads(output.read_text())["status"], "UNHEALTHY")

        prepared, rollback = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        self.env.pop("FAKE_CURL_HTTP_ERROR")
        self.env["FAKE_CURL_FAILURE"] = "1"
        result, output, failure = self.render()
        self.assertNotEqual(result.returncode, 0)
        marker = json.loads(failure.read_text())
        self.assertEqual(marker["classification"], "unknown")
        self.assertEqual(marker["reason_code"], "identity_unavailable")
        partial = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(failure))
        self.assertEqual(partial.returncode, 0, partial.stderr)
        document = json.loads(output.read_text())
        self.assertEqual(document["status"], "PARTIAL")
        self.assertEqual(document["provider_verification"]["reason_code"], "identity_unavailable")
        self.assertEqual(document["provider_verification"]["checked_at"], marker["checked_at"])

    def test_strict_post_readback_rejects_traffic_snapshot_change_between_reads(self):
        for value in (False,):
            with self.subTest(value=value):
                prepared, rollback = self.prepare()
                self.assertEqual(prepared.returncode, 0, prepared.stderr)
                good = self.root / "good-after.json"
                bad = self.root / "bad-after.json"
                good.write_text(json.dumps(fixture("bff-service-after.json")))
                observed_service = fixture("bff-service-after.json")
                if value is None:
                    del observed_service["status"]["traffic"][0]["latestRevision"]
                else:
                    observed_service["status"]["traffic"][0]["latestRevision"] = value
                bad.write_text(json.dumps(observed_service))
                result, output, failure = self.render(service_fixtures=f"{good},{bad}")
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())
                marker = json.loads(failure.read_text())
                self.assertEqual(marker["classification"], "failed")
                self.assertEqual(marker["reason_code"], "traffic_mismatch")
                partial = self.invoke("render-partial", "--rollback-contract", str(rollback), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(failure))
                self.assertEqual(partial.returncode, 0, partial.stderr)
                self.assertEqual(json.loads(output.read_text())["status"], "UNHEALTHY")

    def test_strict_post_readback_rejects_split_tag_implicit_and_non_100_traffic(self):
        cases = [
            [{"revisionName": "llm-wiki-bff-00002-new", "percent": 50}, {"revisionName": "llm-wiki-bff-00001-old", "percent": 50}],
            [{"revisionName": "llm-wiki-bff-00002-new", "percent": 100, "tag": "candidate"}],
            [{"latestRevision": True, "percent": 100}],
            [{"revisionName": "llm-wiki-bff-00002-new", "percent": 99}],
        ]
        for traffic in cases:
            with self.subTest(traffic=traffic):
                prepared, _ = self.prepare()
                self.assertEqual(prepared.returncode, 0, prepared.stderr)
                good = fixture("bff-service-after.json")
                bad = json.loads(json.dumps(good))
                bad["status"]["traffic"] = traffic
                good_path = self.root / "strict-good.json"
                bad_path = self.root / "strict-bad.json"
                good_path.write_text(json.dumps(good))
                bad_path.write_text(json.dumps(bad))
                result, output, failure = self.render(service_fixtures=f"{good_path},{bad_path}")
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())
                self.assertIn(json.loads(failure.read_text())["reason_code"], {"traffic_mismatch", "provider_shape_unsupported", "rollback_race"})

    def test_rollback_race_and_http_identity_gates_fail(self):
        self.env["FAKE_SERVICE_FIXTURES"] = f"{self.before},{self.after}"
        output = self.root / "race-rollback.json"
        result = self.invoke("prepare-rollback", "--artifact-name", ARTIFACT, "--output", str(output))
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())
        prepared, _ = self.prepare()
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        self.env["FAKE_HEADERS"] = "HTTP/2 503\nCache-Control: no-store\n"
        result, output, failure = self.render()
        self.assertNotEqual(result.returncode, 0)
        partial = self.invoke("render-partial", "--rollback-contract", str(self.root / "rollback.json"), "--metadata", str(self.root / "metadata.json"), "--output", str(output), "--failure-output", str(failure))
        self.assertEqual(partial.returncode, 0)
        self.assertEqual(json.loads(output.read_text())["status"], "UNHEALTHY")


if __name__ == "__main__":
    unittest.main()
