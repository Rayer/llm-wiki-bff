# Worker Tmpfs Workspace Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional tmpfs workspace mode to `cmd/olw_worker` so OLW writes to local scratch storage and only syncs durable outputs back to the mounted vault.

**Architecture:** Resolve and validate the original vault as today, then optionally create a workspace under `/tmp` or a configured parent directory. Symlink `raw/`, copy `wiki/`, `cache/`, `.olw/`, and `wiki.toml`, run OLW and postprocess in the workspace, then sync `wiki/`, `cache/`, and `.olw/` back after successful completion while excluding transient lock files.

**Tech Stack:** Go standard library filesystem APIs, Cobra flags, existing `cmd/olw_worker` tests, Debian `rsync` package in worker image.

## Global Constraints

- Keep the worker filesystem-first; do not add a GCS API or `gsutil` write path in this task.
- Keep workspace mode opt-in via flag or env so local/default behavior remains compatible.
- Do not sync `.olw/pipeline.lock` back to the mounted vault.
- Preserve existing `wiki.toml` behavior: create it in the original vault if missing, then copy it to the workspace.

---

### Task 1: Workspace Preparation And Sync

**Files:**
- Modify: `cmd/olw_worker/main_test.go`
- Modify: `cmd/olw_worker/main.go`
- Modify: `internal/handler/v1/handler_test.go`
- Modify: `internal/handler/v1/endpoints.go`

**Interfaces:**
- Produces: `prepareWorkspace(originalVault string, parent string) (*workspaceVault, error)`
- Produces: `(*workspaceVault).syncBack() error`
- Produces: `(*workspaceVault).cleanup() error`

- [ ] **Step 1: Write failing tests**

Add tests proving `raw/` is a symlink, durable directories are copied, sync-back excludes `.olw/pipeline.lock`, and missing optional directories do not fail.

- [ ] **Step 2: Verify tests fail**

Run: `go test ./cmd/olw_worker`

- [ ] **Step 3: Implement workspace helpers**

Add focused helpers in `cmd/olw_worker/main.go` using explicit `filepath.WalkDir` copy and sync functions with selective exclusions.

- [ ] **Step 4: Verify tests pass**

Run: `go test ./cmd/olw_worker`

---

### Task 2: Wire Workspace Mode Into Worker Run

**Files:**
- Modify: `cmd/olw_worker/main_test.go`
- Modify: `cmd/olw_worker/main.go`

**Interfaces:**
- Consumes: `prepareWorkspace`
- Produces config fields `UseWorkspace bool` and `WorkspaceParent string`

- [ ] **Step 1: Write failing tests**

Add tests proving `runWorkerBatch` executes OLW from the workspace when enabled, syncs back after success, and does not sync back after OLW failure.
Add handler tests proving pipeline Cloud Run overrides include `WORKSPACE=true`.

- [ ] **Step 2: Verify tests fail**

Run: `go test ./cmd/olw_worker`

- [ ] **Step 3: Implement flags and run flow**

Add `--workspace`, `WORKSPACE=true`, `--workspace-dir`, and `WORKSPACE_DIR`. Keep default behavior unchanged.
Add `WORKSPACE=true` to pipeline Cloud Run overrides so the production pipeline uses workspace mode.

- [ ] **Step 4: Verify tests pass**

Run: `go test ./cmd/olw_worker ./internal/handler/v1`

---

### Task 3: Container And Docs

**Files:**
- Modify: `cmd/olw_worker/Dockerfile`
- Modify: `cmd/olw_worker/DESIGN.md`

**Interfaces:**
- Consumes: `--workspace` worker flag

- [ ] **Step 1: Add image dependency**

Install `rsync` in the worker image package list for production sync tooling.

- [ ] **Step 2: Document workspace mode**

Update `cmd/olw_worker/DESIGN.md` with the tmpfs flow, sync policy, and Cloud Run args/env guidance.

- [ ] **Step 3: Verify**

Run:

```bash
go test ./cmd/olw_worker
go test ./internal/handler/v1
go test ./...
docker build -f cmd/olw_worker/Dockerfile --target worker -t llm-wiki-bff-olw-worker:test .
```
