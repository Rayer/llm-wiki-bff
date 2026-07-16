# LWC-77 Raw Files API Design

## Goal

Add a backend-owned raw files listing for project-scoped `raw/` files.

The endpoint should let clients display uploaded/imported raw files with file
metadata and a conservative `ingested` flag. Runtime API requests must not open
or query OLW's `.olw/state.db`; OLW state is projected into a cache artifact
after each successful pipeline run.

## Scope

In scope:

- Add `GET /api/v1/raw`.
- List direct regular files under the current project's `raw/` directory.
- Return `name`, `size`, `updated`, `sha256`, and `ingested`.
- Generate `cache/raw_status.json` during worker postprocess.
- Use `.olw/state.db.raw_notes` as the source for raw ingest status.
- Support both production GCS storage and local filesystem development storage.

Out of scope:

- Frontend sidebar/table work.
- Runtime SQLite reads from BFF handlers.
- Recursive raw directory listing.
- Download or preview endpoints.
- Reading GCS object contents to backfill missing SHA256 metadata.

## API Contract

`GET /api/v1/raw` is registered with the existing project-scoped v1 routes,
behind auth and `ProjectMiddleware`, next to `/sources`, `/concepts`, and
`/raw/upload`.

Response:

```json
{
  "files": [
    {
      "name": "seed.md",
      "size": 12345,
      "updated": "2026-07-09T10:00:00Z",
      "sha256": "abc123...",
      "ingested": true
    }
  ]
}
```

Behavior:

- Missing `raw/` directory returns `200` with `{"files":[]}`.
- Missing `cache/raw_status.json` returns `200`; every raw file is reported with
  `ingested: false`.
- Malformed `cache/raw_status.json` returns `500`, because the project artifact
  exists but cannot be trusted.
- Raw listing/storage failures return the existing JSON error shape.

## Raw Status Artifact

Worker postprocess writes:

`cache/raw_status.json`

Schema:

```json
{
  "version": 1,
  "generated_at": "2026-07-09T10:00:00Z",
  "files": {
    "seed.md": {
      "path": "raw/seed.md",
      "sha256": "abc123...",
      "olw_status": "ingested",
      "ingested": true,
      "ingested_at": "2026-06-21T16:25:25Z",
      "error": ""
    }
  }
}
```

The public API only exposes the derived `ingested` boolean. The artifact keeps
`olw_status`, `ingested_at`, and `error` for operational debugging.

Artifact generation rules:

- Read `.olw/state.db` once during worker postprocess.
- Query `raw_notes(path, content_hash, status, ingested_at, error)`.
- Scan current direct files under `raw/`.
- Join by `raw/{filename}`.
- Write entries keyed by filename.
- If `.olw/state.db` is absent, write an empty artifact with `files: {}`.
- If the state DB exists but has an incompatible schema or query error,
  postprocess fails.

## Ingested Rule

A raw file is `ingested: true` only when all of these are true:

```text
status entry exists
AND entry.sha256 == current raw file sha256
AND entry.olw_status IN ("ingested", "compiled")
AND entry.error == ""
```

All other cases are `ingested: false`, including:

- Missing `cache/raw_status.json`.
- Missing file entry in `cache/raw_status.json`.
- Raw file content changed after the status artifact was generated.
- OLW status is not an accepted ingested status.
- OLW recorded an error for the raw file.
- Current raw file SHA256 is unavailable.

## Storage Changes

Add a storage-level raw file listing model and method so handlers remain storage
neutral:

```go
type RawFile struct {
    Name    string
    Path    string
    Size    int64
    Updated time.Time
    SHA256  string
}

ListRawFiles(ctx context.Context) ([]RawFile, error)
```

GCS implementation:

- Use the object iterator under `{project}/raw/`.
- Only include direct child objects.
- Skip directory markers.
- Use object attrs for `size` and `updated`.
- Use object metadata key `sha256` for digest.
- Do not read object contents to compute missing SHA256 values.

Local filesystem implementation:

- Read direct regular files under `raw/`.
- Use file info for `size` and `updated`.
- Compute SHA256 from local file contents.

## Worker Changes

`cmd/olw_worker` postprocess already rebuilds:

- `cache/id_map.json`
- `cache/concepts.jsonl`

Extend postprocess to also build:

- `cache/raw_status.json`

This should run after OLW commands succeed and as part of postprocess-only runs.
The BFF should continue to work when the artifact is missing, but new pipeline
runs should produce it.

## Demo Data

Add `demo/users/local-user/projects/demo/cache/raw_status.json`.

The demo project does not include `.olw/state.db`, so the seed artifact uses
`files: {}`. With an empty artifact, `raw/seed.md` appears with
`ingested: false`.

## Error Handling

`GET /api/v1/raw` should preserve the existing error response style:

```json
{"error":"message"}
```

No new frontend-only error contract is required for missing raw status, because
missing status is handled as a successful response with conservative
`ingested: false`.

## Known Limitation

For GCS raw objects missing `sha256` metadata, the endpoint does not read object
contents to compute a digest. Those files return `sha256: ""` and
`ingested: false`.

This avoids turning a list endpoint into N object-content reads. The normal
upload and sync paths should write SHA256 metadata for new files.

## Test Plan

Unit tests:

- Local filesystem `ListRawFiles` lists direct files, returns size, update time,
  and SHA256, and ignores nested files/directories.
- GCS helper path filtering keeps only direct `raw/` child objects.
- Raw status JSON parsing handles present, missing, malformed, changed-hash,
  error, and accepted status cases.
- Worker raw status generation handles missing state DB as empty status.
- Worker raw status generation fails on incompatible state DB schema.
- Handler `GET /api/v1/raw` returns raw metadata with `ingested` merged from the
  artifact.
- Handler returns `ingested: false` when `cache/raw_status.json` is missing.

Integration/local checks:

- `go test ./cmd/olw_worker ./internal/gcs ./internal/localfs ./internal/handler/v1`
- In local mode, `GET /api/v1/raw` against the demo project returns `seed.md`.
