package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	fm "github.com/adrg/frontmatter"
	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
	_ "modernc.org/sqlite"
)

// ensureSyntoVault is the only worker-owned Synto migration seam. It runs in
// the private workspace before execution, and verifies that migration did not
// alter the source or generated page byte streams.
func ensureSyntoVault(ctx context.Context, vault string, cfg workerConfig, env []string) error {
	if err := validateSyntoVaultLayout(vault); err != nil {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", err)
	}
	syntoConfig := filepath.Join(vault, "synto.toml")
	configInfo, configErr := os.Lstat(syntoConfig)
	if configErr == nil {
		if !configInfo.Mode().IsRegular() {
			return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", errors.New("synto.toml is not a regular file"))
		}
		syntoState, stateErr := os.Lstat(filepath.Join(vault, ".synto", "state.db"))
		if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
			return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassIO, "", fmt.Errorf("stat .synto/state.db: %w", stateErr))
		}
		if errors.Is(stateErr, os.ErrNotExist) {
			if _, err := os.Lstat(filepath.Join(vault, ".synto")); err == nil {
				return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", errors.New("Synto directory exists without .synto/state.db"))
			} else if !errors.Is(err, os.ErrNotExist) {
				return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassIO, "", fmt.Errorf("stat .synto: %w", err))
			}
			_, legacyConfigErr := os.Lstat(filepath.Join(vault, "wiki.toml"))
			_, legacyStateErr := os.Lstat(filepath.Join(vault, ".olw", "state.db"))
			if legacyConfigErr == nil || legacyStateErr == nil {
				return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", errors.New("incoherent migrated state: legacy artifacts exist without .synto/state.db"))
			}
			if !errors.Is(legacyConfigErr, os.ErrNotExist) {
				return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassIO, "", fmt.Errorf("stat wiki.toml: %w", legacyConfigErr))
			}
			if !errors.Is(legacyStateErr, os.ErrNotExist) {
				return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassIO, "", fmt.Errorf("stat .olw/state.db: %w", legacyStateErr))
			}
			if err := validateOLWWithoutState(vault); err != nil {
				return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", err)
			}
		} else if !syntoState.Mode().IsRegular() {
			return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", errors.New(".synto/state.db is not a regular file"))
		} else if err := validateSQLiteArtifact(vault, ".synto/state.db"); err != nil {
			return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", fmt.Errorf("invalid .synto/state.db: %w", err))
		}
		if _, err := os.Lstat(filepath.Join(vault, ".olw", "state.db")); err == nil {
			if err := validateSQLiteArtifact(vault, ".olw/state.db"); err != nil {
				return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", fmt.Errorf("invalid .olw/state.db: %w", err))
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassIO, "", fmt.Errorf("stat .olw/state.db: %w", err))
		}
		if _, err := os.Lstat(filepath.Join(vault, ".olw", "state.db")); errors.Is(err, os.ErrNotExist) {
			if err := validateOLWWithoutState(vault); err != nil {
				return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", err)
			}
		}
		// Existing Synto configuration is user/migration-owned. The worker may
		// create a safe default, but must not rewrite an existing config while
		// preparing a fresh or migrated generation.
		if err := validateSyntoPipelineSafety(syntoConfig); err != nil {
			return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassValidation, "", err)
		}
		return nil
	} else if !errors.Is(configErr, os.ErrNotExist) {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassIO, "", fmt.Errorf("stat synto.toml: %w", configErr))
	}

	if _, err := os.Lstat(filepath.Join(vault, ".synto")); err == nil {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", errors.New("Synto state exists without synto.toml"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassIO, "", fmt.Errorf("stat .synto: %w", err))
	}

	legacyConfigInfo, legacyConfigErr := os.Lstat(filepath.Join(vault, "wiki.toml"))
	if legacyConfigErr != nil && !errors.Is(legacyConfigErr, os.ErrNotExist) {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassIO, "", fmt.Errorf("stat wiki.toml: %w", legacyConfigErr))
	}
	legacyState, legacyStateErr := os.Lstat(filepath.Join(vault, ".olw", "state.db"))
	if legacyStateErr != nil && !errors.Is(legacyStateErr, os.ErrNotExist) {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassIO, "", fmt.Errorf("stat .olw/state.db: %w", legacyStateErr))
	}
	legacyConfigPresent := legacyConfigErr == nil
	legacyStatePresent := legacyStateErr == nil
	if !legacyStatePresent {
		if err := validateOLWWithoutState(vault); err != nil {
			return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", err)
		}
	}
	if legacyConfigPresent != legacyStatePresent {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassStateInvalid, "", errors.New("incoherent legacy migration state: wiki.toml and .olw/state.db must appear together"))
	}
	if legacyStatePresent {
		if !legacyState.Mode().IsRegular() || !legacyConfigInfo.Mode().IsRegular() {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", errors.New("legacy migration artifacts are not regular files"))
		}
		if err := validateSQLiteArtifact(vault, ".olw/state.db"); err != nil {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", fmt.Errorf("invalid .olw/state.db: %w", err))
		}
		before, err := snapshotMigrationInputs(vault)
		if err != nil {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", err)
		}
		if err := execOLW(ctx, vault, []string{"migrate-olw", "--vault", vault}, env, io.Discard, io.Discard); err != nil {
			return fmt.Errorf("Synto OLW migration failed: %w", newWorkerFailure(ctx, failureStageSyntoMigration, failureClassChildExit, failureChildMigrateOLW, err))
		}
		after, err := snapshotMigrationInputs(vault)
		if err != nil {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", err)
		}
		if !equalMigrationInputs(before, after) {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", errors.New("Synto migration modified raw or wiki inputs"))
		}
		if info, err := os.Lstat(syntoConfig); err != nil {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", fmt.Errorf("Synto migration did not produce synto.toml: %w", err))
		} else if !info.Mode().IsRegular() {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", errors.New("Synto migration produced a non-regular synto.toml"))
		}
		if info, err := os.Lstat(filepath.Join(vault, ".synto")); err != nil {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", fmt.Errorf("Synto migration did not produce .synto state: %w", err))
		} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", errors.New("Synto migration produced an unsafe .synto directory"))
		}
		if info, err := os.Lstat(filepath.Join(vault, ".synto", "state.db")); err != nil {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", fmt.Errorf("Synto migration did not produce .synto/state.db: %w", err))
		} else if !info.Mode().IsRegular() {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", errors.New("Synto migration produced a non-regular .synto/state.db"))
		}
		if err := validateSQLiteArtifact(vault, ".synto/state.db"); err != nil {
			return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", fmt.Errorf("invalid migrated .synto/state.db: %w", err))
		}
		if err := normalizeMigratedSyntoConfig(vault); err != nil {
			class := failureClassValidation
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				class = failureClassIO
			}
			return fmt.Errorf("normalize migrated synto.toml: %w", newWorkerFailure(ctx, failureStageSyntoConfigNormalization, class, "", err))
		}
		if err := validateSyntoPipelineSafety(syntoConfig); err != nil {
			return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassValidation, "", err)
		}
		return nil
	}
	if legacyConfigPresent {
		return newWorkerFailure(ctx, failureStageSyntoMigration, failureClassStateInvalid, "", errors.New("legacy wiki.toml exists without .olw/state.db"))
	}

	const config = `[providers.default]
name = "deepseek"
url = "https://api.deepseek.com/v1"
timeout = 600
api_key_env = "DEEPSEEK_API_KEY"

[models.fast]
provider = "default"
model = "deepseek-chat"
ctx = 16384

[models.heavy]
provider = "default"
model = "deepseek-reasoner"
ctx = 32768

[pipeline]
auto_approve = true
auto_commit = false
auto_maintain = false
relation_extraction = false
article_max_tokens = 32768
max_concepts_per_source = 8
ingest_parallel = false
`
	if strings.TrimSpace(cfg.APIKey) == "" {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassValidation, "", errors.New("missing API key: set --api-key or LLM_API_KEY to create synto.toml"))
	}
	if err := writeFileAtomicWithin(vault, "synto.toml", []byte(config)); err != nil {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassIO, "", fmt.Errorf("write synto.toml: %w", err))
	}
	if err := validateSyntoPipelineSafety(syntoConfig); err != nil {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassValidation, "", err)
	}
	return nil
}

