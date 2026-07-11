# LWC-149 registration_enabled Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan step-by-step. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Runtime `registration_enabled` flag blocks self-serve signup when false, with Firestore/env resolution and public + admin endpoints.

**Architecture:** New `internal/syssettings` package owns resolution (Firestore `system/settings` → env `REGISTRATION_ENABLED` → default true), persistence, and handlers. `RegisterHandler` accepts a `RegistrationGate` interface checked before any user/project creation.

**Tech Stack:** Go, Gin, Firestore, testify

## Global Constraints

- Firestore doc: `system/settings`, field `registration_enabled` (bool)
- Env: `REGISTRATION_ENABLED` (`true`/`false`/`1`/`0`)
- Resolution: Firestore doc exists → env → default `true`
- `POST /register` disabled → 403 `{"error":"registration is disabled"}`
- Keep local-mode 503 register behavior unchanged
- Out of scope: frontend, invite codes, demo login changes

---

### Task 1: syssettings resolve + store (TDD)

**Files:**
- Create: `internal/syssettings/resolve.go`, `resolve_test.go`, `store.go`, `store_test.go`, `handlers.go`, `handlers_test.go`

- [ ] RED: table tests for `Resolve` and `ParseEnvBool`
- [ ] GREEN: implement resolve + Firestore store + fake store
- [ ] RED: handler tests (public config, admin GET/PATCH)
- [ ] GREEN: handlers

### Task 2: Register gate (TDD)

**Files:**
- Modify: `internal/auth/register.go`
- Create: `internal/auth/register_test.go`

- [ ] RED: disabled gate → 403, no fs needed
- [ ] GREEN: inject `RegistrationGate`, check before user create

### Task 3: Wire routes + config

**Files:**
- Modify: `main.go`, `main_test.go`, `internal/config/config.go`

- [ ] Add `REGISTRATION_ENABLED` to config
- [ ] Wire public config, admin settings, register gate
- [ ] Admin route tests in main_test.go

### Task 4: Verify + PR

- [ ] `go test ./...`
- [ ] Commit, push, `gh pr create --base develop/1.0`