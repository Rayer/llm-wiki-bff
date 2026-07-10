# LWC-138 Pipeline Rate Limiting — BFF Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enforce per-project pipeline quota on `POST /api/v1/pipeline/run` before Cloud Run, expose evaluate-only `quota` on `GET /api/v1/pipeline/status`, and share the Cloud Run trigger helper with admin.

**Architecture:** Pure evaluation in `internal/pipelinequota`; Firestore transaction reserve/refund on `pipeline_quota/{user}__{project}`; Handler checks demo → already_running → reserve → invoke → refund-on-fail. Worker does not write quota.

**Tech Stack:** Go, Gin, Firestore client, existing Cloud Run HTTP invoke pattern, `go test`.

**Spec:** `docs/superpowers/specs/2026-07-10-lwc-138-pipeline-rate-limit-design.md`

## Global Constraints

- Daily limit default **2**, cooldown **3600s**, min new raw **1**, UTC day key
- Reserve only on BFF; refund only on Cloud Run invoke failure after reserve
- Failed pipeline after accept **counts**
- Admin path skips daily/cooldown/new-raw; still blocks already_running
- `firestore == nil` → `enforced=false`, no crash
- Env: `PIPELINE_DAILY_LIMIT`, `PIPELINE_COOLDOWN_SECONDS`, `PIPELINE_MIN_NEW_RAW`, `PIPELINE_DEMO_USER_IDS`

## File map

| File | Responsibility |
|------|----------------|
| Create `internal/pipelinequota/quota.go` | Types, evaluate pure function, messages, reason constants |
| Create `internal/pipelinequota/quota_test.go` | Table tests for day roll, cooldown, new raw, priority |
| Modify `internal/firestore/client.go` | `ReservePipelineQuota`, `RefundPipelineQuota`, `GetPipelineQuota` with transactions |
| Create `internal/firestore/quota_test.go` | Unit tests with fake/txn helpers if feasible; otherwise pure mapping tests |
| Create `internal/handler/v1/pipeline_quota.go` | Wire evaluate inputs (raw list, lock, running), HTTP status mapping |
| Modify `internal/handler/v1/endpoints.go` | `PipelineRun`, `PipelineStatus`, extract `invokePipelineJob`, admin path |
| Modify `internal/handler/v1/handler.go` | Optional config fields for limits / demo user set |
| Modify `internal/config/config.go` (+ tests) | Load pipeline quota env defaults |
| Modify `main.go` | Pass config into handler if needed |
| Modify `internal/handler/v1/handler_test.go` | New block/refund tests; keep existing run tests green |

---

### Task 1: Pure quota evaluation package

**Files:**
- Create: `internal/pipelinequota/quota.go`
- Create: `internal/pipelinequota/quota_test.go`

**Interfaces:**
- Produces: `type Reason string`, `type Snapshot struct`, `type Limits struct`, `type Input struct`, `func Evaluate(in Input) Snapshot`

- [ ] **Step 1: Write failing tests**

```go
package pipelinequota

import (
	"testing"
	"time"
)

func TestEvaluateDailyLimit(t *testing.T) {
	now := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	got := Evaluate(Input{
		Now: now,
		Limits: Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		RunsToday: 2,
		DayKey: "2026-07-10",
		LastRunAt: now.Add(-2 * time.Hour),
		NewRawFiles: 3,
	})
	if got.Allowed || got.Reason != ReasonDailyLimit {
		t.Fatalf("got %+v", got)
	}
}

func TestEvaluateDayRolloverResetsCount(t *testing.T) {
	now := time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)
	got := Evaluate(Input{
		Now: now,
		Limits: Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		RunsToday: 2,
		DayKey: "2026-07-10",
		LastRunAt: now.Add(-2 * time.Hour),
		NewRawFiles: 1,
	})
	if !got.Allowed || got.RunsToday != 0 {
		t.Fatalf("expected allow with reset runs, got %+v", got)
	}
}

func TestEvaluateCooldown(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	last := now.Add(-30 * time.Minute)
	got := Evaluate(Input{
		Now: now,
		Limits: Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		RunsToday: 1,
		DayKey: "2026-07-10",
		LastRunAt: last,
		NewRawFiles: 2,
	})
	if got.Allowed || got.Reason != ReasonCooldown {
		t.Fatalf("got %+v", got)
	}
}

func TestEvaluateNoNewRaw(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	got := Evaluate(Input{
		Now: now,
		Limits: Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		RunsToday: 0,
		DayKey: "2026-07-10",
		NewRawFiles: 0,
	})
	if got.Allowed || got.Reason != ReasonNoNewRaw {
		t.Fatalf("got %+v", got)
	}
}

func TestEvaluateAlreadyRunningPriority(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	got := Evaluate(Input{
		Now: now,
		Limits: Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		AlreadyRunning: true,
		RunsToday: 2,
		DayKey: "2026-07-10",
		NewRawFiles: 0,
	})
	if got.Reason != ReasonAlreadyRunning {
		t.Fatalf("want already_running first, got %+v", got)
	}
}

func TestEvaluateDemoPriority(t *testing.T) {
	got := Evaluate(Input{
		Now: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		Limits: Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		IsDemo: true,
		AlreadyRunning: true,
		NewRawFiles: 0,
	})
	if got.Reason != ReasonDemo {
		t.Fatalf("want demo first, got %+v", got)
	}
}

func TestCountNewRawFiles(t *testing.T) {
	last := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	files := []time.Time{
		last.Add(-time.Hour),
		last.Add(time.Minute),
	}
	if n := CountNewRaw(files, last); n != 1 {
		t.Fatalf("n=%d", n)
	}
	if n := CountNewRaw(files, time.Time{}); n != 2 {
		t.Fatalf("first run n=%d", n)
	}
}
```

