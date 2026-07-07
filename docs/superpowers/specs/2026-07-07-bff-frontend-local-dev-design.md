# BFF + Frontend Local Development Design

## Goal

Create a fast local feedback loop for validating BFF API behavior, frontend integration, storage layout, ID routing, wikilinks, cache/index behavior, and demo data without deploying to Cloud Run or requiring GCP credentials.

This change treats LWC-100 as BFF + frontend local development. Worker local development and local pipeline triggering are separate follow-up concerns.

## Non-Goals

- Do not build the worker local development environment in this phase.
- Do not decide whether local pipeline execution should use direct OLW CLI, worker CLI, compose profile, dev container, file queue, or BFF HTTP trigger.
- Do not emulate Cloud Run Jobs, IAM, GCS FUSE, or Firestore locally.
- Do not require pipeline execution before the demo app can load.

## Local Mode Contract

Local mode uses a filesystem root from `--local` or `LOCAL_DATA_DIR`. The directory mirrors the existing GCS object layout:

```text
local-data/
└── users/
    └── local-user/
        └── projects/
            └── demo/
                ├── index.md
                ├── wiki.toml
                ├── cache/
                │   ├── concepts.jsonl
                │   └── id_map.json
                ├── raw/
                └── wiki/
                    ├── *.md
                    └── sources/
                        └── *.md
```

The default demo scope is `local-user/demo`. Auth still accepts explicit local dev headers, and frontend local development should send `X-User-ID: local-user` and `X-Project-ID: demo` for scoped API calls.

## Architecture

### Storage Interface

The BFF should depend on a small storage interface instead of concrete `*gcs.Client` usage in handlers. The interface is derived from current handler/cache/index needs:

- `WithScope(userID, projectID string)`
- `Prefix() string`
- `ReadFile(ctx, relPath)`
- `WriteBytes(ctx, data, relPath)`
- `WriteBytesAtomic(ctx, data, tmpPath, finalPath)`
- `ListProjects(ctx, userID)`
- `ListConcepts(ctx, includeDrafts)`
- `ListSources(ctx)`
- `ListConceptsFromCache(ctx)`
- `ListSourcesFromCache(ctx)`
- `GetPage(ctx, slug, category)`
- `ListMarkdownFiles(ctx, dir)`
- `BucketStats(ctx)`
- `GetMetaSHA256(ctx, relPath)`

`gcs.Client` remains the production implementation. `internal/localfs.Client` implements the same contract against the local filesystem.

### Local Filesystem Store

`internal/localfs` maps scoped paths to files below `{root}/users/{uid}/projects/{pid}`. It must reject path traversal, absolute paths, and unsafe symlink escapes before reading or writing. Missing files should return an error compatible with existing not-found handling, or the handlers should be updated to use a shared not-found helper.

Listings should follow existing GCS behavior:

- `ListProjects` scans `users/{uid}/projects/*/index.md` and returns project IDs.
- `ListConceptsFromCache` reads `cache/concepts.jsonl`.
- `ListSourcesFromCache` reads `cache/id_map.json`.
- `ListConcepts` falls back to markdown files under `wiki/` and `wiki/.drafts/`.
- `ListSources` reads markdown under `wiki/sources/`.
- `GetPage` resolves concepts from `wiki/` first, then `wiki/.drafts/`, and sources from `wiki/sources/`.

Writes should create parent directories as needed. Atomic writes should write a temporary file and rename inside the same filesystem.

### BFF Startup

`main.go` selects storage at startup:

- If `--local` or `LOCAL_DATA_DIR` is set, use `localfs.New(root)`.
- Otherwise, use `gcs.NewClient(cfg.Bucket)`.

Firestore is optional in local mode. Local project listing comes from the filesystem and must not fail because Firestore is unavailable.

### Rebuild, Cache, and Index Paths

BFF local mode should allow cache/index/rebuild code paths to run against filesystem storage. Rebuild should not require Firestore locking in local mode. Use an injected local rebuild function or a local lock path so the endpoint can exercise the same index generation logic without Firestore.

### Demo Data

The repo should include `demo/users/local-user/projects/demo/` with prebuilt artifacts. The app should be useful immediately after `make seed`, without running OLW or the worker.

The demo must contain enough data to verify:

- project listing
- concept listing
- source listing
- concept detail
- source detail
- wikilinks
- ID routing through `cache/id_map.json`
- concept cache through `cache/concepts.jsonl`

### Developer Entrypoints

Docker Compose is the canonical integration quickstart:

```sh
make seed
make dev
```

CLI targets support faster inner-loop work:

```sh
make bff-local
make clean-local
```

`make pipeline` is not part of first-phase acceptance. Pipeline/worker regeneration should be specified separately.

## Error Handling

- Local path validation failures return 400-style errors where caused by request input, or 500-style errors where caused by internal path construction.
- Missing local files map to existing not-found responses.
- Empty or missing local demo data should produce actionable startup or endpoint errors pointing to `make seed`.
- Local mode logs should clearly state that GCS and Firestore are not required.

## Testing

Add focused unit tests for:

- localfs path traversal rejection
- localfs project listing
- localfs cache listing
- localfs markdown fallback listing
- localfs page reads for concepts, drafts, and sources
- localfs writes and atomic writes
- BFF startup/storage selection where practical
- handler project listing without Firestore in local mode
- rebuild/cache/index path against local storage

Add smoke verification for:

- `GET /api/v1/projects`
- `GET /api/v1/concepts`
- `GET /api/v1/concepts/{id}`
- `GET /api/v1/sources`
- `GET /api/v1/sources/{id}`

## Acceptance Criteria

- `make seed && make dev` starts BFF on `:8080` and frontend on `:3000`.
- BFF local mode starts without GCP credentials.
- Local project list comes from filesystem data, not Firestore.
- Concepts and sources are visible in frontend from prebuilt demo artifacts.
- Concept/source detail pages work.
- Wikilinks work within local concept pages.
- Local cache/index/rebuild code paths can be tested without GCS.
- `local-data` can be deleted and reseeded repeatably.
- `docs/LOCAL_DEV.md` covers quickstart, inner-loop usage, and troubleshooting.
- Worker local development and pipeline triggering remain explicitly deferred.
