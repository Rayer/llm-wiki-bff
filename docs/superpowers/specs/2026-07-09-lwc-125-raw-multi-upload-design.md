# LWC-125 Raw Multi Upload — BFF Design

## Goal

Extend `POST /api/v1/raw/upload` so same-filename uploads have correct
idempotent / conflict semantics. Multi-file batching stays on the frontend
(per-file calls to this endpoint).

## Scope

In scope:

- Same filename + same SHA256 → `200` + `status: "already_exists"` (no rewrite)
- Same filename + different SHA256 → `409 Conflict` (no rewrite)
- New filename → `201` + `status: "created"` (existing write path)
- Response includes `status` field
- Unit tests for the three outcomes

Out of scope:

- Cross-file content-hash dedupe
- Multipart batch endpoint
- Atomic generation-preconditioned writes (TOCTOU accepted for v1)
- Upload progress / XHR

## API Contract

`POST /api/v1/raw/upload` (auth + project middleware, multipart field `file`).

Success body:

```json
{
  "filename": "note.md",
  "path": "users/{uid}/projects/{pid}/raw/note.md",
  "bytes": 1234,
  "sha256": "abc…",
  "status": "created"
}
```

| HTTP | `status` | Behavior |
|------|----------|----------|
| 201 | `created` | Object did not exist; written |
| 200 | `already_exists` | Object exists with same content hash; not rewritten |
| 409 | — | Object exists with different content; body `{"error":"filename already exists with different content"}` |

Existing validation errors (empty, too large, bad filename, auth, project not ready) unchanged.

## Algorithm

After validate + read body + compute digest:

1. `existing, err := store.GetMetaSHA256(ctx, "raw/"+filename)`
2. If `err` → 500
3. If `existing == ""`, resolve existence via `ReadFile`:
   - not exist → write → 201 `created`
   - exist → `existing = sha256(file bytes)`
   - other error → 500
4. If `existing == digest` → 200 `already_exists` (no write)
5. If `existing != digest` → 409 (no write)
6. Else write → 201 `created`

Step 3 covers GCS objects missing `sha256` metadata and localfs (which hashes on read).

## Compatibility

- Clients that ignore unknown `status` still work for new uploads (`filename`/`bytes`/`sha256` unchanged keys).
- Field remains `sha256` (not `digest`).

## Testing

- Helper/unit coverage for decision branches (created / already_exists / conflict)
- Prefer pure functions where possible so tests do not need full GCS
