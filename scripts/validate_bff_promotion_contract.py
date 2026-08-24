#!/usr/bin/env python3
"""Validate the production-owned BFF promotion receipt and traffic contracts."""

import argparse
import json
import os
from pathlib import Path
import re
import sys
import tempfile
from urllib.parse import urlsplit


RECEIPT_SCHEMA_VERSION = 3
PROMOTION_SCHEMA_VERSION = 1
IMAGE_NAME = "llm-wiki-bff"
WORKFLOW_PATH = ".github/workflows/deploy-bff.yml"
REQUIRED_RUN_JOBS = (
    "production-promotion-ready",
    "main-fast-forward-eligible",
)
CANONICAL_CI_PATH = ".github/workflows/ci.yml"
CANONICAL_CI_EVENT = "push"
CANONICAL_CI_REF = "develop"
CANONICAL_CI_JOB = "test"
ACTIONS_PAGE_ITEM_KEYS = ("workflow_runs", "jobs")
RECEIPT_KEYS = (
    "receipt_schema_version",
    "component",
    "build_ref",
    "source_sha",
    "dev_run_id",
    "ci_run_id",
    "ci_run_attempt",
    "image_digest",
    "image_reference",
    "query_config_revision",
    "query_config_digest",
)
SHA_RE = re.compile(r"[0-9a-f]{40}\Z")
DIGEST_RE = re.compile(r"sha256:[0-9a-f]{64}\Z")
RUN_ID_RE = re.compile(r"[1-9][0-9]*\Z")
REVISION_RE = re.compile(r"[a-z0-9][a-z0-9-]{0,62}\Z")
RUN_URL_RE = re.compile(r"https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/actions/runs/[1-9][0-9]*\Z")


class ContractError(Exception):
    pass


def reject(message):
    raise ContractError(message)


def _reject_non_finite_constant(constant):
    reject(f"invalid JSON constant: {constant}")


def read_json(path):
    try:
        raw = sys.stdin.read() if path == "-" else Path(path).read_text(encoding="utf-8")
        value = json.loads(raw, object_pairs_hook=_object_pairs, parse_constant=_reject_non_finite_constant)
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
        reject(f"receipt must use exactly {len(RECEIPT_KEYS)} LF-terminated lines")
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


def validate_run_jobs(args):
    jobs = read_json(args.jobs_json)
    if not isinstance(jobs, list):
        reject("DEV run jobs evidence must be an array")
    matches = {name: [] for name in REQUIRED_RUN_JOBS}
    for job in jobs:
        if not isinstance(job, dict):
            reject("DEV run job evidence is malformed")
        name = job.get("name")
        if name in matches:
            matches[name].append(job)
    for name, evidence in matches.items():
        if len(evidence) != 1:
            reject(f"DEV run must contain exactly one {name} job")
        job = evidence[0]
        if job.get("run_id") != args.expected_run_id:
            reject(f"{name} job belongs to a different run")
        if job.get("status") != "completed" or job.get("conclusion") != "success":
            reject(f"{name} job did not conclude successfully")


def _positive_int(value, message):
    if type(value) is not int or value <= 0:
        reject(message)
    return value


def _non_negative_int(value, message):
    if type(value) is not int or value < 0:
        reject(message)
    return value


def validate_canonical_ci_run(args):
    if args.expected_path != CANONICAL_CI_PATH:
        reject("canonical CI workflow path is not accepted")
    if args.expected_event != CANONICAL_CI_EVENT:
        reject("canonical CI event is not accepted")
    if args.expected_ref != CANONICAL_CI_REF:
        reject("canonical CI ref is not accepted")
    if not SHA_RE.fullmatch(args.expected_sha):
        reject("canonical CI SHA is invalid")
    runs = read_json(args.runs_json)
    if not isinstance(runs, list):
        reject("canonical CI runs evidence must be an array")
    successful = []
    for run in runs:
        if not isinstance(run, dict):
            reject("canonical CI run evidence is malformed")
        if (
            run.get("path") != args.expected_path
            or run.get("event") != args.expected_event
            or run.get("head_branch") != args.expected_ref
            or run.get("head_sha") != args.expected_sha
        ):
            continue
        if run.get("status") == "completed" and run.get("conclusion") == "success":
            successful.append(run)
    if len(successful) != 1:
        reject("canonical CI must contain exactly one successful matching run")
    run_id = _positive_int(successful[0].get("id"), "canonical CI run ID is invalid")
    run_attempt = _positive_int(successful[0].get("run_attempt"), "canonical CI run attempt is invalid")
    destination = Path(args.output)
    if destination.exists():
        reject("canonical CI run ID output already exists")
    try:
        destination.write_text(f"run_id={run_id}\nrun_attempt={run_attempt}\n", encoding="ascii")
    except OSError as error:
        reject(f"output is unwritable: {error.__class__.__name__}")


