# OLW Worker Local-First Design

## Goal

Build `cmd/olw_worker` as a Cloud Run Job and local CLI execution environment for OLW commands. The worker should run against a mounted or local vault, execute caller-provided OLW command batches, and then generate the cache/index artifacts that BFF reads.

This first phase is local/filesystem-first. The worker will not add a new GCS API write path. In Cloud Run, gcsfuse makes `/data` look like a filesystem; locally, `--vault` points at a project directory.

## Non-Goals

- Do not implement a second GCS API storage path for the worker.
- Do not hard-code a stable pipeline enum yet.
- Do not make BFF rebuild index immediately after triggering the worker job.
- Do not introduce Viper for worker configuration in this phase.

## Command Contract

The primary command is:

```bash
worker run '[["run","--auto-approve"],["lint","--fix"]]'
```

The JSON payload is an array of arrays. Each inner array maps to one `olw` invocation:

```json
[
  ["clear"],
  ["init"],
  ["run", "--auto-approve"],
  ["lint", "--fix"]
]
```

The worker executes these as:

```bash
olw clear
olw init
olw run --auto-approve
olw lint --fix
```

Additional worker command:

```bash
worker postprocess
```

`postprocess` only regenerates cache/index artifacts for the resolved vault.

## Postprocess Behavior

`worker run` defaults to running postprocess after the OLW command batch succeeds.

Generated artifacts:

- `cache/id_map.json`
- `cache/concepts.jsonl`

Development can disable postprocess:

```bash
worker run '[["run","--auto-approve"]]' --no-postprocess
```

`concepts.jsonl` should use the complete concept cache entry shape already used by `internal/cache`:

```json
{"slug":"...","title":"...","body":"...","frontmatter":{},"sources":[]}
```

This keeps BFF query cache loading aligned with the persisted file format.

## Error Handling

The worker supports:

```bash
--stop-on-error
```

Default: `true`.

When `--stop-on-error=true`, the first failed OLW command stops the batch, skips postprocess, and exits non-zero.

When `--stop-on-error=false`, the worker continues through the batch, records failures, skips postprocess if any command failed, and exits non-zero at the end.

## Vault Resolution

The worker resolves the vault in this order:

1. `--vault`
2. `VAULT_PATH`
3. `DATA_DIR` + `USER_ID` + `PROJECT_ID`

The default cloud path is:

```text
/data/users/{USER_ID}/projects/{PROJECT_ID}
```

`DATA_DIR` defaults to `/data`.

## wiki.toml

The worker ensures `wiki.toml` exists before running OLW commands.

First phase behavior:

- If `wiki.toml` does not exist, create it.
- If `wiki.toml` exists, do not overwrite or patch it.

The generated config is DeepSeek-only:

```toml
[provider]
name = "custom"
url = "https://api.deepseek.com/v1"
api_key = "<resolved api key>"

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

API key resolution for the local-first phase:

1. `--api-key`
2. `LLM_API_KEY`

Secret Manager support will be added after local validation:

```text
projects/llm-wiki-cloud/secrets/deepseek-apikey/versions/latest
```

When Secret Manager is added, the expected resolution order is:

1. `--api-key`
2. Secret Manager `deepseek-apikey`
3. `LLM_API_KEY`

## BFF Integration

`AdminPipelineTrigger` should invoke the Cloud Run Job and return the execution ID. It must not run rebuild-index inside the BFF process immediately after job creation, because the worker job has not completed yet.

The default admin trigger command for this phase is:

```json
[["init"],["run","--auto-approve"]]
```

The Cloud Run Jobs API override should pass:

```json
{
  "overrides": {
    "containerOverrides": [
      {
        "args": [
          "run",
          "[[\"init\"],[\"run\",\"--auto-approve\"]]"
        ],
        "env": [
          {"name": "USER_ID", "value": "..."},
          {"name": "PROJECT_ID", "value": "..."}
        ]
      }
    ]
  }
}
```

BFF rebuild endpoints remain available as manual repair/admin tools.

## Shared Code Shape

Move rebuild core out of `internal/handler/v1` into a handler-independent package, for example:

```text
internal/wikiindex
```

The core package should operate on a small storage interface:

```go
type Store interface {
    ListMarkdownFiles(ctx context.Context, dir string) ([]MarkdownFile, error)
    ReadFile(ctx context.Context, relPath string) ([]byte, error)
    WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error)
}
```

Filesystem implementation:

```text
internal/wikiindex/fsstore
```

The filesystem store reads and writes under the resolved vault path. Atomic write uses temp file write, close, then rename.

BFF can keep using the existing GCS-backed behavior by adapting the existing GCS client to the same interface.

## Dockerfile

The existing multi-stage Dockerfile already has a `worker` target:

```dockerfile
FROM python:3.12-slim AS worker
RUN pip install --no-cache-dir obsidian-llm-wiki
COPY --from=build /olw_worker /worker
ENTRYPOINT ["/worker"]
```

Deployments should build and push the `worker` target for the Cloud Run Job.

## Tests

Initial test coverage:

- JSON command batch parsing.
- Worker config and vault resolution.
- `wiki.toml` creation without overwriting existing config.
- Filesystem postprocess with a temp vault.
- BFF admin trigger no longer performs immediate rebuild.

## Acceptance Criteria

- `worker run '[["init"],["run","--auto-approve"]]' --vault <path>` runs OLW commands in the given local vault.
- `worker run ...` generates `cache/id_map.json` and `cache/concepts.jsonl` by default after a successful batch.
- `worker run ... --no-postprocess` skips artifact generation.
- `worker postprocess --vault <path>` regenerates artifacts without running OLW.
- Failed OLW commands produce non-zero exits according to `--stop-on-error`.
- BFF admin pipeline trigger only invokes the Cloud Run Job and does not rebuild stale index data immediately.
