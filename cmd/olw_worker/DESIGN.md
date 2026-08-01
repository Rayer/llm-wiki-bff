# LWC Synto Worker PR Report

## Summary

This change adds `cmd/olw_worker` as the Cloud Run Job entrypoint for running
Synto 0.7.0 against a project selected by user and project IDs. The production
cloud path uses a scoped GCS `objectStore` through the Cloud Storage API. It
materializes the selected project into a private `/tmp/olw-cloud-*` workspace,
runs Synto and postprocess there, and publishes validated immutable generation
objects back to that project prefix.

The active pipeline path is now:

```text
BFF v1 endpoint -> Cloud Run Jobs API -> olw-pipeline-dev job
  -> scoped GCS Storage API objectStore -> private /tmp/olw-cloud-* workspace
  -> bounded Synto + postprocess -> immutable generation objects
  -> current.json CAS commit -> worker-owned receipts
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

Query-chip-only command (regenerates `cache/suggested_queries.json` without
Synto, index rebuild, or reconcile):

```bash
worker suggested-queries --vault /path/to/project
```

`worker postprocess` still runs cache/index rebuild and then the suggested-queries
stage. Pass `--no-suggested-queries` to rebuild index artifacts without calling
the LLM chip generator.

Cloud mode supports the standalone stage on the same Job image:

```bash
# Cloud Run containerOverrides.args
["suggested-queries"]
# with USER_ID, PROJECT_ID, BUCKET (job env), EXECUTION_ID from Cloud Run
```

Admin BFF: `POST /api/v1/admin/projects/{id}/pipeline` with
`{"stage":"suggested-queries"}` (see LWC-232).

The worker rejects every second command, `--force` in any position, and
mutation-capable Synto commands such as compile, ingest, identity curation,
undo, import/export, watch, query, or MCP. It creates a safe `synto.toml` only
for a fresh Synto-only vault; it never overwrites an existing config.

## Execution Modes

Cloud production requires `--bucket`, `--user-id`, and `--project-id`. The
worker rejects `--vault`, `--data-dir`, and `--workspace` in this mode; cloud
workers do not use a mounted project filesystem. The object-store prefix is
`users/{USER_ID}/projects/{PROJECT_ID}/`, and all reads, writes, lists, leases,
generations, and receipts are scoped to that prefix.

Local `--vault` mode remains separately supported for local review and repair.
It resolves `--vault` or `VAULT_PATH` (with the legacy `DATA_DIR` project layout
as a fallback), uses the local project directory, and retains the local private
workspace publication/recovery path. Local mode does not use the cloud
`current.json` object manifest.

## Cloud Run Flow

1. Parse and validate the complete command batch before taking a lease or
   starting a child. The production contract is `[ ["run", "--auto-approve"] ]`.
2. Acquire an exclusive create-only GCS lease at
   `users/{USER_ID}/projects/{PROJECT_ID}/.lwc/publish/lease.json` before
   materialization. The lease payload is JSON
   `{"execution":"<execution-id>","started_at":"..."}` with the real pipeline
   execution id (not redacted) so stuck locks can be attributed. It is held
   through publication, receipts, failure-log recording, and cleanup. On
   create-only conflict the worker probes the holder execution via the Cloud
   Run Jobs Admin API (`executions.get`, scoped to `CLOUD_RUN_JOB` /
   `PIPELINE_JOB_NAME`). **RUNNING** (or uncertain lookup) fails closed with
   the stable public sentinel. **Terminal** holders (`SUCCEEDED` / `FAILED` /
   `CANCELLED`) or proven **not found** job-scoped ids are reclaimed once via
   generation-matched delete + create-only rewrite (LWC-222). Pure age/TTL
   steal is forbidden. Malformed / foreign / redacted holders still need
   operator break-glass (`docs/DEPLOYMENT.md`).
3. Create a private `/tmp/olw-cloud-*` directory. Materialize canonical raw
   objects, annotations, source status, and the previously committed generation
   selected by `.lwc/publish/current.json` through the Storage API. If no current
   manifest exists, bounded legacy generation-owned objects are materialized.
   Symlinks and escaping workspace paths are rejected.
4. Snapshot mapped source bytes and annotation digests. Add deterministic
   annotation trailers only in the workspace; stored raw objects are never
   modified. Validate coherent Synto/legacy state, migrate only in the
   workspace, run the bounded `synto run`, export the exact-release pack, and
   install/validate its `index/INDEX.json` as `.synto/INDEX.json` before
   postprocess and reconciliation.
5. Preflight the complete explicit output allowlist. Upload each validated file
   under the new immutable `.lwc/publish/generations/{generation_id}/` prefix.
   The `.lwc/publish/current.json` object is the generation commit point: write
   it with a create-only or prior-object-generation CAS condition, and include
   `previous_generation_id` when replacing an existing current manifest.
6. A definite CAS conflict leaves the prior/current competing manifest intact;
   any already-uploaded immutable objects are unreachable staging leftovers, not
   a partial live generation, and failure receipts are written. If the manifest
   write or its readback is ambiguous, return `errManifestCommitOutcomeUnknown`:
   the manifest may already be committed, so do not write a failure receipt or
   typed failure diagnostic that could falsify publication truth. Publish the
   raw execution log on a best-effort recording context. Once `current.json` is
   confirmed committed, later receipt or lease-cleanup failures preserve that
   committed generation and are reported as committed-with-recording/cleanup
   errors.
7. After a confirmed commit, write the complete bounded raw execution log and
   merge the source-status receipt using the exact start raw/annotation
   fingerprints. On pre-commit failure, retain the last committed generation,
   write the failure receipt/diagnostic and raw log, and clean up only the
   private workspace. The raw log is never a generation member and remains
   subject to the documented 4 MiB contract below.

`--stop-on-error` defaults to true. When false, the worker continues through the
batch, records failures, skips postprocess if any command failed, and exits
non-zero at the end.

The default production generation includes INDEX generation, postprocess,
reconciliation, and preflight before publication. Local `--no-postprocess`
compatibility runs still execute only in a private scratch workspace and never
publish generation outputs.

## Cloud Failure Diagnostic Artifact

For every owned cloud execution, the worker writes the bounded raw child
stdout/stderr object `cache/pipeline-<execution>.log` alongside the typed
operator diagnostic `cache/pipeline-<execution>.failure.json` when applicable.
Child Synto stdout/stderr is always captured into the execution-scoped pipeline
log when an execution id is present. By default it is also teed to the process
console (redacted). Cloud mode must not force console silence: local bucket
runs and Cloud Logging both need live progress. Pass `--suppress-output` only
when intentionally quiet. The log is persisted on success and every failure
after ownership is established. Its documented maximum is 4 MiB including the
deterministic marker `\n[output truncated at 4194304 bytes]\n`; the marker
replaces the tail when the child exceeds the cap. The failure object is
versioned JSON and is written create-only; it is not part of a generation,
does not create a current pointer, and is never written on success. A
diagnostic write failure is reported through the existing fixed
failure-recording category and cannot publish a generation.

The payload is bounded to 4 KiB and contains `version: 1`, `status: "failed"`,
a finite stage, a finite error class, the pipeline `execution` id, a bounded
redacted `message` (root cause text with credentials scrubbed), and when proven
at a child-process boundary a finite child command plus numeric exit code.
Stages include input materialization, Synto migration/config/run/index export,
source/concept reconciliation, postprocess, generation publication, receipt
recording, and lease cleanup. Error classes include validation, child exit,
timeout, cancellation, I/O, invalid state, publication conflict, recording
failure, and unknown. The accepted user command is only `run`; migration and
index export are worker-owned child seams.

Credentials (`LLM_API_KEY` / `DEEPSEEK_API_KEY` / configured API key) are always
redacted from `message`, pipeline logs, and process exit lines. Control-plane
identity (execution id, tenant/project ids, paths, stage text) is retained for
operators. Source receipt `error` remains the fixed tenant-facing string
`pipeline failed`. Process exit logs preserve operational public boundaries and
their unwrapped causes (for example
`pipeline publish lease unavailable: object generation conflict`). Only
cobra/CLI parse failures collapse to the fixed `worker command rejected` string
so user-supplied tokens are not echoed. Operational failures use stable public
boundary sentinels (so callers and tests can match `Error()` / `errors.Is`) but
must wrap the root cause with `annotateError` rather than replacing it.

The worker is the only producer-side cap: it publishes one complete bounded
execution artifact of at most 4 MiB. The explicit PipelineLog fetch returns
that artifact without head/tail sampling or a second API truncation marker.
Status polling fetches execution metadata only; it does not fetch or proxy
Cloud Logging output. Raw text is never parsed as a stage contract; the typed
diagnostic remains the stable status/timeline/automation contract.

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

## Historical Verification Note

Pre-LWC-170 deployment and mounted-filesystem smoke results are intentionally
not retained here as current architecture or release evidence. Validate the
active Storage API object-store path with the repository tests and deployment
workflow for the revision under review.

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
each destination.

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

- Full project pipeline runs can be long. Keep the Cloud Run Job timeout above
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
