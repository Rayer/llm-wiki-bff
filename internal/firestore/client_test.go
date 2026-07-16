package firestore

import (
	"context"
	"testing"
	"time"

	cloudfirestore "cloud.google.com/go/firestore"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNewClientWithDatabaseSelectsNamedDatabase(t *testing.T) {
	original := newFirestoreClient
	t.Cleanup(func() { newFirestoreClient = original })

	var gotDatabaseID string
	newFirestoreClient = func(_ context.Context, _ string, databaseID string) (*cloudfirestore.Client, error) {
		gotDatabaseID = databaseID
		return &cloudfirestore.Client{}, nil
	}

	client, err := NewClientWithDatabase("project", "named-db", "user", "project-id")
	if err != nil {
		t.Fatalf("NewClientWithDatabase() error = %v", err)
	}
	if gotDatabaseID != "named-db" {
		t.Fatalf("database ID passed to Firestore constructor = %q, want %q", gotDatabaseID, "named-db")
	}
	if client.databaseID != "named-db" {
		t.Fatalf("wrapper databaseID = %q, want %q", client.databaseID, "named-db")
	}
}

func TestNewClientKeepsDefaultDatabaseSelection(t *testing.T) {
	original := newFirestoreClient
	t.Cleanup(func() { newFirestoreClient = original })

	var gotDatabaseID string
	newFirestoreClient = func(_ context.Context, _ string, databaseID string) (*cloudfirestore.Client, error) {
		gotDatabaseID = databaseID
		return &cloudfirestore.Client{}, nil
	}

	if _, err := NewClient("project", "user", "project-id"); err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if gotDatabaseID != "" {
		t.Fatalf("database ID passed to Firestore constructor = %q, want empty default", gotDatabaseID)
	}
}

func TestActiveLockUntilReturnsExpiryForActiveUnexpiredLock(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(time.Minute)

	got, ok := activeLockUntil(map[string]interface{}{
		"status":     "active",
		"expires_at": timestamppb.New(expiresAt),
	}, now)

	if !ok {
		t.Fatal("activeLockUntil returned ok=false")
	}
	if !got.Equal(expiresAt) {
		t.Fatalf("expiry = %s, want %s", got, expiresAt)
	}
}

func TestActiveLockUntilIgnoresReleasedOrExpiredLocks(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		data map[string]interface{}
	}{
		{
			name: "released",
			data: map[string]interface{}{
				"status":     "released",
				"expires_at": now.Add(time.Minute),
			},
		},
		{
			name: "expired",
			data: map[string]interface{}{
				"status":     "active",
				"expires_at": now.Add(-time.Minute),
			},
		},
		{
			name: "missing expiry",
			data: map[string]interface{}{
				"status": "active",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if expiresAt, ok := activeLockUntil(tt.data, now); ok {
				t.Fatalf("activeLockUntil = %s, true; want false", expiresAt)
			}
		})
	}
}

func TestLockDataActiveMatchesActiveLockUntil(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	active := map[string]interface{}{
		"status":     "active",
		"expires_at": timestamppb.New(now.Add(time.Minute)),
	}
	if !LockDataActive(active, now) {
		t.Fatal("LockDataActive = false, want true for active unexpired lock")
	}
	expired := map[string]interface{}{
		"status":     "active",
		"expires_at": timestamppb.New(now.Add(-time.Minute)),
	}
	if LockDataActive(expired, now) {
		t.Fatal("LockDataActive = true, want false for expired lock")
	}
}
