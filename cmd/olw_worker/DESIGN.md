# LWC Synto Worker PR Report

## Summary

This change adds `cmd/olw_worker` as the Cloud Run Job entrypoint for running
Synto 0.7.0 against a project vault mounted from GCS. The worker is filesystem-first:
it does not write to GCS through the Storage API. In Cloud Run, gcsfuse mounts
the bucket at `/data`; locally, `--vault` points at a project directory. Every
generation, including local usage, runs in a private workspace and publishes
only validated worker-owned outputs.

The active pipeline path is now:

```text
BFF v1 endpoint -> Cloud Run Jobs API -> olw-pipeline-dev job -> mounted /data vault
  -> per-vault exclusive lease -> private copied workspace -> Synto + postprocess
  -> staged atomic durable-output publish -> worker-owned receipt
```

The old trigger-file path has been removed. BFF no longer writes
`raw/_pipeline_trigger.md`, and the worker is not a scheduled scanner.

## Main Changes

- Added `cmd/olw_worker` with `run` and `postprocess` commands.
- Added a Docker `worker` target with Go worker binary, Python, git, and the
  exact pinned Synto 0.7.0 wheel.
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

The JSON payload is the one characterized Synto run command. It may omit the
flag for local review workflows, or include `--auto-approve` for production:

```bash
synto run --auto-approve
```

Postprocess-only command:

```bash
worker postprocess --vault /path/to/project
```

The worker rejects every second command, `--force` in any position, and
mutation-capable Synto commands such as compile, ingest, identity curation,
undo, import/export, watch, query, or MCP. It creates a safe `synto.toml` only
for a fresh Synto-only vault; it never overwrites an existing config.

## Vault Resolution

The worker resolves the vault in this order:

1. `--vault`
2. `VAULT_PATH`
3. `DATA_DIR/users/{USER_ID}/projects/{PROJECT_ID}`

`DATA_DIR` defaults to `/data`.

## Run Flow

1. Parse and validate the complete command batch before taking a lease or
   starting a child. The production contract is `[ ["run", "--auto-approve"] ]`.
2. Acquire an exclusive create-only per-vault lease in
   `.olw/lwc-worker-lease.json` before taking any snapshot. The lease records
   owner/execution metadata and is held through success/failure receipts and
   failure-log publication. It fails closed on overlap and is never
   automatically stolen based only on age; an abandoned lease needs operator
   inspection rather than risking a live long-running job.
3. Snapshot mapped source raw bytes and annotation
   digests, then copy `raw/`, `wiki/`, `cache/`, `.olw/`, `.synto/`,
   `wiki.toml`, and `synto.toml` when present into a private directory under
   `WORKSPACE_DIR` (default `/tmp`). Symlinks and escaping paths are rejected.
4. Every mapped source with a non-empty annotation receives one deterministic,
   timestamp-free human annotation trailer on every fresh workspace run,
   independent of its receipt. Empty annotations receive no trailer. Stored
   `raw/**` is never changed or synced back.
5. Remove a stale workspace pipeline lock, validate coherent Synto/legacy state,
   migrate only in the workspace, run `synto run`, then invoke the exact-release
   offline `synto pack export --target agents --out …` step. Install and validate
   its `index/INDEX.json` as `.synto/INDEX.json` before postprocess/reconcile.
   A migrated vault retains the coherent `wiki.toml` + `.olw/state.db`
   rollback pair; a fresh Synto vault creates neither legacy artifact.
6. On success, stage and validate the complete explicit output set under a
   sibling directory in the mounted vault, then publish it using same-filesystem
   atomic renames and a journal/backup. `wiki/` is mirrored so removed
   pages are removed. The allowlist is `wiki/`, `wiki.toml` when migrated,
   `synto.toml`, `cache/id_map.json`, `cache/concepts.jsonl`,
   `cache/dormant_concepts.jsonl`, `cache/raw_status.json`,
   `cache/suggested_queries.json`, `.synto/state.db`, `.synto/INDEX.json`,
   `.olw/state.db` when migrated, and the current bounded pipeline log.
   Uncommitted journals are rolled back when the next lease
   holder starts; committed journals only clean up their stage and backup files.
   This is recovery for normal filesystem interruption, not a
   claim of a crash-proof global transaction on every mounted filesystem.
