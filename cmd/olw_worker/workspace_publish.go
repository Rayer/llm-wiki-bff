package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	leasePath      = ".olw/lwc-worker-lease.json"
	publishJournal = ".lwc-worker-publish-journal.json"
	leaseMaxAge    = 36 * time.Hour
	maxPipelineLog = 4 << 20
)

type vaultLease struct {
	root  string
	owner string
}

type vaultLeaseRecord struct {
	Owner       string `json:"owner"`
	ExecutionID string `json:"execution_id,omitempty"`
	StartedAt   string `json:"started_at"`
	ExpiresAt   string `json:"expires_at"`
}

// acquireVaultLease uses O_EXCL, which is the only create operation relied on
// for cross-container serialization. A stale lease is deliberately not stolen:
// a Cloud Run job may be long-running and a clock-based takeover could publish
// over a live run. Operators must remove an abandoned lease after inspection.
func acquireVaultLease(vault, executionID string) (*vaultLease, error) {
	r, err := os.OpenRoot(vault)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := r.MkdirAll(".olw", 0o755); err != nil {
		return nil, err
	}
	owner := fmt.Sprintf("%s:%d:%d", hostname(), os.Getpid(), time.Now().UnixNano())
	record := vaultLeaseRecord{Owner: owner, ExecutionID: executionID, StartedAt: time.Now().UTC().Format(time.RFC3339), ExpiresAt: time.Now().UTC().Add(leaseMaxAge).Format(time.RFC3339)}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	f, err := r.OpenFile(leasePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, err = f.Write(payload); err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = r.Remove(leasePath)
			return nil, fmt.Errorf("write vault lease: %w", err)
		}
		return &vaultLease{root: vault, owner: owner}, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create vault lease: %w", err)
	}
	data, readErr := r.ReadFile(leasePath)
	if readErr != nil {
		return nil, fmt.Errorf("vault lease exists and cannot be read: %w", readErr)
	}
	var existing vaultLeaseRecord
	if json.Unmarshal(data, &existing) != nil || existing.Owner == "" {
		return nil, errors.New("vault lease exists with invalid owner metadata; refusing overlap")
	}
	return nil, fmt.Errorf("vault lease is held by %s since %s; refusing overlap", existing.Owner, existing.StartedAt)
}

func (l *vaultLease) Release() error {
	r, err := os.OpenRoot(l.root)
	if err != nil {
		return err
	}
	defer r.Close()
	data, err := r.ReadFile(leasePath)
	if err != nil {
		return fmt.Errorf("read vault lease before release: %w", err)
	}
	var record vaultLeaseRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Owner != l.owner {
		return errors.New("vault lease owner changed; refusing to remove another execution's lease")
	}
	if err := r.Remove(leasePath); err != nil {
		return fmt.Errorf("remove vault lease: %w", err)
	}
	return nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-host"
	}
	return h
}

type publishEntry struct {
	Destination string `json:"destination"`
	Stage       string `json:"stage"`
	Backup      string `json:"backup"`
	HadOld      bool   `json:"had_old"`
	Published   bool   `json:"published"`
}

type publishJournalRecord struct {
	Stage   string         `json:"stage"`
	Backup  string         `json:"backup"`
	Entries []publishEntry `json:"entries"`
	Phase   string         `json:"phase"`
}

const publishPhaseCommitted = "committed"

const maxPublishEntries = 24

func validatePublishJournal(journal publishJournalRecord) error {
	if journal.Phase != "" && journal.Phase != "uncommitted" && journal.Phase != publishPhaseCommitted {
		return fmt.Errorf("invalid publish phase %q", journal.Phase)
	}
	if !validPublishDirectory(journal.Stage, ".lwc-worker-stage-") || !validPublishDirectory(journal.Backup, ".lwc-worker-backup-") || journal.Stage == journal.Backup {
		return errors.New("invalid publish stage or backup directory")
	}
	if len(journal.Entries) > maxPublishEntries {
		return fmt.Errorf("too many publish journal entries: %d", len(journal.Entries))
	}
	seen := make(map[string]struct{}, len(journal.Entries))
	for _, entry := range journal.Entries {
		if !validPublishDestination(entry.Destination) {
			return fmt.Errorf("invalid publish destination %q", entry.Destination)
		}
		if _, ok := seen[entry.Destination]; ok {
			return fmt.Errorf("duplicate publish destination %q", entry.Destination)
		}
		seen[entry.Destination] = struct{}{}
		if entry.Stage != filepath.ToSlash(filepath.Join(journal.Stage, entry.Destination)) || entry.Backup != filepath.ToSlash(filepath.Join(journal.Backup, entry.Destination)) {
			return fmt.Errorf("invalid publish paths for %q", entry.Destination)
		}
	}
	return nil
}

