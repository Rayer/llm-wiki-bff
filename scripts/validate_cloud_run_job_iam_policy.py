#!/usr/bin/env python3
"""Fail closed unless stdin is a valid, non-ambiguous Cloud Run Job IAM policy."""

import json
import sys


ROLE = "roles/run.viewer"
REQUIRED_MEMBER = "serviceAccount:lwc-pipeline-dev@llm-wiki-cloud.iam.gserviceaccount.com"


def invalid() -> int:
    print("invalid Cloud Run Job IAM policy", file=sys.stderr)
    return 1


def main() -> int:
    try:
        policy = json.load(sys.stdin)
    except (json.JSONDecodeError, UnicodeDecodeError):
        return invalid()

    if not isinstance(policy, dict) or not isinstance(policy.get("bindings"), list):
        return invalid()

    viewer_bindings = []
    required_member_count = 0
    viewer_required_member_count = 0
    for binding in policy["bindings"]:
        if not isinstance(binding, dict):
            return invalid()
        if not isinstance(binding.get("role"), str) or not isinstance(binding.get("members"), list):
            return invalid()
        if any(not isinstance(member, str) for member in binding["members"]):
            return invalid()
        required_member_count += binding["members"].count(REQUIRED_MEMBER)
        if binding["role"] == ROLE:
            viewer_bindings.append(binding)
            viewer_required_member_count += binding["members"].count(REQUIRED_MEMBER)

    if len(viewer_bindings) != 1:
        return invalid()
    if required_member_count != 1:
        return invalid()
    if viewer_required_member_count != 1:
        return invalid()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