7. After that publish succeeds, atomically merge `cache/source_status.json` with
   exact start raw/annotation fingerprints. Concurrent raw or annotation edits
   remain dirty because the receipt retains the start pair. Failures retain the
   last success and record only the attempted fingerprint/error. On workspace
   failure, the closed current execution log alone is atomically published with
   a size cap and configured-secret redaction before cleanup.

`--stop-on-error` defaults to true. When false, the worker continues through the
batch, records failures, skips postprocess if any command failed, and exits
non-zero at the end.

The default production generation includes INDEX generation, postprocess,
reconciliation, and preflight before publication. Local `--no-postprocess`
compatibility runs still execute only in a private scratch workspace and never
publish generation outputs.

## Cloud Failure Diagnostic Artifact

For a failed cloud execution, the worker writes the operator-only object
`cache/pipeline-<execution>.failure.json` alongside the existing fixed
`cache/pipeline-<execution>.log` event and fixed source receipt error. The
failure object is versioned, deterministic JSON and is written create-only; it
is not part of a generation, does not create a current pointer, and is never
written on success. A diagnostic write failure is reported through the
existing fixed failure-recording category and cannot publish a generation.

The payload is bounded to 4 KiB and contains only `version: 1`,
`status: "failed"`, a finite stage, a finite error class, and when proven at a
child-process boundary a finite child command plus numeric exit code. Stages
include input materialization, Synto migration/config/run/index export,
source/concept reconciliation, postprocess, generation publication, receipt
recording, and lease cleanup. Error classes include validation, child exit, timeout,
cancellation, I/O, invalid state, publication conflict, recording failure, and
unknown. The accepted user command is only `run`; migration and index export
are worker-owned child seams.

The artifact never stores child stdout/stderr, error strings, provider HTTP
status, URLs, paths, arguments, provider bodies, model responses, source or
article text, credentials, tokens, tenant/user/project/execution IDs, or
timestamps. The fixed pipeline log and source receipt contracts remain
sanitized and unchanged; this object is the separate operator diagnostic
channel and is not exposed through a BFF API/UI.

The one deliberate exception is `errManifestCommitOutcomeUnknown`: when the
manifest CAS/readback outcome is ambiguous, the worker writes no failed
diagnostic object and no failure source receipt. The manifest may already have
committed, so recording failure would falsify publication truth and encourage
an unsafe replay; the explicit ambiguous outcome is preserved instead.

## Generated Artifacts

Postprocess uses `internal/wikiindex/fsstore` and writes:

- `cache/id_map.json`
- `cache/concepts.jsonl`

These are the artifacts BFF reads for ID routing and persisted concept cache
data.

When rebuilding `cache/id_map.json`, postprocess reconstructs active Concept and
Source maps from current pages, carries forward validated `dormant_concept`
rows, and carries forward `concept_entity_id` only for rebuilt active or
retained dormant LWC IDs. Valid entity rows for removed IDs are unowned and are
pruned; malformed rows, duplicate retained entity IDs, and active/dormant ID or
slug collisions fail closed before either generated artifact is written. Exact
entity-aware reconciliation owns reactivation of a dormant entity.

## LLM Configuration

If `synto.toml` is missing, the worker creates a safe DeepSeek Synto config:

```toml
[providers.default]
name = "deepseek"
url = "https://api.deepseek.com/v1"
timeout = 600
api_key_env = "DEEPSEEK_API_KEY"

[models.fast]
provider = "default"
model = "deepseek-chat"
ctx = 16384

[models.heavy]
provider = "default"
model = "deepseek-reasoner"
ctx = 32768

[pipeline]
auto_approve = true
auto_commit = false
auto_maintain = false
relation_extraction = false
article_max_tokens = 32768
max_concepts_per_source = 8
ingest_parallel = false
```

API keys are not written into `synto.toml`. Synto resolves the DeepSeek key from
`DEEPSEEK_API_KEY`; the worker accepts `LLM_API_KEY` only as a guard for creating
`synto.toml`.

The worker isolates Synto global configuration for every run by setting
`XDG_CONFIG_HOME` to a private temporary directory before invoking `synto`. This
prevents Synto from reading a developer's global config during Docker smoke
tests. Do not mount host Synto configuration into the worker container.

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

## Historical Pre-LWC-170 Verification Evidence

This section records pre-LWC-170 verification only. It does not describe the
current development deployment or establish a new live deployment.

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
- passed `LLM_API_KEY` or `DEEPSEEK_API_KEY` through env/Secret Manager only
- did not mount host `~/.config/olw`
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

