package v1

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/firestore"
	"github.com/rayer/llm-wiki-bff/internal/pipelinequota"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pipelineQuotaStore is the quota persistence surface used by evaluateQuota.
// *firestore.Client implements it; tests may inject a fake via SetPipelineQuotaStore.
type pipelineQuotaStore interface {
	LoadQuotaState(ctx context.Context, userID, projectID string) (runsToday int, dayKey string, lastRunAt time.Time, err error)
	ReserveQuota(
		ctx context.Context,
		userID, projectID string,
		limits pipelinequota.Limits,
		now time.Time,
		isDemo, alreadyRunning bool,
		newRawFiles int,
	) (prev firestore.QuotaPrev, snap pipelinequota.Snapshot, reserved bool, err error)
	RefundQuotaPrev(ctx context.Context, userID, projectID string, prev firestore.QuotaPrev) error
}

func (h *Handler) effectiveQuotaStore() pipelineQuotaStore {
	if h.quotaStore != nil {
		return h.quotaStore
	}
	if h.firestore != nil {
		return h.firestore
	}
	return nil
}

func (h *Handler) isDemoUser(userID string) bool {
	if userID == "" || len(h.demoUserIDs) == 0 {
		return false
	}
	_, ok := h.demoUserIDs[userID]
	return ok
}

func (h *Handler) pipelineLimits() pipelinequota.Limits {
	daily := h.pipelineDailyLimit
	if daily <= 0 {
		daily = 2
	}
	cooldown := h.pipelineCooldown
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	minNew := h.pipelineMinNewRaw
	if minNew <= 0 {
		minNew = 1
	}
	return pipelinequota.Limits{
		DailyLimit: daily,
		Cooldown:   cooldown,
		MinNewRaw:  minNew,
	}
}

// countNewRawForProject counts raw files newer than the project's last accepted run.
// When last_run_at is zero (never run), all listed raw files count.
func (h *Handler) countNewRawForProject(ctx context.Context, userID, projectID string) (int, error) {
	var lastRunAt time.Time
	if qs := h.effectiveQuotaStore(); qs != nil {
		_, _, at, err := qs.LoadQuotaState(ctx, userID, projectID)
		if err != nil {
			return 0, err
		}
		lastRunAt = at
	}
	if h.store == nil {
		return 0, nil
	}
	files, err := h.store.Scope(userID, projectID).ListRawFiles(ctx)
	if err != nil {
		return 0, err
	}
	updated := make([]time.Time, 0, len(files))
	for _, f := range files {
		updated = append(updated, f.Updated)
	}
	return pipelinequota.CountNewRaw(updated, lastRunAt), nil
}

// isPipelineRunning reports whether the project has an active lock or a Cloud Run execution in RUNNING.
// Latest terminal execution overrides a stale Firestore lock (LWC-144).
func (h *Handler) isPipelineRunning(ctx context.Context, userID, projectID string) (bool, error) {
	locked, err := h.projectLockActive(ctx, userID, projectID)
	if err != nil {
		return false, err
	}

	last, err := h.pipelineExecutionStatus(ctx, "")
	if err != nil {
		// Cloud Run status may be unavailable (local/dev, metadata missing).
		// Rely on the lock signal only in that case; log so transient API
		// failures are visible when diagnosing a false-allow.
		log.Printf("pipeline status for running check %s/%s: %v (using lock only)", userID, projectID, err)
		return locked, nil
	}

	executionStatus := ""
	if last != nil {
		executionStatus = last.Status
	}
	return pipelinequota.ComputeAlreadyRunning(locked, executionStatus), nil
}

// projectLockActive checks locks/{userID}__{projectID} for an active lock.
// Uses the per-project doc id (same pattern as quota) rather than Client.GetStatus,
// which is bound to the lock id fixed at Firestore client construction.
func (h *Handler) projectLockActive(ctx context.Context, userID, projectID string) (bool, error) {
	if h.firestore == nil || h.firestore.Raw() == nil {
		return false, nil
	}
	docID := fmt.Sprintf("%s__%s", userID, projectID)
	doc, err := h.firestore.Raw().Collection("locks").Doc(docID).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return false, nil
		}
		return false, err
	}
	return firestore.LockDataActive(doc.Data(), time.Now()), nil
}

// evaluateQuota builds a quota snapshot for the project.
//
// When reserve is true and a quota store is available, it attempts ReserveQuota.
// Callers must use the returned reserved bool (not snap.Allowed) to decide refund-on-fail:
// a successful reserve re-evaluates post-write and often yields Allowed=false due to cooldown.
//
// When no quota store is configured (e.g. local mode with nil Firestore), Enforced=false
// and reserved is always false.
func (h *Handler) evaluateQuota(
	ctx context.Context,
	userID, projectID string,
	reserve bool,
) (snap pipelinequota.Snapshot, reserved bool, prev firestore.QuotaPrev, err error) {
	limits := h.pipelineLimits()
	now := time.Now().UTC()
	isDemo := h.isDemoUser(userID)

	alreadyRunning, err := h.isPipelineRunning(ctx, userID, projectID)
	if err != nil {
		return pipelinequota.Snapshot{}, false, firestore.QuotaPrev{}, err
	}

	newRaw, err := h.countNewRawForProject(ctx, userID, projectID)
	if err != nil {
		return pipelinequota.Snapshot{}, false, firestore.QuotaPrev{}, err
	}

	qs := h.effectiveQuotaStore()
	if qs == nil {
		snap = pipelinequota.Evaluate(pipelinequota.Input{
			Now:            now,
			Limits:         limits,
			IsDemo:         isDemo,
			AlreadyRunning: alreadyRunning,
			NewRawFiles:    newRaw,
			Enforced:       false,
		})
		return snap, false, firestore.QuotaPrev{}, nil
	}

	if reserve {
		prev, snap, reserved, err = qs.ReserveQuota(
			ctx, userID, projectID, limits, now, isDemo, alreadyRunning, newRaw,
		)
		if err != nil {
			return pipelinequota.Snapshot{}, false, firestore.QuotaPrev{}, err
		}
		return snap, reserved, prev, nil
	}

	runsToday, dayKey, lastRunAt, err := qs.LoadQuotaState(ctx, userID, projectID)
	if err != nil {
		return pipelinequota.Snapshot{}, false, firestore.QuotaPrev{}, err
	}
	snap = pipelinequota.Evaluate(pipelinequota.Input{
		Now:            now,
		Limits:         limits,
		IsDemo:         isDemo,
		AlreadyRunning: alreadyRunning,
		RunsToday:      runsToday,
		DayKey:         dayKey,
		LastRunAt:      lastRunAt,
		NewRawFiles:    newRaw,
		Enforced:       true,
	})
	return snap, false, firestore.QuotaPrev{}, nil
}

// httpStatusForReason maps a blocking quota reason to an HTTP status code.
func httpStatusForReason(r pipelinequota.Reason) int {
	switch r {
	case pipelinequota.ReasonDemo:
		return http.StatusForbidden // 403
	case pipelinequota.ReasonDailyLimit, pipelinequota.ReasonCooldown:
		return http.StatusTooManyRequests // 429
	case pipelinequota.ReasonAlreadyRunning, pipelinequota.ReasonNoNewRaw:
		return http.StatusConflict // 409
	default:
		return http.StatusConflict
	}
}