def validate_canonical_ci_jobs(args):
    if args.required_job != CANONICAL_CI_JOB:
        reject("canonical CI required job is not accepted")
    jobs = read_json(args.jobs_json)
    if not isinstance(jobs, list):
        reject("canonical CI jobs evidence must be an array")
    matches = []
    for job in jobs:
        if not isinstance(job, dict):
            reject("canonical CI job evidence is malformed")
        if job.get("name") == args.required_job:
            matches.append(job)
    if len(matches) != 1:
        reject(f"canonical CI must contain exactly one {args.required_job} job")
    job = matches[0]
    if type(job.get("run_id")) is not int or job.get("run_id") != args.expected_run_id:
        reject("canonical CI job belongs to a different run")
    if type(job.get("run_attempt")) is not int or job.get("run_attempt") != args.expected_run_attempt:
        reject("canonical CI job belongs to a different run attempt")
    if job.get("status") != "completed" or job.get("conclusion") != "success":
        reject(f"{args.required_job} job did not conclude successfully")


def validate_canonical_ci_attempt(args):
    if args.expected_path != CANONICAL_CI_PATH:
        reject("canonical CI workflow path is not accepted")
    if args.expected_event != CANONICAL_CI_EVENT:
        reject("canonical CI event is not accepted")
    if args.expected_ref != CANONICAL_CI_REF:
        reject("canonical CI ref is not accepted")
    if not SHA_RE.fullmatch(args.expected_sha):
        reject("canonical CI SHA is invalid")
    run = read_json(args.run_json)
    if not isinstance(run, dict):
        reject("canonical CI attempt evidence must be an object")
    if type(run.get("id")) is not int or run.get("id") != args.expected_run_id:
        reject("canonical CI attempt run ID does not match")
    if type(run.get("run_attempt")) is not int or run.get("run_attempt") != args.expected_run_attempt:
        reject("canonical CI attempt does not match the pinned attempt")
    if (
        run.get("path") != args.expected_path
        or run.get("event") != args.expected_event
        or run.get("head_branch") != args.expected_ref
        or run.get("head_sha") != args.expected_sha
    ):
        reject("canonical CI attempt identity does not match")
    if run.get("status") != "completed" or run.get("conclusion") != "success":
        reject("canonical CI attempt did not conclude successfully")


def normalize_actions_page(args):
    if args.items_key not in ACTIONS_PAGE_ITEM_KEYS:
        reject("actions page items key is not accepted")
    page = read_json(args.page_json)
    if not isinstance(page, dict):
        reject("actions page evidence must be an object")
    total = page.get("total_count")
    if type(total) is not int or total < 0:
        reject("actions page total_count is malformed")
    items = page.get(args.items_key)
    if not isinstance(items, list):
        reject("actions page items must be an array")
    for item in items:
        if not isinstance(item, dict):
            reject("actions page item is malformed")
    items_path = Path(args.items_output)
    metadata_path = Path(args.metadata_output)
    if items_path.exists() or metadata_path.exists():
        reject("actions page output already exists")
    write_json(args.items_output, items)
    try:
        metadata_path.write_text(f"total_count={total}\nitem_count={len(items)}\n", encoding="ascii")
    except OSError as error:
        reject(f"output is unwritable: {error.__class__.__name__}")


