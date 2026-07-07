# LWC OLW Worker PR Report

## Summary

This change adds `cmd/olw_worker` as the Cloud Run Job entrypoint for running
OLW against a project vault mounted from GCS. The worker is filesystem-first:
it does not write to GCS through the Storage API. In Cloud Run, gcsfuse mounts
the bucket at `/data`; locally, `--vault` points at a project directory.

The active pipeline path is now:

```text
BFF v1 endpoint -> Cloud Run Jobs API -> olw-pipeline job -> /data gcsfuse vault -> OLW -> worker postprocess
```

The old trigger-file path has been removed. BFF no longer writes
`raw/_pipeline_trigger.md`, and the worker is not a scheduled scanner.

## Main Changes

- Added `cmd/olw_worker` with `run` and `postprocess` commands.
- Added a Docker `worker` target with Go worker binary, Python, git, and
  `obsidian-llm-wiki`.
- Added filesystem-backed postprocess support through `internal/wikiindex/fsstore`.
- Updated BFF v1 pipeline triggers to invoke the Cloud Run Job directly.
- Changed admin pipeline trigger to return the Cloud Run execution ID without
  rebuilding index data immediately.
- Removed the legacy raw trigger-file handler from `internal/handler/raw.go`.
- Added tests for worker config, command parsing, GCS listing, Firestore lock
  metadata, v1 pipeline Cloud Run invocation, and admin trigger behavior.

## Command Contract

Primary command:

```bash
worker run '[["run","--auto-approve"]]'
```

The JSON payload is an array of OLW command arrays. Each inner array is executed
as one `olw` invocation from the resolved vault:

```bash
olw run --auto-approve
```

Postprocess-only command:

```bash
worker postprocess --vault /path/to/project
```

`olw init` is intentionally not part of the default Cloud Run command batch.
OLW 0.8.5 requires `olw init <VAULT_PATH>`, and `olw init .` overwrites
`wiki.toml` with local Ollama defaults. The worker instead ensures a suitable
`wiki.toml` exists before running OLW.

## Vault Resolution

The worker resolves the vault in this order:

1. `--vault`
2. `VAULT_PATH`
3. `DATA_DIR/users/{USER_ID}/projects/{PROJECT_ID}`

`DATA_DIR` defaults to `/data`.

## Run Flow

1. Parse the JSON command batch.
2. Resolve and validate the vault directory.
3. Remove `.olw/pipeline.lock` only when it is older than five minutes.
4. Ensure `wiki.toml` exists. Existing config is never overwritten.
5. Run each OLW command in order.
6. If all OLW commands succeed and postprocess is enabled, rebuild BFF artifacts.

`--stop-on-error` defaults to true. When false, the worker continues through the
batch, records failures, skips postprocess if any command failed, and exits
non-zero at the end.

`--no-postprocess` skips artifact generation after a successful batch.

## Generated Artifacts

Postprocess uses `internal/wikiindex/fsstore` and writes:

- `cache/id_map.json`
- `cache/concepts.jsonl`

These are the artifacts BFF reads for ID routing and persisted concept cache
data.

## LLM Configuration

If `wiki.toml` is missing, the worker creates a DeepSeek vault config:

```toml
[provider]
name = "deepseek"
url = "https://api.deepseek.com/v1"

[models]
fast = "deepseek-chat"
heavy = "deepseek-reasoner"

[pipeline]
auto_approve = true
auto_commit = true
auto_maintain = true
article_max_tokens = 32768
max_concepts_per_source = 8
ingest_parallel = false
```

API keys are not written into `wiki.toml`. OLW resolves the DeepSeek key from
`DEEPSEEK_API_KEY`; the worker also accepts `LLM_API_KEY` only as a guard for
creating `wiki.toml`.

Cloud Run should provide the existing Secret Manager secret as both env vars:

```text
LLM_API_KEY=deepseek-apikey:latest
DEEPSEEK_API_KEY=deepseek-apikey:latest
```

## BFF Integration

BFF invokes the Cloud Run Job with:

```json
{
  "args": ["run", "[[\"run\",\"--auto-approve\"]]"],
  "env": [
    {"name": "USER_ID", "value": "..."},
    {"name": "PROJECT_ID", "value": "..."},
    {"name": "TASK_TYPE", "value": "pipeline"}
  ]
}
```

The user endpoint is:

```text
POST /api/v1/pipeline/run
```

The admin endpoint is:

```text
POST /api/v1/admin/projects/{userID}_{projectID}/pipeline
```

Both endpoints invoke the Cloud Run Job immediately and return an execution ID.
They do not write request files and do not rely on a periodic worker.

Manual rebuild endpoints remain available for repair and admin workflows.

## Deployment Used For Verification

Project:

```text
llm-wiki-cloud
```

Region:

```text
asia-east1
```

Job:

```text
olw-pipeline
```

Image:

```text
asia-east1-docker.pkg.dev/llm-wiki-cloud/cloud-run-images/olw-pipeline:80a08fa
```

Mounted bucket:

```text
gs://llm-wiki-data -> /data
```

The tested job configuration uses:

```text
timeoutSeconds=7200
maxRetries=0
memory=2Gi
cpu=1000m
```

The previous `1800s` timeout was too short for the full demo project.

## Verification Results

Local checks:

```bash
go test ./...
docker build --target worker -t llm-wiki-bff-olw-worker:test .
```

Docker smoke test:

- mounted a temporary host directory into `/data`
- ran `olw init`
- copied three Helios raw files into `raw/`
- mounted local OLW config
- ran ingest, compile, and worker postprocess successfully

Cloud Run test:

```text
execution: olw-pipeline-kxbm4
status: success
duration: 42m54.05s
compiled: 107
published: 107
```

Generated artifacts in GCS:

```text
gs://llm-wiki-data/users/test-user/projects/demo/cache/id_map.json
gs://llm-wiki-data/users/test-user/projects/demo/cache/concepts.jsonl
```

Observed artifact sizes:

```text
id_map.json:      29,373 bytes
concepts.jsonl: 1,023,792 bytes
concepts.jsonl: 420 lines
```

## Known Issues And Follow-Ups

- Four concepts failed during the Cloud Run verification because DeepSeek output
  was truncated at `max_tokens=2400`. This is an OLW/model output limit issue,
  not a worker startup or deployment failure.
- gcsfuse logs repeated SQLite journal warnings for `.olw/state.db`, including
  out-of-order writes. The successful run shows this is not immediately fatal,
  but it is a durability/performance risk.
- gcsfuse also logs unsupported hard link operations during git activity. The
  job still completed successfully.
- Long-term, `.olw/state.db` and git operations should probably run on local
  scratch storage, then sync only durable vault/artifact outputs back to GCS.
- Full project pipeline runs are long. The verified demo run needed about
  43 minutes after prior partial progress. Keep the Cloud Run Job timeout above
  the expected full-project runtime or split OLW work into smaller batches.

## Owner Review Notes

Please review these points before merge:

- The active design is direct Cloud Run Job invocation, not trigger-file polling.
- The default worker command intentionally excludes `olw init`.
- Existing `wiki.toml` files are preserved. If a project vault already contains
  local Ollama config, it must be migrated to DeepSeek manually or through a
  separate repair task.
- Secrets should stay in Secret Manager env vars and should not be persisted
  into vault config.
- The legacy raw trigger-file path has been removed from code, but callers must
  use the v1 pipeline endpoints.
