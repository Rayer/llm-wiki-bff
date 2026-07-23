#!/usr/bin/env python3
"""Validate the worker provider read-back and render one canonical evidence file."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


EXPECTED_COMPONENT = "olw-worker"
EXPECTED_ENVIRONMENT = "production"
EXPECTED_ACTION = "promote"
EXPECTED_SCHEMA_VERSION = 1
EXPECTED_ARGS = ["run", "[[\"run\",\"--auto-approve\"]]"]
LEGACY_ENV_NAMES = {"DATA_DIR", "WORKSPACE", "VAULT_PATH", "WORKSPACE_DIR"}
ALLOWLISTED_ENV_NAMES = LEGACY_ENV_NAMES | {"BUCKET"}
SECRET_WORDS = ("secret", "token", "password", "apikey", "privatekey", "valuefrom")
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
TAG_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")
ARTIFACT_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")


class EvidenceError(Exception):
    pass


def reject(message):
    raise EvidenceError(message)


def read_json(path):
    try:
        with Path(path).open(encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, json.JSONDecodeError) as error:
        reject(f"cannot read JSON input {Path(path).name}: {error.__class__.__name__}")
    if not isinstance(value, dict):
        reject(f"JSON input {Path(path).name} must be an object")
    return value


def run_provider(args):
    try:
        result = subprocess.run(args, check=False, capture_output=True, text=True)
    except OSError as error:
        reject(f"provider command unavailable: {args[0]} ({error.__class__.__name__})")
    if result.returncode != 0:
        reject(f"provider command failed: {' '.join(args[:4])}")
    return result.stdout


def provider_job(project, region, job_name):
    output = run_provider(
        [
            "gcloud",
            "run",
            "jobs",
            "describe",
            job_name,
            "--project",
            project,
            "--region",
            region,
            "--format=json",
            "--quiet",
        ]
    )
    try:
        value = json.loads(output)
    except json.JSONDecodeError:
        reject("provider job read-back was not JSON")
    if not isinstance(value, dict):
        reject("provider job read-back must be an object")
    return value


def provider_digest(project, region, image):
    output = run_provider(
        [
            "gcloud",
            "artifacts",
            "docker",
            "images",
            "describe",
            image,
            "--project",
            project,
            "--format=value(image_summary.digest)",
            "--quiet",
        ]
    )
    value = output.strip()
    if not DIGEST_RE.fullmatch(value):
        reject("provider rollback image resolution was not an immutable digest")
    return value


def job_spec(document):
    metadata = document.get("metadata")
    if not isinstance(metadata, dict) or not isinstance(metadata.get("name"), str):
        reject("provider job metadata name is missing or invalid")
    try:
        spec = document["spec"]["template"]["spec"]["template"]["spec"]
    except (KeyError, TypeError):
        reject("missing Cloud Run job template spec")
    if not isinstance(spec, dict):
        reject("Cloud Run job template spec is not an object")
    containers = spec.get("containers")
    if not isinstance(containers, list) or len(containers) != 1 or not isinstance(containers[0], dict):
        reject("expected exactly one Cloud Run container")
    container = containers[0]
    if not isinstance(container.get("image"), str):
        reject("Cloud Run container image is missing or invalid")
    if not isinstance(container.get("env"), list):
        reject("Cloud Run container env is missing or invalid")
    if not isinstance(container.get("args"), list) or not all(isinstance(item, str) for item in container["args"]):
        reject("Cloud Run container args are missing or invalid")
    volumes = spec.get("volumes", [])
    mounts = container.get("volumeMounts", [])
    if volumes is None:
        volumes = []
    if mounts is None:
        mounts = []
    if not isinstance(volumes, list) or not isinstance(mounts, list):
        reject("Cloud Run volumes or volume mounts are invalid")
    return document, spec, container, volumes, mounts


def canonical_handle(project, region, job_name):
    return f"projects/{project}/locations/{region}/jobs/{job_name}"


def normalized_volume(volume):
    if not isinstance(volume, dict) or not isinstance(volume.get("name"), str):
        reject("Cloud Run volume entry is invalid")
    normalized = {"name": volume["name"]}
    csi = volume.get("csi")
    if csi is not None:
        if not isinstance(csi, dict) or not isinstance(csi.get("driver"), str):
            reject("Cloud Run volume CSI entry is invalid")
        normalized["csi"] = {"driver": csi["driver"]}
    return normalized


def normalized_volume_mount(mount):
    if not isinstance(mount, dict):
        reject("Cloud Run volume mount entry is invalid")
    if not isinstance(mount.get("name"), str) or not isinstance(mount.get("mountPath"), str):
        reject("Cloud Run volume mount entry is invalid")
    return {"name": mount["name"], "mountPath": mount["mountPath"]}


def normalized_config(env, args, volumes, mounts, expected_bucket=None, require_bucket=False, require_cloud_shape=False):
    if not isinstance(env, list) or not isinstance(volumes, list) or not isinstance(mounts, list):
        reject("Cloud Run config collections are invalid")
    if not isinstance(args, list) or not all(isinstance(item, str) for item in args):
        reject("Cloud Run container args are missing or invalid")
    allowlisted = []
    for entry in env:
        if not isinstance(entry, dict) or not isinstance(entry.get("name"), str):
            reject("Cloud Run env entry is invalid")
        if entry["name"] in LEGACY_ENV_NAMES:
            if require_cloud_shape:
                reject("production job retains forbidden legacy env")
        if entry["name"] not in ALLOWLISTED_ENV_NAMES:
            continue
        if "valueFrom" in entry or not isinstance(entry.get("value"), str):
            reject("allowlisted env must be a name/value pair")
        allowlisted.append({"name": entry["name"], "value": entry["value"]})
    buckets = [entry["value"] for entry in allowlisted if entry["name"] == "BUCKET"]
    if len(buckets) > 1:
        reject("Cloud Run job contains duplicate BUCKET env entries")
    bucket = buckets[0] if buckets else ""
    if require_bucket and not bucket:
        reject("production BUCKET is missing")
    if expected_bucket is not None and bucket != expected_bucket:
        reject("production BUCKET is incorrect")
    if require_cloud_shape and args != EXPECTED_ARGS:
        reject("production args do not match the cloud worker contract")
    if require_cloud_shape and (volumes or mounts):
        reject("production job retains volumes or volume mounts")
    return {
        "env": allowlisted,
        "bucket": bucket,
        "args": list(args),
        "volumes": [normalized_volume(volume) for volume in volumes],
        "volume_mounts": [normalized_volume_mount(mount) for mount in mounts],
    }


def allowlisted_config(container, volumes, mounts, expected_bucket=None, require_bucket=False, require_cloud_shape=False):
    return normalized_config(
        container["env"],
        container["args"],
        volumes,
        mounts,
        expected_bucket=expected_bucket,
        require_bucket=require_bucket,
        require_cloud_shape=require_cloud_shape,
    )


def normalized_rollback_config(config):
    if not isinstance(config, dict):
        reject("rollback config is missing or invalid")
    if not isinstance(config.get("env"), list) or not isinstance(config.get("volumes"), list) or not isinstance(config.get("volume_mounts"), list):
        reject("rollback config is missing or invalid")
    normalized = normalized_config(
        config.get("env"),
        config.get("args"),
        config.get("volumes"),
        config.get("volume_mounts"),
    )
    if not isinstance(config.get("bucket"), str) or config["bucket"] != normalized["bucket"]:
        reject("rollback config bucket is invalid")
    return normalized


def normalized_hash(value):
    encoded = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def secret_like(value, path=""):
    if isinstance(value, dict):
        for key, child in value.items():
            normalized = re.sub(r"[^a-z0-9]", "", str(key).lower())
            if any(word in normalized for word in SECRET_WORDS):
                return path + str(key)
            found = secret_like(child, path + str(key) + ".")
            if found:
                return found
    elif isinstance(value, list):
        for index, child in enumerate(value):
            found = secret_like(child, path + str(index) + ".")
            if found:
                return found
    elif isinstance(value, str) and re.search(r"(?i)(secret|token|password|api[_-]?key|valuefrom)", value):
        return path.rstrip(".")
    return ""


def validate_no_secret_like_fields(value):
    found = secret_like(value)
    if found:
        reject(f"secret-like field is not permitted: {found}")


def write_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    payload = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n"
    descriptor, temporary = tempfile.mkstemp(prefix=".deployment-evidence-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(payload)
            handle.flush()
            os.fchmod(handle.fileno(), 0o600)
        os.replace(temporary, path)
    except BaseException:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
        raise


def remove_output(path):
    try:
        Path(path).unlink()
    except FileNotFoundError:
        pass
    except OSError as error:
        reject(f"cannot prepare output {Path(path).name}: {error.__class__.__name__}")


def immutable_image(image, image_prefix):
    expected_prefix = image_prefix + "/olw-pipeline"
    if not isinstance(image, str) or not image.startswith(expected_prefix + "@"):
        reject("rollback image must use the worker Artifact Registry repository")
    digest = image[len(expected_prefix) + 1 :]
    if not DIGEST_RE.fullmatch(digest):
        reject("rollback image must be immutable")
    return image


def prepare_rollback(args):
    remove_output(args.output)
    document = provider_job(args.project, args.region, args.job_name)
    _, _, container, volumes, mounts = job_spec(document)
    image = container["image"]
    image_prefix = args.ar_repo + "/olw-pipeline"
    if image.startswith(image_prefix + "@"):
        rollback_image = immutable_image(image, args.ar_repo)
    elif image.startswith(image_prefix + ":"):
        tag = image[len(image_prefix) + 1 :]
        if not TAG_RE.fullmatch(tag):
            reject("prior production image tag is invalid")
        resolved = provider_digest(args.project, args.region, image)
        after = provider_job(args.project, args.region, args.job_name)
        _, _, after_container, after_volumes, after_mounts = job_spec(after)
        if after_container["image"] != image:
            reject("prior production image reference moved during rollback resolution")
        resolved_again = provider_digest(args.project, args.region, image)
        if resolved != resolved_again:
            reject("both prior image resolutions differ")
        container, volumes, mounts = after_container, after_volumes, after_mounts
        rollback_image = image_prefix + "@" + resolved
    else:
        reject("prior production image has the wrong repository or is not a supported reference")
    config = allowlisted_config(container, volumes, mounts)
    rollback = {
        "provider_handle": canonical_handle(args.project, args.region, args.job_name),
        "artifact_name": args.artifact_name,
        "image_reference": rollback_image,
        "config": config,
    }
    validate_no_secret_like_fields(rollback)
    write_json(args.output, rollback)


def positive_int(value, label):
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        reject(f"{label} is missing or invalid")
    return value


def metadata_value(metadata, key):
    value = metadata.get(key)
    if not isinstance(value, dict):
        reject(f"{key} metadata is missing or invalid")
    return value


def validate_metadata(metadata, args):
    validate_no_secret_like_fields(metadata)
    if metadata.get("schema_version") != EXPECTED_SCHEMA_VERSION:
        reject("unsupported evidence schema version")
    if metadata.get("project") != args.project:
        reject("evidence project is invalid")
    if metadata.get("component") != EXPECTED_COMPONENT:
        reject("unsupported evidence component")
    if metadata.get("environment") != EXPECTED_ENVIRONMENT:
        reject("unsupported evidence environment")
    if metadata.get("action") != EXPECTED_ACTION:
        reject("unsupported evidence action")
    artifact_name = metadata.get("rollback_artifact_name")
    if not isinstance(artifact_name, str) or not ARTIFACT_RE.fullmatch(artifact_name):
        reject("rollback artifact name is missing or invalid")

    source = metadata_value(metadata, "source")
    commit_sha = source.get("commit_sha")
    if not isinstance(commit_sha, str) or not SHA_RE.fullmatch(commit_sha):
        reject("source commit SHA is missing or invalid")
    if source.get("ref") != "refs/heads/main":
        reject("source ref is invalid")

    provenance = metadata_value(metadata, "dev_provenance")
    if provenance.get("workflow") != "deploy-worker.yml" or provenance.get("event") != "push":
        reject("dev provenance workflow or event is invalid")
    if provenance.get("head_branch") != "main" or provenance.get("head_sha") != commit_sha:
        reject("dev provenance does not match the source commit")
    if provenance.get("conclusion") != "success":
        reject("dev provenance run was not successful")
    positive_int(provenance.get("run_id"), "dev provenance run ID")
    if not isinstance(provenance.get("run_url"), str) or not provenance["run_url"].startswith("https://"):
        reject("dev provenance run URL is missing or invalid")

    image = metadata_value(metadata, "image")
    digest = image.get("digest")
    expected_image = args.ar_repo + "/olw-pipeline@" + str(digest)
    if not isinstance(digest, str) or not DIGEST_RE.fullmatch(digest):
        reject("promoted image digest is missing or invalid")
    if image.get("reference") != expected_image:
        reject("promoted image reference is invalid")

    if "provider_verification" in metadata:
        reject("provider verification metadata must be generated by the renderer")

    originating = metadata_value(metadata, "originating_workflow")
    if not isinstance(originating.get("repository"), str) or not originating["repository"]:
        reject("originating workflow repository is missing")
    if not isinstance(originating.get("workflow"), str) or not originating["workflow"]:
        reject("originating workflow name is missing")
    positive_int(originating.get("run_id"), "originating workflow run ID")
    positive_int(originating.get("run_attempt"), "originating workflow run attempt")
    return (
        commit_sha,
        image["reference"],
        artifact_name,
        {
            "dev_provenance": {
                "workflow": provenance["workflow"],
                "event": provenance["event"],
                "head_branch": provenance["head_branch"],
                "head_sha": provenance["head_sha"],
                "conclusion": provenance["conclusion"],
                "run_id": provenance["run_id"],
                "run_url": provenance["run_url"],
            },
            "image": {"digest": image["digest"], "reference": image["reference"]},
            "originating_workflow": {
                "repository": originating["repository"],
                "workflow": originating["workflow"],
                "run_id": originating["run_id"],
                "run_attempt": originating["run_attempt"],
            },
        },
    )


def validate_rollback(rollback, args):
    validate_no_secret_like_fields(rollback)
    expected_handle = canonical_handle(args.project, args.region, args.job_name)
    if rollback.get("provider_handle") != expected_handle:
        reject("rollback provider handle is invalid")
    artifact_name = rollback.get("artifact_name")
    if not isinstance(artifact_name, str) or not ARTIFACT_RE.fullmatch(artifact_name):
        reject("rollback artifact name is missing or invalid")
    image = immutable_image(rollback.get("image_reference"), args.ar_repo)
    return artifact_name, image, normalized_rollback_config(rollback.get("config"))


def render_evidence(args):
    remove_output(args.output)
    metadata = read_json(args.metadata)
    rollback = read_json(args.rollback_contract)
    commit_sha, expected_image, metadata_artifact_name, normalized_metadata = validate_metadata(metadata, args)
    artifact_name, rollback_image, rollback_config = validate_rollback(rollback, args)
    if artifact_name != metadata_artifact_name:
        reject("rollback artifact name does not match evidence metadata")

    document = provider_job(args.project, args.region, args.job_name)
    _, spec, container, volumes, mounts = job_spec(document)
    observed_metadata = document.get("metadata", {})
    generation = positive_int(observed_metadata.get("generation"), "observed Job generation")
    if observed_metadata.get("name") != args.job_name:
        reject("observed Job name does not match the requested provider handle")
    if container["image"] != expected_image:
        reject("observed Job image does not match the promoted immutable image")
    service_account = spec.get("serviceAccountName")
    if not isinstance(service_account, str) or not service_account:
        reject("observed runtime service account is missing or invalid")
    config = allowlisted_config(
        container,
        volumes,
        mounts,
        expected_bucket=args.bucket,
        require_bucket=True,
        require_cloud_shape=True,
    )
    config_fingerprint = normalized_hash(config)
    checked_at = datetime.now(timezone.utc).replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")
    provider_handle = canonical_handle(args.project, args.region, args.job_name)
    evidence = {
        "schema_version": EXPECTED_SCHEMA_VERSION,
        "project": metadata["project"],
        "component": metadata["component"],
        "environment": metadata["environment"],
        "action": metadata["action"],
        "source": {"commit_sha": commit_sha, "ref": metadata["source"]["ref"]},
        "dev_provenance": normalized_metadata["dev_provenance"],
        "image": normalized_metadata["image"],
        "provider": {
            "current_handle": provider_handle,
            "rollback_handle": rollback["provider_handle"],
            "rollback_artifact_name": artifact_name,
        },
        "observed_job": {
            "generation": generation,
            "image_reference": container["image"],
            "runtime_service_account": service_account,
        },
        "config": {"result": "verified", "fingerprint": config_fingerprint, "allowlisted": config},
        "provider_verification": {
            "result": "verified",
            "checked_at": checked_at,
            "checks": ["provider_handle", "generation", "image", "runtime_service_account", "allowlisted_config"],
        },
        "originating_workflow": normalized_metadata["originating_workflow"],
        "rollback": {"image_reference": rollback_image, "config": rollback_config},
    }
    validate_no_secret_like_fields(evidence)
    write_json(args.output, evidence)


def add_provider_args(parser):
    parser.add_argument("--project", required=True)
    parser.add_argument("--region", required=True)
    parser.add_argument("--job-name", required=True)


def build_parser():
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="mode", required=True)
    prepare = subparsers.add_parser("prepare-rollback")
    add_provider_args(prepare)
    prepare.add_argument("--ar-repo", required=True)
    prepare.add_argument("--artifact-name", required=True)
    prepare.add_argument("--output", required=True)
    render = subparsers.add_parser("render-evidence")
    add_provider_args(render)
    render.add_argument("--ar-repo", required=True)
    render.add_argument("--bucket", required=True)
    render.add_argument("--rollback-contract", required=True)
    render.add_argument("--metadata", required=True)
    render.add_argument("--output", required=True)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        if args.mode == "prepare-rollback":
            prepare_rollback(args)
        else:
            render_evidence(args)
    except EvidenceError as error:
        print(f"deployment evidence rejected: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