def extract_git_ref_sha(args):
    ref = read_json(args.ref_json)
    if not isinstance(ref, dict):
        reject("git ref evidence must be an object")
    obj = ref.get("object")
    if not isinstance(obj, dict):
        reject("git ref object is malformed")
    sha = obj.get("sha")
    if not isinstance(sha, str) or not SHA_RE.fullmatch(sha):
        reject("git ref SHA is invalid")
    destination = Path(args.output)
    if destination.exists():
        reject("git ref SHA output already exists")
    try:
        destination.write_text(sha + "\n", encoding="ascii")
    except OSError as error:
        reject(f"output is unwritable: {error.__class__.__name__}")


def validate_fast_forward_compare(args):
    compare = read_json(args.compare_json)
    if not isinstance(compare, dict):
        reject("compare evidence must be an object")
    status = compare.get("status")
    if status not in ("ahead", "identical"):
        reject("candidate is not a fast-forward of main")
    behind = compare.get("behind_by")
    if type(behind) is not int or behind != 0:
        reject("candidate is behind main or compare evidence is malformed")
    ahead = _non_negative_int(compare.get("ahead_by"), "compare ahead_by is malformed")
    if status == "identical" and ahead != 0:
        reject("identical compare must not be ahead")
    if status == "ahead" and ahead < 1:
        reject("ahead compare must have commits")


def validate_dev_receipt(args):
    values = read_receipt(args.receipt)
    if values["receipt_schema_version"] != str(RECEIPT_SCHEMA_VERSION):
        reject("receipt schema version is unsupported")
    if values["component"] != args.component:
        reject("receipt component does not match")
    if values["build_ref"] != args.expected_branch:
        reject("receipt build ref does not match")
    if not SHA_RE.fullmatch(values["source_sha"]) or values["source_sha"] != args.expected_sha:
        reject("receipt source SHA does not match")
    if not RUN_ID_RE.fullmatch(values["dev_run_id"]) or int(values["dev_run_id"]) != args.expected_run_id:
        reject("receipt DEV run ID does not match")
    if not RUN_ID_RE.fullmatch(values["ci_run_id"]):
        reject("receipt canonical CI run ID is invalid")
    if not RUN_ID_RE.fullmatch(values["ci_run_attempt"]):
        reject("receipt canonical CI run attempt is invalid")
    ci_run_id = int(values["ci_run_id"])
    ci_run_attempt = int(values["ci_run_attempt"])
    if args.expected_ci_run_id is not None and ci_run_id != args.expected_ci_run_id:
        reject("receipt canonical CI run ID does not match")
    if args.expected_ci_run_attempt is not None and ci_run_attempt != args.expected_ci_run_attempt:
        reject("receipt canonical CI run attempt does not match")
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
    }
    for key, value in expected.items():
        if run.get(key) != value:
            reject(f"DEV run provenance {key} is invalid")
    if args.lifecycle == "readiness":
        if run.get("status") != "in_progress" or run.get("conclusion") is not None:
            reject("readiness requires the same DEV run to be in progress without a conclusion")
        if args.producer_result != "success":
            reject("readiness requires a successful DEV producer dependency")
    elif run.get("status") != "completed" or run.get("conclusion") != "success":
        reject("production requires a completed successful DEV run")
    run_url = run.get("html_url")
    expected_run_url = f"https://github.com/{args.repository}/actions/runs/{args.expected_run_id}"
    if not isinstance(run_url, str) or not RUN_URL_RE.fullmatch(run_url) or run_url != expected_run_url:
        reject("DEV run provenance URL is invalid")

    normalized = {
        "schema_version": PROMOTION_SCHEMA_VERSION,
        "receipt_schema_version": RECEIPT_SCHEMA_VERSION,
        "component": args.component,
        "build_ref": values["build_ref"],
        "result": "ready",
        "source_sha": args.expected_sha,
        "dev_run_id": args.expected_run_id,
        "ci_run_id": ci_run_id,
        "ci_run_attempt": ci_run_attempt,
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
                ("build_ref", values["build_ref"]),
                ("dev_run_head_branch", args.expected_branch),
                ("dev_run_head_sha", args.expected_sha),
                ("dev_run_conclusion", "success"),
                ("digest", values["image_digest"]),
                ("image", values["image_reference"]),
                ("query_config_revision", values["query_config_revision"]),
                ("query_config_digest", values["query_config_digest"]),
            ):
                output.write(f"{key}={value}\n")


