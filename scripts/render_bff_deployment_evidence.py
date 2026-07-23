#!/usr/bin/env python3
"""Validate BFF Cloud Run read-back and render one nonsecret evidence document."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit


PROJECT = "llm-wiki-cloud"
REGION = "asia-east1"
SERVICE = "llm-wiki-bff"
JOB = "olw-pipeline"
AR_REPO = "asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images"
RUNTIME_SERVICE_ACCOUNT = "lwc-bff-prod@llm-wiki-cloud.iam.gserviceaccount.com"
EXPECTED_SCHEMA = 1
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
ARTIFACT_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
CLASS_UNKNOWN = "unknown"
CLASS_FAILED = "failed"
REASONS = {
    "input_invalid": CLASS_UNKNOWN,
    "provider_command_unavailable": CLASS_UNKNOWN,
    "provider_command_failed": CLASS_UNKNOWN,
    "provider_readback_unparseable": CLASS_UNKNOWN,
    "rollback_unavailable": CLASS_UNKNOWN,
    "rollback_race": CLASS_UNKNOWN,
    "provider_shape_unsupported": CLASS_FAILED,
    "provider_handle_mismatch": CLASS_FAILED,
    "revision_mismatch": CLASS_FAILED,
    "image_mismatch": CLASS_FAILED,
    "runtime_service_account_mismatch": CLASS_FAILED,
    "config_mismatch": CLASS_FAILED,
    "traffic_mismatch": CLASS_FAILED,
    "iam_binding_missing": CLASS_FAILED,
    "identity_unavailable": CLASS_UNKNOWN,
    "identity_status_mismatch": CLASS_FAILED,
    "identity_header_mismatch": CLASS_FAILED,
    "identity_body_mismatch": CLASS_FAILED,
    "evidence_output_exists": CLASS_UNKNOWN,
}


class EvidenceError(Exception):
    def __init__(self, message, reason_code="input_invalid"):
        if reason_code not in REASONS:
            raise ValueError("unallowlisted evidence reason code")
        super().__init__(message)
        self.reason_code = reason_code
        self.classification = REASONS[reason_code]


def reject(message, reason_code="input_invalid"):
    raise EvidenceError(message, reason_code)


def read_json(path):
    try:
        with Path(path).open(encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, json.JSONDecodeError) as error:
        reject(f"cannot read JSON input {Path(path).name}: {error.__class__.__name__}")
    if not isinstance(value, dict):
        reject("JSON input must be an object")
    return value


def provider(command, reason_code="provider_command_failed"):
    try:
        result = subprocess.run(command, check=False, capture_output=True, text=True)
    except OSError:
        reject("provider command unavailable", "provider_command_unavailable")
    if result.returncode != 0:
        reject("provider command failed", reason_code)
    return result.stdout


def provider_json(command):
    output = provider(command)
    try:
        value = json.loads(output)
    except json.JSONDecodeError:
        reject("provider read-back was not JSON", "provider_readback_unparseable")
    if not isinstance(value, dict):
        reject("provider read-back is not an object", "provider_shape_unsupported")
    return value


def gcloud_json(*args):
    return provider(["gcloud", *args, "--format=json", "--quiet"])


def service_json(args):
    return provider_json(["gcloud", "run", "services", "describe", args.service_name, "--project", args.project, "--region", args.region, "--format=json", "--quiet"])


def revision_json(args, revision):
    return provider_json(["gcloud", "run", "revisions", "describe", revision, "--project", args.project, "--region", args.region, "--format=json", "--quiet"])


def iam_json(args, kind, name):
    return provider_json(["gcloud", "run", kind, "get-iam-policy", name, "--project", args.project, "--region", args.region, "--format=json", "--quiet"])


def now():
    return datetime.now(timezone.utc).replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


def positive_int(value, label):
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        reject(f"{label} is invalid", "provider_shape_unsupported")
    return value


def handle(args):
    return f"projects/{args.project}/locations/{args.region}/services/{args.service_name}"


def annotations(service):
    metadata = service.get("metadata")
    if not isinstance(metadata, dict) or not isinstance(metadata.get("name"), str):
        reject("service metadata shape is unsupported", "provider_shape_unsupported")
    if not isinstance(metadata.get("annotations", {}), dict):
        reject("service annotations shape is unsupported", "provider_shape_unsupported")
    template = service.get("spec", {}).get("template", {})
    if not isinstance(template, dict) or not isinstance(template.get("metadata", {}).get("annotations", {}), dict):
        reject("service template annotations are missing", "provider_shape_unsupported")
    return metadata, metadata["annotations"], template["metadata"]["annotations"], template.get("spec")


def service_parts(service, args):
    metadata, service_annotations, template_annotations, spec = annotations(service)
    if metadata["name"] != args.service_name:
        reject("service handle does not match", "provider_handle_mismatch")
    if not isinstance(spec, dict):
        reject("service template spec is unsupported", "provider_shape_unsupported")
    containers = spec.get("containers")
    if not isinstance(containers, list) or len(containers) != 1 or not isinstance(containers[0], dict):
        reject("expected exactly one service container", "provider_shape_unsupported")
    container = containers[0]
    if not isinstance(container.get("image"), str) or not isinstance(container.get("env"), list):
        reject("service container shape is unsupported", "provider_shape_unsupported")
    service_account = spec.get("serviceAccountName")
    if not isinstance(service_account, str):
        reject("service runtime identity is missing", "provider_shape_unsupported")
    status = service.get("status")
    if not isinstance(status, dict) or not isinstance(status.get("latestReadyRevisionName"), str) or not isinstance(status.get("traffic"), list):
        reject("service readiness or traffic is missing", "provider_shape_unsupported")
    return metadata, service_annotations, template_annotations, spec, container, service_account, status


def normalized_traffic(traffic):
    result = []
    for entry in traffic:
        if isinstance(entry, dict) and "revision_name" in entry:
            entry = {"revisionName": entry.get("revision_name"), "percent": entry.get("percent"), **({"tag": entry["tag"]} if "tag" in entry else {})}
        if not isinstance(entry, dict) or set(entry) - {"revisionName", "percent", "tag"}:
            reject("traffic snapshot shape is unsupported", "provider_shape_unsupported")
        revision = entry.get("revisionName")
        percent = entry.get("percent")
        if not isinstance(revision, str) or not revision or isinstance(percent, bool) or not isinstance(percent, int) or percent < 0 or percent > 100:
            reject("traffic snapshot value is invalid", "provider_shape_unsupported")
        clean = {"revision_name": revision, "percent": percent}
        if "tag" in entry:
            if not isinstance(entry["tag"], str):
                reject("traffic tag is invalid", "provider_shape_unsupported")
            clean["tag"] = entry["tag"]
        result.append(clean)
    if not result or sum(item["percent"] for item in result) != 100:
        reject("traffic snapshot is not coherent", "rollback_race")
    return result


EXPECTED_VALUES = {
    "GCP_PROJECT": PROJECT,
    "BUCKET": "llm-wiki-data",
    "FIRESTORE_DATABASE_ID": "llm-wiki-cloud-prod",
    "PIPELINE_JOB_URL": "https://run.googleapis.com/v2/projects/llm-wiki-cloud/locations/asia-east1/jobs/olw-pipeline:run",
    "ALLOWED_ORIGINS": "https://wiki.rayer.idv.tw,https://llm-wiki-frontend.vercel.app",
    "DEV_JWT": "false",
}
EXPECTED_SECRETS = {
    "JWT_SECRET": {"secret": "jwt-secret-prod", "version": "latest"},
    "DEEPSEEK_API_KEY": {"secret": "deepseek-apikey", "version": "latest"},
}


def normalized_env(env):
    values = {}
    secrets = {}
    for entry in env:
        if not isinstance(entry, dict) or set(entry) - {"name", "value", "valueSource"} or not isinstance(entry.get("name"), str):
            reject("environment entry shape is unsupported", "config_mismatch")
        name = entry["name"]
        if name in values or name in secrets:
            reject("duplicate environment entry", "config_mismatch")
        if name in EXPECTED_VALUES:
            if set(entry) != {"name", "value"} or not isinstance(entry["value"], str) or entry["value"] != EXPECTED_VALUES[name]:
                reject("environment value does not match production contract", "config_mismatch")
            values[name] = entry["value"]
        elif name in EXPECTED_SECRETS:
            source = entry.get("valueSource")
            ref = source.get("secretKeyRef") if isinstance(source, dict) else None
            if set(entry) != {"name", "valueSource"} or not isinstance(ref, dict) or set(ref) != {"secret", "version"} or ref != EXPECTED_SECRETS[name]:
                reject("secret reference does not match production contract", "config_mismatch")
            secrets[name] = {"secret": ref["secret"], "version": ref["version"]}
        else:
            reject("environment is not allowlisted", "config_mismatch")
    if set(values) != set(EXPECTED_VALUES) or set(secrets) != set(EXPECTED_SECRETS):
        reject("production environment is incomplete", "config_mismatch")
    return {
        "env": [{"name": key, "value": values[key]} for key in sorted(values)],
        "secret_references": [{"name": key, **secrets[key]} for key in sorted(secrets)],
    }


def network_config(service_annotations, template_annotations):
    if service_annotations.get("run.googleapis.com/ingress") != "all":
        reject("service ingress does not match production", "config_mismatch")
    if template_annotations.get("run.googleapis.com/vpc-access-egress") != "private-ranges-only":
        reject("service VPC egress does not match production", "config_mismatch")
    try:
        interfaces = json.loads(template_annotations.get("run.googleapis.com/network-interfaces", ""))
    except (TypeError, json.JSONDecodeError):
        reject("service network configuration is invalid", "config_mismatch")
    if interfaces != [{"network": "default", "subnetwork": "default"}]:
        reject("service network does not match production", "config_mismatch")
    return {"network": "default", "subnet": "default", "vpc_egress": "private-ranges-only", "ingress": "all"}


def normalized_config(parts):
    _, service_annotations, template_annotations, spec, container, service_account, _ = parts
    if service_account != RUNTIME_SERVICE_ACCOUNT:
        reject("runtime service account does not match production", "runtime_service_account_mismatch")
    env = normalized_env(container["env"])
    config = {**env, "network": network_config(service_annotations, template_annotations), "runtime_service_account": service_account}
    return config


def validate_saved_config(config):
    expected = {"env", "secret_references", "network", "runtime_service_account"}
    if not isinstance(config, dict) or set(config) != expected:
        reject("rollback config contains unsupported fields", "rollback_unavailable")
    env = config.get("env")
    refs = config.get("secret_references")
    if not isinstance(env, list) or not isinstance(refs, list):
        reject("rollback config collections are invalid", "rollback_unavailable")
    reconstructed = list(env)
    for ref in refs:
        if not isinstance(ref, dict) or set(ref) != {"name", "secret", "version"}:
            reject("rollback secret reference shape is invalid", "rollback_unavailable")
        reconstructed.append({"name": ref["name"], "valueSource": {"secretKeyRef": {"secret": ref["secret"], "version": ref["version"]}}})
    normalized = normalized_env(reconstructed)
    if config.get("network") != {"network": "default", "subnet": "default", "vpc_egress": "private-ranges-only", "ingress": "all"} or config.get("runtime_service_account") != RUNTIME_SERVICE_ACCOUNT:
        reject("rollback network or identity config is invalid", "rollback_unavailable")
    return {**normalized, "network": config["network"], "runtime_service_account": config["runtime_service_account"]}


def revision_parts(document, expected_name, expected_image=None):
    metadata = document.get("metadata")
    spec = document.get("spec")
    status = document.get("status")
    if not isinstance(metadata, dict) or metadata.get("name") != expected_name or not isinstance(spec, dict) or not isinstance(status, dict):
        reject("revision identity shape does not match", "revision_mismatch")
    containers = spec.get("containers")
    if not isinstance(containers, list) or len(containers) != 1 or not isinstance(containers[0], dict):
        reject("revision container shape is unsupported", "provider_shape_unsupported")
    container = containers[0]
    image = container.get("image")
    digest = status.get("imageDigest")
    if not isinstance(image, str) or not isinstance(digest, str) or not DIGEST_RE.fullmatch(digest):
        reject("revision image digest is not immutable", "image_mismatch")
    if not image.startswith(AR_REPO + "/llm-wiki-bff@") or image.rsplit("@", 1)[-1] != digest:
        reject("revision image is not the expected immutable reference", "image_mismatch")
    if expected_image is not None and image != expected_image:
        reject("revision image does not match promoted image", "image_mismatch")
    if spec.get("serviceAccountName") != RUNTIME_SERVICE_ACCOUNT:
        reject("revision runtime service account does not match", "runtime_service_account_mismatch")
    if not isinstance(container.get("env"), list):
        reject("revision environment is unsupported", "provider_shape_unsupported")
    return image, digest


def binding(policy, role, member):
    bindings = policy.get("bindings") if isinstance(policy, dict) else None
    if not isinstance(bindings, list):
        reject("IAM policy shape is unsupported", "iam_binding_missing")
    for item in bindings:
        if isinstance(item, dict) and item.get("role") == role and isinstance(item.get("members"), list) and member in item["members"]:
            return True
    reject("required IAM binding is missing", "iam_binding_missing")


def immutable_image(image):
    if not isinstance(image, str) or not image.startswith(AR_REPO + "/llm-wiki-bff@") or not DIGEST_RE.fullmatch(image.rsplit("@", 1)[-1]):
        reject("rollback image is not immutable", "rollback_unavailable")
    return image


def prepare_rollback(args):
    remove_output(args.output)
    first = service_json(args)
    parts = service_parts(first, args)
    ready = parts[-1]["latestReadyRevisionName"]
    traffic = normalized_traffic(parts[-1]["traffic"])
    prior = revision_json(args, ready)
    prior_image, prior_digest = revision_parts(prior, ready)
    second = service_json(args)
    second_parts = service_parts(second, args)
    if second_parts[-1]["latestReadyRevisionName"] != ready or normalized_traffic(second_parts[-1]["traffic"]) != traffic:
        reject("service changed while freezing rollback", "rollback_race")
    rollback = {"provider_handle": handle(args), "artifact_name": args.artifact_name, "ready_revision": ready, "image_reference": immutable_image(prior_image), "image_digest": prior_digest, "traffic": traffic, "config": normalized_config(parts)}
    write_json(args.output, rollback)


def validate_metadata(metadata, args):
    if set(metadata) != {"schema_version", "project", "component", "environment", "action", "rollback_artifact_name", "source", "dev_provenance", "image", "originating_workflow"}:
        reject("metadata contains unsupported fields")
    if metadata.get("schema_version") != EXPECTED_SCHEMA or metadata.get("project") != args.project or metadata.get("component") != "lwc-bff" or metadata.get("environment") != "production" or metadata.get("action") != "promote":
        reject("metadata identity is invalid")
    artifact = metadata.get("rollback_artifact_name")
    if not isinstance(artifact, str) or not ARTIFACT_RE.fullmatch(artifact):
        reject("evidence artifact name is invalid")
    source = metadata.get("source")
    if not isinstance(source, dict) or set(source) != {"commit_sha", "ref"} or not isinstance(source.get("commit_sha"), str) or not SHA_RE.fullmatch(source["commit_sha"]) or source["ref"] != "refs/heads/main":
        reject("source identity is invalid")
    provenance = metadata.get("dev_provenance")
    required_provenance = {"workflow", "event", "head_branch", "head_sha", "conclusion", "run_id", "run_url"}
    if not isinstance(provenance, dict) or set(provenance) != required_provenance or provenance.get("workflow") != "deploy-bff.yml" or provenance.get("event") != "push" or provenance.get("head_branch") != "main" or provenance.get("head_sha") != source["commit_sha"] or provenance.get("conclusion") != "success" or not isinstance(provenance.get("run_id"), int) or provenance["run_id"] <= 0 or not isinstance(provenance.get("run_url"), str) or not provenance["run_url"].startswith("https://"):
        reject("dev deployment provenance does not match source")
    image = metadata.get("image")
    if not isinstance(image, dict) or set(image) != {"digest", "reference"} or not isinstance(image.get("digest"), str) or not DIGEST_RE.fullmatch(image["digest"]) or image.get("reference") != AR_REPO + "/llm-wiki-bff@" + image["digest"]:
        reject("promoted image identity is invalid")
    origin = metadata.get("originating_workflow")
    if not isinstance(origin, dict) or set(origin) != {"repository", "workflow", "run_id", "run_attempt"} or not isinstance(origin.get("repository"), str) or not isinstance(origin.get("workflow"), str) or not isinstance(origin.get("run_id"), int) or origin["run_id"] <= 0 or not isinstance(origin.get("run_attempt"), int) or origin["run_attempt"] <= 0:
        reject("originating workflow identity is invalid")
    return source["commit_sha"], image["reference"], artifact


def validate_rollback(rollback, args):
    expected = {"provider_handle", "artifact_name", "ready_revision", "image_reference", "image_digest", "traffic", "config"}
    if not isinstance(rollback, dict) or set(rollback) != expected or rollback.get("provider_handle") != handle(args) or not isinstance(rollback.get("artifact_name"), str) or not ARTIFACT_RE.fullmatch(rollback["artifact_name"]) or not isinstance(rollback.get("ready_revision"), str) or not isinstance(rollback.get("image_digest"), str) or not DIGEST_RE.fullmatch(rollback["image_digest"]) or rollback.get("image_reference") != AR_REPO + "/llm-wiki-bff@" + rollback["image_digest"]:
        reject("rollback contract is invalid")
    traffic = normalized_traffic(rollback["traffic"])
    config = validate_saved_config(rollback["config"])
    return {**rollback, "traffic": traffic, "config": config}


def identity(args, url, commit, revision):
    parsed = urlsplit(url)
    if parsed.scheme != "https" or parsed.username or parsed.password or not parsed.netloc:
        reject("identity endpoint URL is invalid", "identity_unavailable")
    headers_path = tempfile.mktemp(prefix="bff-identity-headers-")
    body_path = tempfile.mktemp(prefix="bff-identity-body-")
    try:
        command = ["curl", "--silent", "--show-error", "--fail-with-body", "--max-time", "20", "-D", headers_path, "-o", body_path, url.rstrip("/") + "/api/v1/public/version"]
        try:
            result = subprocess.run(command, check=False, capture_output=True, text=True)
        except OSError:
            reject("identity endpoint unavailable", "identity_unavailable")
        if result.returncode != 0:
            reject("identity endpoint unavailable", "identity_unavailable")
        try:
            headers = Path(headers_path).read_text(encoding="utf-8").splitlines()
            body = json.loads(Path(body_path).read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            reject("identity response is unreadable", "identity_status_mismatch")
        if not headers or not re.match(r"^HTTP/\S+ 200(?:\s|$)", headers[0]):
            reject("identity endpoint did not return HTTP 200", "identity_status_mismatch")
        parsed_headers = {}
        for line in headers[1:]:
            if ":" in line:
                key, value = line.split(":", 1)
                parsed_headers[key.strip().lower()] = value.strip()
        if parsed_headers.get("cache-control") != "no-store":
            reject("identity Cache-Control is not no-store", "identity_header_mismatch")
        required = {"product_version", "commit", "branch", "tag", "image_tag", "service", "revision"}
        if not isinstance(body, dict) or set(body) != required or body.get("commit") != commit or body.get("branch") != "main" or body.get("image_tag") != commit or body.get("service") != args.service_name or body.get("revision") != revision or not all(isinstance(body.get(key), str) for key in required):
            reject("identity body does not match expected structured identity", "identity_body_mismatch")
        return {"commit": body["commit"], "branch": body["branch"], "image_tag": body["image_tag"], "service": body["service"], "revision": body["revision"]}
    finally:
        for path in (headers_path, body_path):
            try:
                Path(path).unlink()
            except OSError:
                pass


def render_strict(args):
    if Path(args.output).exists():
        reject("deployment evidence already exists", "evidence_output_exists")
    metadata = read_json(args.metadata)
    rollback = validate_rollback(read_json(args.rollback_contract), args)
    commit, expected_image, artifact = validate_metadata(metadata, args)
    if artifact != rollback["artifact_name"]:
        reject("rollback artifact identity does not match metadata")
    first = service_json(args)
    parts = service_parts(first, args)
    ready = parts[-1]["latestReadyRevisionName"]
    if not ready:
        reject("new ready revision is missing", "revision_mismatch")
    revision = revision_json(args, ready)
    observed_image, observed_digest = revision_parts(revision, ready, expected_image)
    config = normalized_config(parts)
    service_policy = iam_json(args, "services", args.service_name)
    binding(service_policy, "roles/run.invoker", "allUsers")
    job_policy = iam_json(args, "jobs", args.pipeline_job_name)
    binding(job_policy, "roles/run.jobsExecutorWithOverrides", "serviceAccount:" + RUNTIME_SERVICE_ACCOUNT)
    second = service_json(args)
    second_parts = service_parts(second, args)
    traffic = normalized_traffic(second_parts[-1]["traffic"])
    if second_parts[-1]["latestReadyRevisionName"] != ready:
        reject("new ready revision changed during read-back", "revision_mismatch")
    if len(traffic) != 1 or traffic[0]["revision_name"] != ready or traffic[0]["percent"] != 100:
        reject("effective traffic is not 100 percent on the new revision", "traffic_mismatch")
    url = args.service_url or parts[-1].get("url")
    if not isinstance(url, str):
        reject("service URL is unavailable", "identity_unavailable")
    identity_result = identity(args, url, commit, ready)
    checked = now()
    return {
        "schema_version": EXPECTED_SCHEMA,
        "project": metadata["project"],
        "component": metadata["component"],
        "environment": metadata["environment"],
        "action": metadata["action"],
        "source": metadata["source"],
        "dev_provenance": metadata["dev_provenance"],
        "image": metadata["image"],
        "provider": {"current_handle": handle(args), "rollback_handle": rollback["provider_handle"], "rollback_artifact_name": artifact},
        "observed_service": {"ready_revision": ready, "image_reference": observed_image, "image_digest": observed_digest, "runtime_service_account": RUNTIME_SERVICE_ACCOUNT, "traffic": traffic},
        "config": {"result": "verified", "fingerprint": "sha256:" + hashlib.sha256(json.dumps(config, sort_keys=True, separators=(",", ":")).encode()).hexdigest(), "allowlisted": config},
        "provider_verification": {"result": "verified", "checked_at": checked, "checks": ["provider_handle", "ready_revision", "image", "runtime_service_account", "network", "traffic", "service_invoker_iam", "pipeline_executor_iam"]},
        "originating_workflow": metadata["originating_workflow"],
        "rollback": rollback,
        "health": {"result": "verified", "checked_at": checked, "identity": identity_result},
        "status": "HEALTHY",
        "reason": None,
        "next_action": "none",
    }


def failure_marker(error):
    return {"classification": error.classification, "reason_code": error.reason_code, "checked_at": now()}


def read_failure(path):
    try:
        marker = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    if not isinstance(marker, dict) or marker.get("classification") not in {CLASS_UNKNOWN, CLASS_FAILED} or marker.get("reason_code") not in REASONS or not isinstance(marker.get("checked_at"), str):
        return None
    return marker


def partial(args):
    if Path(args.output).exists():
        reject("refusing to overwrite existing deployment evidence", "evidence_output_exists")
    metadata = read_json(args.metadata)
    rollback = validate_rollback(read_json(args.rollback_contract), args)
    commit, _, artifact = validate_metadata(metadata, args)
    if artifact != rollback["artifact_name"]:
        reject("rollback artifact identity does not match metadata")
    marker = read_failure(args.failure_output) if args.failure_output else None
    failed = marker is not None and marker["classification"] == CLASS_FAILED
    status = "UNHEALTHY" if failed else "PARTIAL"
    result = "failed" if failed else "unknown"
    reason = marker["reason_code"] if marker else "deployment_outcome_unknown"
    checked = marker["checked_at"] if failed else None
    return {
        "schema_version": EXPECTED_SCHEMA, "project": metadata["project"], "component": metadata["component"], "environment": metadata["environment"], "action": metadata["action"], "source": metadata["source"], "dev_provenance": metadata["dev_provenance"], "image": metadata["image"], "provider": {"current_handle": handle(args), "rollback_handle": rollback["provider_handle"], "rollback_artifact_name": artifact}, "observed_service": None, "config": {"result": result, "fingerprint": None, "allowlisted": None}, "provider_verification": {"result": result, "reason_code": reason if failed else None, "checked_at": checked, "checks": []}, "originating_workflow": metadata["originating_workflow"], "rollback": rollback, "health": {"result": result, "checked_at": checked, "identity": None}, "status": status, "reason": reason, "next_action": "independent provider read-back required before retry or rollback; do not tag the image",
    }


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n"
    fd, temporary = tempfile.mkstemp(prefix=".bff-deployment-evidence-", dir=path.parent)
    try:
        with open(fd, "w", encoding="utf-8") as output:
            output.write(payload)
            output.flush()
        Path(temporary).replace(path)
    except BaseException:
        try:
            Path(temporary).unlink()
        except OSError:
            pass
        raise


def remove_output(path):
    try:
        Path(path).unlink()
    except FileNotFoundError:
        pass
    except OSError as error:
        reject(f"cannot prepare output: {error.__class__.__name__}")


def parser():
    p = argparse.ArgumentParser()
    subs = p.add_subparsers(dest="mode", required=True)
    for mode in ("prepare-rollback", "render-evidence", "render-partial"):
        child = subs.add_parser(mode)
        child.add_argument("--project", required=True)
        child.add_argument("--region", required=True)
        child.add_argument("--service-name", required=True)
        child.add_argument("--pipeline-job-name", required=True)
        child.add_argument("--ar-repo", required=True)
        if mode == "prepare-rollback":
            child.add_argument("--artifact-name", required=True)
            child.add_argument("--output", required=True)
        else:
            child.add_argument("--rollback-contract", required=True)
            child.add_argument("--metadata", required=True)
            child.add_argument("--output", required=True)
            child.add_argument("--failure-output")
            if mode == "render-evidence":
                child.add_argument("--expected-runtime-service-account", required=True)
                child.add_argument("--service-url")
    validate = subs.add_parser("validate-metadata")
    validate.add_argument("--project", required=True)
    validate.add_argument("--region", required=True)
    validate.add_argument("--service-name", required=True)
    validate.add_argument("--pipeline-job-name", required=True)
    validate.add_argument("--ar-repo", required=True)
    validate.add_argument("--metadata", required=True)
    return p


def main(argv=None):
    args = parser().parse_args(argv)
    if args.project != PROJECT or args.region != REGION or args.service_name != SERVICE or args.pipeline_job_name != JOB or args.ar_repo != AR_REPO:
        print("deployment evidence rejected: input_invalid", file=sys.stderr)
        return 1
    if getattr(args, "expected_runtime_service_account", RUNTIME_SERVICE_ACCOUNT) != RUNTIME_SERVICE_ACCOUNT:
        print("deployment evidence rejected: input_invalid", file=sys.stderr)
        return 1
    try:
        if args.mode == "prepare-rollback":
            prepare_rollback(args)
        elif args.mode == "render-evidence":
            remove_output(args.failure_output)
            try:
                write_json(args.output, render_strict(args))
            except EvidenceError as error:
                write_json(args.failure_output, failure_marker(error))
                raise
        elif args.mode == "validate-metadata":
            validate_metadata(read_json(args.metadata), args)
        else:
            write_json(args.output, partial(args))
    except EvidenceError as error:
        print(f"deployment evidence rejected: {error.reason_code}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