- [ ] **Step 2: Run tests — expect FAIL**

```bash
cd /Users/rayer/Documents/Develop/llm-wiki-bff
go test ./internal/pipelinequota/ -count=1
```

Expected: package not found / undefined Evaluate

- [ ] **Step 3: Implement `quota.go`**

```go
package pipelinequota

import (
	"fmt"
	"time"
)

type Reason string

const (
	ReasonNone            Reason = ""
	ReasonDemo            Reason = "demo"
	ReasonDailyLimit      Reason = "daily_limit"
	ReasonCooldown        Reason = "cooldown"
	ReasonAlreadyRunning  Reason = "already_running"
	ReasonNoNewRaw        Reason = "no_new_raw"
)

type Limits struct {
	DailyLimit int
	Cooldown   time.Duration
	MinNewRaw  int
}

type Input struct {
	Now            time.Time
	Limits         Limits
	IsDemo         bool
	AlreadyRunning bool
	RunsToday      int
	DayKey         string // UTC YYYY-MM-DD stored on doc
	LastRunAt      time.Time
	NewRawFiles    int
	Enforced       bool // if false, always allow (local no firestore)
}

type Snapshot struct {
	Enforced        bool       `json:"enforced"`
	Allowed         bool       `json:"allowed"`
	Reason          Reason     `json:"reason,omitempty"`
	Message         string     `json:"message,omitempty"`
	RunsToday       int        `json:"runs_today"`
	DailyLimit      int        `json:"daily_limit"`
	CooldownUntil   *time.Time `json:"cooldown_until,omitempty"`
	NextReset       time.Time  `json:"next_reset"`
	NewRawFiles     int        `json:"new_raw_files"`
	MinNewRaw       int        `json:"min_new_raw"`
	AlreadyRunning  bool       `json:"already_running"`
}

func DayKeyUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func NextResetUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

func CountNewRaw(updatedTimes []time.Time, lastRunAt time.Time) int {
	if lastRunAt.IsZero() {
		return len(updatedTimes)
	}
	n := 0
	for _, u := range updatedTimes {
		if u.After(lastRunAt) {
			n++
		}
	}
	return n
}

func Evaluate(in Input) Snapshot {
	lim := in.Limits
	if lim.DailyLimit <= 0 {
		lim.DailyLimit = 2
	}
	if lim.Cooldown <= 0 {
		lim.Cooldown = time.Hour
	}
	if lim.MinNewRaw <= 0 {
		lim.MinNewRaw = 1
	}
	now := in.Now.UTC()
	today := DayKeyUTC(now)
	runs := in.RunsToday
	if in.DayKey != today {
		runs = 0
	}
	snap := Snapshot{
		Enforced:       in.Enforced,
		RunsToday:      runs,
		DailyLimit:     lim.DailyLimit,
		NextReset:      NextResetUTC(now),
		NewRawFiles:    in.NewRawFiles,
		MinNewRaw:      lim.MinNewRaw,
		AlreadyRunning: in.AlreadyRunning,
	}
	if !in.Enforced {
		snap.Allowed = true
		return snap
	}
	// priority
	if in.IsDemo {
		return block(snap, ReasonDemo, "Demo sessions cannot run the pipeline")
	}
	if in.AlreadyRunning {
		return block(snap, ReasonAlreadyRunning, "A pipeline is already running for this project")
	}
	if runs >= lim.DailyLimit {
		return block(snap, ReasonDailyLimit, fmt.Sprintf("Daily limit reached (%d/%d)", runs, lim.DailyLimit))
	}
	if !in.LastRunAt.IsZero() {
		until := in.LastRunAt.UTC().Add(lim.Cooldown)
		if now.Before(until) {
			snap.CooldownUntil = &until
			mins := int(until.Sub(now).Minutes()) + 1
			return block(snap, ReasonCooldown, fmt.Sprintf("Cooldown active; try again in %d minutes", mins))
		}
		snap.CooldownUntil = &until
	}
	if in.NewRawFiles < lim.MinNewRaw {
		return block(snap, ReasonNoNewRaw, "Upload at least one new or modified raw file before running")
	}
	snap.Allowed = true
	return snap
}

func block(s Snapshot, r Reason, msg string) Snapshot {
	s.Allowed = false
	s.Reason = r
	s.Message = msg
	return s
}
```

