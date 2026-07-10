# LWC-138 — Pipeline rate limiting (BFF)

## Goal

Stop uncontrolled LLM spend from repeated pipeline triggers by enforcing **per-project** limits on `POST /api/v1/pipeline/run` before any Cloud Run Job is started, and by exposing a **read-only quota snapshot** so the frontend can disable the Run button with clear reasons.

## Background

Today:

- `PipelineRun` and `AdminPipelineTrigger` both invoke Cloud Run with no cost controls.
- Demo is blocked only in the frontend (`PipelineClient` + `isDemoSession`).
- Firestore already has per-project **locks** and **executions**; there is no `pipeline_quota` document.
- IP middleware rate limits API QPS, not pipeline cost.

LLM cost scales with pipeline runs, not with HTTP request rate. Enforcement must live in the BFF on the trigger path.

## Scope

### In scope

- Per-project quota document in Firestore (`pipeline_quota`)
- Pre-trigger checks on user `POST /api/v1/pipeline/run`
- Quota fields on `GET /api/v1/pipeline/status` (read-only evaluation)
- Shared Cloud Run trigger helper used by user + admin endpoints
- Admin `POST /api/v1/admin/projects/:id/pipeline` bypasses **quota** limits (daily / cooldown / new-raw) but still respects **already_running** unless force is added later (out of scope)
- Configurable limits via env
- Local mode behavior when Firestore is unavailable
- Unit tests for check / reserve / refund / status payload

### Out of scope

- Worker changes to increment quota (worker must **not** own the counter)
- Global concurrent platform cap (title mentioned “global limits”; v1 uses existing lock + optional future `CountActiveLocks` cap)
- Per-user daily cap across all projects
- Frontend implementation (see frontend design sibling)
- Changing Cloud Run job payload / OLW commands

## Limits (locked)

| Rule | Default | Notes |
|------|---------|--------|
| Daily runs per project | **2** | UTC calendar day (`YYYY-MM-DD` UTC) |
| Cooldown | **1 hour** | From last **accepted** reservation `lastRunAt` |
| Min new/modified raw | **≥ 1** | See “New raw” definition |
| Already running | block | Active Firestore lock **or** last Cloud Run execution `RUNNING` |
| Demo users | block on user path | See “Demo identity” |
| Admin user path | no special case | Only **admin endpoint** bypasses quota |
| Failed pipeline after accept | **counts** | Cost may already be incurred |

Env overrides (names fixed for implementation):

- `PIPELINE_DAILY_LIMIT` (default `2`)
- `PIPELINE_COOLDOWN_SECONDS` (default `3600`)
- `PIPELINE_MIN_NEW_RAW` (default `1`)
- `PIPELINE_DEMO_USER_IDS` (comma-separated user IDs; default empty)

## Architecture

```
POST /api/v1/pipeline/run
  1. Resolve userID + projectID (existing middleware)
  2. If userID ∈ PIPELINE_DEMO_USER_IDS → 403 demo
  3. If already_running (lock or RUNNING execution) → 409
  4. Firestore transaction on pipeline_quota/{userID}__{projectID}:
       - Load or create doc
       - Reset day bucket if dayKey != today UTC
       - Evaluate daily / cooldown / newRaw
       - On allow: reserve (runsToday++, lastRunAt=now, dayKey=today)
  5. Trigger Cloud Run Job (shared helper)
  6. On trigger failure: refund reservation in a second write
  7. 202 + execution_id + quota snapshot (post-reserve)

GET /api/v1/pipeline/status
  - Existing execution status
  - Plus quota snapshot from evaluate-only (no mutate)
```

### Quota document

Collection: `pipeline_quota`  
Doc ID: `{userID}__{projectID}` (same pattern as locks)

```json
{
  "user_id": "u1",
  "project_id": "p1",
  "day_key": "2026-07-10",
  "runs_today": 1,
  "last_run_at": "2026-07-10T08:15:00Z",
  "updated_at": "2026-07-10T08:15:00Z"
}
```

- **Reserve** happens only in the BFF transaction before Cloud Run.
- **Refund** only when Cloud Run HTTP invoke fails (non-2xx / transport error) after a successful reserve. Decrement `runs_today` (floor 0) and leave `last_run_at` as-is **or** restore previous `last_run_at` stored in-memory from pre-reserve snapshot — restore previous snapshot is required for cooldown correctness.
- Worker **does not** write this document.

### New raw definition

Using existing `store.ListRawFiles` metadata:

- Let `lastRunAt` be the quota doc’s `last_run_at` (last **accepted** trigger).
- If `lastRunAt` is zero (never run): `newRawCount = len(raw files)`; allow iff `newRawCount >= PIPELINE_MIN_NEW_RAW` (default 1 → need at least one file).
- If `lastRunAt` is set: `newRawCount = count(files where Updated.After(lastRunAt))`.
- Do **not** use `cache/raw_status.json` for this gate (artifact can lag uploads; LWC-129 already established live raw listing).

Empty vault → `no_new_raw` (or zero files on first run).

### Already running

Block if either:

