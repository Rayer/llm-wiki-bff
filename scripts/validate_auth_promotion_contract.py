#!/usr/bin/env python3
"""Validate the immutable Auth DEV receipt and its GitHub Actions provenance."""

import argparse
import json
from pathlib import Path
import re
import sys


RECEIPT_KEYS = (
    "receipt_schema_version",
    "component",
    "build_ref",
    "source_sha",
    "dev_run_id",
    "image_digest",
    "image_reference",
)
SHA_RE = re.compile(r"[0-9a-f]{40}\Z")
DIGEST_RE = re.compile(r"sha256:[0-9a-f]{64}\Z")
RUN_ID_RE = re.compile(r"[1-9][0-9]*\Z")
RUN_URL_RE = re.compile(r"https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/actions/runs/[1-9][0-9]*\Z")
IMAGE_NAME = "llm-wiki-auth"
WORKFLOW_PATH = ".github/workflows/deploy-auth.yml"


class ContractError(Exception):
    pass


def reject(message):
    raise ContractError(message)


def read_json(path):
    try:
        value = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        reject(f"JSON input is unreadable: {error.__class__.__name__}")
    if not isinstance(value, dict):
        reject("JSON input must be an object")
    return value


def read_receipt(path):
    try:
        raw = Path(path).read_bytes()
    except OSError as error:
        reject(f"receipt is unreadable: {error.__class__.__name__}")
    if not raw.endswith(b"\n") or raw.endswith(b"\n\n") or b"\r" in raw:
        reject("receipt must use exactly seven LF-terminated lines")
    lines = raw[:-1].split(b"\n")
    if len(lines) != len(RECEIPT_KEYS):
        reject("receipt must contain exactly the required fields")
    values = {}
    for expected_key, line in zip(RECEIPT_KEYS, lines):
        if line.count(b"=") != 1:
            reject("receipt contains a malformed line")
        try:
            key, value = line.decode("ascii").split("=", 1)
        except UnicodeDecodeError:
            reject("receipt contains non-ASCII data")
        if key != expected_key or key in values:
            reject("receipt fields are unknown, missing, trailing, or out of order")
        if not value or value != value.strip() or any(character.isspace() for character in value):
            reject(f"receipt field is empty or ambiguous: {key}")
        values[key] = value
    return values


def validate(args):
    values = read_receipt(args.receipt)
    if values["receipt_schema_version"] != "1" or values["component"] != "lwc-auth":
        reject("receipt identity is invalid")
    if values["build_ref"] != "develop":
        reject("receipt build ref does not match canonical develop")
    if not SHA_RE.fullmatch(values["source_sha"]) or values["source_sha"] != args.expected_sha:
        reject("receipt source SHA does not match")
    if not RUN_ID_RE.fullmatch(values["dev_run_id"]) or int(values["dev_run_id"]) != args.expected_run_id:
        reject("receipt DEV run ID does not match")
    if not DIGEST_RE.fullmatch(values["image_digest"]):
        reject("receipt image digest is invalid")
    expected_image = f"{args.ar_repo}/{IMAGE_NAME}@{values['image_digest']}"
    if values["image_reference"] != expected_image:
        reject("receipt image reference is not the expected immutable Auth image")

    run = read_json(args.run_json)
    expected = {
        "id": args.expected_run_id,
        "path": WORKFLOW_PATH,
        "event": "workflow_dispatch",
        "head_branch": "develop",
        "head_sha": args.expected_sha,
        "status": "completed",
        "conclusion": "success",
    }
    for key, value in expected.items():
        if run.get(key) != value:
            reject(f"DEV run provenance {key} is invalid")
    run_url = run.get("html_url")
    expected_url = f"https://github.com/{args.repository}/actions/runs/{args.expected_run_id}"
    if not isinstance(run_url, str) or not RUN_URL_RE.fullmatch(run_url) or run_url != expected_url:
        reject("DEV run provenance URL is invalid")

    normalized = {
        "schema_version": 1,
        "receipt_schema_version": 1,
        "component": "lwc-auth",
        "build_ref": "develop",
        "result": "ready",
        "source_sha": args.expected_sha,
        "dev_run_id": args.expected_run_id,
        "dev_run_url": run_url,
        "image_digest": values["image_digest"],
        "image_reference": values["image_reference"],
    }
    Path(args.output).write_text(json.dumps(normalized, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    if args.github_output:
        with Path(args.github_output).open("a", encoding="utf-8") as output:
            for key, value in (
                ("source_sha", args.expected_sha),
                ("dev_run_id", str(args.expected_run_id)),
                ("dev_run_url", run_url),
                ("dev_workflow", run["path"]),
                ("dev_event", run["event"]),
                ("dev_head_branch", run["head_branch"]),
                ("dev_head_sha", run["head_sha"]),
                ("dev_conclusion", run["conclusion"]),
                ("digest", values["image_digest"]),
                ("image", values["image_reference"]),
            ):
                output.write(f"{key}={value}\n")


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument("--receipt", required=True)
    parser.add_argument("--run-json", required=True)
    parser.add_argument("--expected-sha", required=True)
    parser.add_argument("--expected-run-id", required=True, type=int)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--ar-repo", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--github-output")
    args = parser.parse_args(argv)
    try:
        if args.expected_run_id <= 0 or not SHA_RE.fullmatch(args.expected_sha):
            reject("expected identity is invalid")
        validate(args)
    except ContractError as error:
        print(f"Auth promotion contract rejected: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
