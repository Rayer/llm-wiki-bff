#!/usr/bin/env python3
"""Classify deterministic Cloud Run `services describe` stderr outcomes."""

import argparse
from pathlib import Path
import sys


def is_service_absent(stderr_text, service_name):
    target = f"ERROR: (gcloud.run.services.describe) Cannot find service [{service_name}]"
    return any(line == target for line in stderr_text.splitlines())


def main():
    parser = argparse.ArgumentParser(description="Classify Cloud Run service describe absence.")
    parser.add_argument("--service-name", required=True)
    parser.add_argument("--stderr-file", required=True)
    args = parser.parse_args()

    try:
        stderr = Path(args.stderr_file).read_text(encoding="utf-8", errors="replace")
    except OSError as error:
        print(f"failed to read stderr file: {error}", file=sys.stderr)
        return 1

    return 0 if is_service_absent(stderr, args.service_name) else 1


if __name__ == "__main__":
    raise SystemExit(main())
