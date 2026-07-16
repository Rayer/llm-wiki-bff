# LWC-153 Admin Statistics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Populate Admin Projects concept/source counts and Admin Users project counts from the existing Firestore and project-scoped wiki storage architecture.

**Architecture:** Keep the existing admin routes and JSON contract shape, adding only the required numeric fields. Enrich each Firestore project with counts from its `store.Scope(userID, projectID)`, using the existing cache-first helpers and markdown fallback; derive each user’s project count from the already-enumerated, ownership-qualified project documents so no extra project/GCS scan is introduced.

**Tech Stack:** Go 1.26, Gin, Firestore, project-scoped GCS/localfs storage, Go tests.

## Global Constraints

- Preserve `concept_count`, `source_count`, and `project_count` as JSON numeric fields.
- Use `cache/concepts.jsonl` and `cache/id_map.json` through existing storage abstractions, falling back only when the cache object is missing.
- Preserve zero for empty projects and users with no owned projects.
- Preserve project isolation by scoping reads with each project’s parsed owner and project ID.
- Do not redesign admin routes or frontend payloads.
- Do not introduce unbounded raw GCS object scans or unrelated cleanup.
- Run gofmt on changed Go files, focused tests, `go vet ./...`, and `go test ./...`.

---

### Task 1: Add failing admin statistics regression tests

**Files:**
- Modify: `internal/handler/v1/admin_test.go`
- Modify: `internal/handler/v1/handler_test.go` only if shared test fixtures are needed

**Interfaces:**
- Consumes: Existing `Handler`, `store.RootStore`, cache-first list helpers, and admin response DTO behavior.
- Produces: Failing tests proving non-empty, empty, multi-project, multi-user, and cache-missing isolation expectations.

- [x] **Step 1: Write focused tests first**

Add tests around a fake root store and the statistics enrichment boundary. Assert project A sees only its own cached concepts/sources, project B remains independent, empty projects return zero, cache-missing projects fall back to markdown lists, and user ownership counts distinguish users with 2, 1, and 0 projects. Assert the field names and integer values expected by the Admin API.

- [x] **Step 2: Run the focused tests to verify RED**

Run:
```bash
go test ./internal/handler/v1 -run 'TestAdmin.*Statistics|TestAdmin.*Counts' -count=1
```
Expected: FAIL because the statistics DTO/enrichment behavior does not exist yet.

---

### Task 2: Implement minimal project and user statistics enrichment

**Files:**
- Modify: `internal/handler/v1/endpoints.go`
- Modify: `internal/handler/v1/admin_test.go` only for test fixture support

**Interfaces:**
- Consumes: Existing `listConceptsCacheFirst`, `listSourcesCacheFirst`, `store.RootStore.Scope`, and the raw Firestore project/user enumeration.
- Produces: Admin project entries with `concept_count` and `source_count`; Admin user entries with `project_count`.

- [x] **Step 1: Add numeric fields to local admin response DTOs**

Use:
```go
ConceptCount int `json:"concept_count"`
SourceCount  int `json:"source_count"`
ProjectCount int `json:"project_count"`
```

- [x] **Step 2: Enrich each project through its isolated scoped store**

For each accepted Firestore project, call `h.store.Scope(uid, pid)`, then use `listConceptsCacheFirst(ctx, scopedStore, false)` and `listSourcesCacheFirst(ctx, scopedStore)`; assign lengths and preserve zero-length successful results. Return an error for non-missing-cache storage failures rather than silently fabricating counts.

- [x] **Step 3: Derive user project counts from the accepted project set**

Increment an ownership map while enumerating valid, non-idempotency project documents. When building users, assign the map count by the Firestore user ID, leaving users with no owned project at zero. Do not scan GCS to compute this value.

- [x] **Step 4: Run focused tests to verify GREEN**

Run:
```bash
gofmt -w internal/handler/v1/endpoints.go internal/handler/v1/admin_test.go
go test ./internal/handler/v1 -run 'Test(LoadAdminProjectStatistics|AdminProjectCounts|AdminProjectRecord|AdminStatistics)' -count=1
```
Expected: PASS.

---

### Task 3: Verify, review, commit, push, and open PR

**Files:**
- Review only the final diff; no unrelated files

**Interfaces:**
- Consumes: Passing focused tests and the final branch diff.
- Produces: Verified commit on `fix/lwc-153-admin-statistics`, pushed origin branch, non-draft PR targeting `develop/1.0`, and CI status.

- [x] **Step 1: Run all required verification**

Run:
```bash
gofmt -w internal/handler/v1/endpoints.go internal/handler/v1/admin_test.go
go test ./internal/handler/v1 -run 'TestAdmin.*Statistics|TestAdmin.*Counts' -count=1
go vet ./...
go test ./...
```
Record exact outcomes.

- [x] **Step 2: Review the final diff**

Check field names/types, scope isolation, empty/cache-missing behavior, auth boundaries, and absence of unrelated cleanup. Resolve any finding before commit.

- [ ] **Step 3: Commit**

```bash
git add internal/handler/v1/endpoints.go internal/handler/v1/admin_test.go
git commit -m "fix(LWC-153): populate admin statistics"
```

- [ ] **Step 4: Push and create the PR**

```bash
git push -u origin fix/lwc-153-admin-statistics
gh pr create --base develop/1.0 --head fix/lwc-153-admin-statistics --title "fix(LWC-153): populate admin statistics" --body-file /tmp/lwc-153-pr.md
```
The PR body must include root cause, fix, exact verification results, and `LWC-153`; keep it non-draft and do not merge.

- [ ] **Step 5: Watch CI**

Run `gh pr checks --watch` or the equivalent. If this change fails CI, fix, commit, push, and recheck for at most three cycles. If no checks are configured, report that explicitly.