- [ ] **Step 4: Run tests — expect PASS**

```bash
go test ./internal/pipelinequota/ -count=1
```

- [ ] **Step 5: Commit**

```bash
git add internal/pipelinequota/
git commit -m "feat(LWC-138): pure pipeline quota evaluation"
```

---

### Task 2: Firestore quota reserve / refund

**Files:**
- Modify: `internal/firestore/client.go`
- Create: `internal/firestore/quota.go` (preferred split) + `quota_test.go` for doc ID helpers

**Interfaces:**
- Produces:
  - `func QuotaDocID(userID, projectID string) string`
  - `func (c *Client) LoadQuotaState(ctx, userID, projectID) (runsToday int, dayKey string, lastRunAt time.Time, err error)`
  - `func (c *Client) ReserveQuota(ctx, userID, projectID, limits, now) (prev snapshot for refund, eval pipelinequota.Snapshot, err error)`
  - `func (c *Client) RefundQuota(ctx, userID, projectID, prevRuns, prevDayKey string, prevLastRunAt time.Time) error`

Use `c.fs.RunTransaction`. Collection name: `pipeline_quota`. Doc ID: `fmt.Sprintf("%s__%s", userID, projectID)`.

Reserve algorithm inside txn:
1. Read doc (missing → zeros)
2. Build `pipelinequota.Input` with loaded state + caller-supplied IsDemo/AlreadyRunning/NewRawFiles/Enforced=true
3. If !Allowed → return snapshot, do not write
4. Else write runs_today=runs+1 (after day reset), day_key=today, last_run_at=now, user_id, project_id, updated_at
5. Return previous values for refund

Refund: set fields back to previous snapshot values.

- [ ] **Step 1: Implement methods + a small test for QuotaDocID**

```go
func TestQuotaDocID(t *testing.T) {
	if got := QuotaDocID("u", "p"); got != "u__p" {
		t.Fatal(got)
	}
}
```

- [ ] **Step 2: `go test ./internal/firestore/ -count=1`**

- [ ] **Step 3: Commit**

```bash
git add internal/firestore/
git commit -m "feat(LWC-138): Firestore pipeline_quota reserve and refund"
```

---

### Task 3: Config + handler wiring helpers

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Modify: `internal/handler/v1/handler.go`
- Create: `internal/handler/v1/pipeline_quota.go`

**Config fields:**

```go
PipelineDailyLimit      int
PipelineCooldownSeconds int
PipelineMinNewRaw       int
PipelineDemoUserIDs     []string // split PIPELINE_DEMO_USER_IDS by comma
```

Defaults when unset/zero: 2, 3600, 1, empty.

Handler fields:

```go
pipelineDailyLimit int
pipelineCooldown   time.Duration
pipelineMinNewRaw  int
demoUserIDs        map[string]struct{}
```

Add `SetPipelineQuotaConfig(...)` or set in `New` / main after load.

`pipeline_quota.go` helpers:

```go
func (h *Handler) isDemoUser(userID string) bool
func (h *Handler) pipelineLimits() pipelinequota.Limits
func (h *Handler) countNewRawForProject(ctx, userID, projectID) (int, error)
func (h *Handler) isPipelineRunning(ctx, userID, projectID) (bool, error)
func (h *Handler) evaluateQuota(ctx, userID, projectID, reserve bool) (snap pipelinequota.Snapshot, reserved bool, prev refundPrev, err error)
func httpStatusForReason(r pipelinequota.Reason) int
```

