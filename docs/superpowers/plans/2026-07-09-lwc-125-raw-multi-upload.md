# LWC-125 Raw Multi Upload — BFF Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Same-filename raw upload returns `created` / `already_exists` / `409 conflict` without rewriting on skip or conflict.

**Architecture:** After hashing the upload body, resolve existing object digest via `GetMetaSHA256` + `ReadFile` fallback, then branch before `WriteBytes`.

**Tech Stack:** Go, Gin, `internal/storage.Store`, existing `raw_upload.go` tests.

## Global Constraints

- No cross-file content dedupe
- No batch multipart API
- Keep field name `sha256`
- TOCTOU overwrite race accepted for v1

---

### Task 1: Same-name decision + response status

**Files:**
- Modify: `internal/handler/v1/raw_upload.go`
- Modify: `internal/handler/v1/raw_upload_test.go`

**Interfaces:**
- Produces: `rawUploadResponse.Status` (`created` | `already_exists`)
- Produces: HTTP 200 / 201 / 409 decision before write

- [ ] **Step 1: Add unit tests for response helper and decision helper**

- [ ] **Step 2: Implement status field + resolveExistingDigest + branch in RawUpload**

- [ ] **Step 3: `go test ./internal/handler/v1/ -count=1`**

- [ ] **Step 4: Commit**
