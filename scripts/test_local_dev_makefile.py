#!/usr/bin/env python3
import os
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


class LocalDevMakefileTests(unittest.TestCase):
    def test_local_config_writes_frontend_env_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            frontend = Path(tmp) / "llm-wiki-frontend"
            frontend.mkdir()
            subprocess.run(
                ["make", "local-config", f"FRONTEND_DIR={frontend}"],
                cwd=ROOT,
                check=True,
                capture_output=True,
                text=True,
            )
            self.assertEqual(
                (frontend / ".env.local").read_text(),
                "NEXT_PUBLIC_API_URL=http://localhost:8080\n"
                "NEXT_PUBLIC_AUTH_URL=http://localhost:8081\n"
                "NEXT_PUBLIC_DEV_USER_ID=local-user\n"
                "NEXT_PUBLIC_DEV_PROJECT_ID=demo\n",
            )

    def test_local_config_uses_bff_port_override(self):
        with tempfile.TemporaryDirectory() as tmp:
            frontend = Path(tmp) / "llm-wiki-frontend"
            frontend.mkdir()
            subprocess.run(
                [
                    "make",
                    "local-config",
                    f"FRONTEND_DIR={frontend}",
                    "BFF_PORT=18080",
                ],
                cwd=ROOT,
                check=True,
                capture_output=True,
                text=True,
            )
            self.assertIn(
                "NEXT_PUBLIC_API_URL=http://localhost:18080\n",
                (frontend / ".env.local").read_text(),
            )

    def make_dry_run(self, target, *variables):
        return subprocess.run(
            ["make", "-n", target, *variables],
            cwd=ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout

    def test_dev_runs_native_bff_and_frontend_targets(self):
        output = self.make_dry_run("dev")
        self.assertIn("make -j3 bff-local auth-local frontend-local", output)
        self.assertNotIn("docker compose", output)

    def test_support_bff_runs_auth_and_frontend(self):
        output = self.make_dry_run("support-bff")
        self.assertIn("make -j2 auth-local frontend-local", output)
        self.assertNotIn("make bff-local", output)

    def test_support_frontend_runs_bff_and_auth(self):
        output = self.make_dry_run("support-frontend")
        self.assertIn("make -j2 bff-local auth-local", output)
        self.assertNotIn("make frontend-local", output)

    def test_support_pipeline_runs_bff_auth_and_frontend(self):
        output = self.make_dry_run("support-pipeline")
        self.assertIn("make -j3 bff-local auth-local frontend-local", output)

    def test_port_overrides_reach_component_commands(self):
        bff = self.make_dry_run("bff-local", "BFF_PORT=18080")
        frontend = self.make_dry_run("frontend-local", "FRONTEND_PORT=13000")
        self.assertIn("PORT=18080", bff)
        self.assertIn("--port 13000", frontend)

    def test_deploy_dev_recipe_is_parseable_and_removes_legacy_query_env(self):
        output = self.make_dry_run("deploy-dev")
        self.assertEqual(output.count("gcloud run deploy"), 1)
        subprocess.run(["sh", "-n"], input=output, cwd=ROOT, check=True, text=True)

        legacy = [
            "QUERY_EXPANSION_MODEL", "QUERY_EXPANSION_REASONING", "ANSWER_SYNTHESIS_MODEL",
            "ANSWER_SYNTHESIS_REASONING", "QUERY_SELECTION_LIMIT", "QUERY_SELECTION_EXPLORATION_SLOTS",
            "QUERY_SELECTION_EVIDENCE_THRESHOLD", "QUERY_EXPANSION_KEYWORDS_PER_ATTEMPT",
            "QUERY_EXPANSION_ATTEMPTS", "QUERY_MATCHING_RARE_KEYWORD_MAX_DOCUMENT_FREQUENCY",
        ]
        remove_start = output.index('--remove-env-vars "')
        update_start = output.index('--update-env-vars "')
        remove_block = output[remove_start:update_start]
        update_block = output[update_start:]
        for name in legacy:
            self.assertEqual(output.count(name), 1)
            self.assertIn(name, remove_block)
            self.assertNotIn(name, update_block)
        self.assertIn(
            'QUERY_STAGE_CONFIG_PATH=/app/configs/query/dev/query-dev-2026-08-31.1.json',
            update_block,
        )
        self.assertEqual(update_block.count("QUERY_STAGE_CONFIG_PATH="), 1)
        self.assertEqual(update_block.count("QUERY_"), 1)
        self.assertIn(
            '--update-secrets "JWT_SECRET=jwt-secret-dev:latest,DEEPSEEK_API_KEY=deepseek-apikey:***"',
            output,
        )
        for value in [
            "GCP_PROJECT=llm-wiki-cloud", "BUCKET=llm-wiki-data-dev",
            "FIRESTORE_DATABASE_ID=llm-wiki-cloud-dev", "DEV_JWT=false",
        ]:
            self.assertIn(value, update_block)

    @staticmethod
    def free_port():
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            return listener.getsockname()[1]

    @staticmethod
    def wait_for_listener(port):
        deadline = time.time() + 5
        while time.time() < deadline:
            with socket.socket() as probe:
                if probe.connect_ex(("127.0.0.1", port)) == 0:
                    return
            time.sleep(0.05)
        raise AssertionError(f"listener on port {port} did not start")

    def test_kill_local_terminates_listeners_on_both_ports(self):
        ports = [self.free_port(), self.free_port(), self.free_port()]
        code = (
            "import socket,sys,time; "
            "s=socket.socket(); "
            "s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); "
            "s.bind(('127.0.0.1',int(sys.argv[1]))); "
            "s.listen(); time.sleep(60)"
        )
        processes = [
            subprocess.Popen([sys.executable, "-c", code, str(port)])
            for port in ports
        ]
        try:
            for port in ports:
                self.wait_for_listener(port)
            subprocess.run(
                [
                    "make",
                    "kill-local",
                    f"BFF_PORT={ports[0]}",
                    f"AUTH_PORT={ports[1]}",
                    f"FRONTEND_PORT={ports[2]}",
                ],
                cwd=ROOT,
                check=True,
                capture_output=True,
                text=True,
            )
            for process in processes:
                process.wait(timeout=5)
                self.assertIsNotNone(process.returncode)
        finally:
            for process in processes:
                if process.poll() is None:
                    process.kill()
                    process.wait()

    def test_local_token_logs_in_with_demo_credential(self):
        port = self.free_port()
        server_code = r'''
import http.server
import json
import sys

expected = {"email": "demo@llm-wiki.dev", "password": "demo123456"}

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        if self.path != "/api/v1/auth/login" or body != expected:
            self.send_response(400)
            self.end_headers()
            return
        payload = json.dumps({"access_token": "header.payload.signature"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_):
        pass

server = http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler)
server.serve_forever()
'''
        server = subprocess.Popen([sys.executable, "-c", server_code, str(port)])
        try:
            self.wait_for_listener(port)
            result = subprocess.run(
                ["make", "local-token", f"AUTH_PORT={port}"],
                cwd=ROOT,
                check=True,
                capture_output=True,
                text=True,
            )
            self.assertEqual(result.stdout.strip(), "header.payload.signature")
        finally:
            if server.poll() is None:
                server.kill()
                server.wait()

    def test_pipeline_test_runs_worker_tests(self):
        output = self.make_dry_run("pipeline-test")
        self.assertIn("go test ./cmd/olw_worker", output)


if __name__ == "__main__":
    unittest.main()
