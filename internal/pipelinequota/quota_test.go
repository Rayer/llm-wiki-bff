package pipelinequota

import (
	"testing"
	"time"
)

func TestEvaluateDailyLimit(t *testing.T) {
	now := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	got := Evaluate(Input{
		Now:         now,
		Enforced:    true,
		Limits:      Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		RunsToday:   2,
		DayKey:      "2026-07-10",
		LastRunAt:   now.Add(-2 * time.Hour),
		NewRawFiles: 3,
	})
	if got.Allowed || got.Reason != ReasonDailyLimit {
		t.Fatalf("got %+v", got)
	}
}

func TestEvaluateDayRolloverResetsCount(t *testing.T) {
	now := time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC)
	got := Evaluate(Input{
		Now:         now,
		Enforced:    true,
		Limits:      Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		RunsToday:   2,
		DayKey:      "2026-07-10",
		LastRunAt:   now.Add(-2 * time.Hour),
		NewRawFiles: 1,
	})
	if !got.Allowed || got.RunsToday != 0 {
		t.Fatalf("expected allow with reset runs, got %+v", got)
	}
}

func TestEvaluateCooldown(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	last := now.Add(-30 * time.Minute)
	got := Evaluate(Input{
		Now:         now,
		Enforced:    true,
		Limits:      Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		RunsToday:   1,
		DayKey:      "2026-07-10",
		LastRunAt:   last,
		NewRawFiles: 2,
	})
	if got.Allowed || got.Reason != ReasonCooldown {
		t.Fatalf("got %+v", got)
	}
	if got.CooldownUntil == nil {
		t.Fatal("expected CooldownUntil while cooldown is active")
	}
}

func TestEvaluateExpiredCooldownOmitsCooldownUntil(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	got := Evaluate(Input{
		Now:         now,
		Enforced:    true,
		Limits:      Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		RunsToday:   1,
		DayKey:      "2026-07-10",
		LastRunAt:   now.Add(-2 * time.Hour),
		NewRawFiles: 1,
	})
	if !got.Allowed {
		t.Fatalf("expected allow after cooldown, got %+v", got)
	}
	if got.CooldownUntil != nil {
		t.Fatalf("CooldownUntil = %v, want nil after expiry", got.CooldownUntil)
	}
}

func TestEvaluateNoNewRaw(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	got := Evaluate(Input{
		Now:         now,
		Enforced:    true,
		Limits:      Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		RunsToday:   0,
		DayKey:      "2026-07-10",
		NewRawFiles: 0,
	})
	if got.Allowed || got.Reason != ReasonNoNewRaw {
		t.Fatalf("got %+v", got)
	}
}

func TestEvaluateAlreadyRunningPriority(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	got := Evaluate(Input{
		Now:            now,
		Enforced:       true,
		Limits:         Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		AlreadyRunning: true,
		RunsToday:      2,
		DayKey:         "2026-07-10",
		NewRawFiles:    0,
	})
	if got.Reason != ReasonAlreadyRunning {
		t.Fatalf("want already_running first, got %+v", got)
	}
}

func TestEvaluateDemoPriority(t *testing.T) {
	got := Evaluate(Input{
		Now:            time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		Enforced:       true,
		Limits:         Limits{DailyLimit: 2, Cooldown: time.Hour, MinNewRaw: 1},
		IsDemo:         true,
		AlreadyRunning: true,
		NewRawFiles:    0,
	})
	if got.Reason != ReasonDemo {
		t.Fatalf("want demo first, got %+v", got)
	}
}

func TestComputeAlreadyRunningTerminalOverridesStaleLock(t *testing.T) {
	if ComputeAlreadyRunning(true, "SUCCEEDED") {
		t.Fatal("stale lock must not block after SUCCEEDED")
	}
	if ComputeAlreadyRunning(true, "FAILED") {
		t.Fatal("stale lock must not block after FAILED")
	}
}

func TestComputeAlreadyRunningLiveExecutionWins(t *testing.T) {
	if !ComputeAlreadyRunning(false, "RUNNING") {
		t.Fatal("RUNNING execution must report already_running")
	}
	if !ComputeAlreadyRunning(true, "RUNNING") {
		t.Fatal("RUNNING execution must win even without lock")
	}
}

func TestComputeAlreadyRunningFallsBackToLock(t *testing.T) {
	if !ComputeAlreadyRunning(true, "") {
		t.Fatal("active lock without execution status must block")
	}
	if ComputeAlreadyRunning(false, "") {
		t.Fatal("no lock and no execution must not block")
	}
}

func TestCountNewRawFiles(t *testing.T) {
	last := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	files := []time.Time{
		last.Add(-time.Hour),
		last.Add(time.Minute),
	}
	if n := CountNewRaw(files, last); n != 1 {
		t.Fatalf("n=%d", n)
	}
	if n := CountNewRaw(files, time.Time{}); n != 2 {
		t.Fatalf("first run n=%d", n)
	}
}