1. `firestore.GetStatus` → `Locked == true` for this user/project, or
2. Latest Cloud Run execution status is `RUNNING` (existing `pipelineExecutionStatus` path)

Reason code: `already_running`. This is independent of the 1h cooldown (pipelines may run longer than 1h).

### Demo identity

Frontend `isDemoSession` is not trusted by the server.

BFF demo block: `userID` is listed in `PIPELINE_DEMO_USER_IDS`.

- Production: set env to the shared Try-demo account user id(s).
- Local: default empty so developers can still trigger pipeline while FE demo mode remains client-gated; set the env in environments that need server-side demo lock.

Admin endpoint never applies demo user block by user path (it uses path project id + admin middleware).

### Local mode

| Condition | Behavior |
|-----------|----------|
| Firestore client nil / unavailable | Skip quota reserve; still attempt Cloud Run (or local worker path as today). Status returns `quota.enforced=false`. |
| Firestore available locally | Full enforce |

No file-based quota store in v1 (YAGNI).

### Shared trigger helper

Extract common Cloud Run invoke used by:

- `PipelineRun`
- `AdminPipelineTrigger`

Signature sketch:

```go
func (h *Handler) invokePipelineJob(ctx context.Context, userID, projectID string) (executionID string, err error)
```

User path: demo → already_running → reserve → invoke → refund-on-fail.  
Admin path: already_running → invoke (no reserve).

## API contract

### Quota object (embedded)

```json
{
  "enforced": true,
  "allowed": false,
  "reason": "cooldown",
  "message": "Cooldown active; try again in 42 minutes",
  "runs_today": 1,
  "daily_limit": 2,
  "cooldown_until": "2026-07-10T09:15:00Z",
  "next_reset": "2026-07-11T00:00:00Z",
  "new_raw_files": 0,
  "min_new_raw": 1,
  "already_running": false
}
```

`reason` enum:

- `""` / omit when allowed
- `demo`
- `daily_limit`
- `cooldown`
- `already_running`
- `no_new_raw`

### `GET /api/v1/pipeline/status`

Extend response:

```json
{
  "project_id": "p1",
  "last_execution": { "...": "existing" },
  "quota": { "...": "Quota object" }
}
```

Evaluate-only: never mutates quota.

### `POST /api/v1/pipeline/run`

**Success (202):**

```json
{
  "status": "accepted",
  "command": "run",
  "project_id": "p1",
  "execution_id": "olw-pipeline-...",
  "quota": { "...": "post-reserve snapshot, allowed may still be false for next run" }
}
```

**Blocked:**

| reason | HTTP |
|--------|------|
| `demo` | 403 |
| `daily_limit` | 429 |
| `cooldown` | 429 (+ optional `Retry-After` seconds) |
| `already_running` | 409 |
| `no_new_raw` | 409 |

Body always includes `error` (short machine/human hybrid for existing FE `error` field) and `quota` object:

```json
{
  "error": "pipeline blocked: cooldown",
  "quota": { "...": "..." }
}
```

Keep top-level `error` string so existing `triggerPipeline()` error toast still works; FE will later parse `quota` when present.

## Control flow priority

Evaluation order (first match wins):

1. `demo`
2. `already_running`
3. `daily_limit` (after day reset)
4. `cooldown`
5. `no_new_raw`
6. allow → reserve

## Acceptance criteria (BFF)

1. Two accepted runs on the same UTC day → third `POST /pipeline/run` returns 429 `daily_limit` and does not call Cloud Run.
2. Second run within 1h of first accepted → 429 `cooldown`, no Cloud Run.
3. Concurrent double-submit: only one reserve succeeds (transaction); the other gets daily/cooldown/already_running as applicable — never two Cloud Run invokes for the same reserve slot.
4. Active lock or RUNNING execution → 409 `already_running` without reserve.
5. No raw files newer than `last_run_at` → 409 `no_new_raw`.
6. First ever run with ≥1 raw file → allowed (subject to other rules).
7. User in `PIPELINE_DEMO_USER_IDS` → 403 `demo` on user path.
8. Admin endpoint still triggers without daily/cooldown/new-raw checks; still 409 if already running.
9. Cloud Run invoke failure after reserve → refund restores previous `runs_today` and `last_run_at`.
10. `GET /pipeline/status` includes evaluate-only `quota` matching the same rules.
11. With `firestore == nil`, user path does not crash; `quota.enforced=false` and trigger proceeds as today.
12. Existing pipeline status/log/admin tests remain green; new unit tests cover reserve/refund/reasons.

## Testing

- Table-driven unit tests for pure evaluate function (day rollover, cooldown boundary, new raw counts).
- Handler tests with fake Firestore transaction (or interface) and stub Cloud Run HTTP client:
  - block paths never hit stub
  - allow path hits stub once
  - refund on stub error
- Local mode nil firestore smoke test

## Compatibility

- Existing 202 accepted shape gains optional `quota` field (backward compatible).
- Status gains `quota` field (backward compatible).
- Admin response unchanged except shared helper internals.
- No worker deploy required for v1.
