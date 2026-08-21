#!/usr/bin/env python3
"""Validate the production-owned BFF promotion receipt and traffic contracts."""

import argparse
import json
from pathlib import Path
import re
import sys


RECEIPT_SCHEMA_VERSION = 1
PROMOTION_SCHEMA_VERSION = 1
IMAGE_NAME = "llm-wiki-bff"
WORKFLOW_PATH = ".github/workflows/deploy-bff.yml"
RECEIPT_KEYS = (
    "receipt_schema_version",
    "component",
    "source_sha",
    "dev_run_id",
    "image_digest",
    "image_reference",
    "query_config_revision",
    "query_config_digest",
)
SHA_RE = re.compile(r"[0-9a-f]{40}\Z")
DIGEST_RE = re.compile(r"sha256:[0-9a-f]{64}\Z")
RUN_ID_RE = re.compile(r"[1-9][0-9]*\Z")
REVISION_RE = re.compile(r"[a-z0-9][a-z0-9-]{0,62}\Z")


class ContractError(Exception):
    pass


def reject(message):
    raise ContractError(message)


def read_json(path):
    try:
        raw = sys.stdin.read() if path == "-" else Path(path).read_text(encoding="utf-8")
        value = json.loads(raw, object_pairs_hook=_object_pairs)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        reject(f"JSON input is unreadable: {error.__class__.__name__}")
    return value


def _object_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            reject(f"duplicate JSON field: {key}")
        result[key] = value
    return result


def read_receipt(path):
    try:
        raw = Path(path).read_bytes()
    except OSError as error:
        reject(f"receipt is unreadable: {error.__class__.__name__}")
    if not raw.endswith(b"\n") or raw.endswith(b"\n\n") or b"\r" in raw:
        reject("receipt must use exactly eight LF-terminated lines")
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
        if key != expected_key:
            reject("receipt fields are unknown, missing, trailing, or out of order")
        if key in values:
            reject(f"duplicate receipt field: {key}")
        if not value or value != value.strip() or any(character.isspace() for character in value):
            reject(f"receipt field is empty or ambiguous: {key}")
        values[key] = value
    if set(values) != set(RECEIPT_KEYS):
        reject("receipt must contain exactly the required fields")
    return values


def validate_dev_receipt(args):
    values = read_receipt(args.receipt)
    if values["receipt_schema_version"] != str(RECEIPT_SCHEMA_VERSION):
        reject("receipt schema version is unsupported")
    if values["component"] != args.component:
        reject("receipt component does not match")
    if not SHA_RE.fullmatch(values["source_sha"]) or values["source_sha"] != args.expected_sha:
        reject("receipt source SHA does not match")
    if not RUN_ID_RE.fullmatch(values["dev_run_id"]) or int(values["dev_run_id"]) != args.expected_run_id:
        reject("receipt DEV run ID does not match")
    if not DIGEST_RE.fullmatch(values["image_digest"]):
        reject("receipt image digest is invalid")
    expected_image = f"{args.ar_repo}/{IMAGE_NAME}@{values['image_digest']}"
    if values["image_reference"] != expected_image:
        reject("receipt image reference is not an immutable digest")
    if values["query_config_revision"] != args.query_config_revision:
        reject("receipt query config revision does not match")
    if not DIGEST_RE.fullmatch(values["query_config_digest"]) or values["query_config_digest"] != args.query_config_digest:
        reject("receipt query config digest does not match")

    run = read_json(args.run_json)
    if not isinstance(run, dict):
        reject("DEV run provenance must be an object")
    if run.get("id") != args.expected_run_id:
        reject("DEV run provenance ID does not match")
    expected = {
        "path": WORKFLOW_PATH,
        "event": args.expected_event,
        "head_branch": args.expected_branch,
        "head_sha": args.expected_sha,
        "conclusion": "success",
    }
    for key, value in expected.items():
        if run.get(key) != value:
            reject(f"DEV run provenance {key} is invalid")
    run_url = run.get("html_url")
    if not isinstance(run_url, str) or not run_url.startswith("https://"):
        reject("DEV run provenance URL is invalid")

    normalized = {
        "schema_version": PROMOTION_SCHEMA_VERSION,
        "receipt_schema_version": RECEIPT_SCHEMA_VERSION,
        "component": args.component,
        "result": "ready",
        "source_sha": args.expected_sha,
        "dev_run_id": args.expected_run_id,
        "dev_run_url": run_url,
        "image_digest": values["image_digest"],
        "image_reference": values["image_reference"],
    }
    write_json(args.output, normalized)
    if args.github_output:
        with Path(args.github_output).open("a", encoding="utf-8") as output:
            for key, value in (
                ("source_sha", args.expected_sha),
                ("dev_run_id", str(args.expected_run_id)),
                ("dev_run_url", run_url),
                ("dev_run_event", args.expected_event),
                ("dev_run_head_branch", args.expected_branch),
                ("dev_run_head_sha", args.expected_sha),
                ("dev_run_conclusion", "success"),
                ("digest", values["image_digest"]),
                ("image", values["image_reference"]),
                ("query_config_revision", values["query_config_revision"]),
                ("query_config_digest", values["query_config_digest"]),
            ):
                output.write(f"{key}={value}\n")


