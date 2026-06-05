package firestore

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
)

// Client wraps Firestore operations for pipeline status.
type Client struct {
	fs      *firestore.Client
	lockID  string
	locks   *firestore.CollectionRef
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

// Close closes the Firestore client.
func (c *Client) Close() error {
	return c.fs.Close()
}