def validate_production_readiness(args):
    readiness = read_json(args.readiness)
    values = read_receipt(args.receipt)
    expected = {
        "schema_version": PROMOTION_SCHEMA_VERSION,
        "receipt_schema_version": RECEIPT_SCHEMA_VERSION,
        "component": args.component,
        "result": "ready",
        "build_ref": args.expected_branch,
        "source_sha": args.expected_sha,
        "dev_run_id": args.expected_run_id,
        "ci_run_id": int(values["ci_run_id"]),
        "ci_run_attempt": int(values["ci_run_attempt"]),
        "dev_run_url": f"https://github.com/{args.repository}/actions/runs/{args.expected_run_id}",
        "image_digest": values["image_digest"],
        "image_reference": f"{args.ar_repo}/{IMAGE_NAME}@{values['image_digest']}",
    }
    if readiness != expected:
        reject("normalized readiness receipt does not match the validated DEV receipt")


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
    if set(entry) & {"revision_name", "latest_revision"}:
        reject("provider traffic target uses the saved-artifact dialect")
    if set(entry) - {"revisionName", "latestRevision", "percent", "tag"}:
        reject("provider traffic target contains unknown fields")
    if "revisionName" not in entry:
        reject("provider traffic target has no explicit revision identity")
    revision = entry.get("revisionName")
    if not isinstance(revision, str) or not REVISION_RE.fullmatch(revision) or revision not in recognized_revisions:
        reject("rollback traffic revision is unresolved or not an immutable recognized revision")
    percent = entry.get("percent")
    if isinstance(percent, bool) or percent != 100:
        reject("rollback traffic must route exactly 100 percent")
    if "tag" in entry:
        reject("provider traffic target is tagged")
    if "latestRevision" in entry and not isinstance(entry["latestRevision"], bool):
        reject("provider latestRevision is invalid")
    return {
        "revision_name": revision,
        "percent": 100,
        "latest_revision": entry.get("latestRevision", False),
    }


PROVIDER_READABLE_TRAFFIC_KEYS = {"revisionName", "percent", "latestRevision", "tag", "url", "type"}
PROVIDER_READABLE_TRAFFIC_TYPES = {
    "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST",
    "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION",
}


def validate_provider_readable_entries(entries):
    if not isinstance(entries, list) or not entries:
        reject("provider traffic must be a non-empty array")
    total = 0
    for entry in entries:
        if not isinstance(entry, dict):
            reject("provider traffic entry is malformed")
        if set(entry) - PROVIDER_READABLE_TRAFFIC_KEYS:
            reject("provider traffic entry contains unknown fields")
        latest_revision = entry.get("latestRevision", False)
        if not isinstance(latest_revision, bool):
            reject("provider latestRevision is invalid")
        revision = entry.get("revisionName")
        if latest_revision:
            if revision is not None and (not isinstance(revision, str) or not REVISION_RE.fullmatch(revision)):
                reject("provider traffic entry has an invalid resolved revision identity")
        elif not isinstance(revision, str) or not REVISION_RE.fullmatch(revision):
            reject("provider traffic entry has no resolved revision identity")
        percent = entry.get("percent")
        if type(percent) is not int or not 0 <= percent <= 100:
            reject("provider traffic percent is invalid")
        total += percent
        if "tag" in entry and (not isinstance(entry["tag"], str) or not entry["tag"]):
            reject("provider traffic tag is invalid")
        if "type" in entry and (
            not isinstance(entry["type"], str) or entry["type"] not in PROVIDER_READABLE_TRAFFIC_TYPES
        ):
            reject("provider traffic type is invalid")
        if "url" in entry:
            if not isinstance(entry["url"], str):
                reject("provider traffic URL is invalid")
            try:
                parsed = urlsplit(entry["url"])
                hostname = parsed.hostname
            except ValueError:
                reject("provider traffic URL is invalid")
            if (
                parsed.scheme != "https"
                or not hostname
                or parsed.username is not None
                or parsed.password is not None
                or parsed.path
                or parsed.query
                or parsed.fragment
            ):
                reject("provider traffic URL is invalid")
    if total != 100:
        reject("provider traffic percentages must total 100")