def path_value(document, path):
    value = document
    for part in path.split(".") if path else ():
        if not isinstance(value, dict) or part not in value:
            reject(f"traffic field path is missing: {path}")
        value = value[part]
    return value


def validate_traffic_entries(entries, recognized_revisions):
    if not isinstance(entries, list) or len(entries) != 1:
        reject("rollback traffic must have exactly one target")
    if not recognized_revisions:
        reject("rollback traffic has no recognized current immutable revision")
    entry = entries[0]
    if not isinstance(entry, dict):
        reject("rollback traffic target is malformed")
    if "revisionName" in entry or "latestRevision" in entry:
        revision_key, latest_key = "revisionName", "latestRevision"
    elif "revision_name" in entry or "latest_revision" in entry:
        revision_key, latest_key = "revision_name", "latest_revision"
    else:
        reject("rollback traffic target has no revision identity")
    allowed_explicit = {revision_key, "percent"}
    allowed_latest = allowed_explicit | {latest_key}
    if "tag" in entry or set(entry) not in (allowed_explicit, allowed_latest):
        reject("rollback traffic target is tagged, mixed, or contains unknown fields")
    revision = entry.get(revision_key)
    if not isinstance(revision, str) or not REVISION_RE.fullmatch(revision) or revision not in recognized_revisions:
        reject("rollback traffic revision is unresolved or not an immutable recognized revision")
    percent = entry.get("percent")
    if isinstance(percent, bool) or percent != 100:
        reject("rollback traffic must route exactly 100 percent")
    if latest_key in entry and entry[latest_key] is not True:
        reject("rollback latestRevision must be canonical true")
    return {
        "revision_name": revision,
        "percent": 100,
        "latest_revision": latest_key in entry,
    }


def validate_traffic(args):
    document = read_json(args.traffic_file)
    entries = path_value(document, args.traffic_path)
    validate_traffic_entries(entries, set(args.recognized_revision))


def write_json(path, value):
    try:
        Path(path).write_text(json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    except OSError as error:
        reject(f"output is unwritable: {error.__class__.__name__}")


def parser():
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="mode", required=True)
    receipt = subparsers.add_parser("validate-dev-receipt")
    receipt.add_argument("--receipt", required=True)
    receipt.add_argument("--run-json", required=True)
    receipt.add_argument("--expected-sha", required=True)
    receipt.add_argument("--expected-run-id", required=True, type=int)
    receipt.add_argument("--expected-branch", required=True)
    receipt.add_argument("--expected-event", required=True)
    receipt.add_argument("--component", required=True)
    receipt.add_argument("--ar-repo", required=True)
    receipt.add_argument("--query-config-revision", required=True)
    receipt.add_argument("--query-config-digest", required=True)
    receipt.add_argument("--output", required=True)
    receipt.add_argument("--github-output")
    traffic = subparsers.add_parser("validate-traffic")
    traffic.add_argument("--traffic-file", required=True)
    traffic.add_argument("--traffic-path", required=True)
    traffic.add_argument("--recognized-revision", action="append", default=[])
    return parser


def main(argv=None):
    args = parser().parse_args(argv)
    try:
        if args.mode == "validate-dev-receipt":
            if args.expected_run_id <= 0:
                reject("expected DEV run ID is invalid")
            validate_dev_receipt(args)
        else:
            validate_traffic(args)
    except ContractError as error:
        print(f"promotion contract rejected: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