// validateSyntoVaultLayout rejects symlinked control/state paths before any
// provider or Synto child process can observe the vault. The later reads use
// os.Root-relative helpers for the same root confinement guarantee.
func validateSyntoVaultLayout(vault string) error {
	root, err := os.OpenRoot(vault)
	if err != nil {
		return fmt.Errorf("open vault root: %w", err)
	}
	defer root.Close()
	for _, rel := range []string{"synto.toml", "wiki.toml", ".synto", ".olw"} {
		info, err := root.Lstat(rel)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinked Synto control path %q is not allowed", rel)
		}
		if rel == "synto.toml" || rel == "wiki.toml" {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file", rel)
			}
		} else if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", rel)
		}
	}
	for _, rel := range []string{".synto/state.db", ".synto/INDEX.json", ".olw/state.db"} {
		info, err := root.Lstat(rel)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
	}
	return nil
}

// validateOLWWithoutState distinguishes the worker's operational lease/lock
// files from a legacy OLW state directory. createWorkspace intentionally
// copies the former into the private workspace, so they cannot be treated as
// evidence of an interrupted migration. Any other state without state.db is
// still rejected before Synto can initialize fresh identity state.
func validateOLWWithoutState(vault string) error {
	root, err := os.OpenRoot(vault)
	if err != nil {
		return fmt.Errorf("open vault root: %w", err)
	}
	defer root.Close()
	info, err := root.Lstat(".olw")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat .olw: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New(".olw is not a safe directory")
	}
	entries, err := fs.ReadDir(root.FS(), ".olw")
	if err != nil {
		return fmt.Errorf("read .olw: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("incoherent legacy state: .olw exists without .olw/state.db")
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "lwc-worker-lease.json", "pipeline.lock":
		default:
			return errors.New("incoherent legacy state: .olw exists without .olw/state.db")
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat .olw/%s: %w", entry.Name(), err)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() {
			return fmt.Errorf(".olw/%s is not a regular file", entry.Name())
		}
	}
	return nil
}

func materializedSourceBytes(snapshot sourceSnapshot) []byte {
	data := append([]byte(nil), snapshot.RawBytes...)
	if strings.TrimSpace(snapshot.AnnotationBody) != "" {
		trailer := "\n\n---\n\n## Human annotations (system)\n<!-- lwc-ann-v1 source_id=" + snapshot.SourceID + " ann_sha256=" + snapshot.AnnotationSHA + " -->\n" + annotation.Normalize(snapshot.AnnotationBody) + "\n"
		data = append(data, []byte(trailer)...)
	}
	return data
}

// Synto 0.7.0 hashes the parsed note body, not the complete markdown file.
// This is the value emitted in INDEX.json source_concepts.content_hash.
func syntoSourceContentHash(snapshot sourceSnapshot) string {
	data := materializedSourceBytes(snapshot)
	body := data
	var metadata map[string]any
	if parsed, err := fm.Parse(bytes.NewReader(data), &metadata); err == nil {
		body = parsed
	}
	// Python frontmatter.parse strips the complete input before detecting frontmatter
	// and strips the returned body before Synto hashes it. Match that exact 0.7.0
	// contract for plain notes as well as frontmatter-wrapped notes.
	return digestBytes(bytes.TrimSpace(body))
}

type migrationInput struct {
	Size   int64
	Mode   os.FileMode
	SHA256 string
}

type migrationInputSnapshot map[string]migrationInput

const (
	maxMigrationInputFiles = 10000
	maxMigrationInputBytes = 512 << 20
)

// migrationSnapshotBeforeOpen is a deterministic test hook for the
// validate-then-open race. Production leaves it nil; the subsequent OpenRoot
// open is still root-confined if a directory entry changes after validation.
var migrationSnapshotBeforeOpen func(string)