`isPipelineRunning`: firestore lock Locked OR latest execution status == `RUNNING` (reuse `pipelineExecutionStatus`).

`countNewRawForProject`: Scope store, ListRawFiles, CountNewRaw with lastRunAt from loaded quota (or pass lastRunAt in).

- [ ] **Step 1: Implement config load + unit test for defaults/env**

- [ ] **Step 2: Implement handler helpers (can compile even if PipelineRun not yet switched)**

- [ ] **Step 3: `go test ./internal/config/ ./internal/handler/v1/ -count=1`** (existing tests still pass)

- [ ] **Step 4: Commit**

```bash
git commit -am "feat(LWC-138): pipeline quota config and handler helpers"
```

---

### Task 4: Extract `invokePipelineJob` + update PipelineRun / Admin / Status

**Files:**
- Modify: `internal/handler/v1/endpoints.go`
- Modify: `internal/handler/v1/handler_test.go`

**`invokePipelineJob`:** move body from current `PipelineRun` Cloud Run section; both call sites use it.

**`PipelineRun` flow:**

```go
func (h *Handler) PipelineRun(c *gin.Context) {
	// validate user/project
	// evaluateQuota(..., reserve=true)
	// if !allowed → JSON status httpStatusForReason + error + quota
	// executionID, err := h.invokePipelineJob(...)
	// if err → RefundQuota if reserved; 500
	// 202 accepted + quota post-reserve evaluate (or build from reserve result)
}
```

Post-reserve snapshot: re-evaluate without second reserve, or construct from reserved state (runs_today already incremented). Prefer return snapshot from ReserveQuota after successful write.

**`PipelineStatus`:** after existing last_execution, call evaluateQuota(reserve=false), set `quota` on response struct.

**`AdminPipelineTrigger`:** if already_running → 409; else invokePipelineJob; no reserve.

- [ ] **Step 1: Write failing handler tests**

```go
func TestPipelineRunBlocksDemoUser(t *testing.T) { /* set demoUserIDs, expect 403, no /run hit */ }
func TestPipelineRunBlocksDailyLimit(t *testing.T) { /* inject quota store or pre-seed if fake */ }
```

If Firestore hard to fake: introduce thin interface on Handler:

```go
type quotaBackend interface {
	Reserve(ctx context.Context, ...) (pipelinequota.Snapshot, refundPrev, bool, error)
	Refund(ctx context.Context, prev refundPrev) error
	Evaluate(ctx context.Context, ...) (pipelinequota.Snapshot, error)
}
```

Default impl wraps firestore.Client; tests inject stub.

- [ ] **Step 2: Implement endpoint changes**

- [ ] **Step 3: Update existing tests that decode `map[string]string` body — may need `map[string]any` because `quota` is object**

Especially `TestPipelineRunExecutesCloudRunJob` body decode.

- [ ] **Step 4: Run full package tests**

```bash
go test ./internal/handler/v1/ ./internal/pipelinequota/ ./internal/firestore/ ./internal/config/ -count=1
```

- [ ] **Step 5: Commit**

```bash
git commit -am "feat(LWC-138): enforce pipeline quota on run and status"
```

---

### Task 5: Wire main + smoke

**Files:**
- Modify: `main.go` (or wherever Handler is constructed)

- [ ] **Step 1: After `v1.New(...)`, apply quota config from `cfg`**

- [ ] **Step 2: `go test ./...` (or Makefile test target)**

```bash
go test ./... -count=1
```

- [ ] **Step 3: Commit**

```bash
git commit -am "feat(LWC-138): wire pipeline quota config at startup"
```

---

### Task 6: BFF verification checklist

- [ ] All acceptance criteria in BFF spec covered by tests or manual notes
- [ ] No worker changes
- [ ] README short note on env vars (optional one paragraph)

---

## Spec coverage check

| Spec item | Task |
|-----------|------|
| Pure limits / priority | T1 |
| Firestore txn reserve/refund | T2 |
| Env config | T3 |
| Demo / already_running / run / status / admin | T4 |
| Local firestore nil | T3–T4 evaluate Enforced=false |
| Shared invoke helper | T4 |
| Failed accept counts | T1–T4 (no success-gated refund) |

## Frontend handoff

After BFF merge/deploy (or parallel against contract), implement  
`llm-wiki-frontend/docs/superpowers/plans/2026-07-10-lwc-138-pipeline-rate-limit-frontend.md`.