## Synto 0.7.0 Compatibility Smoke

The canonical offline exact-release gate is:

```sh
bridge_dir="$(mktemp -d /tmp/lwc195-bridge.XXXXXX)"
OLW_BASELINE_ROOT=/path/to/olw-0.8.5 \
  LWC195_EXACT_INDEX_RUN1_PATH="$bridge_dir/run1-INDEX.json" \
  LWC195_EXACT_INDEX_RUN2_PATH="$bridge_dir/run2-INDEX.json" \
  LWC195_RAW_SOURCE_PATH="$bridge_dir/source.md" \
  LWC197_MIGRATED_CONFIG_PATH="$bridge_dir/migrated-synto.toml" \
  /path/to/synto-0.7.0/.venv/bin/python \
  cmd/olw_worker/testdata/synto_exact_release_smoke.py
```

It asserts the installed `synto==0.7.0`, runs the real `migrate-olw`, seeds one
deterministic `Alpha` article/entity/raw source before exact CLI run #1, then
runs `synto run --vault … --auto-approve` followed by the exact
`synto pack export --target agents --out …` command twice. Both exports are
non-empty and must contain `articles/Alpha.md`, the same article identity, the
same non-empty engine entity ID, and the expected `raw/source.md`/`Alpha`
source edge. The first and second authoritative `index/INDEX.json` bytes, the
actual raw source bytes, and the migrated `synto.toml` are written to four
bridge paths supplied by environment variables. These paths are one bundle:
any publication failure removes every destination, with no stale prior
artifact retained. The test process patches only exact-release client construction
with a local health-only/fail-if-called provider: the real CLI dependency
loader, router, StateDB, orchestrator, and pack exporter remain exercised,
while any generation or embedding call fails the gate. It does not claim a
provider-backed content-generation E2E.

The non-empty seed uses exact-release helpers rather than guessed state values:
`synto.vault.parse_note`, `pipeline.ingest._content_hash`,
`pipeline.ingest._ingest_prompt_version`, `pipeline.compile._content_hash`,
`StateDB.upsert_raw`, `StateDB.upsert_concepts`,
`StateDB.mark_concept_compile_state(..., "compiled")`, and
`StateDB.upsert_article`. This supplies the parsed-body hash and current ingest
prompt fingerprint required by the raw-scan skip path, plus the compiled
per-concept state required by the compile scheduler. The Go adapter mirrors
Synto's `frontmatter.parse(...).content.strip()` hash semantics, including
plain markdown's terminal newline trimming. Bridge files are staged until all
assertions pass, then atomically replaced with temporary files created beside
each destination so bind-mounted Docker output paths remain same-filesystem.

The pinned `src/synto/pack_export.py` contract is important at the adapter
boundary: it writes `index/INDEX.json`, emits `articles/<vault article path>`
for concept articles, and copies the same path into the export. The exact
release supports nested article paths in a general pack, but this production
adapter deliberately accepts only the root `articles/<single-filename>.md`
form. It also keeps the pre-existing direct `wiki/<single-filename>.md` form as
an explicit compatibility input. Both forms normalize to the exact filename
slug; case-insensitive lookup, path flattening, and fuzzy name matching are not
performed (case variants are rejected as collisions).

The parent Go selector consumes both exact export files through
`readSyntoIndexTruth`, independently reads the raw fixture through
`snapshotSources`/`syntoSourceContentHash`, and verifies the exported source
edge hash. It seeds a prior `stable-alpha` LWC concept bound to the first
export's engine entity, reconciles the second export, and verifies that the
stable ID reactivates without a transient replacement. It then changes the
raw bytes and verifies the same stable ID becomes Dormant while retaining its
engine binding:

```sh
bridge_dir="$(mktemp -d /tmp/lwc195-bridge.XXXXXX)"
OLW_BASELINE_ROOT=/path/to/olw-0.8.5 \
LWC195_EXACT_INDEX_RUN1_PATH="$bridge_dir/run1-INDEX.json" \
LWC195_EXACT_INDEX_RUN2_PATH="$bridge_dir/run2-INDEX.json" \
LWC195_RAW_SOURCE_PATH="$bridge_dir/source.md" \
LWC197_MIGRATED_CONFIG_PATH="$bridge_dir/migrated-synto.toml" \
  /path/to/synto-0.7.0/.venv/bin/python \
  cmd/olw_worker/testdata/synto_exact_release_smoke.py
LWC195_EXACT_INDEX_RUN1_PATH="$bridge_dir/run1-INDEX.json" \
LWC195_EXACT_INDEX_RUN2_PATH="$bridge_dir/run2-INDEX.json" \
LWC195_RAW_SOURCE_PATH="$bridge_dir/source.md" \
LWC197_MIGRATED_CONFIG_PATH="$bridge_dir/migrated-synto.toml" \
  go test ./cmd/olw_worker -run '^TestExactSyntoPackExportBridge$' -count=1 -v
```