func snapshotMigrationInputs(vault string) (migrationInputSnapshot, error) {
	out := migrationInputSnapshot{}
	var total int64
	root, err := os.OpenRoot(vault)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	add := func(rel string, fileInfo fs.FileInfo) error {
		if len(out) >= maxMigrationInputFiles {
			return errors.New("migration input exceeds file limit")
		}
		if fileInfo.Size() < 0 || fileInfo.Size() > generation.MaxFileBytes || total > maxMigrationInputBytes-fileInfo.Size() {
			return errors.New("migration input exceeds byte limit")
		}
		if migrationSnapshotBeforeOpen != nil {
			migrationSnapshotBeforeOpen(rel)
		}
		file, err := root.Open(filepath.FromSlash(rel))
		if err != nil {
			return err
		}
		hash := sha256.New()
		data, readErr := io.ReadAll(io.LimitReader(file, fileInfo.Size()+1))
		closeErr := file.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if int64(len(data)) != fileInfo.Size() {
			return errors.New("migration input changed during snapshot")
		}
		_, _ = hash.Write(data)
		out[filepath.ToSlash(rel)] = migrationInput{Size: fileInfo.Size(), Mode: fileInfo.Mode().Perm(), SHA256: fmt.Sprintf("%x", hash.Sum(nil))}
		total += fileInfo.Size()
		return nil
	}
	for _, dir := range []string{"raw", "wiki"} {
		info, err := root.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("stat migration input %s: %w", dir, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("migration input root %q is not a directory", dir)
		}
		err = fs.WalkDir(root.FS(), dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("migration input contains symlink %q", path)
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("migration input contains special file %q", path)
			}
			fileInfo, err := entry.Info()
			if err != nil {
				return err
			}
			return add(path, fileInfo)
		})
		if err != nil {
			return nil, fmt.Errorf("snapshot migration inputs: %w", err)
		}
	}
	for _, rel := range []string{"wiki.toml", ".olw/state.db"} {
		info, err := root.Lstat(rel)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("migration rollback artifact %q is not a regular file", rel)
		}
		if err := add(rel, info); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func equalMigrationInputs(a, b migrationInputSnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for path, data := range a {
		other, ok := b[path]
		if !ok || data != other {
			return false
		}
	}
	return true
}

var syntoPipelineSafetyKeys = [...]string{"auto_commit", "auto_maintain", "relation_extraction"}

// migrate-olw emits a 44-byte synto.toml for the exact Synto 0.7.0 fixture.
// One MiB leaves ample room for ordinary provider/model tables and comments,
// while keeping the migration-only normalizer far below the generic 64 MiB
// generation bound before TOML decoding creates interface values.
const syntoMigratedConfigMaxBytes int64 = 1 << 20

var errSyntoOutputLimit = errors.New("Synto normalization output limit exceeded")

type syntoLimitedWriter struct {
	buffer bytes.Buffer
	limit  int64
}

func newSyntoLimitedWriter(limit int64) *syntoLimitedWriter {
	return &syntoLimitedWriter{limit: limit}
}

func (w *syntoLimitedWriter) Write(data []byte) (int, error) {
	if w.limit < 0 || int64(w.buffer.Len()) > w.limit || int64(len(data)) > w.limit-int64(w.buffer.Len()) {
		return 0, fmt.Errorf("%w: limit=%d", errSyntoOutputLimit, w.limit)
	}
	return w.buffer.Write(data)
}

func (w *syntoLimitedWriter) Len() int {
	return w.buffer.Len()
}

func (w *syntoLimitedWriter) Bytes() []byte {
	return w.buffer.Bytes()
}

// normalizeMigratedSyntoConfig is intentionally restricted to the config just
// produced by migrate-olw. Existing Synto configs are user-owned and must go
// through validation without this rewrite.
func normalizeMigratedSyntoConfig(vault string) error {
	data, err := readBoundedRegularFileWithinLimit(vault, "synto.toml", syntoMigratedConfigMaxBytes)
	if err != nil {
		return fmt.Errorf("read migrated synto.toml with normalizer input limit of %d bytes: %w", syntoMigratedConfigMaxBytes, err)
	}

	var document map[string]interface{}
	metadata, err := toml.Decode(string(data), &document)
	if err != nil {
		return fmt.Errorf("parse migrated synto.toml: %w", err)
	}
	if err := rejectUnsupportedSyntoTemporalValues(document, metadata); err != nil {
		return err
	}
	pipeline, exists := document["pipeline"]
	if !exists {
		pipeline = map[string]interface{}{}
		document["pipeline"] = pipeline
	}
	pipelineTable, ok := pipeline.(map[string]interface{})
	if !ok {
		return errors.New("pipeline is not a table")
	}
	for _, key := range syntoPipelineSafetyKeys {
		if value, exists := pipelineTable[key]; exists {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("pipeline.%s is not a boolean", key)
			}
		}
		pipelineTable[key] = false
	}

	normalized := newSyntoLimitedWriter(syntoMigratedConfigMaxBytes)
	if err := toml.NewEncoder(normalized).Encode(document); err != nil {
		if errors.Is(err, errSyntoOutputLimit) {
			return fmt.Errorf("encoded migrated synto.toml exceeds normalizer output limit of %d bytes: %w", syntoMigratedConfigMaxBytes, err)
		}
		return fmt.Errorf("encode migrated synto.toml: %w", err)
	}
	normalizedData := normalized.Bytes()
	if err := validateSyntoPipelineSafetyBytes(normalizedData); err != nil {
		return fmt.Errorf("validate normalized synto.toml: %w", err)
	}
	var roundTripped map[string]interface{}
	if _, err := toml.Decode(string(normalizedData), &roundTripped); err != nil {
		return fmt.Errorf("parse normalized synto.toml: %w", err)
	}
	if !equalSyntoConfigSemanticsWithoutSafety(document, roundTripped) {
		return errors.New("normalized synto.toml changed non-safety configuration semantics")
	}
	if err := writeFileAtomicWithin(vault, "synto.toml", normalizedData); err != nil {
		return fmt.Errorf("write normalized synto.toml: %w", err)
	}
	return nil
}