func validPublishDirectory(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return false
	}
	for _, r := range name[len(prefix):] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func validPublishDestination(destination string) bool {
	switch destination {
	case "wiki", "wiki.toml", "synto.toml", "cache/id_map.json", "cache/concepts.jsonl", "cache/dormant_concepts.jsonl", "cache/raw_status.json", "cache/suggested_queries.json", ".olw/state.db", ".synto/state.db", ".synto/INDEX.json":
		return true
	}
	const prefix = "cache/pipeline-"
	if !strings.HasPrefix(destination, prefix) || !strings.HasSuffix(destination, ".log") {
		return false
	}
	return validPipelineExecutionID(strings.TrimSuffix(strings.TrimPrefix(destination, prefix), ".log"))
}

// publishRename is replaceable only in tests to exercise rollback after an
// otherwise normal mounted-filesystem rename error.
var publishRename = func(root *os.Root, oldName, newName string) error {
	return root.Rename(oldName, newName)
}

var publishRemoveAll = func(root *os.Root, name string) error {
	return root.RemoveAll(name)
}

// syncWorkspaceOutputs stages every worker-owned durable output under the
// mounted vault, validates that stage, and then publishes under the caller's
// shared lease. No output file is ever written in place.
func syncWorkspaceOutputs(workspace, vault, executionID string) error {
	stage, err := stageWorkspaceOutputs(workspace, vault, executionID)
	if err != nil {
		return err
	}
	return publishStagedOutputs(vault, stage)
}

func stageWorkspaceOutputs(workspace, vault, executionID string) (string, error) {
	vaultRoot, err := os.OpenRoot(vault)
	if err != nil {
		return "", err
	}
	defer vaultRoot.Close()
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer workspaceRoot.Close()
	stage := ".lwc-worker-stage-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if err := vaultRoot.Mkdir(stage, 0o700); err != nil {
		return "", fmt.Errorf("create output stage: %w", err)
	}
	stageRoot, err := vaultRoot.OpenRoot(stage)
	if err != nil {
		_ = vaultRoot.RemoveAll(stage)
		return "", err
	}
	defer stageRoot.Close()
	fail := func(err error) (string, error) {
		_ = vaultRoot.RemoveAll(stage)
		return "", err
	}

	// wiki is intentionally created even if OLW produced no pages, so publish
	// mirrors deletions from the worker-owned tree.
	if err := stageRoot.Mkdir("wiki", 0o755); err != nil {
		return fail(err)
	}
	if err := copyTreeRoot(workspaceRoot, stageRoot, "wiki", "wiki", nil); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(fmt.Errorf("stage wiki: %w", err))
	}
	if err := copyOneIfExists(workspaceRoot, stageRoot, "wiki.toml"); err != nil {
		return fail(err)
	}
	if err := copyOneIfExists(workspaceRoot, stageRoot, "synto.toml"); err != nil {
		return fail(err)
	}
	for _, name := range []string{"cache/id_map.json", "cache/concepts.jsonl", "cache/dormant_concepts.jsonl", "cache/raw_status.json", "cache/suggested_queries.json", ".olw/state.db", ".synto/state.db", ".synto/INDEX.json"} {
		if err := copyOneIfExists(workspaceRoot, stageRoot, name); err != nil {
			return fail(err)
		}
	}
	if strings.TrimSpace(executionID) != "" {
		path, err := pipelineLogPath(workspace, executionID)
		if err != nil {
			return fail(err)
		}
		if err := copyOneIfExists(workspaceRoot, stageRoot, filepath.ToSlash(strings.TrimPrefix(path, workspace+string(filepath.Separator)))); err != nil {
			return fail(err)
		}
	}
	if err := validateStagedOutputs(stageRoot); err != nil {
		return fail(err)
	}
	return stage, nil
}

func validateStagedOutputs(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("staged symlink %q is not allowed", path)
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("staged non-regular file %q is not allowed", path)
		}
		return nil
	})
}

func copyTreeRoot(src, dst *os.Root, source, destination string, include func(string) bool) error {
	if err := safeRelativePath(source); err != nil {
		return err
	}
	info, err := src.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a safe directory", source)
	}
	return fs.WalkDir(src.FS(), source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe staged path %q", path)
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %q is not allowed", path)
		}
		if entry.IsDir() {
			return dst.MkdirAll(filepath.Join(destination, rel), 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("non-regular file %q is not allowed", path)
		}
		if include != nil && !include(rel) {
			return nil
		}
		return copyRootFile(src, dst, path, filepath.Join(destination, rel))
	})
}