def validate_traffic(args):
    document = read_json(args.traffic_file)
    recognized = set(args.recognized_revision)
    if args.traffic_mode == "artifact":
        entries = path_value(document, args.traffic_path)
        if not isinstance(entries, list) or len(entries) != 1:
            reject("saved rollback traffic must have exactly one target")
        entry = entries[0]
        if not isinstance(entry, dict) or set(entry) - {"revision_name", "latest_revision", "percent"}:
            reject("saved rollback traffic must use only canonical snake_case fields")
        if "revision_name" not in entry:
            reject("saved rollback traffic has no explicit revision identity")
        revision = entry["revision_name"]
        if not isinstance(revision, str) or not REVISION_RE.fullmatch(revision) or revision not in recognized:
            reject("saved rollback traffic revision is unresolved or unrecognized")
        if isinstance(entry.get("percent"), bool) or entry.get("percent") != 100:
            reject("saved rollback traffic must route exactly 100 percent")
        if "latest_revision" in entry and not isinstance(entry["latest_revision"], bool):
            reject("saved rollback latest_revision is invalid")
        return

    entries = path_value(document, args.traffic_path)
    if args.traffic_mode == "provider-dev-convergence":
        compare_entries = path_value(document, args.compare_path)
        status = validate_traffic_entries(entries, recognized)
        spec = validate_traffic_entries(compare_entries, recognized)
        if status != spec or status["latest_revision"]:
            reject("DEV status/spec traffic is not exact explicit convergence")
        return

    if args.traffic_mode == "provider-post-rollback":
        result = validate_traffic_entries(entries, recognized)
        if result["latest_revision"] or args.expected_revision not in recognized or result["revision_name"] != args.expected_revision:
            reject("restored provider traffic is not the frozen explicit revision")
        return

    result = validate_traffic_entries(entries, recognized)
    if result["latest_revision"]:
        reject("provider traffic target is implicit latest routing")


def write_json(path, value):
    try:
        destination = Path(path)
        destination.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(mode="w", dir=destination.parent, prefix=f".{destination.name}.", delete=False, encoding="utf-8") as output:
            temporary = Path(output.name)
            output.write(json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, destination)
    except OSError as error:
        reject(f"output is unwritable: {error.__class__.__name__}")
    finally:
        if "temporary" in locals() and temporary.exists():
            try:
                temporary.unlink()
            except OSError:
                pass


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
    receipt.add_argument("--lifecycle", choices=("readiness", "production"), required=True)
    receipt.add_argument("--producer-result")
    receipt.add_argument("--component", required=True)
    receipt.add_argument("--repository", required=True)
    receipt.add_argument("--ar-repo", required=True)
    receipt.add_argument("--query-config-revision", required=True)
    receipt.add_argument("--query-config-digest", required=True)
    receipt.add_argument("--expected-ci-run-id", type=int)
    receipt.add_argument("--expected-ci-run-attempt", type=int)
    receipt.add_argument("--output", required=True)
    receipt.add_argument("--github-output")
    readiness = subparsers.add_parser("validate-production-readiness")
    readiness.add_argument("--readiness", required=True)
    readiness.add_argument("--expected-sha", required=True)
    readiness.add_argument("--expected-run-id", required=True, type=int)
    readiness.add_argument("--expected-branch", required=True)
    readiness.add_argument("--component", required=True)
    readiness.add_argument("--repository", required=True)
    readiness.add_argument("--ar-repo", required=True)
    readiness.add_argument("--receipt", required=True)
    traffic = subparsers.add_parser("validate-traffic")
    traffic.add_argument("--traffic-file", required=True)
    traffic.add_argument("--traffic-path", required=True)
    traffic.add_argument("--traffic-mode", choices=("artifact", "provider-readable", "provider-pre-mutation", "provider-post-rollback", "provider-dev-convergence"), required=True)
    traffic.add_argument("--compare-path")
    traffic.add_argument("--expected-revision")
    traffic.add_argument("--recognized-revision", action="append", default=[])
    jobs = subparsers.add_parser("validate-run-jobs")
    jobs.add_argument("--jobs-json", required=True)
    jobs.add_argument("--expected-run-id", required=True, type=int)
    ci_run = subparsers.add_parser("validate-canonical-ci-run")
    ci_run.add_argument("--runs-json", required=True)
    ci_run.add_argument("--expected-sha", required=True)
    ci_run.add_argument("--expected-path", required=True)
    ci_run.add_argument("--expected-event", required=True)
    ci_run.add_argument("--expected-ref", required=True)
    ci_run.add_argument("--output", required=True)
    ci_jobs = subparsers.add_parser("validate-canonical-ci-jobs")
    ci_jobs.add_argument("--jobs-json", required=True)
    ci_jobs.add_argument("--expected-run-id", required=True, type=int)
    ci_jobs.add_argument("--expected-run-attempt", required=True, type=int)
    ci_jobs.add_argument("--required-job", required=True)
    ci_attempt = subparsers.add_parser("validate-canonical-ci-attempt")
    ci_attempt.add_argument("--run-json", required=True)
    ci_attempt.add_argument("--expected-sha", required=True)
    ci_attempt.add_argument("--expected-path", required=True)
    ci_attempt.add_argument("--expected-event", required=True)
    ci_attempt.add_argument("--expected-ref", required=True)
    ci_attempt.add_argument("--expected-run-id", required=True, type=int)
    ci_attempt.add_argument("--expected-run-attempt", required=True, type=int)
    page = subparsers.add_parser("normalize-actions-page")
    page.add_argument("--page-json", required=True)
    page.add_argument("--items-key", required=True)
    page.add_argument("--items-output", required=True)
    page.add_argument("--metadata-output", required=True)
    compare = subparsers.add_parser("validate-fast-forward-compare")
    compare.add_argument("--compare-json", required=True)
    git_ref = subparsers.add_parser("extract-git-ref-sha")
    git_ref.add_argument("--ref-json", required=True)
    git_ref.add_argument("--output", required=True)
    return parser


