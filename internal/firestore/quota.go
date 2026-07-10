package firestore

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/rayer/llm-wiki-bff/internal/pipelinequota"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const pipelineQuotaCollection = "pipeline_quota"

// QuotaPrev holds the pre-reserve document fields needed by RefundQuota.
type QuotaPrev struct {
	RunsToday int
	DayKey    string
	LastRunAt time.Time
}

// QuotaDocID returns the pipeline_quota document ID for a user/project pair.
func QuotaDocID(userID, projectID string) string {
	return fmt.Sprintf("%s__%s", userID, projectID)
}

// LoadQuotaState reads the pipeline_quota document (missing → zeros).
func (c *Client) LoadQuotaState(ctx context.Context, userID, projectID string) (runsToday int, dayKey string, lastRunAt time.Time, err error) {
	doc, err := c.fs.Collection(pipelineQuotaCollection).Doc(QuotaDocID(userID, projectID)).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return 0, "", time.Time{}, nil
		}
		return 0, "", time.Time{}, err
	}
	runsToday, dayKey, lastRunAt = quotaStateFromData(doc.Data())
	return runsToday, dayKey, lastRunAt, nil
}

// ReserveQuota evaluates quota inside a transaction and, if allowed, reserves one run.
//
// Caller-supplied isDemo / alreadyRunning / newRawFiles are not stored on the doc.
// On success, reserved is true and snap is the post-reserve evaluation (Allowed may be
// false for a *subsequent* run due to cooldown / daily limit). On block, reserved is
// false, snap is the blocking evaluation, and no write is performed.
func (c *Client) ReserveQuota(
	ctx context.Context,
	userID, projectID string,
	limits pipelinequota.Limits,
	now time.Time,
	isDemo, alreadyRunning bool,
	newRawFiles int,
) (prev QuotaPrev, snap pipelinequota.Snapshot, reserved bool, err error) {
	now = now.UTC()
	ref := c.fs.Collection(pipelineQuotaCollection).Doc(QuotaDocID(userID, projectID))

	err = c.fs.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		prev = QuotaPrev{}
		snap = pipelinequota.Snapshot{}
		reserved = false

		runsToday, dayKey, lastRunAt, readErr := readQuotaStateTx(tx, ref)
		if readErr != nil {
			return readErr
		}
		prev = QuotaPrev{
			RunsToday: runsToday,
			DayKey:    dayKey,
			LastRunAt: lastRunAt,
		}

		pre := pipelinequota.Evaluate(pipelinequota.Input{
			Now:            now,
			Limits:         limits,
			IsDemo:         isDemo,
			AlreadyRunning: alreadyRunning,
			RunsToday:      runsToday,
			DayKey:         dayKey,
			LastRunAt:      lastRunAt,
			NewRawFiles:    newRawFiles,
			Enforced:       true,
		})
		if !pre.Allowed {
			snap = pre
			return nil
		}

		today := pipelinequota.DayKeyUTC(now)
		// Evaluate already applied day-key rollover into RunsToday for the allow decision.
		newRuns := pre.RunsToday + 1
		if writeErr := tx.Set(ref, map[string]interface{}{
			"user_id":     userID,
			"project_id":  projectID,
			"day_key":     today,
			"runs_today":  newRuns,
			"last_run_at": now,
			"updated_at":  now,
		}); writeErr != nil {
			return writeErr
		}

		// Post-reserve snapshot for callers (status of the *next* potential run).
		snap = pipelinequota.Evaluate(pipelinequota.Input{
			Now:            now,
			Limits:         limits,
			IsDemo:         isDemo,
			AlreadyRunning: alreadyRunning,
			RunsToday:      newRuns,
			DayKey:         today,
			LastRunAt:      now,
			NewRawFiles:    newRawFiles,
			Enforced:       true,
		})
		reserved = true
		return nil
	})
	if err != nil {
		return QuotaPrev{}, pipelinequota.Snapshot{}, false, err
	}
	return prev, snap, reserved, nil
}

// RefundQuota restores pipeline_quota fields to a pre-reserve snapshot after a failed trigger.
func (c *Client) RefundQuota(ctx context.Context, userID, projectID string, prevRuns int, prevDayKey string, prevLastRunAt time.Time) error {
	ref := c.fs.Collection(pipelineQuotaCollection).Doc(QuotaDocID(userID, projectID))
	now := time.Now().UTC()

	updates := []firestore.Update{
		{Path: "runs_today", Value: prevRuns},
		{Path: "day_key", Value: prevDayKey},
		{Path: "updated_at", Value: now},
	}
	if prevLastRunAt.IsZero() {
		// Avoid writing a non-zero epoch timestamp that would break IsZero() on reload.
		updates = append(updates, firestore.Update{Path: "last_run_at", Value: firestore.Delete})
	} else {
		updates = append(updates, firestore.Update{Path: "last_run_at", Value: prevLastRunAt.UTC()})
	}

	_, err := ref.Update(ctx, updates)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// Nothing to restore.
			return nil
		}
		return err
	}
	return nil
}

// RefundQuotaPrev is a convenience wrapper around RefundQuota using QuotaPrev.
func (c *Client) RefundQuotaPrev(ctx context.Context, userID, projectID string, prev QuotaPrev) error {
	return c.RefundQuota(ctx, userID, projectID, prev.RunsToday, prev.DayKey, prev.LastRunAt)
}

func readQuotaStateTx(tx *firestore.Transaction, ref *firestore.DocumentRef) (runsToday int, dayKey string, lastRunAt time.Time, err error) {
	snap, err := tx.Get(ref)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return 0, "", time.Time{}, nil
		}
		return 0, "", time.Time{}, err
	}
	runsToday, dayKey, lastRunAt = quotaStateFromData(snap.Data())
	return runsToday, dayKey, lastRunAt, nil
}

// quotaStateFromData extracts quota fields from a Firestore document map.
// Exported-style pure helper for unit tests (same package).
func quotaStateFromData(data map[string]interface{}) (runsToday int, dayKey string, lastRunAt time.Time) {
	if data == nil {
		return 0, "", time.Time{}
	}
	runsToday = intFromFirestore(data["runs_today"])
	if v, ok := data["day_key"].(string); ok {
		dayKey = v
	}
	if t, ok := firestoreTimestamp(data["last_run_at"]); ok {
		lastRunAt = t
	}
	return runsToday, dayKey, lastRunAt
}

func intFromFirestore(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	default:
		return 0
	}
}