For the fresh worker image, run the same exact smoke with the testdata and
baseline mounted read-only while the bridge output is mounted read-write:

```sh
docker build --target worker -f cmd/olw_worker/Dockerfile -t llm-wiki-bff-olw-worker:test .
docker run --rm --entrypoint python \
  -v "$PWD/cmd/olw_worker/testdata:/testdata:ro" \
  -v "$bridge_dir:/bridge" \
  -v "/path/to/olw-0.8.5:/olw-baseline:ro" \
  -e OLW_BASELINE_ROOT=/olw-baseline \
  -e LWC195_EXACT_INDEX_RUN1_PATH=/bridge/run1-INDEX.json \
  -e LWC195_EXACT_INDEX_RUN2_PATH=/bridge/run2-INDEX.json \
  -e LWC195_RAW_SOURCE_PATH=/bridge/source.md \
  -e LWC197_MIGRATED_CONFIG_PATH=/bridge/migrated-synto.toml \
  llm-wiki-bff-olw-worker:test \
  /testdata/synto_exact_release_smoke.py
LWC195_EXACT_INDEX_RUN1_PATH="$bridge_dir/run1-INDEX.json" \
LWC195_EXACT_INDEX_RUN2_PATH="$bridge_dir/run2-INDEX.json" \
LWC195_RAW_SOURCE_PATH="$bridge_dir/source.md" \
LWC197_MIGRATED_CONFIG_PATH="$bridge_dir/migrated-synto.toml" \
  go test ./cmd/olw_worker -run '^TestExactSyntoPackExportBridge$' -count=1 -v
```

Expected Python output includes non-zero run1/run2 INDEX sizes,
`EXACT_PACK_RUN1_RUN2_ARTICLE_ENTITY_CONTINUITY=PASS`, and
`EXACT_PACK_SOURCE_EDGE_INDEPENDENT_HASH=PASS`. The verbose Go selector must
print `LWC195_RUN1_RUN2_NON_EMPTY_ENTITY_CONTINUITY=PASS`,
`LWC195_INDEPENDENT_SOURCE_HASH=PASS`,
`LWC195_STABLE_LWC_ID_REACTIVATED=PASS`, and
`LWC195_CHANGED_SOURCE_DORMANT_STABLE_ID=PASS`. The exact OLW baseline path
is mandatory for the manual-edit companion; neither command makes provider
calls.

`OLW_BASELINE_ROOT` is required. The gate runs the manual-edit zero-provider-call
companion against the exact OLW 0.8.5 source tree in the same process boundary;
the companion's existing `llm_calls == 0` contract must pass before any parent
PASS markers are printed.

The exact-release manual-edit parity procedure is
`testdata/synto_manual_edit_smoke.py`. Run it with the Synto 0.7.0 environment
and `OLW_BASELINE_ROOT` pointing to the exact OLW 0.8.5 source tree:

```sh
OLW_BASELINE_ROOT=/path/to/olw-0.8.5 \
  python cmd/olw_worker/testdata/synto_manual_edit_smoke.py
```

It invokes `migrate-olw --vault`, verifies byte-preserved human edits and the
tracked baseline hash, runs ordinary compile with a provider that fails if
called, and requires `deferred_manual_edit`, no draft/failure, and zero calls.
It is intentionally separate from `go test ./...` and requires no network.

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
- The default worker command intentionally excludes initialization.
- `--init` is rejected; initialization is outside the accepted Synto command
  contract.
- Existing `wiki.toml` files are preserved. If a project vault already contains
  local Ollama config, it must be migrated to DeepSeek manually or through a
  separate repair task.
- Secrets should stay in Secret Manager env vars and should not be persisted
  into vault config.
- Smoke tests must not mount developer OLW config. Use explicit env vars and
  rely on the worker's isolated `XDG_CONFIG_HOME`.
- The legacy raw trigger-file path has been removed from code, but callers must
  use the v1 pipeline endpoints.
