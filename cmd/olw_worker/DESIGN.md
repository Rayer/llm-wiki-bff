# LWC OLW Worker Design

## Purpose

`cmd/olw_worker` is a local-first CLI and Cloud Run Job entrypoint for running
OLW against a project vault. The worker does not use the GCS API directly. In
Cloud Run, gcsfuse mounts the bucket under `/data`; locally, `--vault` points at
the project directory.

## Command Contract

Primary command:

```bash
worker run '[["init"],["run","--auto-approve"]]'
```

The JSON payload is an array of OLW command arrays. Each inner array is executed
as one `olw` invocation from the resolved vault:

```bash
olw init
olw run --auto-approve
```

Postprocess-only command:

```bash
worker postprocess --vault /path/to/project
```

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

These are the same artifacts BFF reads for ID routing and persisted concept
cache data.

## wiki.toml

If `wiki.toml` is missing, the worker creates a DeepSeek config using the API key
from:

1. `--api-key`
2. `LLM_API_KEY`

The generated config sets `auto_approve`, `auto_commit`, `auto_maintain`,
`article_max_tokens = 32768`, and `ingest_parallel = false`.

## BFF Integration

BFF invokes the Cloud Run Job with:

```json
{
  "args": ["run", "[[\"init\"],[\"run\",\"--auto-approve\"]]"],
  "env": [
    {"name": "USER_ID", "value": "..."},
    {"name": "PROJECT_ID", "value": "..."},
    {"name": "TASK_TYPE", "value": "pipeline"}
  ]
}
```

The admin pipeline trigger only starts the worker and returns the Cloud Run
execution ID. It does not rebuild the index immediately, because the worker job
has not completed yet. Manual rebuild endpoints remain available for repair and
admin workflows.

## Deployment

The worker image should include:

- the compiled Go worker binary
- Python
- `obsidian-llm-wiki`
- a Cloud Run Job gcsfuse volume mounted at `/data`

## Verification

Expected local checks:

```bash
go test ./cmd/olw_worker ./internal/wikiindex/... ./internal/handler/v1
go build -o /tmp/olw_worker ./cmd/olw_worker
/tmp/olw_worker postprocess --vault /tmp/example-vault --api-key local-test
```
