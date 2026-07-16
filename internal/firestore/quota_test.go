package firestore

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestQuotaDocID(t *testing.T) {
	if got := QuotaDocID("u", "p"); got != "u__p" {
		t.Fatal(got)
	}
	if got := QuotaDocID("user-1", "proj-2"); got != "user-1__proj-2" {
		t.Fatal(got)
	}
}

func TestQuotaStateFromDataMissingAndNil(t *testing.T) {
	runs, day, last := quotaStateFromData(nil)
	if runs != 0 || day != "" || !last.IsZero() {
		t.Fatalf("nil data = (%d, %q, %v), want zeros", runs, day, last)
	}

	runs, day, last = quotaStateFromData(map[string]interface{}{})
	if runs != 0 || day != "" || !last.IsZero() {
		t.Fatalf("empty data = (%d, %q, %v), want zeros", runs, day, last)
	}
}

func TestQuotaStateFromDataParsesFields(t *testing.T) {
	last := time.Date(2026, 7, 10, 8, 15, 0, 0, time.UTC)
	runs, day, gotLast := quotaStateFromData(map[string]interface{}{
		"runs_today":  int64(2),
		"day_key":     "2026-07-10",
		"last_run_at": timestamppb.New(last),
	})
	if runs != 2 {
		t.Fatalf("runs_today = %d, want 2", runs)
	}
	if day != "2026-07-10" {
		t.Fatalf("day_key = %q, want 2026-07-10", day)
	}
	if !gotLast.Equal(last) {
		t.Fatalf("last_run_at = %v, want %v", gotLast, last)
	}
}

func TestIntFromFirestore(t *testing.T) {
	tests := []struct {
		in   interface{}
		want int
	}{
		{nil, 0},
		{"x", 0},
		{int(3), 3},
		{int32(4), 4},
		{int64(5), 5},
		{float64(6), 6},
		{float32(7), 7},
	}
	for _, tt := range tests {
		if got := intFromFirestore(tt.in); got != tt.want {
			t.Fatalf("intFromFirestore(%T %v) = %d, want %d", tt.in, tt.in, got, tt.want)
		}
	}
}