def main(argv=None):
    args = parser().parse_args(argv)
    try:
        if args.mode == "validate-dev-receipt":
            if args.expected_run_id <= 0:
                reject("expected DEV run ID is invalid")
            if args.expected_ci_run_id is not None and args.expected_ci_run_id <= 0:
                reject("expected canonical CI run ID is invalid")
            if args.expected_ci_run_attempt is not None and args.expected_ci_run_attempt <= 0:
                reject("expected canonical CI run attempt is invalid")
            validate_dev_receipt(args)
        elif args.mode == "validate-production-readiness":
            if args.expected_run_id <= 0 or not SHA_RE.fullmatch(args.expected_sha):
                reject("expected readiness identity is invalid")
            validate_production_readiness(args)
        elif args.mode == "validate-run-jobs":
            if args.expected_run_id <= 0:
                reject("expected DEV run ID is invalid")
            validate_run_jobs(args)
        elif args.mode == "validate-canonical-ci-run":
            validate_canonical_ci_run(args)
        elif args.mode == "validate-canonical-ci-jobs":
            if args.expected_run_id <= 0:
                reject("expected canonical CI run ID is invalid")
            if args.expected_run_attempt <= 0:
                reject("expected canonical CI run attempt is invalid")
            validate_canonical_ci_jobs(args)
        elif args.mode == "validate-canonical-ci-attempt":
            if args.expected_run_id <= 0:
                reject("expected canonical CI run ID is invalid")
            if args.expected_run_attempt <= 0:
                reject("expected canonical CI run attempt is invalid")
            validate_canonical_ci_attempt(args)
        elif args.mode == "normalize-actions-page":
            normalize_actions_page(args)
        elif args.mode == "validate-fast-forward-compare":
            validate_fast_forward_compare(args)
        elif args.mode == "extract-git-ref-sha":
            extract_git_ref_sha(args)
        else:
            if args.traffic_mode == "provider-readable":
                validate_provider_readable_entries(path_value(read_json(args.traffic_file), args.traffic_path))
                return 0
            if args.traffic_mode == "provider-dev-convergence" and not args.compare_path:
                reject("DEV convergence validation requires a comparison path")
            if args.traffic_mode == "provider-post-rollback" and not args.expected_revision:
                reject("post-rollback validation requires the frozen revision")
            validate_traffic(args)
    except ContractError as error:
        print(f"promotion contract rejected: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
