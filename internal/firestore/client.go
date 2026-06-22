package firestore

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
)

// Client wraps Firestore operations for pipeline status.
type Client struct {
	fs     *firestore.Client
	lockID string
	locks  *firestore.CollectionRef
}

// Status represents the current pipeline state.
type Status struct {
	Locked     bool      `json:"locked"`
	LockExpiry time.Time `json:"lock_expiry,omitempty"`
	Worker     string    `json:"worker,omitempty"`
}

// NewClient creates a Firestore client.
func NewClient(project, userID, projectID string) (*Client, error) {
	ctx := context.Background()
	fs, err := firestore.NewClient(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("firestore client: %w", err)
	}

	lockID := fmt.Sprintf("%s__%s", userID, projectID)
	return &Client{
		fs:     fs,
		lockID: lockID,
		locks:  fs.Collection("locks"),
	}, nil
}

// GetStatus returns the current pipeline lock status.
func (c *Client) GetStatus(ctx context.Context) (*Status, error) {
	doc, err := c.locks.Doc(c.lockID).Get(ctx)
	if err != nil {
		// No lock document = not locked
		return &Status{Locked: false}, nil
	}

	data := doc.Data()
	status, _ := data["status"].(string)
	if status != "active" {
		return &Status{Locked: false}, nil
	}

	s := &Status{Locked: true}
	if w, ok := data["worker"].(string); ok {
		s.Worker = w
	}
	if t, ok := data["expires_at"].(time.Time); ok {
		s.LockExpiry = t
	}

	return s, nil
}

// CountActiveLocks returns the number of active (running) pipeline locks
// across all users/projects. Each lock with status="active" and expires_at > now
// represents one running pipeline.
func (c *Client) CountActiveLocks(ctx context.Context) (int, error) {
	iter := c.locks.Where("status", "==", "active").Documents(ctx)
	count := 0
	now := time.Now()
	for {
		doc, err := iter.Next()
		if err != nil {
			if err.Error() == "iterator done" {
				break
			}
			return count, err
		}
		data := doc.Data()
		if t, ok := data["expires_at"].(time.Time); ok && t.After(now) {
			count++
		}
	}
	return count, nil
}

// ExecutionRecord represents one pipeline execution for metrics.
type ExecutionRecord struct {
	UserID      string    `json:"user_id"`
	ProjectID   string    `json:"project_id"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	DurationSec float64   `json:"duration_sec,omitempty"`
	Status      string    `json:"status"` // "running", "completed", "failed"
}

// WriteExecutionStart records a pipeline execution start.
func (c *Client) WriteExecutionStart(ctx context.Context, userID, projectID string, startedAt time.Time) (string, error) {
	doc := c.fs.Collection("executions").NewDoc()
	_, err := doc.Set(ctx, map[string]interface{}{
		"user_id":    userID,
		"project_id": projectID,
		"started_at": startedAt,
		"status":     "running",
	})
	if err != nil {
		return "", err
	}
	return doc.ID, nil
}

// WriteExecutionEnd updates a pipeline execution with completion data.
func (c *Client) WriteExecutionEnd(ctx context.Context, docID string, finishedAt time.Time, status string) error {
	doc := c.fs.Collection("executions").Doc(docID)
	dsnap, err := doc.Get(ctx)
	if err != nil {
		return err
	}
	startedAt, _ := dsnap.Data()["started_at"].(time.Time)
	durationSec := finishedAt.Sub(startedAt).Seconds()

	_, err = doc.Update(ctx, []firestore.Update{
		{Path: "finished_at", Value: finishedAt},
		{Path: "duration_sec", Value: durationSec},
		{Path: "status", Value: status},
	})
	return err
}

// ListRecentExecutions returns recent pipeline execution records for metrics.
func (c *Client) ListRecentExecutions(ctx context.Context, limit int) ([]ExecutionRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	iter := c.fs.Collection("executions").OrderBy("started_at", firestore.Desc).Limit(limit).Documents(ctx)
	var records []ExecutionRecord
	for {
		doc, err := iter.Next()
		if err != nil {
			break
		}
		data := doc.Data()
		r := ExecutionRecord{}
		if v, ok := data["user_id"].(string); ok {
			r.UserID = v
		}
		if v, ok := data["project_id"].(string); ok {
			r.ProjectID = v
		}
		if v, ok := data["started_at"].(time.Time); ok {
			r.StartedAt = v
		}
		if v, ok := data["finished_at"].(time.Time); ok {
			r.FinishedAt = v
		}
		if v, ok := data["duration_sec"].(float64); ok {
			r.DurationSec = v
		}
		if v, ok := data["status"].(string); ok {
			r.Status = v
		}
		records = append(records, r)
	}
	return records, nil
}

// Close closes the Firestore client.
func (c *Client) Close() error {
	return c.fs.Close()
}

// Raw exposes the underlying firestore.Client for direct operations.
func (c *Client) Raw() *firestore.Client {
	return c.fs
}
