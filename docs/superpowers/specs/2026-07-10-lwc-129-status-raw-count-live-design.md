# LWC-129 — Status `raw_count` as live raw/ listing (BFF)

## Goal

Make `GET /api/v1/status` → `raw_count` reflect the **current number of files under `raw/`**, so clients that refresh status after upload show a correct sidebar Raw badge.

## Background

LWC-129 (sidebar badge stale after multi-upload) needs a correct data source.

Current `rawFileCount` prefers `cache/raw_status.json` `file_count` (pipeline postprocess artifact) and only lists `raw/` when the artifact is **missing**. After a project has run the pipeline once, new uploads do not update the artifact until the next run — so `getStatus()` re-fetch still returns the old count.

LWC-127 originally specified Raw badge from `GET /api/v1/raw` file list length. Status `raw_count` should match that live semantics for UI counts.

## Scope

In scope:

- Change `rawFileCount` to use `ListRawFiles` as the source of truth for `raw_count`
- Update unit tests that assumed artifact-only count
- Document that `raw_status.json` remains for **ingested** flags on the raw list page, not for nav counts

Out of scope:

- Frontend changes (separate FE design/spec)
- Updating `raw_status.json` on upload
- Changing `GET /api/v1/raw` response shape

## Behavior

```go
func rawFileCount(ctx context.Context, wikiStore store.Store) int {
    files, err := wikiStore.ListRawFiles(ctx)
    if err != nil {
        return 0
    }
    return len(files)
}
```

| Condition | `raw_count` |
|-----------|-------------|
| N files in project `raw/` | N |
| Empty / missing `raw/` | 0 |
| List error | 0 (existing soft-fail style; status endpoint still 200) |

**Do not** read `cache/raw_status.json` for this field.

## Compatibility

- `sources_count` / `concepts_count` unchanged
- Home / Status UI that display `raw_count` will show live file count (desired)
- Ingested status continues to come only from raw list + artifact join

## Acceptance criteria (BFF)

1. With stale `raw_status.json` (`file_count: 2`) but **3** real files under `raw/`, `GET /api/v1/status` returns `raw_count: 3`.
2. With no `raw_status.json` and 1 raw file, `raw_count: 1`.
3. With no raw files, `raw_count: 0`.
4. Existing status tests for non-raw fields still pass; artifact-based raw count test is replaced by live-list tests.

## Testing

- Replace `TestStatusIncludesRawCountFromArtifact` with tests that plant real files under `raw/` (and optionally a misleading artifact) and assert live `raw_count`.
