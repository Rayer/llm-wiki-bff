package pipelinequota

import (
	"fmt"
	"time"
)

type Reason string

const (
	ReasonNone           Reason = ""
	ReasonDemo           Reason = "demo"
	ReasonDailyLimit     Reason = "daily_limit"
	ReasonCooldown       Reason = "cooldown"
	ReasonAlreadyRunning Reason = "already_running"
	ReasonNoNewRaw       Reason = "no_new_raw"
)

type Limits struct {
	DailyLimit int
	Cooldown   time.Duration
	MinNewRaw  int
}

type Input struct {
	Now                  time.Time
	Limits               Limits
	IsDemo               bool
	AlreadyRunning       bool
	RunsToday            int
	DayKey               string // UTC YYYY-MM-DD stored on doc
	LastRunAt            time.Time
	NewRawFiles          int
	RawDirtyFiles        int
	AnnotationDirtyFiles int
	Enforced             bool // if false, always allow (local no firestore)
}

type Snapshot struct {
	Enforced             bool       `json:"enforced"`
	Allowed              bool       `json:"allowed"`
	Reason               Reason     `json:"reason,omitempty"`
	Message              string     `json:"message,omitempty"`
	RunsToday            int        `json:"runs_today"`
	DailyLimit           int        `json:"daily_limit"`
	CooldownUntil        *time.Time `json:"cooldown_until,omitempty"`
	NextReset            time.Time  `json:"next_reset"`
	NewRawFiles          int        `json:"new_raw_files"`
	RawDirtyFiles        int        `json:"raw_dirty_files"`
	AnnotationDirtyFiles int        `json:"annotation_dirty_files"`
	PendingWork          int        `json:"pending_work"`
	MinNewRaw            int        `json:"min_new_raw"`
	AlreadyRunning       bool       `json:"already_running"`
}

func DayKeyUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func NextResetUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

func CountNewRaw(updatedTimes []time.Time, lastRunAt time.Time) int {
	if lastRunAt.IsZero() {
		return len(updatedTimes)
	}
	n := 0
	for _, u := range updatedTimes {
		if u.After(lastRunAt) {
			n++
		}
	}
	return n
}

func Evaluate(in Input) Snapshot {
	lim := in.Limits
	if lim.DailyLimit <= 0 {
		lim.DailyLimit = 2
	}
	if lim.Cooldown <= 0 {
		lim.Cooldown = time.Hour
	}
	if lim.MinNewRaw <= 0 {
		lim.MinNewRaw = 1
	}
	now := in.Now.UTC()
	today := DayKeyUTC(now)
	runs := in.RunsToday
	if in.DayKey != today {
		runs = 0
	}
	snap := Snapshot{
		Enforced:             in.Enforced,
		RunsToday:            runs,
		DailyLimit:           lim.DailyLimit,
		NextReset:            NextResetUTC(now),
		NewRawFiles:          in.NewRawFiles,
		RawDirtyFiles:        in.RawDirtyFiles,
		AnnotationDirtyFiles: in.AnnotationDirtyFiles,
		PendingWork:          in.NewRawFiles + in.RawDirtyFiles + in.AnnotationDirtyFiles,
		MinNewRaw:            lim.MinNewRaw,
		AlreadyRunning:       in.AlreadyRunning,
	}
	if !in.Enforced {
		snap.Allowed = true
		return snap
	}
	// priority
	if in.IsDemo {
		return block(snap, ReasonDemo, "Demo sessions cannot run the pipeline")
	}
	if in.AlreadyRunning {
		return block(snap, ReasonAlreadyRunning, "A pipeline is already running for this project")
	}
	if runs >= lim.DailyLimit {
		return block(snap, ReasonDailyLimit, fmt.Sprintf("Daily limit reached (%d/%d)", runs, lim.DailyLimit))
	}
	if !in.LastRunAt.IsZero() {
		until := in.LastRunAt.UTC().Add(lim.Cooldown)
		// Only expose cooldown_until while the cooldown is still active.
		// A past timestamp is contract noise for clients (FE already treats ms≤0 as clear).
		if now.Before(until) {
			snap.CooldownUntil = &until
			mins := int(until.Sub(now).Minutes()) + 1
			return block(snap, ReasonCooldown, fmt.Sprintf("Cooldown active; try again in %d minutes", mins))
		}
	}
	if in.NewRawFiles+in.RawDirtyFiles < lim.MinNewRaw && in.AnnotationDirtyFiles == 0 {
		return block(snap, ReasonNoNewRaw, "Upload at least one new or modified raw file before running")
	}
	snap.Allowed = true
	return snap
}

// IsTerminalExecutionStatus reports whether a Cloud Run execution has finished.
func IsTerminalExecutionStatus(status string) bool {
	switch status {
	case "SUCCEEDED", "FAILED", "CANCELLED":
		return true
	default:
		return false
	}
}

// ComputeAlreadyRunning derives already_running from lock state and latest execution status.
// A terminal latest execution overrides a stale lock so SUCCEEDED does not look blocked (LWC-144).
func ComputeAlreadyRunning(lockActive bool, executionStatus string) bool {
	if executionStatus == "RUNNING" {
		return true
	}
	if IsTerminalExecutionStatus(executionStatus) {
		return false
	}
	return lockActive
}

func block(s Snapshot, r Reason, msg string) Snapshot {
	s.Allowed = false
	s.Reason = r
	s.Message = msg
	return s
}