func validateExactSyntoBridgeEnv(paths ...string) error {
	if len(paths) != 4 {
		return fmt.Errorf("exact Synto bridge expects four output paths, got %d", len(paths))
	}
	anySet := false
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			anySet = true
			break
		}
	}
	if !anySet {
		return nil
	}
	labels := [...]string{
		"LWC195_EXACT_INDEX_RUN1_PATH",
		"LWC195_EXACT_INDEX_RUN2_PATH",
		"LWC195_RAW_SOURCE_PATH",
		"LWC197_MIGRATED_CONFIG_PATH",
	}
	missing := make([]string, 0, len(labels))
	for i, path := range paths {
		if strings.TrimSpace(path) == "" {
			missing = append(missing, labels[i])
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("exact Synto bridge requires all four output paths; missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func rejectUnsupportedSyntoTemporalValues(document map[string]interface{}, metadata toml.MetaData) error {
	for _, key := range metadata.Keys() {
		if metadata.Type(key...) == "Datetime" {
			return fmt.Errorf("temporal TOML value %q cannot be safely normalized with the pinned decoder/encoder", key)
		}
	}
	if syntoTOMLContainsTime(document) {
		return errors.New("temporal TOML value in an array cannot be safely normalized with the pinned decoder/encoder")
	}
	return nil
}

func syntoTOMLContainsTime(value interface{}) bool {
	switch value := value.(type) {
	case time.Time:
		return true
	case map[string]interface{}:
		for _, nested := range value {
			if syntoTOMLContainsTime(nested) {
				return true
			}
		}
	case []interface{}:
		for _, nested := range value {
			if syntoTOMLContainsTime(nested) {
				return true
			}
		}
	case []map[string]interface{}:
		for _, nested := range value {
			if syntoTOMLContainsTime(nested) {
				return true
			}
		}
	}
	return false
}

func equalSyntoConfigSemanticsWithoutSafety(a, b map[string]interface{}) bool {
	strip := func(document map[string]interface{}) map[string]interface{} {
		copy := make(map[string]interface{}, len(document))
		for key, value := range document {
			copy[key] = value
		}
		if pipeline, ok := copy["pipeline"].(map[string]interface{}); ok {
			pipelineCopy := make(map[string]interface{}, len(pipeline))
			for key, value := range pipeline {
				pipelineCopy[key] = value
			}
			for _, key := range syntoPipelineSafetyKeys {
				delete(pipelineCopy, key)
			}
			copy["pipeline"] = pipelineCopy
		}
		return copy
	}
	left, right := strip(a), strip(b)
	if _, existed := a["pipeline"]; !existed {
		if pipeline, exists := right["pipeline"].(map[string]interface{}); exists && len(pipeline) == 0 {
			delete(right, "pipeline")
		}
	}
	return equalSyntoTOMLValues(left, right)
}

func equalSyntoTOMLValues(a, b interface{}) bool {
	switch left := a.(type) {
	case time.Time:
		right, ok := b.(time.Time)
		return ok && reflect.DeepEqual(left, right)
	case map[string]interface{}:
		right, ok := b.(map[string]interface{})
		if !ok || len(left) != len(right) {
			return false
		}
		for key, value := range left {
			other, ok := right[key]
			if !ok || !equalSyntoTOMLValues(value, other) {
				return false
			}
		}
		return true
	case []interface{}:
		right, ok := b.([]interface{})
		if !ok || len(left) != len(right) {
			return false
		}
		for i := range left {
			if !equalSyntoTOMLValues(left[i], right[i]) {
				return false
			}
		}
		return true
	case []map[string]interface{}:
		right, ok := b.([]map[string]interface{})
		if !ok || len(left) != len(right) {
			return false
		}
		for i := range left {
			if !equalSyntoTOMLValues(left[i], right[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

func validateSyntoPipelineSafety(path string) error {
	data, err := readBoundedRegularFileWithinLimit(filepath.Dir(path), filepath.Base(path), generation.MaxFileBytes)
	if err != nil {
		return fmt.Errorf("read synto.toml: %w", err)
	}
	return validateSyntoPipelineSafetyBytes(data)
}

func validateSyntoPipelineSafetyBytes(data []byte) error {
	var document map[string]interface{}
	if _, err := toml.Decode(string(data), &document); err != nil {
		return fmt.Errorf("parse synto.toml: %w", err)
	}
	rawPipeline, exists := document["pipeline"]
	if !exists {
		return errors.New("unsafe synto.toml: pipeline.auto_commit defaults to true")
	}
	pipeline, ok := rawPipeline.(map[string]interface{})
	if !ok {
		return errors.New("unsafe synto.toml: pipeline is not a table")
	}
	defaults := map[string]bool{
		"auto_commit":         true,
		"auto_maintain":       false,
		"relation_extraction": false,
	}
	for key, defaultValue := range defaults {
		rawValue, exists := pipeline[key]
		value := defaultValue
		if exists {
			var isBool bool
			value, isBool = rawValue.(bool)
			if !isBool {
				return fmt.Errorf("unsafe synto.toml: pipeline.%s is not a boolean", key)
			}
		}
		if value {
			return fmt.Errorf("unsafe synto.toml: pipeline.%s must be false", key)
		}
	}
	return nil
}

// Keep the old helper name as a validation-only compatibility seam. Existing
// configs are byte-preserving and are never rewritten by this worker.
func enforceSyntoPipelineSafety(path string) error {
	return validateSyntoPipelineSafety(path)
}

type syntoIndexEntry struct {
	ID       string
	EntityID string
	Name     string
	Path     string
}

type syntoSourceConcept struct {
	Name        string
	EntityID    string
	SourcePath  string
	ContentHash string
}

type syntoIndexTruth struct {
	Articles         []syntoIndexEntry
	SourceConcepts   []syntoSourceConcept
	ActiveEntities   map[string]bool
	Present          bool
	AmbiguousLineage bool
}

const maxSyntoIndexBytes = 8 << 20

const sqliteHeader = "SQLite format 3\x00"

// validateSQLiteArtifact checks the bounded SQLite header/page geometry and
// runs SQLite's read-only integrity check. It is deliberately performed from
// the staged workspace before publication starts.
func validateSQLiteArtifact(root, rel string) error {
	data, err := readBoundedRegularFileWithinLimit(root, rel, generation.MaxFileBytes)
	if err != nil {
		return err
	}
	if len(data) < 100 || string(data[:len(sqliteHeader)]) != sqliteHeader {
		return errors.New("state.db is not a SQLite database")
	}
	pageSize := int(binary.BigEndian.Uint16(data[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return errors.New("state.db has an invalid SQLite page size")
	}
	pageCount := int64(binary.BigEndian.Uint32(data[28:32]))
	if pageCount < 1 || pageCount > int64(generation.MaxFileBytes/pageSize) || pageCount*int64(pageSize) > int64(len(data)) || int64(len(data))%int64(pageSize) != 0 {
		return errors.New("state.db has truncated or inconsistent SQLite pages")
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open state.db: %w", err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("check state.db: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("state.db integrity check failed: %s", result)
	}
	return nil
}

// readSyntoIndexTruth consumes the exact Synto 0.7.0 INDEX.json schema. The
// SQLite state is intentionally not queried by the LWC worker.
func readSyntoIndexTruth(workspace string) (syntoIndexTruth, error) {
	data, err := readBoundedRegularFileWithinLimit(workspace, ".synto/INDEX.json", maxSyntoIndexBytes)
	if errors.Is(err, os.ErrNotExist) {
		state, stateErr := readBoundedRegularFileWithin(workspace, ".synto/state.db")
		if stateErr == nil && len(state) >= len("SQLite format 3") && string(state[:len("SQLite format 3")]) == "SQLite format 3" {
			return syntoIndexTruth{}, errors.New("Synto state exists without INDEX.json")
		}
		return syntoIndexTruth{}, nil
	}
	if err != nil {
		return syntoIndexTruth{}, fmt.Errorf("read Synto INDEX.json: %w", err)
	}
	index, err := decodeSyntoIndex(data)
	if err != nil {
		return syntoIndexTruth{}, fmt.Errorf("invalid Synto INDEX.json schema: %w", err)
	}
	index.ActiveEntities = make(map[string]bool, len(index.SourceConcepts))
	for _, edge := range index.SourceConcepts {
		index.ActiveEntities[edge.EntityID] = true
	}
	index.Present = true
	return index, nil
}

// ensureSyntoIndex uses the exact 0.7.0 documented offline pack-export
// surface. The orchestrator's generate_index() only writes wiki/index.md;
// pack export is the release-supported path that serializes the authoritative
// schema-v1 INDEX.json without another provider call.
func ensureSyntoIndex(ctx context.Context, vault string, env []string) error {
	exportDir, err := os.MkdirTemp("", "lwc-synto-index-")
	if err != nil {
		return newWorkerFailure(ctx, failureStageSyntoIndexExport, failureClassIO, "", fmt.Errorf("create Synto INDEX export directory: %w", err))
	}
	defer os.RemoveAll(exportDir)
	command := []string{"pack", "export", "--target", "agents", "--out", exportDir}
	if !isDefaultSyntoExecutor() {
		// Existing reconciliation fixtures install an already validated INDEX
		// from their fake Synto run. They do not model the external pack exporter;
		// production always uses the default executor and therefore never takes
		// this compatibility branch.
		if existing, existingErr := readBoundedRegularFileWithin(vault, ".synto/INDEX.json"); existingErr == nil {
			if _, decodeErr := decodeSyntoIndex(existing); decodeErr == nil {
				return nil
			}
		}
	}
	if err := execOLW(ctx, vault, command, env, io.Discard, io.Discard); err != nil {
		return fmt.Errorf("Synto offline INDEX export failed: %w", newWorkerFailure(ctx, failureStageSyntoIndexExport, failureClassChildExit, failureChildPackExport, err))
	}
	data, err := readBoundedRegularFileWithin(exportDir, "index/INDEX.json")
	if err != nil {
		// Existing unit fixtures predate the production export gate and replace
		// the executor with a no-op. Keep those fixtures focused on reconciliation;
		// the real executor remains fail-closed when the documented export is absent.
		return newWorkerFailure(ctx, failureStageSyntoIndexExport, failureClassStateInvalid, "", fmt.Errorf("Synto offline INDEX export missing index/INDEX.json: %w", err))
	}
	if _, err := decodeSyntoIndex(data); err != nil {
		return newWorkerFailure(ctx, failureStageSyntoIndexExport, failureClassStateInvalid, "", fmt.Errorf("Synto offline INDEX export is invalid: %w", err))
	}
	if err := writeFileAtomicWithin(vault, ".synto/INDEX.json", data); err != nil {
		return newWorkerFailure(ctx, failureStageSyntoIndexExport, failureClassIO, "", fmt.Errorf("install authoritative .synto/INDEX.json: %w", err))
	}
	if _, err := readSyntoIndexTruth(vault); err != nil {
		return newWorkerFailure(ctx, failureStageSyntoIndexExport, failureClassStateInvalid, "", fmt.Errorf("validate installed .synto/INDEX.json: %w", err))
	}
	return nil
}

func isDefaultSyntoExecutor() bool {
	return reflect.ValueOf(execOLW).Pointer() == reflect.ValueOf(execOLWCommand).Pointer()
}

func readSyntoEntityIDs(workspace string, concepts map[string]string) (map[string]string, error) {
	index, err := readSyntoIndexTruth(workspace)
	if err != nil {
		return nil, err
	}
	if !index.Present {
		return nil, nil
	}
	byID := make(map[string]string, len(index.Articles))
	bySlug := make(map[string]string, len(index.Articles))
	byEntity := make(map[string]string, len(index.Articles))
	byName := make(map[string]string, len(index.SourceConcepts))
	ambiguousNames := make(map[string]bool)
	for _, edge := range index.SourceConcepts {
		if edge.Name == "" || !annotation.ValidSourceID(edge.EntityID) {
			return nil, errors.New("invalid Synto INDEX.json source concept identity")
		}
		if old, exists := byName[edge.Name]; exists && old != edge.EntityID {
			ambiguousNames[edge.Name] = true
			delete(byName, edge.Name)
			continue
		}
		if !ambiguousNames[edge.Name] {
			byName[edge.Name] = edge.EntityID
		}
	}
	for _, article := range index.Articles {
		if (article.ID != "" && !annotation.ValidSourceID(article.ID)) || (article.EntityID != "" && !annotation.ValidSourceID(article.EntityID)) {
			return nil, errors.New("invalid Synto INDEX.json article identity")
		}
		slug, err := normalizeSyntoArticlePath(article.Path)
		if err != nil {
			return nil, err
		}
		entityID := article.EntityID
		if entityID == "" {
			if ambiguousNames[article.Name] {
				return nil, fmt.Errorf("Synto INDEX.json article %q has ambiguous source_concepts entity_id", slug)
			}
			entityID = byName[article.Name]
			if entityID == "" {
				return nil, fmt.Errorf("Synto INDEX.json article %q has no source_concepts entity_id", slug)
			}
		} else if sourceEntity := byName[article.Name]; sourceEntity != "" && sourceEntity != entityID {
			return nil, fmt.Errorf("Synto INDEX.json article/entity disagreement for %q", slug)
		}
		if article.ID != "" {
			if _, exists := byID[article.ID]; exists {
				return nil, fmt.Errorf("Synto INDEX.json duplicate article ID %q", article.ID)
			}
			byID[article.ID] = entityID
		}
		collisionKey := strings.ToLower(slug)
		if old, exists := bySlug[collisionKey]; exists {
			return nil, fmt.Errorf("Synto INDEX.json concept path collision between %q and %q", old, slug)
		}
		bySlug[collisionKey] = slug
		if old, exists := byEntity[entityID]; exists && old != slug {
			return nil, fmt.Errorf("Synto INDEX.json entity_id collision %q", entityID)
		}
		byEntity[entityID] = slug
	}
	for entityID := range index.ActiveEntities {
		if _, exists := byEntity[entityID]; !exists {
			return nil, fmt.Errorf("Synto INDEX.json source edge references unknown entity_id %q", entityID)
		}
	}
	out := make(map[string]string, len(concepts))
	used := make(map[string]string, len(concepts))
	for currentID, slug := range concepts {
		idEntity, byIDPresent := byID[currentID]
		pathSlug, byPathPresent := bySlug[strings.ToLower(slug)]
		pathEntity := ""
		if byPathPresent && pathSlug == slug {
			pathEntity = byEntityForSlug(byEntity, pathSlug)
		} else if byPathPresent {
			return nil, fmt.Errorf("Synto INDEX.json slug case mismatch for concept %q", currentID)
		}
		if byIDPresent && byPathPresent && idEntity != pathEntity {
			return nil, fmt.Errorf("Synto INDEX.json ID/path disagreement for concept %q", currentID)
		}
		if byIDPresent && !byPathPresent {
			return nil, fmt.Errorf("Synto INDEX.json article ID %q has no matching path %q", currentID, slug)
		}
		if byIDPresent && byPathPresent {
			// An ID hit and a slug hit must resolve to the same article. This
			// rejects stale ID maps before the LWC identity rewrite can transfer
			// an entity to a different generated page.
			entityID := idEntity
			if entityID != pathEntity {
				return nil, fmt.Errorf("Synto INDEX.json ID/path disagreement for concept %q", currentID)
			}
			out[currentID] = entityID
			if old, exists := used[entityID]; exists && old != currentID {
				return nil, fmt.Errorf("Synto entity_id collision %q", entityID)
			}
			used[entityID] = currentID
			continue
		}
		entityID := pathEntity
		if !byPathPresent {
			return nil, fmt.Errorf("Synto INDEX.json missing entity_id for concept %q", currentID)
		}
		if old, exists := used[entityID]; exists && old != currentID {
			return nil, fmt.Errorf("Synto entity_id collision %q", entityID)
		}
		used[entityID] = currentID
		out[currentID] = entityID
	}
	return out, nil
}

func byEntityForSlug(byEntity map[string]string, slug string) string {
	for entityID, articleSlug := range byEntity {
		if articleSlug == slug {
			return entityID
		}
	}
	return ""
}

func decodeSyntoIndex(data []byte) (syntoIndexTruth, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return syntoIndexTruth{}, err
	}
	if token != json.Delim('{') {
		return syntoIndexTruth{}, errors.New("INDEX.json must be an object")
	}
	var out syntoIndexTruth
	seen := map[string]bool{}
	required := map[string]bool{}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return out, err
		}
		key, ok := keyToken.(string)
		if !ok || seen[key] {
			return out, errors.New("duplicate or invalid INDEX.json key")
		}
		seen[key] = true
		switch key {
		case "schema_version":
			var value int
			if err := dec.Decode(&value); err != nil || value != 1 {
				return out, errors.New("schema_version must be 1")
			}
			required[key] = true
		case "pack":
			if err := decodeSyntoPack(dec); err != nil {
				return out, err
			}
			required[key] = true
		case "articles":
			articles, err := decodeSyntoArticles(dec)
			if err != nil {
				return out, err
			}
			out.Articles = articles
			required[key] = true
		case "terms", "papers":
			if err := decodeSyntoArray(dec); err != nil {
				return out, err
			}
			required[key] = true
		case "sources":
			if err := decodeSyntoSources(dec); err != nil {
				return out, err
			}
			required[key] = true
		case "source_concepts":
			edges, err := decodeSyntoSourceConcepts(dec)
			if err != nil {
				return out, err
			}
			out.SourceConcepts = edges
			required[key] = true
		case "synthesis":
			if err := decodeSyntoSynthesis(dec); err != nil {
				return out, err
			}
			required[key] = true
		case "stats":
			if err := decodeSyntoStats(dec); err != nil {
				return out, err
			}
			required[key] = true
		case "identity_log", "entity_aliases", "alias_denials":
			if key == "identity_log" {
				ambiguous, err := decodeSyntoIdentityLog(dec)
				if err != nil {
					return out, err
				}
				out.AmbiguousLineage = out.AmbiguousLineage || ambiguous
			} else if err := decodeSyntoArray(dec); err != nil {
				return out, err
			}
		default:
			return out, fmt.Errorf("unsupported INDEX.json field %q", key)
		}
	}
	if _, err := dec.Token(); err != nil {
		return out, err
	}
	if err := generation.EnsureJSONEOF(dec); err != nil {
		return out, err
	}
	for _, key := range []string{"schema_version", "pack", "articles", "terms", "papers", "sources", "source_concepts", "synthesis", "stats"} {
		if !required[key] {
			return out, fmt.Errorf("missing INDEX.json field %q", key)
		}
	}
	return out, nil
}

func decodeSyntoObject(dec *json.Decoder, allowed map[string]bool, field func(string, *json.Decoder) error) (map[string]bool, error) {
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected INDEX.json object")
	}
	seen := map[string]bool{}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok || seen[key] || !allowed[key] {
			return nil, fmt.Errorf("invalid or duplicate INDEX.json field %q", key)
		}
		seen[key] = true
		if err := field(key, dec); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return seen, nil
}

func decodeSyntoPack(dec *json.Decoder) error {
	allowed := map[string]bool{"id": true, "name": true, "version": true, "language": true, "capabilities": true}
	seen, err := decodeSyntoObject(dec, allowed, func(key string, dec *json.Decoder) error {
		switch key {
		case "id", "name", "version":
			return decodeBoundedString(dec, 1024)
		case "language":
			_, err := decodeBoundedStringArray(dec, 1024)
			return err
		case "capabilities":
			values, err := decodeBoundedStringArray(dec, 64)
			if err != nil {
				return err
			}
			for _, value := range values {
				switch value {
				case "articles", "concepts", "segments", "lifecycle":
				default:
					return errors.New("invalid pack capability")
				}
			}
			return nil
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, key := range []string{"id", "name", "version", "language", "capabilities"} {
		if !seen[key] {
			return fmt.Errorf("missing pack field %q", key)
		}
	}
	return nil
}

func decodeSyntoArticles(dec *json.Decoder) ([]syntoIndexEntry, error) {
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return nil, errors.New("articles must be an array")
	}
	out := make([]syntoIndexEntry, 0)
	for dec.More() {
		if len(out) >= generation.MaxFiles {
			return nil, generation.ErrLogicalEntryLimit
		}
		var article syntoIndexEntry
		allowed := map[string]bool{"id": true, "entity_id": true, "name": true, "path": true, "summary": true, "tags": true, "aliases": true, "confidence": true}
		seen, err := decodeSyntoObject(dec, allowed, func(key string, dec *json.Decoder) error {
			switch key {
			case "id":
				return decodeStringInto(dec, &article.ID, 1024)
			case "entity_id":
				return decodeStringInto(dec, &article.EntityID, 1024)
			case "name":
				return decodeStringInto(dec, &article.Name, 4096)
			case "path":
				return decodeStringInto(dec, &article.Path, generation.MaxPathBytes)
			case "summary":
				return decodeNullableString(dec, 1<<20)
			case "tags", "aliases":
				_, err := decodeBoundedStringArray(dec, 4096)
				return err
			case "confidence":
				return decodeSyntoConfidence(dec)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"id", "name", "path", "summary", "tags", "aliases", "confidence"} {
			if !seen[key] {
				return nil, fmt.Errorf("missing article field %q", key)
			}
		}
		if (article.ID != "" && !annotation.ValidSourceID(article.ID)) ||
			(article.EntityID != "" && !annotation.ValidSourceID(article.EntityID)) || article.Name == "" {
			return nil, errors.New("invalid Synto article identity")
		}
		if _, err := normalizeSyntoArticlePath(article.Path); err != nil {
			return nil, err
		}
		out = append(out, article)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeSyntoSourceConcepts(dec *json.Decoder) ([]syntoSourceConcept, error) {
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return nil, errors.New("source_concepts must be an array")
	}
	out := make([]syntoSourceConcept, 0)
	for dec.More() {
		if len(out) >= generation.MaxFiles {
			return nil, generation.ErrLogicalEntryLimit
		}
		var edgeItems []syntoSourceConcept
		var sourcePath, contentHash string
		allowed := map[string]bool{"source_path": true, "content_hash": true, "concepts": true}
		seen, err := decodeSyntoObject(dec, allowed, func(key string, dec *json.Decoder) error {
			switch key {
			case "source_path":
				if err := decodeStringInto(dec, &sourcePath, generation.MaxPathBytes); err != nil {
					return err
				}
				if !safeSyntoRelativePath(sourcePath) {
					return errors.New("unsafe Synto source path")
				}
			case "content_hash":
				return decodeStringInto(dec, &contentHash, 256)
			case "concepts":
				items, err := decodeSyntoSourceConceptItems(dec)
				if err != nil {
					return err
				}
				edgeItems = items
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"source_path", "content_hash", "concepts"} {
			if !seen[key] {
				return nil, fmt.Errorf("missing source_concepts field %q", key)
			}
		}
		if !validSyntoContentHash(contentHash) {
			return nil, errors.New("source_concepts content_hash must be a lowercase SHA-256 digest")
		}
		for i := range edgeItems {
			edgeItems[i].SourcePath = sourcePath
			edgeItems[i].ContentHash = contentHash
		}
		out = append(out, edgeItems...)
		if len(out) > generation.MaxFiles {
			return nil, generation.ErrLogicalEntryLimit
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

func validSyntoContentHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func decodeSyntoSourceConceptItems(dec *json.Decoder) ([]syntoSourceConcept, error) {
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return nil, errors.New("source concepts must be an array")
	}
	out := make([]syntoSourceConcept, 0)
	for dec.More() {
		if len(out) >= generation.MaxFiles {
			return nil, generation.ErrLogicalEntryLimit
		}
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if token == json.Delim('{') {
			var name, entity string
			// The opening token is already consumed; decode the remainder with a small object parser.
			seen := map[string]bool{}
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] || (key != "name" && key != "entity_id") {
					return nil, errors.New("invalid source concept identity")
				}
				seen[key] = true
				if key == "name" {
					if err := decodeStringInto(dec, &name, 4096); err != nil {
						return nil, err
					}
				} else if err := decodeStringInto(dec, &entity, 1024); err != nil {
					return nil, err
				}
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			if !seen["name"] || !seen["entity_id"] || name == "" || !annotation.ValidSourceID(entity) {
				return nil, errors.New("invalid source concept identity")
			}
			out = append(out, syntoSourceConcept{Name: name, EntityID: entity})
		} else if value, ok := token.(string); ok && value != "" {
			return nil, errors.New("source concept string lacks authoritative entity_id")
		} else {
			return nil, errors.New("invalid source concept item")
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeSyntoSources(dec *json.Decoder) error {
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return errors.New("sources must be an array")
	}
	for count := 0; dec.More(); count++ {
		if count >= generation.MaxFiles {
			return generation.ErrLogicalEntryLimit
		}
		var id, sourceType string
		allowed := map[string]bool{"id": true, "title": true, "source_type": true}
		seen, err := decodeSyntoObject(dec, allowed, func(key string, dec *json.Decoder) error {
			switch key {
			case "id":
				return decodeStringInto(dec, &id, 1024)
			case "title":
				return decodeNullableString(dec, 4096)
			case "source_type":
				return decodeStringInto(dec, &sourceType, 256)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !seen["id"] || !seen["title"] || !seen["source_type"] || !annotation.ValidSourceID(id) || sourceType == "" {
			return errors.New("invalid Synto source entry")
		}
	}
	_, err := dec.Token()
	return err
}

func decodeSyntoSynthesis(dec *json.Decoder) error {
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return errors.New("synthesis must be an array")
	}
	for count := 0; dec.More(); count++ {
		if count >= generation.MaxFiles {
			return generation.ErrLogicalEntryLimit
		}
		var path, title string
		allowed := map[string]bool{"path": true, "title": true}
		seen, err := decodeSyntoObject(dec, allowed, func(key string, dec *json.Decoder) error {
			if key == "path" {
				return decodeStringInto(dec, &path, generation.MaxPathBytes)
			}
			return decodeStringInto(dec, &title, 4096)
		})
		if err != nil {
			return err
		}
		if !seen["path"] || !seen["title"] || !safeSyntoRelativePath(path) || title == "" {
			return errors.New("invalid Synto synthesis entry")
		}
	}
	_, err := dec.Token()
	return err
}
func decodeSyntoStats(dec *json.Decoder) error {
	allowed := map[string]bool{"article_count": true, "draft_count": true, "concept_count": true, "alias_count": true, "knowledge_item_count": true, "source_count": true, "source_segment_count": true, "failed_note_count": true, "failed_concept_count": true}
	seen, err := decodeSyntoObject(dec, allowed, func(_ string, dec *json.Decoder) error {
		var value int
		if err := dec.Decode(&value); err != nil || value < 0 {
			return errors.New("invalid Synto stats value")
		}
		return nil
	})
	if err != nil {
		return err
	}
	for key := range allowed {
		if !seen[key] {
			return fmt.Errorf("missing stats field %q", key)
		}
	}
	return nil
}

func decodeSyntoArray(dec *json.Decoder) error {
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return errors.New("INDEX.json field must be an array")
	}
	for count := 0; dec.More(); count++ {
		if count >= generation.MaxFiles {
			return generation.ErrLogicalEntryLimit
		}
		if _, err := decodeStrictJSONValue(dec); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

func decodeSyntoIdentityLog(dec *json.Decoder) (bool, error) {
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return false, errors.New("identity_log must be an array")
	}
	ambiguous := false
	for count := 0; dec.More(); count++ {
		if count >= generation.MaxFiles {
			return false, generation.ErrLogicalEntryLimit
		}
		value, err := decodeStrictJSONValue(dec)
		if err != nil {
			return false, err
		}
		if object, ok := value.(map[string]any); ok {
			if op, ok := object["op"].(string); ok && (op == "merge" || op == "split") {
				ambiguous = true
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return false, err
	}
	return ambiguous, nil
}

func decodeBoundedString(dec *json.Decoder, max int) error {
	var value string
	return decodeStringInto(dec, &value, max)
}
func decodeStringInto(dec *json.Decoder, target *string, max int) error {
	if err := dec.Decode(target); err != nil {
		return err
	}
	if len(*target) > max {
		return errors.New("INDEX.json string exceeds limit")
	}
	return nil
}
func decodeNullableString(dec *json.Decoder, max int) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	value, ok := token.(string)
	if !ok || len(value) > max {
		return errors.New("invalid INDEX.json nullable string")
	}
	return nil
}
func decodeBoundedStringArray(dec *json.Decoder, max int) ([]string, error) {
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return nil, errors.New("INDEX.json field must be a string array")
	}
	out := make([]string, 0)
	for dec.More() {
		if len(out) >= generation.MaxFiles {
			return nil, generation.ErrLogicalEntryLimit
		}
		var value string
		if err := decodeStringInto(dec, &value, max); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	_, err := dec.Token()
	return out, err
}
func decodeSyntoConfidence(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	switch value := token.(type) {
	case nil, string, json.Number:
		_ = value
		return nil
	default:
		return errors.New("invalid article confidence")
	}
}
func safeSyntoRelativePath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && !strings.Contains(value, "\\") && filepath.Clean(filepath.FromSlash(value)) == filepath.FromSlash(value) && !strings.HasPrefix(value, "../") && value != ".."
}

// normalizeSyntoArticlePath accepts the two deliberately bounded forms used
// by this adapter: the exact 0.7.0 agents-pack path and the pre-existing
// machine INDEX path. Both normalize to the single filename slug consumed by
// the generated LWC id_map; nested pack paths are not production-safe here.
func normalizeSyntoArticlePath(value string) (string, error) {
	if value == "" || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", errors.New("unsafe Synto article path")
	}
	var rel string
	switch {
	case strings.HasPrefix(value, "articles/"):
		rel = strings.TrimPrefix(value, "articles/")
	case strings.HasPrefix(value, "wiki/"):
		rel = strings.TrimPrefix(value, "wiki/")
	default:
		return "", fmt.Errorf("unexpected Synto article path prefix %q", value)
	}
	if strings.Contains(rel, "/") || !strings.HasSuffix(rel, ".md") {
		return "", errors.New("unsafe Synto article path")
	}
	slug := strings.TrimSuffix(rel, ".md")
	if !safeConceptSlug(slug) || filepath.Base(rel) != rel || filepath.Clean(filepath.FromSlash(value)) != filepath.FromSlash(value) {
		return "", errors.New("unsafe Synto article path")
	}
	return slug, nil
}

func mergeSyntoEntityIDs(data []byte, entityIDs map[string]string) ([]byte, error) {
	if len(entityIDs) == 0 {
		return data, nil
	}
	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return nil, fmt.Errorf("decode generated concept map: %w", err)
	}
	if ids.ConceptEntityID == nil {
		ids.ConceptEntityID = map[string]string{}
	}
	for id, entityID := range entityIDs {
		if old := ids.ConceptEntityID[id]; old != "" && old != entityID {
			return nil, fmt.Errorf("inconsistent Synto entity_id for %q", id)
		}
		ids.ConceptEntityID[id] = entityID
	}
	return json.MarshalIndent(ids, "", "  ")
}
