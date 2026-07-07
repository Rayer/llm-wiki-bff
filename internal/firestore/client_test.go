package firestore

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

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
