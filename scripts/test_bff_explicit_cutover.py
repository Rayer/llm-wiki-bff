#!/usr/bin/env python3
"""Execute the production BFF revision transaction against a fake provider."""

import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import textwrap
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = ROOT / ".github/workflows/release-bff.yml"
SHA = "a" * 40
IMAGE = "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/llm-wiki-bff@sha256:" + "d" * 64
FROZEN_CREATED = "llm-wiki-bff-00071-zgk"
FROZEN_REVISION = "llm-wiki-bff-00070-f5c"
CANDIDATE = "llm-wiki-bff-00072-7qf"


class ExplicitCutoverHarnessTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = Path(self.tempdir.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.state = self.root / "provider-state.json"
        self.log = self.root / "provider.log"
        self.gh_count = self.root / "gh-count"
        self.output = self.root / "github-output"
        self.github_env = self.root / "github-env"
        self.rollback = self.root / "rollback-contract.json"
        self.rollback.write_text(json.dumps({
            "ready_revision": FROZEN_REVISION,
            "traffic": [{"revision_name": FROZEN_REVISION, "percent": 100}],
        }))
        self.state.write_text(json.dumps({"traffic": FROZEN_REVISION, "deployed": False}))
        self._write_fake_provider()

    def tearDown(self):
        self.tempdir.cleanup()

    def _write_fake_provider(self):
        gcloud = self.bin / "gcloud"
        gcloud.write_text(textwrap.dedent(r'''
            #!/usr/bin/env python3
            import json, os, sys
            from pathlib import Path

            args = sys.argv[1:]
            log = Path(os.environ["FAKE_LOG"])
            with log.open("a") as output:
                output.write("gcloud " + " ".join(args) + "\n")
            state_path = Path(os.environ["FAKE_STATE"])
            state = json.loads(state_path.read_text())
            mode = os.environ.get("FAKE_MODE", "success")

            def save():
                state_path.write_text(json.dumps(state))

            def service():
                traffic = state["traffic"]
                if mode == "pretraffic-drift" and not state.get("updated"):
                    traffic = "llm-wiki-bff-00069-drift"
                if mode == "pretraffic-split" and not state.get("updated"):
                    traffic = [{"revisionName": FROZEN_REVISION, "percent": 50}, {"revisionName": CANDIDATE, "percent": 50}]
                if mode == "pretraffic-tag" and not state.get("updated"):
                    traffic = [{"revisionName": FROZEN_REVISION, "percent": 100, "tag": "stable"}]
                if mode == "pretraffic-latest" and not state.get("updated"):
                    traffic = [{"revisionName": FROZEN_REVISION, "percent": 100, "latestRevision": True}]
                if isinstance(traffic, list):
                    provider_traffic = traffic
                else:
                    provider_traffic = [{"revisionName": traffic, "percent": 100}]
                latest_created = CANDIDATE if state.get("deployed") else FROZEN_CREATED
                if mode == "unchanged-created":
                    latest_created = FROZEN_CREATED
                return {
                    "status": {
                        "latestCreatedRevisionName": latest_created,
                        "latestReadyRevisionName": FROZEN_CREATED,
                        "traffic": provider_traffic,
                        "url": "https://llm-wiki-bff-abc.a.run.app",
                    }
                }

            if args[:3] == ["run", "deploy", "llm-wiki-bff"]:
                state["deployed"] = True
                save()
            elif args[:3] == ["run", "services", "describe"]:
                if mode == "rollback-unavailable" and state.get("updated"):
                    raise SystemExit(9)
                if "value(status.url)" in args:
                    print("https://llm-wiki-bff-abc.a.run.app")
                else:
                    print(json.dumps(service()))
            elif args[:3] == ["run", "revisions", "describe"]:
                image = IMAGE
                ready = True
                container_ready = True
                container_healthy = False
                if mode == "image-mismatch":
                    image = IMAGE.replace("d" * 64, "e" * 64)
                elif mode == "not-ready":
                    ready = False
                elif mode == "not-container-ready":
                    container_ready = False
                elif mode == "container-healthy-only":
                    container_ready = False
                    container_healthy = True
                elif mode == "transient-not-ready" and state.get("revision_describes", 0) == 0:
                    ready = False
                state["revision_describes"] = state.get("revision_describes", 0) + 1
                save()
                active = state.get("updated", False) or mode == "active-true-zero-traffic"
                active_reason = "Active" if active else ("Deploying" if mode == "active-false-other" else "Retired")
                conditions = [
                    {"type": "Ready", "status": "True" if ready else "False", "reason": "Retired" if ready else "ContainerFailed"},
                    *([] if mode == "container-healthy-only" else [{"type": "ContainerReady", "status": "True" if container_ready else "False", "reason": "Retired" if container_ready else "ContainerFailed"}]),
                    *([{"type": "ContainerHealthy", "status": "True", "reason": "Retired"}] if container_healthy else []),
                    {"type": "Active", "status": "True" if active else "False", "reason": active_reason},
                ]
                print(json.dumps({
                    "metadata": {"name": CANDIDATE},
                    "spec": {"containers": [{"image": image}]},
                    "status": {"imageDigest": image, "conditions": conditions},
                }))
            elif args[:3] == ["run", "services", "update-traffic"]:
                state["updated"] = True
                target = args[args.index("--to-revisions") + 1]
                if target.startswith(FROZEN_REVISION + "="):
                    state["traffic"] = FROZEN_REVISION
                    save()
                    raise SystemExit(0)
                if mode == "update-nonzero-wins":
                    state["traffic"] = CANDIDATE
                    save()
                    raise SystemExit(7)
                if mode == "update-success-old":
                    state["traffic"] = FROZEN_REVISION
                elif mode == "update-success-split":
                    state["traffic"] = [{"revisionName": FROZEN_REVISION, "percent": 50}, {"revisionName": CANDIDATE, "percent": 50}]
                elif mode == "update-success-tag":
                    state["traffic"] = [{"revisionName": CANDIDATE, "percent": 100, "tag": "candidate"}]
                elif mode == "update-success-latest":
                    state["traffic"] = [{"revisionName": CANDIDATE, "percent": 100, "latestRevision": True}]
                elif mode == "update-success-other":
                    state["traffic"] = "llm-wiki-bff-00069-drift"
                elif mode == "update-success-unknown":
                    state["traffic"] = [{"revisionName": CANDIDATE, "percent": 100, "unknown": True}]
                elif mode == "update-success-wrong-types":
                    state["traffic"] = [{"revisionName": CANDIDATE, "percent": "100"}]
                elif mode == "update-success-invalid-percent":
                    state["traffic"] = [{"revisionName": CANDIDATE, "percent": 101}]
                elif mode == "update-success-invalid-sum":
                    state["traffic"] = [{"revisionName": CANDIDATE, "percent": 60}, {"revisionName": FROZEN_REVISION, "percent": 30}]
                elif mode == "update-success-empty":
                    state["traffic"] = []
                else:
                    state["traffic"] = CANDIDATE
                save()
            else:
                raise SystemExit("unexpected gcloud invocation: " + " ".join(args))
        ''').replace("FROZEN_REVISION", repr(FROZEN_REVISION)).replace("FROZEN_CREATED", repr(FROZEN_CREATED)).replace("CANDIDATE", repr(CANDIDATE)).replace("IMAGE", repr(IMAGE)).lstrip())
        gcloud.chmod(gcloud.stat().st_mode | stat.S_IXUSR)

        gh = self.bin / "gh"
        gh.write_text(textwrap.dedent(f'''\
            #!/usr/bin/env bash
            set -euo pipefail
            if [[ "${{FAKE_MODE:-}}" == "main-changed" ]]; then
              count=0
              if [[ -f "$FAKE_GH_COUNT" ]]; then count=$(<"$FAKE_GH_COUNT"); fi
              count=$((count + 1))
              printf '%s\\n' "$count" > "$FAKE_GH_COUNT"
              if (( count >= 2 )); then
                printf '%s\\n' '{"b" * 40}'
              else
                printf '%s\\n' '{SHA}'
              fi
            elif [[ "${{FAKE_MODE:-}}" == "initial-main-mismatch" ]]; then
              printf '%s\\n' '{"b" * 40}'
            else
              printf '%s\\n' '{SHA}'
            fi
        '''))
        gh.chmod(gh.stat().st_mode | stat.S_IXUSR)

    def _workflow_step(self, name):
        workflow = yaml.safe_load(WORKFLOW.read_text())
        for step in workflow["jobs"]["promote"]["steps"]:
            if step.get("name") == name:
                return step["run"]
        raise AssertionError(f"workflow step {name!r} is missing")

    def _env(self, mode):
        return {
            **os.environ,
            "PATH": f"{self.bin}:{os.environ['PATH']}",
            "FAKE_LOG": str(self.log),
            "FAKE_STATE": str(self.state),
            "FAKE_MODE": mode,
            "FAKE_GH_COUNT": str(self.gh_count),
            "SERVICE_NAME": "llm-wiki-bff",
            "PROJECT_ID": "llm-wiki-cloud",
            "REGION": "asia-east1",
            "RUNTIME_SERVICE_ACCOUNT": "lwc-bff-prod@llm-wiki-cloud.iam.gserviceaccount.com",
            "IMMUTABLE_IMAGE": IMAGE,
            "COMMIT_SHA": SHA,
            "EXPECTED_COMMIT": SHA,
            "GITHUB_REPOSITORY": "Rayer/llm-wiki-bff",
            "GITHUB_OUTPUT": str(self.output),
            "GITHUB_ENV": str(self.github_env),
            "ROLLBACK_CONTRACT": str(self.rollback),
            "FROZEN_CREATED_REVISION": FROZEN_CREATED,
            "JWT_SECRET_NAME": "jwt-secret-prod",
            "DEEPSEEK_SECRET_NAME": "deepseek-apikey",
            "BUCKET": "llm-wiki-data",
            "FIRESTORE_DATABASE_ID": "llm-wiki-cloud-prod",
            "PIPELINE_JOB_NAME": "olw-pipeline",
            "ALLOWED_ORIGINS": "https://wiki.rayer.idv.tw,https://llm-wiki-frontend.vercel.app",
            "QUERY_STAGE_CONFIG_PATH": "/app/configs/query/dev/query-dev-2026-08-22.1.json",
            "CANDIDATE_DISCOVERY_TIMEOUT_SECONDS": "1",
            "CANDIDATE_READINESS_TIMEOUT_SECONDS": "5",
            "CUTOVER_VERIFY_TIMEOUT_SECONDS": "1",
        }

    def _run(self, mode="success", include_deploy=True):
        env = self._env(mode)
        scripts = []
        if include_deploy:
            scripts.append(self._workflow_step("Deploy existing immutable image to Cloud Run"))
        scripts.append(self._workflow_step("Identify, validate, and explicitly cut over exact BFF candidate"))
        script = "\n".join(scripts).replace("sleep 5", "SECONDS=$((SECONDS + 1))").replace("sleep 10", "SECONDS=$CUTOVER_VERIFY_DEADLINE")
        return subprocess.run(["bash", "-c", script], cwd=ROOT, env=env, capture_output=True, text=True)

    def _run_rollback(self, mode):
        env = self._env(mode)
        script = self._workflow_step("Restore frozen production traffic after query-config readback failure")
        script = script.replace("sleep 5", "SECONDS=$ROLLBACK_READBACK_DEADLINE").replace("sleep 10", "SECONDS=$ROLLBACK_READBACK_DEADLINE")
        return subprocess.run(["bash", "-c", script], cwd=ROOT, env=env, capture_output=True, text=True)

    def test_pinned_traffic_promotes_the_new_retired_candidate(self):
        result = self._run()
        self.assertEqual(result.returncode, 0, result.stderr)
        log = self.log.read_text()
        self.assertIn("--no-traffic", log)
        self.assertIn(f"--to-revisions {CANDIDATE}=100", log)
        self.assertEqual(log.count("run services update-traffic"), 1)
        self.assertIn(f"candidate_revision={CANDIDATE}", self.output.read_text().splitlines())

    def test_candidate_discovery_and_readiness_fail_closed(self):
        for mode in ("unchanged-created", "image-mismatch", "not-ready", "not-container-ready", "active-false-other", "initial-main-mismatch", "pretraffic-drift", "pretraffic-split", "pretraffic-tag", "pretraffic-latest"):
            with self.subTest(mode=mode):
                result = self._run(mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("run services update-traffic", self.log.read_text())

    def test_container_healthy_only_and_active_true_zero_traffic_are_accepted(self):
        for mode in ("container-healthy-only", "active-true-zero-traffic"):
            with self.subTest(mode=mode):
                self.state.write_text(json.dumps({"traffic": FROZEN_REVISION, "deployed": False}))
                self.log.unlink(missing_ok=True)
                self.output.unlink(missing_ok=True)
                self.gh_count.unlink(missing_ok=True)
                result = self._run(mode)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.log.read_text().count("run services update-traffic"), 1)

    def test_main_change_is_only_seen_on_second_just_before_cutover_read(self):
        result = self._run("main-changed")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.gh_count.read_text().strip(), "2")
        log = self.log.read_text()
        self.assertIn(f"run revisions describe {CANDIDATE}", log)
        self.assertNotIn("run services update-traffic", log)

    def test_update_error_that_wins_is_reconciled_without_retry(self):
        result = self._run("update-nonzero-wins")
        self.assertEqual(result.returncode, 0, result.stderr)
        log = self.log.read_text()
        self.assertEqual(log.count("run services update-traffic"), 1)
        self.assertIn("update-traffic returned 7", result.stdout)

    def test_failed_readback_invokes_frozen_rollback(self):
        for mode, want_rollback_write in (("update-success-old", False), ("update-success-split", True), ("update-success-tag", True), ("update-success-latest", True), ("update-success-other", True)):
            with self.subTest(mode=mode):
                self.state.write_text(json.dumps({"traffic": FROZEN_REVISION, "deployed": False}))
                self.log.unlink(missing_ok=True)
                self.output.unlink(missing_ok=True)
                result = self._run(mode)
                self.assertNotEqual(result.returncode, 0)
                rollback = self._run_rollback(mode)
                self.assertNotEqual(rollback.returncode, 0)
                log = self.log.read_text()
                expected_updates = 2 if want_rollback_write else 1
                self.assertEqual(log.count("run services update-traffic"), expected_updates)
                if want_rollback_write:
                    self.assertIn(f"--to-revisions {FROZEN_REVISION}=100", log)
                    self.assertIn("restored effective routing", rollback.stdout)

    def test_unavailable_rollback_readback_does_not_mutate(self):
        result = self._run("rollback-unavailable")
        self.assertNotEqual(result.returncode, 0)
        rollback = self._run_rollback("rollback-unavailable")
        self.assertNotEqual(rollback.returncode, 0)
        self.assertEqual(self.log.read_text().count("run services update-traffic"), 1)

    def test_unreadable_rollback_traffic_does_not_mutate(self):
        for mode in ("update-success-unknown", "update-success-wrong-types", "update-success-invalid-percent", "update-success-invalid-sum", "update-success-empty"):
            with self.subTest(mode=mode):
                self.state.write_text(json.dumps({"traffic": FROZEN_REVISION, "deployed": False}))
                self.log.unlink(missing_ok=True)
                result = self._run(mode)
                self.assertNotEqual(result.returncode, 0)
                rollback = self._run_rollback(mode)
                self.assertNotEqual(rollback.returncode, 0)
                self.assertEqual(self.log.read_text().count("run services update-traffic"), 1)

    def test_transient_exact_candidate_readiness_is_polled(self):
        result = self._run("transient-not-ready")
        self.assertEqual(result.returncode, 0, result.stderr)
        log = self.log.read_text()
        self.assertGreaterEqual(log.count("run revisions describe"), 3)
        self.assertEqual(log.count("run services update-traffic"), 1)
        self.assertIn(f"--to-revisions {CANDIDATE}=100", log)


if __name__ == "__main__":
    unittest.main()