func copyOneIfExists(src, dst *os.Root, name string) error {
	info, err := src.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", name)
	}
	return copyRootFile(src, dst, name, name)
}

func copyRootFile(src, dst *os.Root, source, destination string) error {
	data, err := src.ReadFile(source)
	if err != nil {
		return err
	}
	info, err := src.Lstat(source)
	if err != nil {
		return err
	}
	return atomicRootWrite(dst, destination, data, info.Mode().Perm())
}

func atomicRootWrite(root *os.Root, name string, data []byte, perm os.FileMode) error {
	if err := safeRelativePath(name); err != nil {
		return err
	}
	dir := filepath.Dir(name)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".write-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

func publishStagedOutputs(vault, stage string) (runErr error) {
	if !validPublishDirectory(stage, ".lwc-worker-stage-") {
		return fmt.Errorf("unsafe publish stage %q", stage)
	}
	if _, err := preflightGenerationOutputs(filepath.Join(vault, stage)); err != nil {
		return fmt.Errorf("generation output validation failed: %w", err)
	}
	r, err := os.OpenRoot(vault)
	if err != nil {
		return err
	}
	defer r.Close()
	stageRoot, err := r.OpenRoot(stage)
	if err != nil {
		return err
	}
	if err := validateStagedOutputs(stageRoot); err != nil {
		stageRoot.Close()
		return err
	}
	stageRoot.Close()
	backup := ".lwc-worker-backup-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if err := r.Mkdir(backup, 0o700); err != nil {
		return err
	}
	journal := publishJournalRecord{Stage: stage, Backup: backup}
	for _, destination := range []string{"wiki", "wiki.toml", "synto.toml", "cache/id_map.json", "cache/concepts.jsonl", "cache/dormant_concepts.jsonl", "cache/raw_status.json", "cache/suggested_queries.json", ".olw/state.db", ".synto/state.db", ".synto/INDEX.json"} {
		journal.Entries = append(journal.Entries, publishEntry{Destination: destination, Stage: filepath.Join(stage, destination), Backup: filepath.Join(backup, destination)})
	}
	cacheEntries, err := r.OpenRoot(stage)
	if err != nil {
		return err
	}
	entries, err := fs.ReadDir(cacheEntries.FS(), "cache")
	cacheEntries.Close()
	if err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "pipeline-") && strings.HasSuffix(entry.Name(), ".log") {
				name := filepath.ToSlash(filepath.Join("cache", entry.Name()))
				journal.Entries = append(journal.Entries, publishEntry{Destination: name, Stage: filepath.Join(stage, name), Backup: filepath.Join(backup, name)})
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := validatePublishJournal(journal); err != nil {
		return err
	}
	if err := writePublishJournal(vault, journal); err != nil {
		return err
	}
	defer func() {
		if runErr != nil && journal.Phase != publishPhaseCommitted {
			runErr = errors.Join(runErr, rollbackPublish(vault, journal))
		}
	}()
	for i := range journal.Entries {
		entry := &journal.Entries[i]
		if _, err := r.Lstat(entry.Stage); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if _, err := r.Lstat(entry.Destination); err == nil {
			info, statErr := r.Lstat(entry.Destination)
			if statErr != nil {
				return statErr
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("destination symlink %q is not allowed", entry.Destination)
			}
			if err := r.MkdirAll(filepath.Dir(entry.Backup), 0o700); err != nil {
				return err
			}
			// Persist intent before either rename. Recovery can safely treat an
			// intended move as completed: removing a missing destination is harmless,
			// while restoring a backup avoids exposing a mixed generation.
			entry.HadOld = true
			if err := writePublishJournal(vault, journal); err != nil {
				return err
			}
			if err := publishRename(r, entry.Destination, entry.Backup); err != nil {
				return fmt.Errorf("backup %s: %w", entry.Destination, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := r.MkdirAll(filepath.Dir(entry.Destination), 0o755); err != nil {
			return err
		}
		entry.Published = true
		if err := writePublishJournal(vault, journal); err != nil {
			return err
		}
		if err := publishRename(r, entry.Stage, entry.Destination); err != nil {
			return fmt.Errorf("publish %s: %w", entry.Destination, err)
		}
	}
	// Once this state reaches disk the new generation is authoritative. Cleanup
	// may be retried by recovery, but it must never roll back published output.
	journal.Phase = publishPhaseCommitted
	if err := writePublishJournal(vault, journal); err != nil {
		return err
	}
	if err := publishRemoveAll(r, backup); err != nil {
		return err
	}
	if err := publishRemoveAll(r, stage); err != nil {
		return err
	}
	return r.Remove(publishJournal)
}

func writePublishJournal(vault string, journal publishJournalRecord) error {
	if err := validatePublishJournal(journal); err != nil {
		return err
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return writeFileAtomicWithin(vault, publishJournal, data)
}

func recoverInterruptedPublish(vault string) error {
	r, err := os.OpenRoot(vault)
	if err != nil {
		return err
	}
	defer r.Close()
	data, err := r.ReadFile(publishJournal)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal publishJournalRecord
	if err := json.Unmarshal(data, &journal); err != nil {
		return errors.New("invalid interrupted publish journal; refusing to publish")
	}
	if err := validatePublishJournal(journal); err != nil {
		return fmt.Errorf("invalid interrupted publish journal; refusing to publish: %w", err)
	}
	if journal.Phase == publishPhaseCommitted {
		return cleanupCommittedPublish(vault, journal)
	}
	return rollbackPublish(vault, journal)
}

func cleanupCommittedPublish(vault string, journal publishJournalRecord) error {
	if err := validatePublishJournal(journal); err != nil {
		return fmt.Errorf("invalid interrupted publish journal; refusing cleanup: %w", err)
	}
	r, err := os.OpenRoot(vault)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := publishRemoveAll(r, journal.Backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := publishRemoveAll(r, journal.Stage); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := r.Remove(publishJournal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func rollbackPublish(vault string, journal publishJournalRecord) error {
	if err := validatePublishJournal(journal); err != nil {
		return fmt.Errorf("invalid interrupted publish journal; refusing rollback: %w", err)
	}
	r, err := os.OpenRoot(vault)
	if err != nil {
		return err
	}
	defer r.Close()
	var errs []error
	for i := len(journal.Entries) - 1; i >= 0; i-- {
		entry := journal.Entries[i]
		if entry.Published {
			if err := r.RemoveAll(entry.Destination); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
		if entry.HadOld {
			if err := r.MkdirAll(filepath.Dir(entry.Destination), 0o755); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := r.Rename(entry.Backup, entry.Destination); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	if err := publishRemoveAll(r, journal.Stage); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := publishRemoveAll(r, journal.Backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		if err := r.Remove(publishJournal); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func publishWorkspaceFailureLog(workspace, vault string, cfg workerConfig) error {
	if strings.TrimSpace(cfg.ExecutionID) == "" {
		return nil
	}
	path, err := pipelineLogPath(workspace, cfg.ExecutionID)
	if err != nil {
		return err
	}
	name := filepath.ToSlash(filepath.Join("cache", filepath.Base(path)))
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer workspaceRoot.Close()
	info, err := workspaceRoot.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("failure log %q is not a regular file", name)
	}
	data, err := sanitizedPipelineLogData(workspaceRoot, name, logSecrets(cfg))
	if err != nil {
		return err
	}
	vaultRoot, err := os.OpenRoot(vault)
	if err != nil {
		return err
	}
	defer vaultRoot.Close()
	stage := ".lwc-worker-failure-log-stage-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if err := vaultRoot.Mkdir(stage, 0o700); err != nil {
		return err
	}
	defer vaultRoot.RemoveAll(stage)
	stageRoot, err := vaultRoot.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer stageRoot.Close()
	if err := atomicRootWrite(stageRoot, name, data, 0o600); err != nil {
		return err
	}
	if err := validateStagedOutputs(stageRoot); err != nil {
		return err
	}
	return atomicRootWrite(vaultRoot, name, data, 0o600)
}

func sanitizePipelineLog(vault, executionID string, secrets []string) error {
	if strings.TrimSpace(executionID) == "" {
		return nil
	}
	path, err := pipelineLogPath(vault, executionID)
	if err != nil {
		return err
	}
	name := filepath.ToSlash(filepath.Join("cache", filepath.Base(path)))
	r, err := os.OpenRoot(vault)
	if err != nil {
		return err
	}
	defer r.Close()
	data, err := sanitizedPipelineLogData(r, name, secrets)
	if err != nil {
		return err
	}
	return atomicRootWrite(r, name, data, 0o600)
}

func sanitizedPipelineLogData(root *os.Root, name string, secrets []string) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxPipelineLog+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	for _, secret := range secrets {
		if secret != "" {
			data = []byte(strings.ReplaceAll(string(data), secret, "[REDACTED]"))
		}
	}
	if len(data) > maxPipelineLog {
		data = data[:maxPipelineLog]
	}
	return data, nil
}
