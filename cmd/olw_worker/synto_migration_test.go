package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

const expectedSyntoWheelSHA256 = "4bc8dcf14b53f45fac32ce737ecf878f1a46d6d0b010c7decbe6c3b7b10afa77"

func TestSyntoMigrationChildFailureCarriesBoundedDiagnosticMetadata(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy config"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))

	child := exec.Command("sh", "-c", "exit 17")
	childErr := child.Run()
	var exitErr *exec.ExitError
	if !errors.As(childErr, &exitErr) {
		t.Fatalf("child error=%T %v, want *exec.ExitError", childErr, childErr)
	}
	old := execOLW
	t.Cleanup(func() { execOLW = old })
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		return childErr
	}

	err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "unused"}, nil)
	if err == nil {
		t.Fatal("migration unexpectedly succeeded")
	}
	var failure *workerFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error=%v does not carry worker failure metadata", err)
	}
	if failure.Stage != failureStageSyntoMigration || failure.Class != failureClassChildExit || failure.Child != failureChildMigrateOLW || failure.ExitCode == nil || *failure.ExitCode != 17 {
		t.Fatalf("failure metadata=%+v", failure)
	}
	data, err := marshalFailureDiagnostic(err)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic failureDiagnostic
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&diagnostic); err != nil {
		t.Fatal(err)
	}
	if diagnostic.Stage != failureStageSyntoMigration || diagnostic.ErrorClass != failureClassChildExit || diagnostic.Child != failureChildMigrateOLW || diagnostic.ExitCode == nil || *diagnostic.ExitCode != 17 {
		t.Fatalf("diagnostic=%+v", diagnostic)
	}
}

func TestSyntoChildBoundariesClassifyRunExportAndContext(t *testing.T) {
	newExitError := func(t *testing.T, code int) error {
		t.Helper()
		child := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code))
		err := child.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("child error=%T %v, want *exec.ExitError", err, err)
		}
		return err
	}

	t.Run("accepted run", func(t *testing.T) {
		vault := t.TempDir()
		mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
		old := execOLW
		t.Cleanup(func() { execOLW = old })
		execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
			return newExitError(t, 19)
		}
		err := runOLWBatch(context.Background(), vault, [][]string{{"run"}}, true, nil, nil, nil)
		var failure *workerFailure
		if !errors.As(err, &failure) || failure.Stage != failureStageSyntoRun || failure.Class != failureClassChildExit || failure.Child != failureChildRun || failure.ExitCode == nil || *failure.ExitCode != 19 {
			t.Fatalf("error=%v metadata=%+v", err, failure)
		}
	})

	t.Run("pack export", func(t *testing.T) {
		vault := t.TempDir()
		old := execOLW
		t.Cleanup(func() { execOLW = old })
		execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
			return newExitError(t, 29)
		}
		err := ensureSyntoIndex(context.Background(), vault, nil)
		var failure *workerFailure
		if !errors.As(err, &failure) || failure.Stage != failureStageSyntoIndexExport || failure.Class != failureClassChildExit || failure.Child != failureChildPackExport || failure.ExitCode == nil || *failure.ExitCode != 29 {
			t.Fatalf("error=%v metadata=%+v", err, failure)
		}
	})

	for _, tc := range []struct {
		name  string
		ctx   func() (context.Context, context.CancelFunc)
		class failureErrorClass
	}{
		{name: "deadline", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, class: failureClassTimeout},
		{name: "cancelled", ctx: func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, class: failureClassCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
			ctx, cancel := tc.ctx()
			cancel()
			old := execOLW
			t.Cleanup(func() { execOLW = old })
			execOLW = func(ctx context.Context, _ string, _ []string, _ []string, _, _ io.Writer) error {
				return ctx.Err()
			}
			err := runOLWBatch(ctx, vault, [][]string{{"run"}}, true, nil, nil, nil)
			var failure *workerFailure
			if !errors.As(err, &failure) || failure.Stage != failureStageSyntoRun || failure.Class != tc.class || failure.Child != failureChildRun || failure.ExitCode != nil {
				t.Fatalf("error=%v metadata=%+v", err, failure)
			}
		})
	}
}

func TestSyntoChildBoundariesKeepOrdinaryErrorsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T) string
		invoke func(context.Context, string) error
		stage  failureStage
		child  failureChildCommand
	}{
		{
			name: "migrate-olw",
			setup: func(t *testing.T) string {
				vault := t.TempDir()
				mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy config"))
				writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
				return vault
			},
			invoke: func(ctx context.Context, vault string) error {
				return ensureSyntoVault(ctx, vault, workerConfig{APIKey: "unused"}, nil)
			},
			stage: failureStageSyntoMigration,
			child: failureChildMigrateOLW,
		},
		{
			name: "run",
			setup: func(t *testing.T) string {
				vault := t.TempDir()
				mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
				return vault
			},
			invoke: func(ctx context.Context, vault string) error {
				return runOLWBatch(ctx, vault, [][]string{{"run"}}, true, nil, nil, nil)
			},
			stage: failureStageSyntoRun,
			child: failureChildRun,
		},
		{
			name:  "pack-export",
			setup: func(t *testing.T) string { return t.TempDir() },
			invoke: func(ctx context.Context, vault string) error {
				return ensureSyntoIndex(ctx, vault, nil)
			},
			stage: failureStageSyntoIndexExport,
			child: failureChildPackExport,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := execOLW
			t.Cleanup(func() { execOLW = old })
			execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
				return errors.New("ordinary child failure")
			}
			err := tc.invoke(context.Background(), tc.setup(t))
			diagnostic := diagnosticForError(err)
			if diagnostic.Stage != tc.stage || diagnostic.Child != tc.child || diagnostic.ErrorClass != failureClassUnknown || diagnostic.ExitCode != nil {
				t.Fatalf("diagnostic=%+v error=%v", diagnostic, err)
			}
		})
	}
}

func TestWorkerFailureTypedNilExitErrorIsSafe(t *testing.T) {
	var typedNil *exec.ExitError
	var err error = typedNil
	failure := newWorkerFailure(context.Background(), failureStageSyntoRun, failureClassChildExit, failureChildRun, err)
	if failure == nil {
		t.Fatal("typed-nil failure unexpectedly normalized to nil")
	}
	if got := failure.Error(); got == "" {
		t.Fatal("typed-nil failure has empty safe error")
	}
	diagnostic := diagnosticForError(failure)
	if diagnostic.ErrorClass != failureClassUnknown || diagnostic.Child != failureChildRun || diagnostic.ExitCode != nil {
		t.Fatalf("diagnostic=%+v", diagnostic)
	}
}

func TestSyntoMigrationAndIndexChildBoundariesHonorContextClassification(t *testing.T) {
	t.Run("migration timeout wins over canceled child cause", func(t *testing.T) {
		vault := t.TempDir()
		mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy"))
		writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		old := execOLW
		t.Cleanup(func() { execOLW = old })
		execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
			return fmt.Errorf("child: %w", context.Canceled)
		}
		err := ensureSyntoVault(ctx, vault, workerConfig{APIKey: "unused"}, nil)
		var failure *workerFailure
		if !errors.As(err, &failure) || failure.Stage != failureStageSyntoMigration || failure.Class != failureClassTimeout || failure.Child != failureChildMigrateOLW {
			t.Fatalf("error=%v metadata=%+v", err, failure)
		}
	})

	t.Run("index export cancelled wins over deadline child cause", func(t *testing.T) {
		vault := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		old := execOLW
		t.Cleanup(func() { execOLW = old })
		execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
			return fmt.Errorf("child: %w", context.DeadlineExceeded)
		}
		err := ensureSyntoIndex(ctx, vault, nil)
		var failure *workerFailure
		if !errors.As(err, &failure) || failure.Stage != failureStageSyntoIndexExport || failure.Class != failureClassCancelled || failure.Child != failureChildPackExport {
			t.Fatalf("error=%v metadata=%+v", err, failure)
		}
	})
}

func TestSyntoMigrationPostconditionsAreStateInvalid(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte("before"))
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
	old := execOLW
	t.Cleanup(func() { execOLW = old })
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		mustWriteFile(t, filepath.Join(work, "raw", "source.md"), []byte("changed"))
		mustWriteFile(t, filepath.Join(work, "synto.toml"), []byte("[pipeline]\nauto_commit=false\nauto_maintain=false\nrelation_extraction=false\n"))
		writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		return nil
	}
	err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "unused"}, nil)
	diagnostic := diagnosticForError(err)
	if diagnostic.Stage != failureStageSyntoMigration || diagnostic.ErrorClass != failureClassStateInvalid {
		t.Fatalf("diagnostic=%+v error=%v", diagnostic, err)
	}
}

func TestWorkerIncoherentSyntoStateFailsBeforeChildWithStateDiagnostic(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy"))
	old := execOLW
	t.Cleanup(func() { execOLW = old })
	called := false
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		called = true
		return errors.New("child must not run")
	}

	err := runWorkerBatchAtVault(context.Background(), workerConfig{APIKey: "unused", Postprocess: true}, [][]string{{"run"}}, vault)
	if err == nil {
		t.Fatal("incoherent Synto/legacy state unexpectedly succeeded")
	}
	if called {
		t.Fatal("child ran before incoherent state was rejected")
	}
	diagnostic := diagnosticForError(err)
	if diagnostic.Stage != failureStageSyntoConfigValidation || diagnostic.ErrorClass != failureClassStateInvalid || diagnostic.Child != "" {
		t.Fatalf("diagnostic=%+v error=%v", diagnostic, err)
	}
}

func TestRunOLWBatchSafetyValidationCarriesConfigDiagnostic(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = true\n"))
	old := execOLW
	t.Cleanup(func() { execOLW = old })
	called := false
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		called = true
		return errors.New("child must not run")
	}

	err := runOLWBatch(context.Background(), vault, [][]string{{"run"}}, true, nil, nil, nil)
	if err == nil {
		t.Fatal("unsafe Synto config unexpectedly succeeded")
	}
	if called {
		t.Fatal("child ran before safety validation")
	}
	diagnostic := diagnosticForError(err)
	if diagnostic.Stage != failureStageSyntoConfigValidation || diagnostic.ErrorClass != failureClassValidation || diagnostic.Child != "" {
		t.Fatalf("diagnostic=%+v error=%v", diagnostic, err)
	}
}

func TestSyntoIndexExportValidationCarriesIndexStateDiagnostic(t *testing.T) {
	vault := t.TempDir()
	old := execOLW
	t.Cleanup(func() { execOLW = old })
	execOLW = func(_ context.Context, _ string, command []string, _ []string, _, _ io.Writer) error {
		out := command[len(command)-1]
		if err := os.MkdirAll(filepath.Join(out, "index"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(out, "index", "INDEX.json"), []byte(`{"version":999}`))
		return nil
	}

	err := ensureSyntoIndex(context.Background(), vault, nil)
	if err == nil {
		t.Fatal("invalid exported INDEX unexpectedly succeeded")
	}
	diagnostic := diagnosticForError(err)
	if diagnostic.Stage != failureStageSyntoIndexExport || diagnostic.ErrorClass != failureClassStateInvalid || diagnostic.Child != "" {
		t.Fatalf("diagnostic=%+v error=%v", diagnostic, err)
	}
}

func TestSyntoWorkerImagePinsExactWheel(t *testing.T) {
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "synto-0.7.0-py3-none-any.whl") || !strings.Contains(text, expectedSyntoWheelSHA256) {
		t.Fatalf("worker Dockerfile does not pin synto 0.7.0 with the owner-accepted wheel hash")
	}
	if strings.Contains(text, "obsidian_llm_wiki") || strings.Contains(text, "pip install synto") {
		t.Fatalf("worker Dockerfile retains a floating or OLW dependency")
	}
}

func TestSyntoMigrationRunsPrivatelyAndPreservesInputs(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte("raw bytes"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("wiki bytes"))
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy config"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
	before, err := snapshotMigrationInputs(vault)
	if err != nil {
		t.Fatal(err)
	}
	old := execOLW
	defer func() { execOLW = old }()
	var commands [][]string
	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		commands = append(commands, append([]string(nil), command...))
		mustWriteFile(t, filepath.Join(work, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
		writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		return nil
	}
	env, err := prepareOLWEnvironment(workerConfig{APIKey: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupOLWEnvironment(env)
	if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "fake"}, env); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || strings.Join(commands[0], " ") != "migrate-olw --vault "+vault {
		t.Fatalf("migration commands = %#v", commands)
	}
	for path, want := range map[string]string{"raw/source.md": "raw bytes", "wiki/alpha.md": "wiki bytes", "wiki.toml": "legacy config"} {
		data, err := os.ReadFile(filepath.Join(vault, filepath.FromSlash(path)))
		if err != nil || string(data) != want {
			t.Fatalf("migration changed %s=%q err=%v", path, data, err)
		}
	}
	after, err := snapshotMigrationInputs(vault)
	if err != nil {
		t.Fatal(err)
	}
	if !equalMigrationInputs(before, after) {
		t.Fatal("migration changed rollback artifacts")
	}
}

func TestSyntoMigrationRejectsOversizedConfigBeforeNormalization(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy\n"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
	const normalizerLimit = 1 << 20
	migrated := []byte("# oversized migrated config\n" + strings.Repeat("x", normalizerLimit) + "\n")
	expected := append([]byte(nil), migrated...)
	old := execOLW
	t.Cleanup(func() { execOLW = old })
	var commands [][]string
	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		commands = append(commands, append([]string(nil), command...))
		if len(commands) != 1 || strings.Join(command, " ") != "migrate-olw --vault "+vault {
			return fmt.Errorf("unexpected Synto command %v", command)
		}
		mustWriteFile(t, filepath.Join(work, "synto.toml"), migrated)
		writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		return nil
	}

	err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "unused"}, nil)
	if err == nil || !strings.Contains(err.Error(), "normalizer input limit") {
		t.Fatalf("oversized migrated config error = %v, want normalizer input limit", err)
	}
	got, readErr := os.ReadFile(filepath.Join(vault, "synto.toml"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, expected) {
		t.Fatalf("oversized migrated config changed: size before=%d after=%d", len(expected), len(got))
	}
	if len(commands) != 1 || strings.Join(commands[0], " ") != "migrate-olw --vault "+vault {
		t.Fatalf("Synto child calls = %#v, want migration only", commands)
	}
}

func TestSyntoLimitedEncoderStopsExpansionBeforeMaterializingOutput(t *testing.T) {
	writer := newSyntoLimitedWriter(16)
	err := toml.NewEncoder(writer).Encode(map[string]interface{}{"payload": strings.Repeat("x", 64)})
	if err == nil {
		t.Fatal("limited Synto encoder accepted output beyond its configured limit")
	}
	if writer.Len() > 16 {
		t.Fatalf("limited Synto encoder materialized %d bytes beyond limit", writer.Len())
	}
}

func TestExactSyntoBridgeEnvironmentRequiresCombinedOutputs(t *testing.T) {
	allPaths := []string{"run1", "run2", "raw", "config"}
	if err := validateExactSyntoBridgeEnv(allPaths...); err != nil {
		t.Fatalf("complete bridge environment rejected: %v", err)
	}
	if err := validateExactSyntoBridgeEnv("", "", "", ""); err != nil {
		t.Fatalf("unset bridge environment should be skippable: %v", err)
	}
	for i := range allPaths {
		paths := append([]string(nil), allPaths...)
		paths[i] = ""
		if err := validateExactSyntoBridgeEnv(paths...); err == nil {
			t.Fatalf("bridge environment missing %s was accepted", allPaths[i])
		}
	}
}

func TestSyntoMigrationMissingPipelineReachesRunAfterNormalization(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("[models]\nfast = \"offline\"\nheavy = \"offline\"\n"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
	old := execOLW
	defer func() { execOLW = old }()
	var commands [][]string
	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		commands = append(commands, append([]string(nil), command...))
		switch strings.Join(command, " ") {
		case "migrate-olw --vault " + vault:
			// Exact observed Synto 0.7.0 migrate-olw output for a legacy
			// config containing [models]: no [pipeline] table is emitted.
			mustWriteFile(t, filepath.Join(work, "synto.toml"), []byte("[models]\nfast = \"offline\"\nheavy = \"offline\"\n"))
			writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		case "run --auto-approve":
		default:
			return fmt.Errorf("unexpected Synto command %v", command)
		}
		return nil
	}

	err := runWorkerBatchAtVault(context.Background(), workerConfig{APIKey: "offline"}, [][]string{{"run", "--auto-approve"}}, vault)
	if err != nil {
		t.Fatalf("missing-pipeline migrated config was rejected: %v", err)
	}
	if len(commands) != 2 || strings.Join(commands[0], " ") != "migrate-olw --vault "+vault || strings.Join(commands[1], " ") != "run --auto-approve" {
		t.Fatalf("Synto commands = %#v, want migration followed by run", commands)
	}
}

func TestRunWorkerBatchLegacyFullTransactionPreservesIdentityAndSourceLifecycle(t *testing.T) {
	old := execOLW
	t.Cleanup(func() { execOLW = old })

	vault := t.TempDir()
	workspaceDir := t.TempDir()
	const rawBody = "legacy source body"
	contentHash := sha256Text(rawBody)
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("name = \"legacy\"\n"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
	mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte(rawBody))
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: a3f7b2c01d9d\nsources:\n  - stable-source\n---\nlegacy article\n"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "sources", "source.md"), []byte("---\nid: legacy-source\nsource_file: raw/source.md\n---\nlegacy source\n"))
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"a3f7b2c01d9d":"alpha"},"source":{"stable-source":"source"},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"a3f7b2c01d9d","sources":["stable-source"]}}`+"\n"))
	rawBefore := []byte(rawBody)

	postRunIndex := `{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[{"id":"generated-alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","name":"Drifted Alpha","path":"articles/alpha.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":[{"source_path":"raw/source.md","content_hash":"` + contentHash + `","concepts":[{"name":"Unrelated source label","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}]}],"synthesis":[],"stats":{"article_count":1,"draft_count":0,"concept_count":1,"alias_count":0,"knowledge_item_count":0,"source_count":1,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`
	conceptsExport := `{"schema_version":1,"concepts":[{"name":"Drifted Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","aliases":[],"canonical_article_id":"generated-alpha","article_path":"articles/alpha.md","related_names":[]}]}`
	var commands []string
	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		commands = append(commands, strings.Join(command, " "))
		switch command[0] {
		case "migrate-olw":
			if len(command) != 3 || command[1] != "--vault" || command[2] != work {
				return fmt.Errorf("unexpected migration command %v", command)
			}
			mustWriteFile(t, filepath.Join(work, "synto.toml"), []byte("[models]\nfast = \"offline\"\n"))
			writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		case "run":
			if len(command) != 2 || command[1] != "--auto-approve" {
				return fmt.Errorf("unexpected run command %v", command)
			}
			mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: d4c8f9b0a177\nsources:\n  - transient-source\n---\nDrifted Alpha\n"))
			mustWriteFile(t, filepath.Join(work, "wiki", "sources", "source.md"), []byte("---\nid: transient-source\nsource_file: raw/source.md\n---\ngenerated source\n"))
			mustWriteFile(t, filepath.Join(work, "cache", "id_map.json"), []byte(`{"concept":{"d4c8f9b0a177":"alpha"},"source":{"transient-source":"source"},"redirects":{}}`))
			mustWriteFile(t, filepath.Join(work, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"d4c8f9b0a177","sources":["transient-source"]}}`+"\n"))
			writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		case "pack":
			if len(command) != 6 || command[1] != "export" || command[2] != "--target" || command[3] != "agents" || command[4] != "--out" {
				return fmt.Errorf("unexpected export command %v", command)
			}
			mustWriteFile(t, filepath.Join(command[5], "index", "INDEX.json"), []byte(postRunIndex))
			mustWriteFile(t, filepath.Join(command[5], "agent", "concepts.json"), []byte(conceptsExport))
		default:
			return fmt.Errorf("unexpected Synto command %v", command)
		}
		return nil
	}

	cfg := workerConfig{VaultPath: vault, APIKey: "offline", Workspace: true, WorkspaceDir: workspaceDir, Postprocess: true, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatalf("full legacy transaction failed: %v", err)
	}
	if len(commands) != 3 || !strings.HasPrefix(commands[0], "migrate-olw --vault ") || commands[1] != "run --auto-approve" || !strings.HasPrefix(commands[2], "pack export --target agents --out ") {
		t.Fatalf("Synto command sequence = %#v, want migrate -> run -> one pack export", commands)
	}
	ids := mustSnapshotIDMap(t, vault)
	if ids.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXABC"] != "alpha" || len(ids.Concept) != 1 || len(ids.ConceptEntityID) != 0 {
		t.Fatalf("published direct concept identity = %#v", ids)
	}
	if _, exists := ids.Concept["generated-alpha"]; exists {
		t.Fatalf("transient generated concept ID remained: %#v", ids.Concept)
	}
	if ids.Source["stable-source"] != "source" || ids.Source["transient-source"] != "" {
		t.Fatalf("published source identity = %#v, want stable-source -> source", ids.Source)
	}
	sourcePage, err := os.ReadFile(filepath.Join(vault, "wiki", "sources", "source.md"))
	if err != nil || !strings.Contains(string(sourcePage), "id: stable-source\n") {
		t.Fatalf("published source page = %q, err=%v", sourcePage, err)
	}
	rawAfter, err := os.ReadFile(filepath.Join(vault, "raw", "source.md"))
	if err != nil || !bytes.Equal(rawAfter, rawBefore) {
		t.Fatalf("original raw source changed: %q, err=%v", rawAfter, err)
	}
}

func TestNormalizeMigratedSyntoConfigPreservesSemantics(t *testing.T) {
	input := []byte(`title = "migrated"
numbers = [1, 2, 3]
ratios = [1.5, 2.5]

[providers.default]
name = "offline"
url = "https://example.invalid/v1"
options = { retries = 2, labels = ["a", "b"] }

[models.fast]
provider = "default"
model = "offline-fast"
ctx = 16384

[unknown.nested]
enabled = true
limits = [1, 2, 3]

[[unknown.array]]
name = "first"

[[unknown.array]]
name = "second"

[pipeline]
auto_commit = true
article_max_tokens = 32768
ingest_parallel = false
`)
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), input)
	if err := normalizeMigratedSyntoConfig(vault); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(filepath.Join(vault, "synto.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSyntoPipelineSafety(filepath.Join(vault, "synto.toml")); err != nil {
		t.Fatalf("normalized config is unsafe: %v", err)
	}
	var before, after map[string]interface{}
	if _, err := toml.Decode(string(input), &before); err != nil {
		t.Fatal(err)
	}
	if _, err := toml.Decode(string(output), &after); err != nil {
		t.Fatal(err)
	}
	if !equalSyntoConfigSemanticsWithoutSafety(before, after) {
		t.Fatalf("normalized config changed non-safety semantics:\n%s", output)
	}
	pipeline, ok := after["pipeline"].(map[string]interface{})
	if !ok {
		t.Fatalf("normalized pipeline = %#v", after["pipeline"])
	}
	for _, key := range syntoPipelineSafetyKeys {
		if value, ok := pipeline[key].(bool); !ok || value {
			t.Fatalf("pipeline.%s = %#v, want explicit false", key, pipeline[key])
		}
	}
}

func TestBurntSushiTemporalSupportAndMetadata(t *testing.T) {
	tests := []struct {
		name       string
		literal    string
		supported  bool
		wantHour   int
		wantOffset string
	}{
		{name: "offset datetime", literal: "1979-05-27T07:32:00-07:00", supported: true, wantHour: 7, wantOffset: "-07:00"},
		{name: "local datetime", literal: "1979-05-27T07:32:00", supported: true, wantHour: 7, wantOffset: ""},
		{name: "local date", literal: "1979-05-27", supported: true, wantHour: 0, wantOffset: ""},
		{name: "local time", literal: "07:32:00", supported: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var document map[string]interface{}
			metadata, err := toml.Decode("value = "+test.literal+"\n", &document)
			if !test.supported {
				if err == nil {
					t.Fatalf("pinned TOML decoder unexpectedly accepted local time: %#v", document)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := metadata.Type("value"); got != "Datetime" {
				t.Fatalf("MetaData.Type(value)=%q, want Datetime", got)
			}
			value, ok := document["value"].(time.Time)
			if !ok {
				t.Fatalf("decoded temporal value=%T %#v, want time.Time", document["value"], document["value"])
			}
			if value.Hour() != test.wantHour {
				t.Fatalf("decoded hour=%d, want %d", value.Hour(), test.wantHour)
			}
			if test.wantOffset != "" && value.Format("-07:00") != test.wantOffset {
				t.Fatalf("decoded offset=%q, want %q", value.Format("-07:00"), test.wantOffset)
			}
		})
	}
}

func TestNormalizeMigratedSyntoConfigFailsClosedForSupportedTemporalForms(t *testing.T) {
	for _, test := range []struct {
		name    string
		literal string
	}{
		{name: "offset datetime", literal: "1979-05-27T07:32:00-07:00"},
		{name: "local datetime", literal: "1979-05-27T07:32:00"},
		{name: "local date", literal: "1979-05-27"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := []byte("created = " + test.literal + "\n\n[pipeline]\nauto_commit = true\n")
			vault := t.TempDir()
			path := filepath.Join(vault, "synto.toml")
			mustWriteFile(t, path, input)
			if err := normalizeMigratedSyntoConfig(vault); err == nil || !strings.Contains(err.Error(), "temporal") {
				t.Fatalf("temporal config error=%v, want fail-closed temporal error", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, input) {
				t.Fatalf("failed temporal normalization changed original bytes: %q", got)
			}
			entries, err := os.ReadDir(vault)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".atomic-") {
					t.Fatalf("failed temporal normalization left temporary file %q", entry.Name())
				}
			}
		})
	}
}

func TestEqualSyntoTOMLValuesDoesNotCollapseTemporalRepresentations(t *testing.T) {
	offset := time.Date(1979, 5, 27, 7, 32, 0, 0, time.FixedZone("-0700", -7*60*60))
	utc := offset.UTC()
	if offset.Equal(utc) != true {
		t.Fatal("test temporal values do not represent the same instant")
	}
	if equalSyntoTOMLValues(offset, utc) {
		t.Fatal("equalSyntoTOMLValues collapsed distinct temporal representations")
	}
}

func TestNormalizeMigratedSyntoConfigCreatesMissingPipeline(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[models]\nfast = \"offline\"\n"))
	if err := normalizeMigratedSyntoConfig(vault); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(vault, "synto.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[pipeline]") {
		t.Fatalf("normalized config has no pipeline table: %s", data)
	}
	if err := validateSyntoPipelineSafetyBytes(data); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeMigratedSyntoConfigFailsClosedWithoutPartialWrite(t *testing.T) {
	tests := map[string]string{
		"malformed":                "[pipeline]\nauto_commit = false\nnot valid",
		"duplicate key":            "[pipeline]\nauto_commit = false\nauto_commit = true",
		"duplicate table":          "[pipeline]\nauto_commit = false\n\n[pipeline]\nauto_maintain = false",
		"non-boolean safety value": "[pipeline]\nauto_commit = \"false\"",
		"non-table pipeline":       "pipeline = true",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			vault := t.TempDir()
			path := filepath.Join(vault, "synto.toml")
			before := []byte(input)
			mustWriteFile(t, path, before)
			if err := normalizeMigratedSyntoConfig(vault); err == nil {
				t.Fatal("invalid migrated config was accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("failed normalization changed config: %q", after)
			}
			entries, err := os.ReadDir(vault)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".atomic-") {
					t.Fatalf("failed normalization left temporary file %q", entry.Name())
				}
			}
		})
	}
}

func TestSnapshotMigrationInputsIsBoundedAndDetectsChanges(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "a.md"), []byte("a"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "a.md"), []byte("wiki"))
	first, err := snapshotMigrationInputs(vault)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(vault, "raw", "b.md"), []byte("new"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "a.md"), []byte("changed"))
	second, err := snapshotMigrationInputs(vault)
	if err != nil {
		t.Fatal(err)
	}
	if equalMigrationInputs(first, second) {
		t.Fatal("snapshot comparison accepted additions and content changes")
	}
	if err := os.Remove(filepath.Join(vault, "raw", "b.md")); err != nil {
		t.Fatal(err)
	}
	if equalMigrationInputs(first, second) {
		t.Fatal("snapshot comparison ignored removals")
	}

	large := filepath.Join(vault, "raw", "too-large.md")
	file, err := os.OpenFile(large, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(generation.MaxFileBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotMigrationInputs(vault); err == nil {
		t.Fatal("oversized migration input was accepted")
	}
	if err := os.Remove(large); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(vault, "outside"), filepath.Join(vault, "raw", "link.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotMigrationInputs(vault); err == nil {
		t.Fatal("symlink migration input was accepted")
	}
}

func TestSyntoConfigDisablesPrivateGitAndCurationSideEffects(t *testing.T) {
	vault := t.TempDir()
	if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "fake"}, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(vault, "synto.toml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"auto_commit = false", "auto_maintain = false", "relation_extraction = false"} {
		if !strings.Contains(text, want) {
			t.Fatalf("synto config missing %q: %s", want, text)
		}
	}
}

func TestSyntoConfigMaterializationIsIndependentAndBytePreserving(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-synto-only", true: "migrated-dual-config"}[migrated], func(t *testing.T) {
			vault := t.TempDir()
			syntoConfig := []byte("# preserve comments\n[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n")
			mustWriteFile(t, filepath.Join(vault, "synto.toml"), syntoConfig)
			if migrated {
				mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy\n"))
				writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
				writeValidSQLiteState(t, filepath.Join(vault, ".synto", "state.db"))
			}
			if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "unused"}, nil); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(vault, "synto.toml"))
			if err != nil || string(got) != string(syntoConfig) {
				t.Fatalf("Synto config changed: %q err=%v", got, err)
			}
			workspace, err := createWorkspace(t.TempDir(), vault)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(workspace) })
			if got, err := os.ReadFile(filepath.Join(workspace, "synto.toml")); err != nil || string(got) != string(syntoConfig) {
				t.Fatalf("workspace Synto config changed: %q err=%v", got, err)
			}
			if !migrated {
				if _, err := os.Stat(filepath.Join(workspace, "wiki.toml")); !os.IsNotExist(err) {
					t.Fatalf("fresh Synto workspace materialized legacy config: %v", err)
				}
			}
		})
	}
}

func TestExistingSafeSyntoConfigIsByteIdenticalAndSkipsMigration(t *testing.T) {
	vault := t.TempDir()
	config := []byte("# user-owned\n[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n")
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), config)
	old := execOLW
	defer func() { execOLW = old }()
	childCalls := 0
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		childCalls++
		return errors.New("migration must not run for an existing Synto config")
	}
	if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "unused"}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(vault, "synto.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, config) {
		t.Fatalf("existing safe config changed: %q", got)
	}
	if childCalls != 0 {
		t.Fatalf("existing safe config made %d migration/child calls", childCalls)
	}
}

func TestSyntoPipelineSafetyRejectsUnsafeEffectiveValues(t *testing.T) {
	tests := map[string]string{
		"omitted auto_commit": `[pipeline]
auto_maintain = false
relation_extraction = false
`,
		"explicit auto_commit": `[pipeline]
auto_commit = true
auto_maintain = false
relation_extraction = false
`,
		"explicit auto_maintain": `[pipeline]
auto_commit = false
auto_maintain = true
relation_extraction = false
`,
		"explicit relation_extraction": `[pipeline]
auto_commit = false
auto_maintain = false
relation_extraction = true
`,
		"duplicate key": `[pipeline]
auto_commit = false
auto_commit = true
auto_maintain = false
relation_extraction = false
`,
		"duplicate table": `[pipeline]
auto_commit = false
auto_maintain = false
relation_extraction = false

[pipeline]
auto_commit = false
`,
	}
	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			vault := t.TempDir()
			path := filepath.Join(vault, "synto.toml")
			mustWriteFile(t, path, []byte(config))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "unused"}, nil); err == nil {
				t.Fatal("unsafe Synto pipeline configuration was accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("unsafe config was mutated: %q", after)
			}
		})
	}
}

func TestSyntoPipelineSafetyAcceptsExplicitSafeFalse(t *testing.T) {
	vault := t.TempDir()
	config := []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n")
	path := filepath.Join(vault, "synto.toml")
	mustWriteFile(t, path, config)
	if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "unused"}, nil); err != nil {
		t.Fatalf("explicit safe false config rejected: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(config) {
		t.Fatalf("safe config changed: %q err=%v", got, err)
	}
}

func TestSyntoPipelineSafetyIgnoresCommentsAndStringLookalikes(t *testing.T) {
	vault := t.TempDir()
	config := `# auto_commit = true
[pipeline]
auto_commit = false # auto_maintain = true
auto_maintain = false
relation_extraction = false
label = "relation_extraction = true"
`
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte(config))
	if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "unused"}, nil); err != nil {
		t.Fatalf("safe commented/string config rejected: %v", err)
	}
}

func TestSyntoPipelineSafetyNormalizesMigratedConfig(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy\n"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
	old := execOLW
	defer func() { execOLW = old }()
	migrationCalls := 0
	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		migrationCalls++
		if strings.Join(command, " ") != "migrate-olw --vault "+vault {
			return fmt.Errorf("unexpected migration command %v", command)
		}
		mustWriteFile(t, filepath.Join(work, "synto.toml"), []byte("[pipeline]\nauto_commit = true\narticle_max_tokens = 32768\n"))
		writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		return nil
	}
	if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "unused"}, nil); err != nil {
		t.Fatalf("migrated Synto configuration was not normalized: %v", err)
	}
	if migrationCalls != 1 {
		t.Fatalf("migration calls = %d, want 1", migrationCalls)
	}
	data, err := os.ReadFile(filepath.Join(vault, "synto.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSyntoPipelineSafetyBytes(data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "article_max_tokens = 32768") {
		t.Fatalf("normalization discarded unrelated pipeline field: %s", data)
	}
}

func TestSyntoPipelineSafetyBlocksChildProcess(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = true\n"))
	old := execOLW
	defer func() { execOLW = old }()
	calls := 0
	execOLW = func(_ context.Context, _ string, _ []string, _ []string, _, _ io.Writer) error {
		calls++
		return nil
	}
	if err := runOLWBatch(context.Background(), vault, [][]string{{"run", "--auto-approve"}}, true, nil, nil, nil); err == nil {
		t.Fatal("unsafe Synto pipeline configuration reached execution")
	}
	if calls != 0 {
		t.Fatalf("unsafe configuration made %d child calls", calls)
	}
}

func TestSyntoCommandContractRejectsForceAndUnsafeSecondCommandsBeforeChild(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	childCalls := 0
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		childCalls++
		return nil
	}
	for _, commands := range [][][]string{
		{{"run", "--force"}},
		{{"run", "--auto-approve", "--force"}},
		{{"run", "--force", "--auto-approve"}},
		{{"run", "--force=true"}},
		{{"run", "--auto-approve=1"}},
		{{"run", "--auto-approve"}, {"compile"}},
		{{"run", "--auto-approve"}, {"ingest", "--all"}},
		{{"identity", "merge"}},
		{{"undo", "--force"}},
		{{"pack", "export", "--target", "agents"}},
		{{"query", "question"}},
		{{"serve"}},
	} {
		if err := validateWorkerInput(workerConfig{Postprocess: true}, commands); err == nil {
			t.Fatalf("unsafe command batch accepted: %#v", commands)
		}
		if err := runOLWBatch(context.Background(), t.TempDir(), commands, true, nil, nil, nil); err == nil {
			t.Fatalf("unsafe command batch reached runOLWBatch: %#v", commands)
		}
	}
	if childCalls != 0 {
		t.Fatalf("unsafe command validation made %d child calls", childCalls)
	}
}

func TestWorkerProductionSequenceInstallsPackExportIndexBeforePostprocess(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := t.TempDir()
	workspaceDir := t.TempDir()
	var calls []string
	var generatedIndex []byte
	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		calls = append(calls, strings.Join(command, " "))
		if strings.HasPrefix(strings.Join(command, " "), "run") {
			writeFreshSyntoRequiredOutputs(t, work)
			var err error
			generatedIndex, err = os.ReadFile(filepath.Join(work, ".synto", "INDEX.json"))
			if err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(work, ".synto", "INDEX.json")); err != nil {
				return err
			}
			return nil
		}
		if len(command) != 6 || command[0] != "pack" || command[1] != "export" || command[2] != "--target" || command[3] != "agents" || command[4] != "--out" {
			return fmt.Errorf("unexpected offline command %v", command)
		}
		mustWriteFile(t, filepath.Join(command[5], "index", "INDEX.json"), generatedIndex)
		mustWriteFile(t, filepath.Join(command[5], "agent", "concepts.json"), []byte(`{"schema_version":1,"concepts":[{"name":"Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","aliases":[],"canonical_article_id":null,"article_path":null,"related_names":[]}]}`))
		return nil
	}
	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "offline", Workspace: true, WorkspaceDir: workspaceDir, Postprocess: true}, `[["run","--auto-approve"]]`); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "run --auto-approve" || !strings.HasPrefix(calls[1], "pack export --target agents --out ") {
		t.Fatalf("production Synto sequence=%q", calls)
	}
	if _, err := os.Stat(filepath.Join(vault, ".synto", "INDEX.json")); err != nil {
		t.Fatalf("authoritative INDEX was not published: %v", err)
	}
}

func TestSyntoOfflineExportJoinsAgentConceptIdentity(t *testing.T) {
	old := execOLW
	t.Cleanup(func() { execOLW = old })
	vault := t.TempDir()
	index := strings.Replace(syntoIndexFixture("article", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", false), `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC",`, "", 1)
	concepts := `{"schema_version":1,"concepts":[{"name":"Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","aliases":[],"canonical_article_id":"article","article_path":"articles/alpha.md","related_names":[]}]}`
	execOLW = func(_ context.Context, _ string, command []string, _ []string, _, _ io.Writer) error {
		if len(command) != 6 || command[0] != "pack" || command[1] != "export" {
			return fmt.Errorf("unexpected command %v", command)
		}
		mustWriteFile(t, filepath.Join(command[5], "index", "INDEX.json"), []byte(index))
		mustWriteFile(t, filepath.Join(command[5], "agent", "concepts.json"), []byte(concepts))
		return nil
	}
	joined, err := exportSyntoIndex(context.Background(), vault, nil)
	if err != nil {
		t.Fatal(err)
	}
	truth, err := decodeSyntoIndex(joined)
	if err != nil {
		t.Fatal(err)
	}
	if len(truth.Articles) != 1 || truth.Articles[0].EntityID != "" {
		t.Fatalf("joined INDEX articles = %#v, want omitted entity to remain ordinary", truth.Articles)
	}
}

func exportSyntoIndexFixture(t *testing.T, index, concepts string) []byte {
	t.Helper()
	old := execOLW
	t.Cleanup(func() { execOLW = old })
	vault := t.TempDir()
	execOLW = func(_ context.Context, _ string, command []string, _ []string, _, _ io.Writer) error {
		if len(command) != 6 || command[0] != "pack" || command[1] != "export" || command[2] != "--target" || command[3] != "agents" || command[4] != "--out" {
			return fmt.Errorf("unexpected command %v", command)
		}
		mustWriteFile(t, filepath.Join(command[5], "index", "INDEX.json"), []byte(index))
		mustWriteFile(t, filepath.Join(command[5], "agent", "concepts.json"), []byte(concepts))
		return nil
	}
	joined, err := exportSyntoIndex(context.Background(), vault, nil)
	if err != nil {
		t.Fatal(err)
	}
	return joined
}

func TestSyntoOfflineExportPreservesNullArticleEntity(t *testing.T) {
	const entity = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	index := strings.Replace(syntoIndexFixture("article", entity, "alpha", false), `"entity_id":"`+entity+`",`, `"entity_id":null,`, 1)
	concepts := `{"schema_version":1,"concepts":[{"name":"Alpha","entity_id":"` + entity + `","aliases":[],"canonical_article_id":"article","article_path":"articles/alpha.md","related_names":[]}]}`
	joined := exportSyntoIndexFixture(t, index, concepts)
	truth, err := decodeSyntoIndex(joined)
	if err != nil {
		t.Fatal(err)
	}
	if truth.Articles[0].EntityID != "" {
		t.Fatalf("null article entity was populated: %#v", truth.Articles[0])
	}
	plan, err := syntoIdentityPlanFromIndex(truth)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ByPath) != 0 {
		t.Fatalf("null article entered worker Concept plan: %#v", plan.ByPath)
	}
}

func TestSyntoOfflineExportPreservesOmittedArticleEntity(t *testing.T) {
	const entity = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	index := strings.Replace(syntoIndexFixture("article", entity, "alpha", false), `"entity_id":"`+entity+`",`, "", 1)
	concepts := `{"schema_version":1,"concepts":[{"name":"Alpha","entity_id":"` + entity + `","aliases":[],"canonical_article_id":"article","article_path":"articles/alpha.md","related_names":[]}]}`
	joined := exportSyntoIndexFixture(t, index, concepts)
	truth, err := decodeSyntoIndex(joined)
	if err != nil {
		t.Fatal(err)
	}
	if truth.Articles[0].EntityID != "" {
		t.Fatalf("omitted article entity was populated: %#v", truth.Articles[0])
	}
	plan, err := syntoIdentityPlanFromIndex(truth)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ByPath) != 0 {
		t.Fatalf("omitted article entered worker Concept plan: %#v", plan.ByPath)
	}
}

func TestSyntoIdentityPlanValidatesDuplicateMetadataForEntitylessArticles(t *testing.T) {
	for _, test := range []struct {
		name     string
		articles []syntoIndexEntry
	}{
		{name: "duplicate article ID", articles: []syntoIndexEntry{{ID: "same", Path: "wiki/alpha.md"}, {ID: "same", Path: "wiki/beta.md"}}},
		{name: "duplicate article slug", articles: []syntoIndexEntry{{ID: "alpha-a", Path: "wiki/alpha.md"}, {ID: "alpha-b", Path: "wiki/ALPHA.md"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := syntoIdentityPlanFromIndex(syntoIndexTruthForEntityMapping(test.articles, nil, nil)); err == nil {
				t.Fatal("entityless duplicate article metadata was accepted")
			}
		})
	}
}

func TestSyntoAgentConceptDoesNotVetoExplicitArticleEntity(t *testing.T) {
	const articleEntity = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	const agentEntity = "01JAZ5N7Y3K8M2Q4R6T9VWXABD"
	index := syntoIndexFixture("article", articleEntity, "alpha", false)
	concepts := `{"schema_version":1,"concepts":[{"name":"Alpha","entity_id":"` + agentEntity + `","aliases":[],"canonical_article_id":"article","article_path":"articles/alpha.md","related_names":[]}]}`
	joined, err := enrichSyntoIndexWithAgentConcepts([]byte(index), []byte(concepts))
	if err != nil {
		t.Fatalf("agent disagreement vetoed explicit article entity: %v", err)
	}
	truth, err := decodeSyntoIndex(joined)
	if err != nil {
		t.Fatal(err)
	}
	if truth.Articles[0].EntityID != articleEntity {
		t.Fatalf("explicit article entity changed to %q", truth.Articles[0].EntityID)
	}
}

func TestSyntoAgentConceptJoinFailsClosed(t *testing.T) {
	base := strings.Replace(syntoIndexFixture("article-a", "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", "alpha", false), `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0",`, "", 1)
	tests := []struct {
		name     string
		index    string
		concepts string
		want     string
	}{
		{name: "ID-only match", index: base, concepts: agentConceptsFixture(`"canonical_article_id":"article-a","article_path":null`, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"`), want: "incomplete canonical article binding"},
		{name: "path-only match", index: base, concepts: agentConceptsFixture(`"canonical_article_id":null,"article_path":"articles/alpha.md"`, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"`), want: "incomplete canonical article binding"},
		{name: "ID/path point to different articles", index: strings.Replace(strings.Replace(syntoIndexFixtureWithEntitiesHash([]string{"article-a:01JAZ5N7Y3K8M2Q4R6T9VWXAC0:alpha", "article-b:01JAZ5N7Y3K8M2Q4R6T9VWXAC1:beta"}, nil, strings.Repeat("0", 64)), `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0",`, "", 1), `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC1",`, "", 1), concepts: agentConceptsFixture(`"canonical_article_id":"article-a","article_path":"articles/beta.md"`, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"`), want: "ID/path disagreement"},
		{name: "same article multiple entities", index: base, concepts: `{"schema_version":1,"concepts":[{"name":"Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0","aliases":[],"canonical_article_id":"article-a","article_path":"articles/alpha.md","related_names":[]},{"name":"Alpha alias","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC1","aliases":[],"canonical_article_id":"article-a","article_path":"articles/alpha.md","related_names":[]}]}`, want: "multiple entity_id"},
		{name: "same article duplicate proof", index: base, concepts: `{"schema_version":1,"concepts":[{"name":"Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0","aliases":[],"canonical_article_id":"article-a","article_path":"articles/alpha.md","related_names":[]},{"name":"Alpha duplicate","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0","aliases":[],"canonical_article_id":"article-a","article_path":"articles/alpha.md","related_names":[]}]}`, want: "duplicate canonical article proof"},
		{name: "unsafe entity", index: base, concepts: agentConceptsFixture(`"canonical_article_id":"article-a","article_path":"articles/alpha.md"`, `"entity_id":"../escape"`), want: "entity_id is unsafe"},
		{name: "unsafe ID", index: base, concepts: agentConceptsFixture(`"canonical_article_id":"../escape","article_path":"articles/alpha.md"`, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"`), want: "canonical_article_id is unsafe"},
		{name: "unsafe path", index: base, concepts: agentConceptsFixture(`"canonical_article_id":"article-a","article_path":"../alpha.md"`, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"`), want: "article_path is unsafe"},
		{name: "linked entry without entity", index: base, concepts: agentConceptsFixture(`"canonical_article_id":"article-a","article_path":"articles/alpha.md"`, `"entity_id":""`), want: "invalid entity_id"},
		{name: "malformed trailing JSON", index: base, concepts: agentConceptsFixture(`"canonical_article_id":"article-a","article_path":"articles/alpha.md"`, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"`) + ` {}`, want: "unexpected trailing JSON"},
		{name: "schema mismatch", index: base, concepts: strings.Replace(agentConceptsFixture(`"canonical_article_id":"article-a","article_path":"articles/alpha.md"`, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"`), `"schema_version":1`, `"schema_version":2`, 1), want: "schema_version must be 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := enrichSyntoIndexWithAgentConcepts([]byte(tc.index), []byte(tc.concepts)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("join error=%v, want substring %q", err, tc.want)
			}
		})
	}

	joined, err := enrichSyntoIndexWithAgentConcepts([]byte(base), []byte(`{"schema_version":1,"concepts":[{"name":"Other","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABF","aliases":[],"canonical_article_id":null,"article_path":null,"related_names":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	truth, err := decodeSyntoIndex(joined)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mapSyntoEntityIDsFromIndexTruth(truth, map[string]string{"article-a": "alpha"})
	if err != nil || len(got) != 0 {
		t.Fatalf("ordinary article identity result=%#v error=%v, want no Concept mapping", got, err)
	}
}

func TestSyntoOmittedArticleDoesNotUseExactSourceNameAsIdentity(t *testing.T) {
	base := strings.Replace(syntoIndexFixture("article-a", "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", "alpha", true), `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0",`, "", 1)
	concepts := agentConceptsFixture(`"canonical_article_id":null,"article_path":null`, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"`)
	joined, err := enrichSyntoIndexWithAgentConcepts([]byte(base), []byte(concepts))
	if err != nil {
		t.Fatalf("enrichment with unbound exact-name concept failed: %v", err)
	}
	truth, err := decodeSyntoIndex(joined)
	if err != nil {
		t.Fatal(err)
	}
	if truth.Articles[0].EntityID != "" {
		t.Fatalf("enrichment assigned omitted article entity=%q", truth.Articles[0].EntityID)
	}
	got, err := mapSyntoEntityIDsFromIndexTruth(truth, map[string]string{"article-a": "alpha"})
	if err != nil || len(got) != 0 {
		t.Fatalf("exact source-name identity result=%#v error=%v, want no Concept mapping", got, err)
	}
}

func TestSyntoDirectAgentProofAcceptsTitleDriftWithConsistentSourceEdge(t *testing.T) {
	base := syntoIndexFixture("article-a", "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", "alpha", true)
	base = strings.Replace(base, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0",`, "", 1)
	base = strings.ReplaceAll(base, `"path":"wiki/alpha.md"`, `"path":"articles/alpha.md"`)
	base = strings.ReplaceAll(base, `"name":"alpha"`, `"name":"INDEX Title Drift"`)
	concepts := agentConceptsFixture(`"canonical_article_id":"article-a","article_path":"articles/alpha.md"`, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"`)
	joined, err := enrichSyntoIndexWithAgentConcepts([]byte(base), []byte(concepts))
	if err != nil {
		t.Fatal(err)
	}
	truth, err := decodeSyntoIndex(joined)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mapSyntoEntityIDsFromIndexTruth(truth, map[string]string{"article-a": "alpha"})
	if err != nil || len(got) != 0 {
		t.Fatalf("agent proof populated ordinary article identity: map=%#v error=%v", got, err)
	}
}

func agentConceptsFixture(binding, entity string) string {
	return `{"schema_version":1,"concepts":[{"name":"Alpha",` + entity + `,"aliases":[],` + binding + `,"related_names":[]}]}`
}

func TestSyntoMigrationStateMatrixFailsClosedBeforeChild(t *testing.T) {
	tests := map[string]func(string){
		"config plus partial synto directory": func(vault string) {
			mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
			if err := os.Mkdir(filepath.Join(vault, ".synto"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"config plus legacy state without synto state": func(vault string) {
			mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
			writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
		},
		"synto directory without config": func(vault string) {
			if err := os.Mkdir(filepath.Join(vault, ".synto"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"legacy config without state": func(vault string) {
			mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy"))
		},
		"legacy state without config": func(vault string) {
			writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
		},
		"symlinked synto config": func(vault string) {
			outside := filepath.Join(t.TempDir(), "synto.toml")
			mustWriteFile(t, outside, []byte("[pipeline]\nauto_commit = false\n"))
			if err := os.Symlink(outside, filepath.Join(vault, "synto.toml")); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked legacy state directory": func(vault string) {
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(vault, ".olw")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			vault := t.TempDir()
			setup(vault)
			calls := 0
			old := execOLW
			execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
				calls++
				return nil
			}
			t.Cleanup(func() { execOLW = old })
			if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "fake"}, nil); err == nil {
				t.Fatal("incoherent vault state accepted")
			}
			if calls != 0 {
				t.Fatalf("invalid state made %d child calls", calls)
			}
		})
	}

	t.Run("fresh config before first run is allowed", func(t *testing.T) {
		vault := t.TempDir()
		if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "fake"}, nil); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMigrationSnapshotProtectsRollbackArtifactsAndRootRace(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte("raw"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "article.md"), []byte("wiki"))
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy config bytes"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
	first, err := snapshotMigrationInputs(vault)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("changed config bytes"))
	second, err := snapshotMigrationInputs(vault)
	if err != nil {
		t.Fatal(err)
	}
	if equalMigrationInputs(first, second) {
		t.Fatal("wiki.toml mutation was not detected")
	}

	external := t.TempDir()
	mustWriteFile(t, filepath.Join(external, "outside.db"), []byte("external bytes"))
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy config bytes"))
	if err := os.Remove(filepath.Join(vault, ".olw", "state.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "outside.db"), filepath.Join(vault, ".olw", "state.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotMigrationInputs(vault); err == nil {
		t.Fatal("symlink rollback state accepted")
	}

	_ = os.Remove(filepath.Join(vault, ".olw", "state.db"))
	writeValidSQLiteState(t, filepath.Join(vault, ".olw", "state.db"))
	oldHook := migrationSnapshotBeforeOpen
	t.Cleanup(func() { migrationSnapshotBeforeOpen = oldHook })
	replaced := false
	migrationSnapshotBeforeOpen = func(rel string) {
		if rel != "raw/source.md" || replaced {
			return
		}
		replaced = true
		_ = os.Remove(filepath.Join(vault, "raw", "source.md"))
		_ = os.Symlink(filepath.Join(external, "outside.db"), filepath.Join(vault, "raw", "source.md"))
	}
	if _, err := snapshotMigrationInputs(vault); err == nil {
		t.Fatal("validated file replacement was read through a symlink")
	}
}

func TestSyntoIndexIdentityAndHashValidationFailBeforeRewrite(t *testing.T) {
	workspace := t.TempDir()
	mapData := []byte(`{"concept":{"article-a":"beta"},"source":{},"redirects":{}}`)
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), mapData)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "beta.md"), []byte("---\nid: article-a\n---\nbody\n"))
	index := syntoIndexFixtureWithEntities([]string{"article-a:01JAZ5N7Y3K8M2Q4R6T9VWXAC0:alpha", "article-b:01JAZ5N7Y3K8M2Q4R6T9VWXAC1:beta"}, nil)
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(index))
	if _, err := readSyntoEntityIDs(workspace, map[string]string{"article-a": "beta"}); err == nil {
		t.Fatal("ID/path disagreement was accepted")
	}
	before, err := os.ReadFile(filepath.Join(workspace, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileWorkspaceConcepts(workspace, nil); err == nil {
		t.Fatal("reconcile accepted ID/path disagreement")
	}
	after, _ := os.ReadFile(filepath.Join(workspace, "cache", "id_map.json"))
	if string(before) != string(after) {
		t.Fatal("identity mismatch rewrote id_map")
	}

	valid := syntoIndexFixture("article", "01JAZ5N7Y3K8M2Q4R6T9VWXAC8", "alpha", true)
	for name, hash := range map[string]string{
		"empty":     "",
		"short":     "abc",
		"non-hex":   strings.Repeat("g", 64),
		"uppercase": strings.Repeat("A", 64),
		"valid":     strings.Repeat("0", 64),
	} {
		t.Run(name, func(t *testing.T) {
			data := strings.Replace(valid, strings.Repeat("0", 64), hash, 1)
			root := t.TempDir()
			mustWriteFile(t, filepath.Join(root, ".synto", "INDEX.json"), []byte(data))
			_, err := readSyntoIndexTruth(root)
			if name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("malformed content_hash accepted")
			}
		})
	}
}

func TestSyntoPackExportArticlesPathIsConsumedByAdapter(t *testing.T) {
	data := strings.Replace(syntoIndexFixture("article", "01JAZ5N7Y3K8M2Q4R6T9VWXAC8", "alpha", true), `"path":"wiki/alpha.md"`, `"path":"articles/alpha.md"`, 1)
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(data))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"article":"alpha"},"source":{},"redirects":{}}`))
	if _, err := readSyntoIndexTruth(workspace); err != nil {
		t.Fatalf("exact pack INDEX was rejected: %v", err)
	}
	entities, err := readSyntoEntityIDs(workspace, map[string]string{"article": "alpha"})
	if err != nil {
		t.Fatalf("exact pack INDEX did not reach entity reconciliation: %v", err)
	}
	if entities["article"] != "01JAZ5N7Y3K8M2Q4R6T9VWXAC8" {
		t.Fatalf("entity mapping = %#v, want article -> canonical entity", entities)
	}
}

func TestSyntoPackExportOmittedArticleEntityUsesPriorStablePathIdentity(t *testing.T) {
	workspace := t.TempDir()
	raw := []byte("authoritative source\n")
	contentHash := sha256Text(string(raw))
	mustWriteFile(t, filepath.Join(workspace, "raw", "source.md"), raw)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: stable-alpha\nsources:\n  - stable-source\n---\nprior annotation\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha"},"concept_entity_id":{"stable-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"},"source":{"stable-source":"source"},"source_meta":{"stable-source":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"stable-alpha","sources":["stable-source"]}}`+"\n"))
	prior, err := snapshotConcepts(workspace, []sourceSnapshot{{SourceID: "stable-source", RawPath: "raw/source.md"}})
	if err != nil {
		t.Fatal(err)
	}

	// This is the released pack-export shape: articles use articles/*.md,
	// entity_id is omitted, and source_concepts is grouped under a source.
	index := `{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[{"id":"generated-alpha","name":"Current Alpha Article","path":"articles/alpha.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":[{"source_path":"raw/source.md","content_hash":"` + contentHash + `","concepts":[{"name":"Current Alpha Concept","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}]}],"synthesis":[],"stats":{"article_count":1,"draft_count":0,"concept_count":1,"alias_count":0,"knowledge_item_count":0,"source_count":1,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(index))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"generated-alpha":"alpha"},"source":{"stable-source":"source"},"source_meta":{"stable-source":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))

	if err := reconcileWorkspaceConcepts(workspace, prior, []sourceSnapshot{{SourceID: "stable-source", RawPath: "raw/source.md", RawBytes: raw, SyntoContentHash: contentHash}}); err == nil {
		t.Fatal("omitted article was accepted from prior/source evidence")
	}
}

func TestSyntoPackExportOmittedArticleEntityUsesPriorStableIdentityAcrossMultipleSources(t *testing.T) {
	prior := []conceptSnapshot{{ConceptID: "stable-alpha", Slug: "alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePaths: []string{"raw/a.md", "raw/b.md"}}}
	indexData := []byte(`{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[{"id":"generated-alpha","name":"Current Alpha","path":"articles/alpha.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":[{"source_path":"raw/a.md","content_hash":"` + strings.Repeat("0", 64) + `","concepts":[{"name":"Current Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABE"}]},{"source_path":"raw/b.md","content_hash":"` + strings.Repeat("1", 64) + `","concepts":[{"name":"Current Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABE"}]}],"synthesis":[],"stats":{"article_count":1,"draft_count":0,"concept_count":1,"alias_count":0,"knowledge_item_count":0,"source_count":2,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`)
	index, err := decodeSyntoIndex(indexData)
	if err != nil {
		t.Fatalf("nested source_concepts fixture was rejected: %v", err)
	}

	got, err := mapSyntoEntityIDsFromIndexTruth(index, map[string]string{"generated-alpha": "alpha"}, prior)
	if err != nil || len(got) != 0 {
		t.Fatalf("prior/source evidence populated omitted article: map=%#v error=%v", got, err)
	}
}

func TestSyntoPackExportOmittedArticleDoesNotReusePriorPathAcrossNestedSource(t *testing.T) {
	workspace := t.TempDir()
	priorPage := []byte("---\nid: stable-alpha\nsources:\n  - stable-old-source\n---\nprior annotation\n")
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), priorPage)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "other.md"), []byte("---\nid: generated-other\n---\nother\n"))
	mustWriteFile(t, filepath.Join(workspace, "raw", "current.md"), []byte("current source\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha","generated-other":"other"},"concept_entity_id":{"stable-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABE","generated-other":"01JAZ5N7Y3K8M2Q4R6T9VWXABF"},"source":{"stable-old-source":"old-source"},"source_meta":{"stable-old-source":{"slug":"old-source","source_file":"raw/old.md"}},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"stable-alpha","sources":["stable-old-source"]}}`+"\n"+`{"slug":"other","frontmatter":{"id":"generated-other"}}`+"\n"))
	prior, err := snapshotConcepts(workspace, []sourceSnapshot{{SourceID: "stable-old-source", RawPath: "raw/old.md"}})
	if err != nil {
		t.Fatal(err)
	}

	contentHash := sha256Text("current source\n")
	index := `{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[{"id":"generated-new","name":"Unrelated Name","path":"articles/alpha.md","summary":null,"tags":[],"aliases":[],"confidence":"high"},{"id":"generated-other","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABF","name":"Other Name","path":"articles/other.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":[{"source_path":"raw/current.md","content_hash":"` + contentHash + `","concepts":[{"name":"Other Name","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABF"}]}],"synthesis":[],"stats":{"article_count":2,"draft_count":0,"concept_count":2,"alias_count":0,"knowledge_item_count":0,"source_count":1,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`
	// The nested current source edge belongs to 01JAZ5N7Y3K8M2Q4R6T9VWXABF, not the prior
	// 01JAZ5N7Y3K8M2Q4R6T9VWXABE. Reusing alpha's prior path would transfer identity silently.
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(index))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"generated-new":"alpha","generated-other":"other"},"concept_entity_id":{"generated-other":"01JAZ5N7Y3K8M2Q4R6T9VWXABF"},"source":{},"redirects":{}}`))
	if err := reconcileWorkspaceConcepts(workspace, prior, []sourceSnapshot{{RawPath: "raw/current.md", RawBytes: []byte("current source\n"), SyntoContentHash: contentHash}}); err != nil {
		t.Fatalf("ordinary article reconciliation failed: %v", err)
	}
	ids := mustSnapshotIDMap(t, workspace)
	if _, exists := ids.Concept["stable-alpha"]; exists {
		t.Fatalf("ordinary article reused prior identity: %#v", ids.Concept)
	}
}

func TestSyntoPackExportSourcesIDAcceptsRawRelativePath(t *testing.T) {
	const sourcesSeam = `"sources":[]`
	index := syntoIndexFixture("article", "01JAZ5N7Y3K8M2Q4R6T9VWXAC8", "alpha", true)
	if !strings.Contains(index, sourcesSeam) {
		t.Fatalf("fixture missing sources seam %q", sourcesSeam)
	}
	index = strings.Replace(index, sourcesSeam, `"sources":[{"id":"raw/source.md","title":"Source File","source_type":"raw"}]`, 1)
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(index))
	if _, err := readSyntoIndexTruth(workspace); err != nil {
		t.Fatalf("exact Synto pack export source id path was rejected: %v", err)
	}
}

func TestSafeSyntoSourceID(t *testing.T) {
	for _, c := range []struct {
		name  string
		value string
		ok    bool
	}{
		{name: "accept bare id", value: "s1", ok: true},
		{name: "accept raw path", value: "raw/source.md", ok: true},
		{name: "reject empty", value: "", ok: false},
		{name: "reject dot", value: ".", ok: false},
		{name: "reject dotdot", value: "..", ok: false},
		{name: "reject absolute", value: "/outside", ok: false},
		{name: "reject traversal", value: "../outside", ok: false},
		{name: "reject normalized traversal", value: "raw/../outside", ok: false},
		{name: "reject backslash path", value: `raw\\source.md`, ok: false},
		{name: "reject windows volume", value: "C:/outside", ok: false},
		{name: "reject windows partial volume", value: "C:outside", ok: false},
		{name: "reject newline", value: "raw\nsource.md", ok: false},
		{name: "reject del", value: "raw\x7fsource.md", ok: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := safeSyntoSourceID(c.value); got != c.ok {
				t.Fatalf("safeSyntoSourceID(%q) = %v, want %v", c.value, got, c.ok)
			}
		})
	}
}

func TestSyntoArticlePathNormalizationIsStrict(t *testing.T) {
	for _, path := range []string{"articles/Alpha.md", "wiki/Alpha.md"} {
		if got, err := normalizeSyntoArticlePath(path); err != nil || got != "Alpha" {
			t.Errorf("normalizeSyntoArticlePath(%q) = %q, %v", path, got, err)
		}
	}
	for _, path := range []string{
		"articles/nested/Alpha.md",
		"articles/../Alpha.md",
		"articles/Alpha\\Beta.md",
		"/articles/Alpha.md",
		"articles/Alpha.txt",
		"articles/.md",
		"articles/",
		"exports/Alpha.md",
	} {
		if _, err := normalizeSyntoArticlePath(path); err == nil {
			t.Errorf("normalizeSyntoArticlePath(%q) accepted malformed path", path)
		}
	}
}

func TestSyntoPackExportMalformedPathsAndCaseCollisionsLeaveIDMapUnchanged(t *testing.T) {
	base := syntoIndexFixture("article", "01JAZ5N7Y3K8M2Q4R6T9VWXAC8", "alpha", true)
	for name, index := range map[string]string{
		"nested path":       strings.Replace(base, `"path":"wiki/alpha.md"`, `"path":"articles/nested/alpha.md"`, 1),
		"unexpected prefix": strings.Replace(base, `"path":"wiki/alpha.md"`, `"path":"exports/alpha.md"`, 1),
		"case collision":    strings.Replace(base, `],"terms":[]`, `,{"id":"article-b","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC1","name":"Alpha","path":"articles/ALPHA.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[]`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(index))
			idMap := []byte(`{"concept":{"article":"alpha"},"source":{},"redirects":{}}`)
			mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), idMap)
			mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: article\n---\nbody\n"))
			if err := reconcileWorkspaceConcepts(workspace, nil); err == nil {
				t.Fatal("malformed exact INDEX was accepted")
			}
			got, err := os.ReadFile(filepath.Join(workspace, "cache", "id_map.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(idMap) {
				t.Fatalf("malformed exact INDEX rewrote id_map: %q", got)
			}
		})
	}
}

// TestExactSyntoPackExportBridge is the parent-side selector for the exact
// Python release smoke. It is skipped during ordinary unit runs and consumes
// both INDEX.json files plus the raw source bytes written by that smoke.
func TestExactSyntoPackExportBridge(t *testing.T) {
	run1Path := strings.TrimSpace(os.Getenv("LWC195_EXACT_INDEX_RUN1_PATH"))
	run2Path := strings.TrimSpace(os.Getenv("LWC195_EXACT_INDEX_RUN2_PATH"))
	rawPath := strings.TrimSpace(os.Getenv("LWC195_RAW_SOURCE_PATH"))
	configPath := strings.TrimSpace(os.Getenv("LWC197_MIGRATED_CONFIG_PATH"))
	if err := validateExactSyntoBridgeEnv(run1Path, run2Path, rawPath, configPath); err != nil {
		t.Fatal(err)
	}
	if run1Path == "" {
		t.Skip("set the four exact Synto bridge output paths")
	}

	readExport := func(path string) syntoIndexTruth {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read exact exported INDEX %q: %v", path, err)
		}
		if len(data) == 0 {
			t.Fatalf("exact exported INDEX %q is empty", path)
		}
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), data)
		truth, err := readSyntoIndexTruth(workspace)
		if err != nil {
			t.Fatalf("actual Go adapter rejected exact export %q: %v", path, err)
		}
		if !truth.Present {
			t.Fatalf("exact export %q is not present", path)
		}
		return truth
	}
	first, second := readExport(run1Path), readExport(run2Path)

	alpha := func(label string, truth syntoIndexTruth) (syntoIndexEntry, syntoSourceConcept) {
		t.Helper()
		var article *syntoIndexEntry
		for i := range truth.Articles {
			if truth.Articles[i].Path == "articles/Alpha.md" {
				if article != nil {
					t.Fatalf("%s export contains duplicate articles/Alpha.md", label)
				}
				article = &truth.Articles[i]
			}
		}
		if article == nil || article.ID == "" {
			t.Fatalf("%s export lacks non-empty Alpha article identity: %#v", label, truth.Articles)
		}
		var edge *syntoSourceConcept
		for i := range truth.SourceConcepts {
			candidate := &truth.SourceConcepts[i]
			if candidate.SourcePath == "raw/source.md" && candidate.Name == "Alpha" {
				if candidate.EntityID == "" {
					t.Fatalf("%s export has empty Alpha engine entity ID", label)
				}
				if edge != nil {
					t.Fatalf("%s export contains duplicate Alpha/raw/source.md edges", label)
				}
				edge = candidate
			}
		}
		if edge == nil {
			t.Fatalf("%s export lacks expected Alpha/raw/source.md edge", label)
		}
		return *article, *edge
	}
	firstArticle, firstEdge := alpha("run1", first)
	secondArticle, secondEdge := alpha("run2", second)
	if firstArticle.ID != secondArticle.ID || firstArticle.Path != secondArticle.Path || firstEdge.EntityID != secondEdge.EntityID {
		t.Fatalf("non-empty run identity continuity failed: run1=%#v run2=%#v", firstArticle, secondArticle)
	}
	if firstEdge.ContentHash == "" || firstEdge.ContentHash != secondEdge.ContentHash {
		t.Fatalf("non-empty source edge continuity failed: run1=%#v run2=%#v", firstEdge, secondEdge)
	}
	t.Logf("LWC195_RUN1_RUN2_NON_EMPTY_ENTITY_CONTINUITY=PASS entity_id=%s article_id=%s", firstEdge.EntityID, firstArticle.ID)

	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatalf("read actual raw source fixture: %v", err)
	}
	if !bytes.Equal(raw, []byte("bridge source\n")) {
		t.Fatalf("unexpected raw source fixture: %q", raw)
	}
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "raw", "source.md"), raw)
	stableMap := wikiindex.IDMap{
		Concept:         map[string]string{},
		DormantConcept:  map[string]string{"stable-alpha": "Alpha"},
		ConceptEntityID: map[string]string{"stable-alpha": firstEdge.EntityID},
		Source:          map[string]string{"stable-source": "source"},
		SourceMeta:      map[string]wikiindex.SourceMeta{"stable-source": {Slug: "source", SourceFile: "raw/source.md"}},
		Redirects:       map[string][]string{},
	}
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), mustJSON(t, stableMap))
	mustWriteFile(t, filepath.Join(workspace, "wiki", ".dormant", "Alpha.md"), []byte("---\nid: stable-alpha\n---\nprior Alpha\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "dormant_concepts.jsonl"), []byte(`{"slug":"Alpha","frontmatter":{"id":"stable-alpha"}}`+"\n"))
	prior, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatalf("snapshot stable prior concept: %v", err)
	}
	sources, err := snapshotSources(workspace)
	if err != nil {
		t.Fatalf("snapshot actual raw source: %v", err)
	}
	if len(sources) != 1 || !bytes.Equal(sources[0].RawBytes, raw) || sources[0].SyntoContentHash != firstEdge.ContentHash {
		t.Fatalf("independent source snapshot/hash mismatch: sources=%#v edge=%#v", sources, firstEdge)
	}
	t.Logf("LWC195_INDEPENDENT_SOURCE_HASH=PASS content_hash=%s", sources[0].SyntoContentHash)

	transientMap := wikiindex.IDMap{
		Concept:         map[string]string{"transient-alpha": "Alpha"},
		ConceptEntityID: map[string]string{"transient-alpha": secondEdge.EntityID},
		Source:          stableMap.Source, SourceMeta: stableMap.SourceMeta, Redirects: map[string][]string{},
	}
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), mustJSON(t, transientMap))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "Alpha.md"), []byte("---\nid: e5a1b2c3d4e5\n---\nsecond Alpha\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"Alpha","frontmatter":{"id":"transient-alpha"}}`+"\n"))
	secondWorkspace := filepath.Join(workspace, ".synto", "INDEX.json")
	secondData, err := os.ReadFile(run2Path)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, secondWorkspace, secondData)
	if err := reconcileWorkspaceConcepts(workspace, prior, sources); err != nil {
		t.Fatalf("reconcile exact second export: %v", err)
	}
	ids := mustSnapshotIDMap(t, workspace)
	if ids.Concept["stable-alpha"] != "Alpha" || ids.ConceptEntityID["stable-alpha"] != firstEdge.EntityID || ids.DormantConcept["stable-alpha"] != "" {
		t.Fatalf("stable LWC identity did not survive/reactivate: %#v", ids)
	}
	if _, exists := ids.Concept["transient-alpha"]; exists {
		t.Fatalf("transient replacement remained after reconciliation: %#v", ids.Concept)
	}
	t.Log("LWC195_STABLE_LWC_ID_REACTIVATED=PASS stable_id=stable-alpha")

	priorChanged, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatal(err)
	}
	changedRaw := []byte("bridge source changed\n")
	mustWriteFile(t, filepath.Join(workspace, "raw", "source.md"), changedRaw)
	changedSources, err := snapshotSources(workspace)
	if err != nil {
		t.Fatalf("snapshot changed raw source: %v", err)
	}
	if changedSources[0].SyntoContentHash == firstEdge.ContentHash {
		t.Fatal("changed raw source retained the exported content hash")
	}
	if err := reconcileWorkspaceConcepts(workspace, priorChanged, changedSources); err != nil {
		t.Fatalf("reconcile changed source: %v", err)
	}
	ids = mustSnapshotIDMap(t, workspace)
	if len(ids.Concept) != 0 || ids.DormantConcept["stable-alpha"] != "Alpha" || ids.ConceptEntityID["stable-alpha"] != firstEdge.EntityID {
		t.Fatalf("changed source did not dormant stable identity: %#v", ids)
	}
	t.Log("LWC195_CHANGED_SOURCE_DORMANT_STABLE_ID=PASS stable_id=stable-alpha")
}

// TestExactSyntoMigratedConfigBridge consumes bytes emitted by the real
// Synto 0.7.0 migrate-olw smoke and sends them through the Go normalization
// and safety-validation seam.
func TestExactSyntoMigratedConfigBridge(t *testing.T) {
	run1Path := strings.TrimSpace(os.Getenv("LWC195_EXACT_INDEX_RUN1_PATH"))
	run2Path := strings.TrimSpace(os.Getenv("LWC195_EXACT_INDEX_RUN2_PATH"))
	rawPath := strings.TrimSpace(os.Getenv("LWC195_RAW_SOURCE_PATH"))
	path := strings.TrimSpace(os.Getenv("LWC197_MIGRATED_CONFIG_PATH"))
	if err := validateExactSyntoBridgeEnv(run1Path, run2Path, rawPath, path); err != nil {
		t.Fatal(err)
	}
	if run1Path == "" {
		t.Skip("set the four exact Synto bridge output paths")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read exact migrated config %q: %v", path, err)
	}
	if len(data) == 0 {
		t.Fatal("exact migrated config is empty")
	}
	var before map[string]interface{}
	if _, err := toml.Decode(string(data), &before); err != nil {
		t.Fatalf("exact migrated config is not valid TOML: %v", err)
	}
	if _, exists := before["pipeline"]; exists {
		t.Fatalf("exact migrated fixture unexpectedly already has pipeline: %s", data)
	}
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), data)
	if err := normalizeMigratedSyntoConfig(vault); err != nil {
		t.Fatalf("Go normalization rejected exact migrated config: %v", err)
	}
	normalized, err := os.ReadFile(filepath.Join(vault, "synto.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSyntoPipelineSafetyBytes(normalized); err != nil {
		t.Fatalf("Go safety validation rejected normalized exact config: %v", err)
	}
	var after map[string]interface{}
	if _, err := toml.Decode(string(normalized), &after); err != nil {
		t.Fatal(err)
	}
	if !equalSyntoConfigSemanticsWithoutSafety(before, after) {
		t.Fatalf("Go normalization changed exact migrated semantics: %s", normalized)
	}
	t.Logf("LWC197_EXACT_MIGRATED_CONFIG_NORMALIZED=PASS bytes_before=%d bytes_after=%d", len(data), len(normalized))
}

func TestPostprocessCreatesAndPreservesDormantCache(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: alpha\ntitle: Alpha\n---\nbody\n"))
	if err := runPostprocess(context.Background(), vault); err != nil {
		t.Fatal(err)
	}
	dormantPath := filepath.Join(vault, "cache", "dormant_concepts.jsonl")
	data, err := os.ReadFile(dormantPath)
	if err != nil || len(data) != 0 {
		t.Fatalf("fresh dormant cache=%q err=%v", data, err)
	}
	mustWriteFile(t, dormantPath, []byte("{\"slug\":\"old\"}\n"))
	if err := runPostprocess(context.Background(), vault); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(dormantPath)
	if err != nil || string(data) != "{\"slug\":\"old\"}\n" {
		t.Fatalf("existing dormant cache was not preserved: %q err=%v", data, err)
	}
}

func TestSyntoIndexDecoderRejectsAdversarialJSON(t *testing.T) {
	base := syntoIndexFixture("article", "01JAZ5N7Y3K8M2Q4R6T9VWXAC8", "alpha", true)
	for name, data := range map[string]string{
		"duplicate top-level key": strings.Replace(base, `"terms":[]`, `"terms":[],"terms":[]`, 1),
		"trailing JSON":           base + ` {}`,
		"extra field":             strings.Replace(base, `"stats":{`, `"extra":1,"stats":{`, 1),
		"unsafe source path":      strings.Replace(base, `"raw/source.md"`, `"../outside.md"`, 1),
		"missing required field":  strings.Replace(base, `,"aliases":[]`, "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(data))
			if _, err := readSyntoIndexTruth(workspace); err == nil {
				t.Fatal("adversarial INDEX.json accepted")
			}
		})
	}
	workspace := t.TempDir()
	file := filepath.Join(workspace, ".synto", "INDEX.json")
	mustWriteFile(t, file, nil)
	stat, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(file, maxSyntoIndexBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readSyntoIndexTruth(workspace); err == nil || stat.Size() > maxSyntoIndexBytes {
		t.Fatal("oversized INDEX.json accepted")
	}
}

func TestSyntoIndexLimitMatchesSharedGenerationMaximum(t *testing.T) {
	if maxSyntoIndexBytes != generation.MaxFileBytes {
		t.Fatalf("worker INDEX limit=%d, shared generation limit=%d", maxSyntoIndexBytes, generation.MaxFileBytes)
	}
	base := []byte(syntoIndexFixture("article", "01JAZ5N7Y3K8M2Q4R6T9VWXAC8", "alpha", true))
	justAboveFormerLimit := append(append([]byte(nil), base...), bytes.Repeat([]byte(" "), (8<<20)+1-len(base))...)
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), justAboveFormerLimit)
	if _, err := readSyntoIndexTruth(workspace); err != nil {
		t.Fatalf("valid INDEX just above former worker limit rejected: %v", err)
	}

	tooLarge := filepath.Join(t.TempDir(), ".synto", "INDEX.json")
	if err := os.MkdirAll(filepath.Dir(tooLarge), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tooLarge, base, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(tooLarge, generation.MaxFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readSyntoIndexTruth(filepath.Dir(filepath.Dir(tooLarge))); err == nil {
		t.Fatal("INDEX above shared generation limit accepted")
	}
}

func TestStrictJSONNestingDepthBoundary(t *testing.T) {
	nested := func(depth int) string {
		return strings.Repeat("[", depth) + "null" + strings.Repeat("]", depth)
	}
	for _, depth := range []int{maxStrictJSONDepth, maxStrictJSONDepth + 1} {
		dec := json.NewDecoder(strings.NewReader(nested(depth)))
		_, err := decodeStrictJSONValue(dec)
		if depth == maxStrictJSONDepth && err != nil {
			t.Fatalf("depth %d rejected at boundary: %v", depth, err)
		}
		if depth > maxStrictJSONDepth && err == nil {
			t.Fatalf("depth %d accepted beyond boundary", depth)
		}
	}
}

func testEntityMappingErrorDetail(t *testing.T, err error, want conceptReconcileDetailCode) {
	t.Helper()
	var detail conceptReconciliationDetail
	if !errors.As(err, &detail) {
		t.Fatalf("expected concept detail, got %T: %v", err, err)
	}
	if got := detail.ConceptReconciliationDetail(); got != want {
		t.Fatalf("got detail %q, want %q", got, want)
	}
}

func testEntityMappingErrorDetailCode(t *testing.T, err error) conceptReconcileDetailCode {
	t.Helper()
	var detail conceptReconciliationDetail
	if !errors.As(err, &detail) {
		t.Fatalf("expected concept detail, got %T: %v", err, err)
	}
	return detail.ConceptReconciliationDetail()
}

func syntoIndexTruthForEntityMapping(articles []syntoIndexEntry, sourceConcepts []syntoSourceConcept, extraActiveEntities []string) syntoIndexTruth {
	active := make(map[string]bool, len(sourceConcepts)+len(extraActiveEntities))
	for _, concept := range sourceConcepts {
		active[concept.EntityID] = true
	}
	for _, entityID := range extraActiveEntities {
		active[entityID] = true
	}
	return syntoIndexTruth{
		Articles:       articles,
		SourceConcepts: sourceConcepts,
		ActiveEntities: active,
		Present:        true,
	}
}

func TestSyntoEntityMappingRejectsEntitylessIDPathDisagreement(t *testing.T) {
	const entityID = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	index, err := decodeSyntoIndex([]byte(syntoCrossArtifactEntitylessIDPathFixture(entityID)))
	if err != nil {
		t.Fatal(err)
	}

	got, err := mapSyntoEntityIDsFromIndexTruth(index, map[string]string{"article-a": "alpha"})
	if err == nil {
		t.Fatalf("malformed entityless ID/bound-path fixture was accepted with map=%#v", got)
	}
	testEntityMappingErrorDetail(t, err, conceptDetailEntityMappingConceptIDPathDisagreement)

	got, err = mapSyntoEntityIDsFromIndexTruth(index, map[string]string{"article-a": "unknown"})
	if err == nil {
		t.Fatalf("known entityless ID with unknown path was accepted with map=%#v", got)
	}
	testEntityMappingErrorDetail(t, err, conceptDetailEntityMappingConceptIDPathDisagreement)

	reservedAndOrdinary := syntoIndexTruthForEntityMapping(
		[]syntoIndexEntry{
			{ID: "root-index", Name: "Index", Path: "wiki/index.md"},
			{ID: "ordinary-id", Name: "Ordinary", Path: "wiki/ordinary.md"},
		},
		nil,
		nil,
	)
	got, err = mapSyntoEntityIDsFromIndexTruth(reservedAndOrdinary, map[string]string{"root-index": "ordinary"})
	if err == nil {
		t.Fatalf("reserved article ID was treated as an unknown transient ID: map=%#v", got)
	}
	testEntityMappingErrorDetail(t, err, conceptDetailEntityMappingConceptIDPathDisagreement)
}

func TestSyntoEntityMappingSkipsReservedRootPagesBeforeMandatoryMapping(t *testing.T) {
	index := syntoIndexTruthForEntityMapping(
		[]syntoIndexEntry{
			{ID: "root-index", Name: "Index", Path: "wiki/index.md"},
			{ID: "root-log", Name: "Log", Path: "wiki/log.md"},
			// Omitted entity_id matches the released agents pack export. The
			// source edge is the current title-drift-compatible identity proof.
			{ID: "generated-alpha", Name: "Current Alpha", Path: "articles/alpha.md"},
		},
		[]syntoSourceConcept{{Name: "Current Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABC", SourcePath: "raw/source.md"}},
		nil,
	)

	got, err := mapSyntoEntityIDsFromIndexTruth(index, map[string]string{
		"generated-alpha": "alpha",
	}, []conceptSnapshot{{
		ConceptID:   "stable-alpha",
		Slug:        "alpha",
		EntityID:    "01JAZ5N7Y3K8M2Q4R6T9VWXABC",
		SourcePaths: []string{"raw/source.md"},
	}})
	if err == nil || len(got) != 0 {
		t.Fatalf("normal entityless article was mapped: map=%#v error=%v", got, err)
	}
}

func TestSyntoEntityMappingReservedRootsPreservesAgentIdentityJoin(t *testing.T) {
	indexData := `{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[{"id":"root-index","name":"Index","path":"wiki/index.md","summary":null,"tags":[],"aliases":[],"confidence":"high"},{"id":"root-log","name":"Log","path":"wiki/log.md","summary":null,"tags":[],"aliases":[],"confidence":"high"},{"id":"generated-alpha","name":"Current Alpha","path":"articles/alpha.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":[{"source_path":"raw/source.md","content_hash":"` + strings.Repeat("0", 64) + `","concepts":[{"name":"Current Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}]}],"synthesis":[],"stats":{"article_count":3,"draft_count":0,"concept_count":1,"alias_count":0,"knowledge_item_count":0,"source_count":1,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`
	conceptsData := agentConceptsFixture(`"canonical_article_id":"generated-alpha","article_path":"articles/alpha.md"`, `"entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"`)
	joined, err := enrichSyntoIndexWithAgentConcepts([]byte(indexData), []byte(conceptsData))
	if err != nil {
		t.Fatalf("agent identity join failed: %v", err)
	}
	index, err := decodeSyntoIndex(joined)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mapSyntoEntityIDsFromIndexTruth(index, map[string]string{"generated-alpha": "alpha"}, []conceptSnapshot{{
		ConceptID:   "stable-alpha",
		Slug:        "alpha",
		EntityID:    "01JAZ5N7Y3K8M2Q4R6T9VWXABC",
		SourcePaths: []string{"raw/source.md"},
	}})
	if err != nil || len(got) != 0 {
		t.Fatalf("agent-joined normal article was mapped: map=%#v error=%v", got, err)
	}
}

func TestSyntoEntityMappingReservedRootsStillValidateIdentity(t *testing.T) {
	tests := []struct {
		name    string
		article syntoIndexEntry
	}{
		{name: "unsafe article ID", article: syntoIndexEntry{ID: "../root", Name: "Index", Path: "wiki/index.md"}},
		{name: "unsafe entity ID", article: syntoIndexEntry{ID: "root-log", EntityID: "../entity", Name: "Log", Path: "wiki/log.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index := syntoIndexTruthForEntityMapping([]syntoIndexEntry{tt.article}, nil, nil)
			_, err := mapSyntoEntityIDsFromIndexTruth(index, map[string]string{})
			if err == nil {
				t.Fatal("unsafe reserved-root identity was skipped")
			}
			testEntityMappingErrorDetail(t, err, conceptDetailEntityMappingArticleIdentity)
		})
	}
}

func TestSyntoEntityMappingReservedRootPathMatrixFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		path string
		want conceptReconcileDetailCode
	}{
		{name: "uppercase index", path: "wiki/Index.md"},
		{name: "uppercase log", path: "wiki/Log.md", want: conceptDetailEntityMappingConceptIDPathDisagreement},
		{name: "nested index", path: "wiki/nested/index.md", want: conceptDetailEntityMappingArticlePath},
		{name: "nested log", path: "wiki/nested/log.md", want: conceptDetailEntityMappingArticlePath},
		{name: "index lookalike", path: "wiki/index2.md", want: conceptDetailEntityMappingConceptIDPathDisagreement},
		{name: "log lookalike", path: "wiki/logbook.md", want: conceptDetailEntityMappingConceptIDPathDisagreement},
		{name: "articles index", path: "articles/index.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index := syntoIndexTruthForEntityMapping(
				[]syntoIndexEntry{{ID: "not-a-root", Name: "Reserved Lookalike", Path: tt.path}},
				nil,
				nil,
			)
			_, err := mapSyntoEntityIDsFromIndexTruth(index, map[string]string{"not-a-root": "index"})
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ordinary path %q failed: %v", tt.path, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("unsafe path %q was accepted", tt.path)
			}
			testEntityMappingErrorDetail(t, err, tt.want)
		})
	}
}

func TestReadSyntoEntityIDsReturnsReasonedDetailCodes(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(`{"schema_version":1}`))
	if _, err := readSyntoEntityIDs(workspace, map[string]string{}); err == nil {
		t.Fatal("malformed INDEX expected to fail")
	} else {
		testEntityMappingErrorDetail(t, err, conceptDetailEntityMappingIndexTruth)
	}

	base := syntoIndexFixture("article-a", "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", "alpha", true)
	decoderBranches := []struct {
		name string
		path string
		want conceptReconcileDetailCode
	}{
		{
			name: "decode article identity",
			path: strings.Replace(base, `"id":"article-a"`, `"id":"article/a"`, 1),
			want: conceptDetailEntityMappingArticleIdentity,
		},
		{
			name: "decode article path",
			path: strings.Replace(base, `"wiki/alpha.md"`, `"exports/alpha.md"`, 1),
			want: conceptDetailEntityMappingArticlePath,
		},
		{
			name: "decode source concept identity",
			path: strings.Replace(base, `"name":"alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC0"`, `"name":"alpha","entity_id":"entity/a"`, 1),
			want: conceptDetailEntityMappingSourceConceptIdentity,
		},
	}
	for _, tc := range decoderBranches {
		t.Run(tc.name, func(t *testing.T) {
			mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(tc.path))
			if _, err := readSyntoEntityIDs(workspace, map[string]string{}); err == nil {
				t.Fatalf("decoder-level branch %q accepted", tc.name)
			} else {
				testEntityMappingErrorDetail(t, err, tc.want)
			}
		})
	}

	conceptBranches := []struct {
		name     string
		index    syntoIndexTruth
		concepts map[string]string
		want     conceptReconcileDetailCode
	}{
		{
			name:     "source concept identity",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{}, []syntoSourceConcept{{Name: "", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}, nil),
			concepts: map[string]string{},
			want:     conceptDetailEntityMappingSourceConceptIdentity,
		},
		{
			name:     "article identity",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article/a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"}}, []syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}, nil),
			concepts: map[string]string{},
			want:     conceptDetailEntityMappingArticleIdentity,
		},
		{
			name: "source concept ambiguity",
			index: syntoIndexTruthForEntityMapping(
				[]syntoIndexEntry{{ID: "article-a", Name: "Alpha", Path: "wiki/alpha.md"}},
				[]syntoSourceConcept{
					{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"},
					{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1"},
				}, nil),
			concepts: map[string]string{"article-a": "alpha"},
			want:     conceptDetailEntityMappingActiveEntityUnknown,
		},
		{
			name:     "source concept missing",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article-a", Name: "Alpha", Path: "wiki/alpha.md"}}, []syntoSourceConcept{{Name: "Beta", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1"}}, nil),
			concepts: map[string]string{"article-a": "alpha"},
			want:     conceptDetailEntityMappingActiveEntityUnknown,
		},
		{
			name:     "article path",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "articles/nested/alpha.md"}}, []syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}, nil),
			concepts: map[string]string{"article-a": "alpha"},
			want:     conceptDetailEntityMappingArticlePath,
		},
		{
			name: "article source disagreement",
			index: syntoIndexTruthForEntityMapping(
				[]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"}},
				[]syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1"}}, nil),
			concepts: map[string]string{"article-a": "alpha"},
			want:     conceptDetailEntityMappingActiveEntityUnknown,
		},
		{
			name:     "duplicate article ID",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"}, {ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1", Name: "Beta", Path: "wiki/beta.md"}}, []syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}, {Name: "Beta", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1"}}, nil),
			concepts: map[string]string{"article-a": "alpha", "article-b": "beta"},
			want:     conceptDetailEntityMappingDuplicateArticleID,
		},
		{
			name: "duplicate article path",
			index: syntoIndexTruthForEntityMapping(
				[]syntoIndexEntry{
					{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/Alpha.md"},
					{ID: "article-b", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1", Name: "Alpha", Path: "wiki/alpha.md"},
				},
				[]syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}, {Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1"}},
				nil,
			),
			concepts: map[string]string{"article-a": "alpha"},
			want:     conceptDetailEntityMappingDuplicateArticlePath,
		},
		{
			name:     "duplicate entity ID",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"}, {ID: "article-b", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Beta", Path: "wiki/beta.md"}}, []syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}, {Name: "Beta", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}, nil),
			concepts: map[string]string{"article-a": "alpha", "article-b": "beta"},
			want:     conceptDetailEntityMappingDuplicateEntityID,
		},
		{
			name:     "active entity collision",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"}}, []syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}, []string{"01JAZ5N7Y3K8M2Q4R6T9VWXAC1"}),
			concepts: map[string]string{"article-a": "alpha"},
			want:     conceptDetailEntityMappingActiveEntityUnknown,
		},
		{
			name:     "concept slug case mismatch",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"}}, []syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}, nil),
			concepts: map[string]string{"article-a": "ALPHA"},
			want:     conceptDetailEntityMappingConceptSlugCase,
		},
		{
			name: "concept ID-path disagreement",
			index: syntoIndexTruthForEntityMapping(
				[]syntoIndexEntry{
					{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"},
					{ID: "article-b", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1", Name: "Beta", Path: "wiki/beta.md"},
				},
				[]syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}, {Name: "Beta", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1"}},
				nil,
			),
			concepts: map[string]string{"article-a": "beta"},
			want:     conceptDetailEntityMappingConceptIDPathDisagreement,
		},
		{
			name:     "concept missing mapping",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"}}, []syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}, nil),
			concepts: map[string]string{"article-a": "missing"},
			want:     conceptDetailEntityMappingConceptMissingMapping,
		},
		{
			name:     "concept ID has no matching path",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"}}, []syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}, nil),
			concepts: map[string]string{"article-a": "beta"},
			want:     conceptDetailEntityMappingConceptMissingMapping,
		},
		{
			name:     "concept is missing id and slug from INDEX",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"}}, []syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}, nil),
			concepts: map[string]string{"missing-id": "missing"},
			want:     conceptDetailEntityMappingConceptMissingMapping,
		},
		{
			name:     "concept entity collision",
			index:    syntoIndexTruthForEntityMapping([]syntoIndexEntry{{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"}}, []syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}}, nil),
			concepts: map[string]string{"article-a": "alpha", "article-b": "alpha"},
			want:     conceptDetailEntityMappingConceptEntityCollision,
		},
	}
	for _, tc := range conceptBranches {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mapSyntoEntityIDsFromIndexTruth(tc.index, tc.concepts); err == nil {
				t.Fatalf("index variant accepted for %q", tc.name)
			} else {
				testEntityMappingErrorDetail(t, err, tc.want)
			}
		})
	}
}

func TestSyntoEntityFirstMappingPreservesRenameAndFailsClosed(t *testing.T) {
	prior := []conceptSnapshot{{ConceptID: "stable-alpha", Slug: "alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABC"}}
	out, _, err := reconcileConceptIDMapWithEntities([]byte(`{"concept":{"generated":"renamed-alpha"},"concept_entity_id":{"generated":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"},"source":{},"redirects":{}}`), prior, true)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := wikiindex.DecodeIDMap(out)
	if err != nil {
		t.Fatal(err)
	}
	if ids.Concept["stable-alpha"] != "renamed-alpha" || ids.ConceptEntityID["stable-alpha"] != "01JAZ5N7Y3K8M2Q4R6T9VWXABC" {
		t.Fatalf("rename mapping=%s", out)
	}

	cases := map[string]string{
		"collision":       `{"concept":{"a":"one","b":"two"},"concept_entity_id":{"a":"same","b":"same"},"source":{},"redirects":{}}`,
		"missing mapping": `{"concept":{"a":"renamed"},"source":{},"redirects":{}}`,
		"changed entity":  `{"concept":{"a":"alpha"},"concept_entity_id":{"a":"01JAZ5N7Y3K8M2Q4R6T9VWXABF"},"source":{},"redirects":{}}`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := reconcileConceptIDMapWithEntities([]byte(data), prior, true); err == nil {
				t.Fatal("ambiguous entity mapping accepted")
			}
		})
	}
}

func TestSyntoOmittedArticleAmbiguityRequiresPriorOwnedSourceEvidence(t *testing.T) {
	prior := []conceptSnapshot{{ConceptID: "stable-alpha", Slug: "alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePaths: []string{"raw/alpha.md", "raw/beta.md"}}}
	newIndex := func(edges []syntoSourceConcept) syntoIndexTruth {
		return syntoIndexTruthForEntityMapping(
			[]syntoIndexEntry{
				{ID: "generated-new", Name: "Shared Name", Path: "articles/alpha.md"},
				{ID: "generated-other", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", Name: "Other", Path: "articles/other.md"},
			},
			edges,
			nil,
		)
	}
	baseEdges := []syntoSourceConcept{
		{Name: "Shared Name", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePath: "raw/alpha.md"},
		{Name: "Shared Name", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePath: "raw/beta.md"},
		{Name: "Shared Name", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", SourcePath: "raw/other.md"},
		{Name: "Other", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", SourcePath: "raw/other.md"},
	}

	t.Run("one prior-owned match selects prior entity", func(t *testing.T) {
		got, err := mapSyntoEntityIDsFromIndexTruth(newIndex(baseEdges), map[string]string{"generated-new": "alpha", "generated-other": "other"}, prior)
		if err == nil || got["generated-new"] != "" {
			t.Fatalf("omitted article identity was inferred: map=%#v error=%v", got, err)
		}
	})

	t.Run("duplicate prior-owned support selects prior entity", func(t *testing.T) {
		edges := append(append([]syntoSourceConcept(nil), baseEdges...), syntoSourceConcept{Name: "Shared Name", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePath: "raw/alpha.md"})
		got, err := mapSyntoEntityIDsFromIndexTruth(newIndex(edges), map[string]string{"generated-new": "alpha", "generated-other": "other"}, prior)
		if err == nil || got["generated-new"] != "" {
			t.Fatalf("omitted article identity was inferred: map=%#v error=%v", got, err)
		}
	})

	for _, tc := range []struct {
		name  string
		edges []syntoSourceConcept
	}{
		{name: "zero prior-owned matches", edges: []syntoSourceConcept{
			{Name: "Shared Name", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", SourcePath: "raw/other.md"},
			{Name: "Shared Name", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC4", SourcePath: "raw/third.md"},
			{Name: "Other", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", SourcePath: "raw/other.md"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mapSyntoEntityIDsFromIndexTruth(newIndex(tc.edges), map[string]string{"generated-new": "alpha", "generated-other": "other"}, prior); err == nil {
				t.Fatal("omitted article was accepted without prior-owned source evidence")
			} else {
				testEntityMappingErrorDetail(t, err, conceptDetailEntityMappingActiveEntityUnknown)
			}
		})
	}
}

func TestSyntoOmittedArticlePriorRecoveryIgnoresCurrentArticleTitle(t *testing.T) {
	prior := []conceptSnapshot{{ConceptID: "stable-alpha", Slug: "alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePaths: []string{"raw/alpha.md"}}}
	index := syntoIndexTruthForEntityMapping(
		[]syntoIndexEntry{
			{ID: "generated-alpha", Name: "Shared", Path: "articles/alpha.md"},
			{ID: "generated-other", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", Name: "Other", Path: "articles/other.md"},
			{ID: "generated-third", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC4", Name: "Third", Path: "articles/third.md"},
		},
		[]syntoSourceConcept{
			{Name: "Old Concept", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePath: "raw/alpha.md"},
			{Name: "Shared", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", SourcePath: "raw/other.md"},
			{Name: "Shared", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC4", SourcePath: "raw/third.md"},
		}, nil,
	)

	got, err := mapSyntoEntityIDsFromIndexTruth(index, map[string]string{"generated-alpha": "alpha", "generated-other": "other", "generated-third": "third"}, prior)
	if err == nil || got["generated-alpha"] != "" {
		t.Fatalf("prior/source evidence populated omitted article: map=%#v error=%v", got, err)
	}
}

func TestSyntoArticleEntityMustMatchCurrentTitleCandidates(t *testing.T) {
	articles := []syntoIndexEntry{
		{ID: "generated-alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC4", Name: "Shared", Path: "articles/alpha.md"},
		{ID: "generated-old", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", Name: "Old", Path: "articles/old.md"},
		{ID: "generated-other", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", Name: "Other", Path: "articles/other.md"},
	}
	baseEdges := []syntoSourceConcept{
		{Name: "Shared", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePath: "raw/shared.md"},
		{Name: "Shared", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", SourcePath: "raw/shared.md"},
		{Name: "Old", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePath: "raw/old.md"},
		{Name: "Other", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", SourcePath: "raw/other.md"},
	}
	index := syntoIndexTruthForEntityMapping(articles, baseEdges, nil)

	got, err := mapSyntoEntityIDsFromIndexTruth(index, map[string]string{
		"generated-alpha": "alpha",
		"generated-old":   "old",
		"generated-other": "other",
	}, nil)
	if err != nil {
		t.Fatalf("direct entity outside ambiguous title candidates was rejected: %v", err)
	}
	if got["generated-alpha"] != "01JAZ5N7Y3K8M2Q4R6T9VWXAC4" {
		t.Fatalf("generated-alpha mapped to %q", got["generated-alpha"])
	}

	omitted := syntoIndexTruthForEntityMapping(
		[]syntoIndexEntry{
			{ID: "generated-alpha", Name: "Shared", Path: "articles/alpha.md"},
			{ID: "generated-old", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", Name: "Old", Path: "articles/old.md"},
			{ID: "generated-other", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", Name: "Other", Path: "articles/other.md"},
		},
		baseEdges, nil,
	)
	prior := []conceptSnapshot{{ConceptID: "stable-alpha", Slug: "alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePaths: []string{"raw/alpha.md"}}}
	got, err = mapSyntoEntityIDsFromIndexTruth(omitted, map[string]string{
		"generated-alpha": "alpha",
		"generated-old":   "old",
		"generated-other": "other",
	}, prior)
	if err != nil || got["generated-alpha"] != "" {
		t.Fatalf("omitted prior mapping populated identity: map=%#v error=%v", got, err)
	}
}

func TestSyntoOmittedArticleIdentityRejectsUnprovenSourceOwnership(t *testing.T) {
	prior := []conceptSnapshot{{ConceptID: "stable-alpha", Slug: "alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePaths: []string{"raw/alpha.md"}}}
	tests := []struct {
		name  string
		index syntoIndexTruth
		want  conceptReconcileDetailCode
	}{
		{
			name: "wrong current source path",
			index: syntoIndexTruthForEntityMapping(
				[]syntoIndexEntry{{ID: "generated-alpha", Name: "Alpha", Path: "articles/alpha.md"}},
				[]syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", SourcePath: "raw/current.md"}}, nil),
			want: conceptDetailEntityMappingActiveEntityUnknown,
		},
		{
			name: "same path wrong entity",
			index: syntoIndexTruthForEntityMapping(
				[]syntoIndexEntry{{ID: "generated-alpha", Name: "Alpha", Path: "articles/alpha.md"}},
				[]syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", SourcePath: "raw/alpha.md"}}, nil),
			want: conceptDetailEntityMappingActiveEntityUnknown,
		},
		{
			name: "ordinary unknown article",
			index: syntoIndexTruthForEntityMapping(
				[]syntoIndexEntry{{ID: "generated-beta", Name: "Beta", Path: "articles/beta.md"}},
				[]syntoSourceConcept{{Name: "Other", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", SourcePath: "raw/other.md"}}, nil),
			want: conceptDetailEntityMappingActiveEntityUnknown,
		},
		{
			name: "direct entity disagreement",
			index: syntoIndexTruthForEntityMapping(
				[]syntoIndexEntry{{ID: "generated-alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE", Name: "Alpha", Path: "articles/alpha.md"}},
				[]syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABF", SourcePath: "raw/alpha.md"}}, nil),
			want: conceptDetailEntityMappingActiveEntityUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := mapSyntoEntityIDsFromIndexTruth(tc.index, map[string]string{tc.index.Articles[0].ID: "alpha"}, prior); err == nil {
				t.Fatal("unproven omitted article identity was accepted")
			} else {
				testEntityMappingErrorDetail(t, err, tc.want)
			}
		})
	}
}

func TestSyntoIdentityLogRejectsMergeAndSplitLineage(t *testing.T) {
	for _, op := range []string{"merge", "split"} {
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"generated":"alpha"},"concept_entity_id":{"generated":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"},"source":{},"redirects":{}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: generated\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"generated"}}`+"\n"))
		index := strings.Replace(syntoIndexFixture("generated", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", true), `"terms":[]`, `"identity_log":[{"op":"`+op+`"}],"terms":[]`, 1)
		mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(index))
		if err := reconcileWorkspaceConcepts(workspace, nil); err == nil {
			t.Fatalf("%s lineage accepted", op)
		}
	}
}

func TestMapSyntoEntityIDsFromIndexTruthConceptIterationIsDeterministic(t *testing.T) {
	index := syntoIndexTruthForEntityMapping(
		[]syntoIndexEntry{
			{ID: "article-a", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", Name: "Alpha", Path: "wiki/alpha.md"},
			{ID: "article-b", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1", Name: "Beta", Path: "wiki/beta.md"},
		},
		[]syntoSourceConcept{{Name: "Alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0"}, {Name: "Beta", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1"}},
		nil,
	)
	buildConcepts := func(order []string) map[string]string {
		seed := make(map[string]string)
		for _, entry := range order {
			switch entry {
			case "a":
				seed["article-a"] = "ALPHA"
			case "b":
				seed["article-b"] = "alpha"
			}
		}
		return seed
	}
	orders := [][]string{
		{"a", "b"},
		{"b", "a"},
	}
	for i := 0; i < 16; i++ {
		for _, order := range orders {
			_, err := mapSyntoEntityIDsFromIndexTruth(index, buildConcepts(order))
			if err == nil {
				t.Fatalf("conflicting concepts unexpectedly accepted")
			}
			detail := testEntityMappingErrorDetailCode(t, err)
			if detail != conceptDetailEntityMappingConceptSlugCase {
				t.Fatalf("non-deterministic concept reason=%q (order=%v run=%d)", detail, order, i)
			}
		}
	}
}

func TestExecSyntoUsesAllowlistedEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture uses POSIX executable semantics")
	}
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "env.txt")
	script := filepath.Join(bin, "synto")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv | sort > "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("SYNTO_TEST_ENV", record)
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-cross-boundary")
	if err := execOLWCommand(context.Background(), t.TempDir(), []string{"run", "--auto-approve"}, []string{"XDG_CONFIG_HOME=/tmp/isolated-synto-config", "SYNTO_API_KEY=fake"}, nil, nil); err != nil {
		t.Fatalf("synto command execution error = %v", err)
	}
	env, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := string(env)
	if strings.Contains(got, "AWS_SECRET_ACCESS_KEY=") || strings.Contains(got, "SYNTO_TEST_ENV=") {
		t.Fatalf("child inherited non-allowlisted environment: %s", got)
	}
	for _, want := range []string{"PATH=", "XDG_CONFIG_HOME=/tmp/isolated-synto-config", "SYNTO_API_KEY=fake"} {
		if !strings.Contains(got, want) {
			t.Fatalf("child environment missing %q: %s", want, got)
		}
	}
}

func TestCreateWorkspaceCopiesSyntoMigrationState(t *testing.T) {
	vault := t.TempDir()
	workspaceParent := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte("raw"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "concept.md"), []byte("wiki"))
	mustWriteFile(t, filepath.Join(vault, "wiki.toml"), []byte("legacy config"))
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("synto config"))
	mustWriteFile(t, filepath.Join(vault, ".olw", "state.db"), []byte("legacy state"))
	mustWriteFile(t, filepath.Join(vault, ".synto", "state.db"), []byte("synto state"))

	workspace, err := createWorkspace(workspaceParent, vault)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	for _, rel := range []string{"raw/source.md", "wiki/concept.md", "wiki.toml", "synto.toml", ".olw/state.db", ".synto/state.db"} {
		if _, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(rel))); err != nil {
			t.Errorf("workspace missing %s: %v", rel, err)
		}
	}
}

func TestGenerationOwnsSyntoStateAndNotSyntoExports(t *testing.T) {
	for _, path := range []string{"synto.toml", ".synto/state.db", ".synto/INDEX.json"} {
		if !generation.GenerationOwned(path) {
			t.Errorf("GenerationOwned(%q) = false", path)
		}
	}
	for _, path := range []string{".synto/exports/agents/INDEX.json", ".synto/pipeline.lock"} {
		if generation.GenerationOwned(path) {
			t.Errorf("GenerationOwned(%q) = true", path)
		}
	}
}

func TestStageWorkspaceOutputsIncludesSyntoState(t *testing.T) {
	workspace := t.TempDir()
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "wiki", "concept.md"), []byte("wiki"))
	mustWriteFile(t, filepath.Join(workspace, "synto.toml"), []byte("synto config"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "state.db"), []byte("synto state"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("article", "01JAZ5N7Y3K8M2Q4R6T9VWXAC8", "alpha", false)))
	stage, err := stageWorkspaceOutputs(workspace, vault, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(vault, stage)) })
	for _, rel := range []string{"synto.toml", ".synto/state.db", ".synto/INDEX.json"} {
		if _, err := os.Stat(filepath.Join(vault, stage, filepath.FromSlash(rel))); err != nil {
			t.Errorf("stage missing %s: %v", rel, err)
		}
	}
	original, err := os.ReadFile(filepath.Join(workspace, ".synto", "INDEX.json"))
	if err != nil {
		t.Fatal(err)
	}
	staged, err := os.ReadFile(filepath.Join(vault, stage, ".synto", "INDEX.json"))
	if err != nil || string(staged) != string(original) {
		t.Fatalf("INDEX.json changed during staging: %q err=%v", staged, err)
	}
}

func TestSyntoCommandBoundaryDoesNotAddForce(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	var got []string
	execOLW = func(_ context.Context, _ string, command []string, _ []string, _, _ io.Writer) error {
		got = append([]string(nil), command...)
		return nil
	}
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
	if err := runOLWBatch(context.Background(), vault, [][]string{{"run", "--auto-approve"}}, true, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "run --auto-approve" {
		t.Fatalf("synto command = %v", got)
	}
}

func TestSyntoLifecycleDormantsZeroSourceAndReactivatesSameID(t *testing.T) {
	workspace := t.TempDir()
	priorPage := []byte("---\nid: stable-alpha\naliases:\n  - old-name\n---\nannotated history\n")
	priorRow := []byte(`{"slug":"alpha","frontmatter":{"id":"stable-alpha","aliases":["old-name"]}}`)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), priorPage)
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha"},"concept_entity_id":{"stable-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), priorRow)
	prior, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatal(err)
	}

	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: e5a1b2c3d4e5\n---\nnew empty output\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"e5a1b2c3d4e5":"alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"e5a1b2c3d4e5"}}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("e5a1b2c3d4e5", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", false)))
	if err := reconcileWorkspaceConcepts(workspace, prior); err != nil {
		t.Fatalf("dormant reconcile: %v", err)
	}
	assertLifecycleState(t, workspace, true)
	dormantPrior := mustSnapshotConcepts(t, workspace)

	// The next Synto generation publishes the same entity again with a source;
	// the prior dormant mapping must reactivate the original LWC ID.
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: e5a1b2c3d4e5\n---\nreactivated\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"e5a1b2c3d4e5":"alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","sources":["stable-source"],"frontmatter":{"id":"e5a1b2c3d4e5"}}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("e5a1b2c3d4e5", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", true)))
	if err := reconcileWorkspaceConcepts(workspace, dormantPrior, []sourceSnapshot{{RawPath: "raw/source.md", SyntoContentHash: strings.Repeat("0", 64)}}); err != nil {
		t.Fatalf("reactivation reconcile: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ids generationIDMapFixture
	if err := json.Unmarshal(data, &ids); err != nil {
		t.Fatal(err)
	}
	if ids.Concept["stable-alpha"] != "alpha" || len(ids.DormantConcept) != 0 || ids.ConceptEntityID["stable-alpha"] != "01JAZ5N7Y3K8M2Q4R6T9VWXABC" {
		t.Fatalf("reactivated map = %s", data)
	}
	if _, err := os.Stat(filepath.Join(workspace, "wiki", "alpha.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "wiki", ".dormant", "alpha.md")); !os.IsNotExist(err) {
		t.Fatalf("dormant page remains: %v", err)
	}
}

func TestPostprocessPreservesDormantLineageForEntityAwareReactivation(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: stable-alpha\n---\nAlpha"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha"},"dormant_concept":{"stable-beta":"beta"},"concept_entity_id":{"stable-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","stable-beta":"01JAZ5N7Y3K8M2Q4R6T9VWXABD"},"source":{},"redirects":{}}`))
	if err := runPostprocess(context.Background(), workspace); err != nil {
		t.Fatalf("postprocess: %v", err)
	}
	prior, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatalf("snapshot after postprocess: %v", err)
	}

	// Synto now emits the dormant entity under a new generated page ID. The
	// 01JAZ5N7Y3K8M2Q4R6T9VWXAC0ware reconciler, not postprocess, must restore stable-beta.
	mustWriteFile(t, filepath.Join(workspace, "wiki", "beta.md"), []byte("---\nid: transient-beta\n---\nBeta reactivated"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha","transient-beta":"beta"},"concept_entity_id":{"stable-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","transient-beta":"01JAZ5N7Y3K8M2Q4R6T9VWXABD"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte("{\"slug\":\"alpha\"}\n{\"slug\":\"beta\",\"sources\":[\"raw/source.md\"]}\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntities([]string{"stable-alpha:01JAZ5N7Y3K8M2Q4R6T9VWXABC:alpha", "transient-beta:01JAZ5N7Y3K8M2Q4R6T9VWXABD:beta"}, []string{"01JAZ5N7Y3K8M2Q4R6T9VWXABD"})))
	if err := reconcileWorkspaceConcepts(workspace, prior, []sourceSnapshot{{RawPath: "raw/source.md", SyntoContentHash: strings.Repeat("0", 64)}}); err != nil {
		t.Fatalf("reactivation reconcile: %v", err)
	}
	ids := mustSnapshotIDMap(t, workspace)
	if ids.Concept["stable-beta"] != "beta" || ids.DormantConcept["stable-beta"] != "" || ids.ConceptEntityID["stable-beta"] != "01JAZ5N7Y3K8M2Q4R6T9VWXABD" {
		t.Fatalf("reactivated identity map = %#v", ids)
	}
	if _, ok := ids.Concept["transient-beta"]; ok {
		t.Fatalf("transient reactivation ID remains active: %#v", ids.Concept)
	}
}

func TestSyntoProductionLifecycleUsesAuthoritativeEmptyAndTombstoneSourceSets(t *testing.T) {
	for _, tc := range []struct {
		name          string
		sourceMeta    string
		raw           string
		index         string
		wantDormant   bool
		wantActiveRow bool
	}{
		{
			name:        "explicitly empty source artifact",
			index:       syntoIndexFixture("transient-alpha", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", true),
			wantDormant: true,
		},
		{
			name:        "tombstone-only source artifact",
			sourceMeta:  `,"source":{"s1":"source"},"source_meta":{"s1":{"slug":"source","source_file":"raw/source.md"}}`,
			index:       syntoIndexFixture("transient-alpha", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", true),
			wantDormant: true,
		},
		{
			name:          "matching non-empty source artifact",
			sourceMeta:    `,"source":{"s1":"source"},"source_meta":{"s1":{"slug":"source","source_file":"raw/source.md"}}`,
			raw:           "current source",
			index:         syntoIndexFixtureWithEntitiesHash([]string{"transient-alpha:01JAZ5N7Y3K8M2Q4R6T9VWXABC:alpha"}, []string{"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}, sha256Text("current source")),
			wantActiveRow: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := execOLW
			defer func() { execOLW = old }()
			vault := t.TempDir()
			mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
			mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: a3f7b2c01d9d\n---\nHuman annotation and historical body\n"))
			mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"a3f7b2c01d9d":"alpha"},"concept_entity_id":{"a3f7b2c01d9d":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}`+tc.sourceMeta+`,"redirects":{}}`))
			mustWriteFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"a3f7b2c01d9d"}}`+"\n"))
			if tc.raw != "" {
				mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte(tc.raw))
			}
			execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
				if strings.Join(command, " ") != "run --auto-approve" {
					return fmt.Errorf("unexpected command %v", command)
				}
				mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: e5a1b2c3d4e5\n---\nnew generated body\n"))
				mustWriteFile(t, filepath.Join(work, "cache", "id_map.json"), []byte(`{"concept":{"e5a1b2c3d4e5":"alpha"},"concept_entity_id":{"e5a1b2c3d4e5":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"},"source":{},"redirects":{}}`))
				mustWriteFile(t, filepath.Join(work, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"e5a1b2c3d4e5"}}`+"\n"))
				mustWriteFile(t, filepath.Join(work, "cache", "raw_status.json"), []byte("{}"))
				mustWriteFile(t, filepath.Join(work, "cache", "suggested_queries.json"), []byte("{}"))
				mustWriteFile(t, filepath.Join(work, ".synto", "INDEX.json"), []byte(tc.index))
				writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
				return nil
			}
			cfg := workerConfig{VaultPath: vault, APIKey: "fake", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true, StopOnError: true}
			if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
				t.Fatalf("production lifecycle run: %v", err)
			}
			ids := mustSnapshotIDMap(t, vault)
			if ids.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXABC"] != "alpha" || len(ids.Concept) != 1 || len(ids.ConceptEntityID) != 0 {
				t.Fatalf("direct entity identity changed with source set: %#v", ids)
			}
		})
	}
}

func TestSyntoLifecycleFailsClosedWhenCurrentSourceTruthIsUnavailable(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: stable-alpha\n---\nhistory\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha"},"concept_entity_id":{"stable-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha"}`+"\n"))
	prior, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: e5a1b2c3d4e5\n---\nnew\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-alpha":"alpha"},"concept_entity_id":{"transient-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha"}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("transient-alpha", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", true)))

	// No current-source argument means truth is unavailable; stale INDEX edges
	// must not keep the prior Concept active.
	if err := reconcileWorkspaceConcepts(workspace, prior); err != nil {
		t.Fatalf("fail-closed reconcile: %v", err)
	}
	ids := mustSnapshotIDMap(t, workspace)
	if ids.DormantConcept["stable-alpha"] != "alpha" || len(ids.Concept) != 0 {
		t.Fatalf("unavailable source truth kept Concept active: %#v", ids)
	}
}

func TestSyntoSourceEdgesDormantUsesAuthoritativeIndexNotStaleArticleCache(t *testing.T) {
	workspace := t.TempDir()
	for slug, page := range map[string]string{
		"alpha": "---\nid: stable-alpha\n---\nAlpha annotation\n",
		"beta":  "---\nid: stable-beta\n---\nBeta\n",
	} {
		mustWriteFile(t, filepath.Join(workspace, "wiki", slug+".md"), []byte(page))
	}
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha","stable-beta":"beta"},"concept_entity_id":{"stable-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","stable-beta":"01JAZ5N7Y3K8M2Q4R6T9VWXABD"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte("{\"slug\":\"alpha\",\"sources\":[\"raw/old.md\"]}\n{\"slug\":\"beta\",\"sources\":[\"raw/old.md\"]}\n"))
	prior, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: e5a1b2c3d4e5\n---\nnew stale article\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "beta.md"), []byte("---\nid: transient-beta\n---\nBeta\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "gamma.md"), []byte("---\nid: transient-gamma\n---\nGamma\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-alpha":"alpha","transient-beta":"beta","transient-gamma":"gamma"},"concept_entity_id":{"transient-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","transient-beta":"01JAZ5N7Y3K8M2Q4R6T9VWXABD","transient-gamma":"01JAZ5N7Y3K8M2Q4R6T9VWXAC5"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte("{\"slug\":\"alpha\",\"sources\":[\"raw/old.md\"]}\n{\"slug\":\"beta\",\"sources\":[\"raw/new.md\"]}\n{\"slug\":\"gamma\",\"sources\":[\"raw/new.md\"]}\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntities([]string{"transient-alpha:01JAZ5N7Y3K8M2Q4R6T9VWXABC:alpha", "transient-beta:01JAZ5N7Y3K8M2Q4R6T9VWXABD:beta", "transient-gamma:01JAZ5N7Y3K8M2Q4R6T9VWXAC5:gamma"}, []string{"01JAZ5N7Y3K8M2Q4R6T9VWXABD", "01JAZ5N7Y3K8M2Q4R6T9VWXAC5"})))
	if err := reconcileWorkspaceConcepts(workspace, prior, []sourceSnapshot{{RawPath: "raw/source.md", SyntoContentHash: strings.Repeat("0", 64)}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ids generationIDMapFixture
	if err := json.Unmarshal(data, &ids); err != nil {
		t.Fatal(err)
	}
	if len(ids.Concept) != 2 || ids.Concept["stable-beta"] != "beta" || ids.Concept["transient-gamma"] != "gamma" || ids.DormantConcept["stable-alpha"] != "alpha" {
		t.Fatalf("lifecycle map=%s", data)
	}
	if _, err := os.Stat(filepath.Join(workspace, "wiki", "alpha.md")); !os.IsNotExist(err) {
		t.Fatalf("stale alpha article remained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "wiki", ".dormant", "alpha.md")); err != nil {
		t.Fatalf("dormant alpha missing: %v", err)
	}
}

func TestSyntoSourceEdgesIntersectCurrentMaterializedHashAndTombstones(t *testing.T) {
	newCase := func(t *testing.T, sources []sourceSnapshot, groups string, wantActive bool) {
		t.Helper()
		workspace := t.TempDir()
		for _, slug := range []string{"alpha", "beta", "gamma"} {
			mustWriteFile(t, filepath.Join(workspace, "wiki", slug+".md"), []byte("---\nid: transient-"+slug+"\n---\n"+slug+"\n"))
		}
		mustWriteFile(t, filepath.Join(workspace, "raw", "beta.md"), []byte("beta current"))
		mustWriteFile(t, filepath.Join(workspace, "raw", "gamma.md"), []byte("gamma current"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-alpha":"alpha","transient-beta":"beta","transient-gamma":"gamma"},"concept_entity_id":{"transient-alpha":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","transient-beta":"01JAZ5N7Y3K8M2Q4R6T9VWXABD","transient-gamma":"01JAZ5N7Y3K8M2Q4R6T9VWXAC5"},"source":{},"redirects":{}}`))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte("{\"slug\":\"alpha\"}\n{\"slug\":\"beta\"}\n{\"slug\":\"gamma\"}\n"))
		mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithSourceGroups([]string{"transient-alpha:01JAZ5N7Y3K8M2Q4R6T9VWXABC:alpha", "transient-beta:01JAZ5N7Y3K8M2Q4R6T9VWXABD:beta", "transient-gamma:01JAZ5N7Y3K8M2Q4R6T9VWXAC5:gamma"}, groups)))
		prior := []conceptSnapshot{
			{ConceptID: "stable-alpha", Slug: "alpha", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABC", Page: []byte("---\nid: stable-alpha\n---\nhistory\n")},
			{ConceptID: "stable-beta", Slug: "beta", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABD", Page: []byte("---\nid: stable-beta\n---\nhistory\n")},
		}
		if err := reconcileWorkspaceConcepts(workspace, prior, sources); err != nil {
			t.Fatal(err)
		}
		ids := mustSnapshotIDMap(t, workspace)
		if ids.DormantConcept["stable-alpha"] != "alpha" || wantActive && (ids.Concept["stable-beta"] != "beta" || ids.Concept["transient-gamma"] != "gamma") {
			t.Fatalf("source/hash intersection lifecycle = %#v", ids)
		}
	}

	alphaOld := sha256Text("alpha old")
	betaCurrent := sha256Text("beta current")
	gammaCurrent := sha256Text("gamma current")
	t.Run("changed source set", func(t *testing.T) {
		newCase(t, []sourceSnapshot{
			{RawPath: "raw/beta.md", RawBytes: []byte("beta current"), SyntoContentHash: betaCurrent},
			{RawPath: "raw/gamma.md", RawBytes: []byte("gamma current"), SyntoContentHash: gammaCurrent},
		}, `[{"source_path":"raw/alpha.md","content_hash":"`+alphaOld+`","concepts":[{"name":"Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}]},{"source_path":"raw/beta.md","content_hash":"`+betaCurrent+`","concepts":[{"name":"Beta","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABD"}]},{"source_path":"raw/gamma.md","content_hash":"`+gammaCurrent+`","concepts":[{"name":"Gamma","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXAC5"}]}]`, true)
	})
	t.Run("removed source tombstone", func(t *testing.T) {
		newCase(t, []sourceSnapshot{{RawPath: "raw/alpha.md", Tombstone: true}}, `[{"source_path":"raw/alpha.md","content_hash":"`+alphaOld+`","concepts":[{"name":"Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}]}]`, false)
	})
}

func syntoIndexFixtureWithEntities(articles, active []string) string {
	return syntoIndexFixtureWithEntitiesHash(articles, active, strings.Repeat("0", 64))
}

func mustSnapshotIDMap(t *testing.T, workspace string) wikiindex.IDMap {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func syntoIndexFixtureWithSourceGroups(articles []string, groups string) string {
	articleJSON := make([]string, 0, len(articles))
	for _, item := range articles {
		parts := strings.Split(item, ":")
		articleJSON = append(articleJSON, `{"id":"`+parts[0]+`","entity_id":"`+parts[1]+`","name":"`+parts[2]+`","path":"wiki/`+parts[2]+`.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`)
	}
	return `{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[` + strings.Join(articleJSON, ",") + `],"terms":[],"papers":[],"sources":[],"source_concepts":` + groups + `,"synthesis":[],"stats":{"article_count":3,"draft_count":0,"concept_count":3,"alias_count":0,"knowledge_item_count":0,"source_count":3,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`
}

func TestWorkerDecodeSyntoIndexRejectsDuplicateSourceConceptGroupsAndRows(t *testing.T) {
	tests := []struct {
		name   string
		groups string
	}{
		{
			name:   "duplicate source path",
			groups: `[{"source_path":"raw/source.md","content_hash":"` + strings.Repeat("0", 64) + `","concepts":[]},{"source_path":"raw/source.md","content_hash":"` + strings.Repeat("1", 64) + `","concepts":[]}]`,
		},
		{
			name:   "duplicate source concept row",
			groups: `[{"source_path":"raw/source.md","content_hash":"` + strings.Repeat("0", 64) + `","concepts":[{"name":"Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"},{"name":"Alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}]}]`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := syntoIndexFixtureWithSourceGroups(nil, test.groups)
			if _, err := decodeSyntoIndex([]byte(data)); err == nil {
				t.Fatal("worker decoder accepted duplicate source concept data")
			}
		})
	}
}

func TestSyntoIdentityPlanValidatesReservedRootEntityUniquenessBeforeExclusion(t *testing.T) {
	index := syntoIndexTruth{
		Articles: []syntoIndexEntry{
			{ID: "article-root", Name: "Index", Path: "wiki/index.md", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABC"},
			{ID: "article-alpha", Name: "Alpha", Path: "wiki/alpha.md", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABC"},
		},
	}
	if _, err := syntoIdentityPlanFromIndex(index); err == nil {
		t.Fatal("reserved root duplicate entity identity was accepted")
	}
}

func TestSyntoIdentityPlanFromIndexDoesNotVetoExplicitEntityBySourceConceptName(t *testing.T) {
	const explicitEntity = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	cases := []struct {
		name     string
		sources  []syntoSourceConcept
		wantSize int
	}{
		{
			name: "single conflicting source concept",
			sources: []syntoSourceConcept{
				{Name: "Ordinary", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", SourcePath: "raw/source.md", ContentHash: strings.Repeat("0", 64)},
			},
			wantSize: 1,
		},
		{
			name: "ambiguous source concepts",
			sources: []syntoSourceConcept{
				{Name: "Ordinary", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC0", SourcePath: "raw/source.md", ContentHash: strings.Repeat("0", 64)},
				{Name: "Ordinary", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXAC1", SourcePath: "raw/source.md", ContentHash: strings.Repeat("1", 64)},
			},
			wantSize: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := syntoIndexTruth{
				Articles: []syntoIndexEntry{
					{ID: "article-ordinary", Name: "Ordinary", Path: "wiki/ordinary.md", EntityID: explicitEntity},
				},
				SourceConcepts: tc.sources,
			}
			plan, err := syntoIdentityPlanFromIndex(index)
			if err != nil {
				t.Fatalf("syntoIdentityPlanFromIndex() error = %v", err)
			}
			if got := plan.ByPath["wiki/ordinary.md"]; got != explicitEntity {
				t.Fatalf("ByPath[wiki/ordinary.md] = %q, want %q", got, explicitEntity)
			}
			if len(plan.ActiveEntities) != tc.wantSize {
				t.Fatalf("len(plan.ActiveEntities) = %d, want %d", len(plan.ActiveEntities), tc.wantSize)
			}
		})
	}
}

func TestSyntoIdentityPlanFromIndexEntitylessRowsParticipateInValidation(t *testing.T) {
	type testCase struct {
		name     string
		articles []syntoIndexEntry
		needle   string
	}
	cases := []testCase{
		{
			name: "duplicate id with null and omitted",
			articles: []syntoIndexEntry{
				{ID: "dup", Name: "Duplicate", Path: "wiki/first.md"},
				{ID: "dup", Name: "Duplicate", Path: "wiki/second.md"},
			},
			needle: "duplicate Synto article ID",
		},
		{
			name: "duplicate slug with null and omitted",
			articles: []syntoIndexEntry{
				{ID: "slug-a", Name: "Alpha", Path: "wiki/A.md", EntityID: "01JAZ5N7Y3K8M2Q4R6T9VWXABE"},
				{ID: "slug-b", Name: "Alpha", Path: "wiki/a.md"},
			},
			needle: "duplicate Synto article slug",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := syntoIndexTruth{
				Articles: tc.articles,
			}
			if _, err := syntoIdentityPlanFromIndex(index); err == nil || !strings.Contains(err.Error(), tc.needle) {
				t.Fatalf("error=%v, want substring %q", err, tc.needle)
			}
		})
	}
}

func syntoIndexFixtureWithEntitiesHash(articles, active []string, contentHash string) string {
	articleJSON := make([]string, 0, len(articles))
	for _, item := range articles {
		parts := strings.Split(item, ":")
		articleJSON = append(articleJSON, `{"id":"`+parts[0]+`","entity_id":"`+parts[1]+`","name":"`+parts[2]+`","path":"wiki/`+parts[2]+`.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}`)
	}
	edges := make([]string, 0, len(active))
	for _, entity := range active {
		edges = append(edges, `{"name":"concept","entity_id":"`+entity+`"}`)
	}
	return `{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[` + strings.Join(articleJSON, ",") + `],"terms":[],"papers":[],"sources":[],"source_concepts":[{"source_path":"raw/source.md","content_hash":"` + contentHash + `","concepts":[` + strings.Join(edges, ",") + `]}],"synthesis":[],"stats":{"article_count":3,"draft_count":0,"concept_count":3,"alias_count":0,"knowledge_item_count":0,"source_count":1,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`
}

func TestSyntoWorkerPrivateWorkspacePersistsEntityMapping(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := t.TempDir()
	workspaceDir := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte("raw"))
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"source_meta":{"source-1":{"source_file":"raw/source.md"}}}`))
	gen := 0
	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		if len(command) != 2 || command[0] != "run" || command[1] != "--auto-approve" {
			return fmt.Errorf("unexpected Synto command %v", command)
		}
		gen++
		id := fmt.Sprintf("f%011x", gen)
		mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: "+id+"\nsources:\n  - source-1\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(work, "wiki", "sources", "source.md"), []byte("---\nid: source-1\nsource_file: raw/source.md\n---\nsource\n"))
		mustWriteFile(t, filepath.Join(work, "cache", "id_map.json"), []byte(`{"concept":{"`+id+`":"alpha"},"source":{"source-1":"source"},"source_meta":{"source-1":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))
		mustWriteFile(t, filepath.Join(work, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","sources":["source-1"],"frontmatter":{"id":"`+id+`"}}`+"\n"))
		mustWriteFile(t, filepath.Join(work, "cache", "raw_status.json"), []byte("{}"))
		mustWriteFile(t, filepath.Join(work, "cache", "suggested_queries.json"), []byte("{}"))
		mustWriteFile(t, filepath.Join(work, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntitiesHash([]string{id + ":01JAZ5N7Y3K8M2Q4R6T9VWXABC:alpha"}, []string{"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}, sha256Text("raw"))))
		writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		return nil
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "fake", Workspace: true, WorkspaceDir: workspaceDir, Postprocess: true, StopOnError: true}
	for i := 0; i < 2; i++ {
		if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
			t.Fatalf("private workspace run %d: %v", i+1, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(vault, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ids generationIDMapFixture
	if err := json.Unmarshal(data, &ids); err != nil {
		t.Fatal(err)
	}
	if ids.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXABC"] != "alpha" || len(ids.ConceptEntityID) != 0 {
		t.Fatalf("worker did not emit direct canonical mapping: %s", data)
	}
	if _, err := os.Stat(filepath.Join(vault, "synto.toml")); err != nil {
		t.Fatalf("private workspace did not publish synto.toml: %v", err)
	}
}

func TestSyntoWorkerDirectEntityPathExcludesEntitylessPageWithoutChangingBytes(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := t.TempDir()
	workspaceDir := t.TempDir()
	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		if strings.Join(command, " ") != "run --auto-approve" {
			return fmt.Errorf("unexpected Synto command %v", command)
		}
		mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: e5a1b2c3d4e5\n---\nalpha generated\n"))
		mustWriteFile(t, filepath.Join(work, "wiki", "ordinary.md"), []byte("---\nid: transient-ordinary\n---\nordinary generated\n"))
		mustWriteFile(t, filepath.Join(work, "cache", "id_map.json"), []byte(`{"concept":{"e5a1b2c3d4e5":"alpha","transient-ordinary":"ordinary"},"source":{},"redirects":{}}`))
		mustWriteFile(t, filepath.Join(work, "cache", "concepts.jsonl"), []byte(""))
		mustWriteFile(t, filepath.Join(work, "cache", "raw_status.json"), []byte("{}"))
		mustWriteFile(t, filepath.Join(work, "cache", "suggested_queries.json"), []byte("{}"))
		mustWriteFile(t, filepath.Join(work, ".synto", "INDEX.json"), []byte(`{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[{"id":"article","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","name":"alpha","path":"wiki/alpha.md","summary":null,"tags":[],"aliases":[],"confidence":"high"},{"id":"ordinary","entity_id":null,"name":"ordinary","path":"wiki/ordinary.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":[],"synthesis":[],"stats":{"article_count":2,"draft_count":0,"concept_count":2,"alias_count":0,"knowledge_item_count":0,"source_count":0,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`))
		writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		return nil
	}
	ordinary := []byte("---\nid: transient-ordinary\n---\nordinary generated\n")
	cfg := workerConfig{VaultPath: vault, APIKey: "fake", Workspace: true, WorkspaceDir: workspaceDir, Postprocess: true, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(vault, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		t.Fatal(err)
	}
	if ids.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXABC"] != "alpha" || len(ids.Concept) != 1 || len(ids.ConceptEntityID) != 0 {
		t.Fatalf("direct Synto map = %#v", ids)
	}
	page, err := os.ReadFile(filepath.Join(vault, "wiki", "ordinary.md"))
	if err != nil || !bytes.Equal(page, ordinary) {
		t.Fatalf("entity-less page bytes=%q err=%v", page, err)
	}
	cache, err := os.ReadFile(filepath.Join(vault, "cache", "concepts.jsonl"))
	if err != nil || strings.Contains(string(cache), "ordinary") || !strings.Contains(string(cache), `"id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"`) {
		t.Fatalf("direct concept cache=%q err=%v", cache, err)
	}
}

func TestReconcileWorkspaceConceptsDirectEntityMigratesOldIDRedirect(t *testing.T) {
	workspace := t.TempDir()
	const entityID = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	prior := []conceptSnapshot{{ConceptID: "a3f7b2c01d9d", Slug: "alpha"}}
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"`+entityID+`":"alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: "+entityID+"\n---\nAlpha\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"`+entityID+`"}}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("article-alpha", entityID, "alpha", false)))

	if err := reconcileWorkspaceConcepts(workspace, prior); err != nil {
		t.Fatalf("reconcileWorkspaceConcepts() error = %v", err)
	}
	ids := mustSnapshotIDMap(t, workspace)
	if len(ids.Concept) != 1 || ids.Concept[entityID] != "alpha" {
		t.Fatalf("concept map = %#v, want only %s -> alpha", ids.Concept, entityID)
	}
	if got := ids.IDRedirects["a3f7b2c01d9d"]; got != entityID {
		t.Fatalf("old ID redirect = %q, want %q", got, entityID)
	}
}

func TestSyntoWorkerDirectEntityMigratesOldIDRedirectIdempotently(t *testing.T) {
	old := execOLW
	t.Cleanup(func() { execOLW = old })
	vault := t.TempDir()
	workspaceDir := t.TempDir()
	const entityID = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"a3f7b2c01d9d":"alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: a3f7b2c01d9d\n---\nAlpha\n"))
	mustWriteFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"a3f7b2c01d9d"}}`+"\n"))

	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		if strings.Join(command, " ") != "run --auto-approve" {
			return fmt.Errorf("unexpected Synto command %v", command)
		}
		mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: a1b2c3d4e5f6\n---\nAlpha regenerated\n"))
		mustWriteFile(t, filepath.Join(work, "cache", "id_map.json"), []byte(`{"concept":{"a1b2c3d4e5f6":"alpha"},"source":{},"redirects":{}}`))
		mustWriteFile(t, filepath.Join(work, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"a1b2c3d4e5f6"}}`+"\n"))
		mustWriteFile(t, filepath.Join(work, "cache", "raw_status.json"), []byte("{}"))
		mustWriteFile(t, filepath.Join(work, "cache", "suggested_queries.json"), []byte("{}"))
		mustWriteFile(t, filepath.Join(work, ".synto", "INDEX.json"), []byte(syntoIndexFixture("article-alpha", entityID, "alpha", false)))
		writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		return nil
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "fake", Workspace: true, WorkspaceDir: workspaceDir, Postprocess: true, StopOnError: true}
	var first []byte
	for run := 1; run <= 2; run++ {
		if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
			t.Fatalf("runWorkerBatch() run %d error = %v", run, err)
		}
		data, err := os.ReadFile(filepath.Join(vault, "cache", "id_map.json"))
		if err != nil {
			t.Fatal(err)
		}
		ids, err := wikiindex.DecodeIDMap(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids.Concept) != 1 || ids.Concept[entityID] != "alpha" || len(ids.ConceptEntityID) != 0 {
			t.Fatalf("run %d direct map = %#v", run, ids)
		}
		if got := ids.IDRedirects["a3f7b2c01d9d"]; got != entityID {
			t.Fatalf("run %d old ID redirect = %q, want %q", run, got, entityID)
		}
		if run == 1 {
			first = append([]byte(nil), data...)
		} else if !bytes.Equal(first, data) {
			t.Fatalf("second run changed id_map.json:\nfirst=%s\nsecond=%s", first, data)
		}
	}
}

func TestSyntoDirectRedirectsAllowManyLegacySourcesAndRejectNonLegacyPriorIDs(t *testing.T) {
	const target = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
	generated := wikiindex.IDMap{
		Concept: map[string]string{target: "alpha"},
		IDRedirects: map[string]string{
			"a3f7b2c01d9d": target,
			"b7e2c9a4d113": target,
		},
	}
	if err := planSyntoDirectEntityIDRedirects(&generated, nil); err != nil {
		t.Fatalf("many-to-one redirects rejected: %v", err)
	}
	if len(generated.IDRedirects) != 2 {
		t.Fatalf("redirects=%#v, want both legacy sources", generated.IDRedirects)
	}
	generated = wikiindex.IDMap{Concept: map[string]string{target: "alpha"}}
	if err := planSyntoDirectEntityIDRedirects(&generated, []conceptSnapshot{{ConceptID: "stable-alpha", Slug: "alpha"}}); err == nil {
		t.Fatal("non-legacy prior ID was migrated")
	}
}

func TestReconcileWorkspaceConceptsDirectEntityRejectsInvalidIDRedirectsAtomically(t *testing.T) {
	tests := []struct {
		name       string
		concepts   string
		redirects  string
		wantDetail string
	}{
		{
			name:       "conflicting existing redirect",
			concepts:   `"01JAZ5N7Y3K8M2Q4R6T9VWXABC":"alpha","01JAZ5N7Y3K8M2Q4R6T9VWXABD":"beta"`,
			redirects:  `"a3f7b2c01d9d":"01JAZ5N7Y3K8M2Q4R6T9VWXABD"`,
			wantDetail: "ID redirect conflict",
		},
		{
			name:       "redirect chain",
			concepts:   `"01JAZ5N7Y3K8M2Q4R6T9VWXABC":"alpha"`,
			redirects:  `"a3f7b2c01d9d":"b7e2c9a4d113","b7e2c9a4d113":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"`,
			wantDetail: "unsafe ID redirect target",
		},
		{
			name:       "missing target",
			concepts:   `"01JAZ5N7Y3K8M2Q4R6T9VWXABC":"alpha"`,
			redirects:  `"a3f7b2c01d9d":"missing"`,
			wantDetail: "ID redirect target",
		},
		{
			name:       "current ID source",
			concepts:   `"01JAZ5N7Y3K8M2Q4R6T9VWXABC":"alpha"`,
			redirects:  `"01JAZ5N7Y3K8M2Q4R6T9VWXABC":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"`,
			wantDetail: "active concept",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{`+test.concepts+`},"source":{},"redirects":{},"id_redirects":{`+test.redirects+`}}`))
			mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: 01JAZ5N7Y3K8M2Q4R6T9VWXABC\n---\nAlpha\n"))
			cache := []byte(`{"slug":"alpha","frontmatter":{"id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}}` + "\n")
			articles := []string{"article-alpha:01JAZ5N7Y3K8M2Q4R6T9VWXABC:alpha"}
			if strings.Contains(test.concepts, "01JAZ5N7Y3K8M2Q4R6T9VWXABD") {
				mustWriteFile(t, filepath.Join(workspace, "wiki", "beta.md"), []byte("---\nid: 01JAZ5N7Y3K8M2Q4R6T9VWXABD\n---\nBeta\n"))
				cache = append(cache, []byte(`{"slug":"beta","frontmatter":{"id":"01JAZ5N7Y3K8M2Q4R6T9VWXABD"}}`+"\n")...)
				articles = append(articles, "article-beta:01JAZ5N7Y3K8M2Q4R6T9VWXABD:beta")
			}
			mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), cache)
			mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntities(articles, nil)))
			relevant := []string{"cache/id_map.json", "cache/concepts.jsonl", "wiki/alpha.md", ".synto/INDEX.json"}
			if strings.Contains(test.concepts, "01JAZ5N7Y3K8M2Q4R6T9VWXABD") {
				relevant = append(relevant, "wiki/beta.md")
			}
			before := snapshotRelevantVaultBytes(t, workspace, relevant...)

			priorID := "a3f7b2c01d9d"
			if test.name == "redirect chain" {
				priorID = "01JAZ5N7Y3K8M2Q4R6T9VWXABC"
			}
			err := reconcileWorkspaceConcepts(workspace, []conceptSnapshot{{ConceptID: priorID, Slug: "alpha"}})
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("error = %v, want %q", err, test.wantDetail)
			}
			assertVaultBytesUnchanged(t, workspace, before)
		})
	}
}

func TestFreshSyntoRunInitializesAndPublishesWithoutLegacyArtifacts(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := t.TempDir()
	execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
		if strings.Join(command, " ") != "run --auto-approve" {
			return fmt.Errorf("unexpected command %v", command)
		}
		writeFreshSyntoRequiredOutputs(t, work)
		return nil
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "offline", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"synto.toml", ".synto/state.db", ".synto/INDEX.json"} {
		if _, err := os.Stat(filepath.Join(vault, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("fresh publication missing %s: %v", rel, err)
		}
	}
	for _, rel := range []string{"wiki.toml", ".olw/state.db"} {
		if _, err := os.Stat(filepath.Join(vault, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("fresh publication fabricated %s: %v", rel, err)
		}
	}
}

func TestFreshSyntoCloudRunPublishesWithoutLegacyArtifacts(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	objects := newMemoryObjects()
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		writeFreshSyntoRequiredOutputs(t, work)
		return nil
	}
	if err := runCloudWorkerBatch(context.Background(), cloudCfg(), [][]string{{"run", "--auto-approve"}}, objects); err != nil {
		t.Fatal(err)
	}
	prefix := "users/user-secret/projects/project-secret/"
	names, err := objects.List(context.Background(), prefix+generation.Prefix, generation.MaxFiles)
	if err != nil || len(names) == 0 {
		t.Fatalf("fresh cloud generation missing: %v", err)
	}
	all, err := objects.List(context.Background(), prefix, generation.MaxFiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range all {
		if strings.HasSuffix(object.Name, "/wiki.toml") || strings.HasSuffix(object.Name, "/.olw/state.db") {
			t.Fatalf("fresh cloud publication fabricated legacy artifact %s", object.Name)
		}
	}
}

type generationIDMapFixture struct {
	Concept         map[string]string `json:"concept"`
	DormantConcept  map[string]string `json:"dormant_concept"`
	ConceptEntityID map[string]string `json:"concept_entity_id"`
}

func mustSnapshotConcepts(t *testing.T, workspace string) []conceptSnapshot {
	t.Helper()
	got, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func assertLifecycleState(t *testing.T, workspace string, wantDormant bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ids generationIDMapFixture
	if err := json.Unmarshal(data, &ids); err != nil {
		t.Fatal(err)
	}
	if wantDormant && (len(ids.Concept) != 0 || ids.DormantConcept["stable-alpha"] != "alpha" || ids.ConceptEntityID["stable-alpha"] != "01JAZ5N7Y3K8M2Q4R6T9VWXABC") {
		t.Fatalf("dormant map = %s", data)
	}
	if _, err := os.Stat(filepath.Join(workspace, "wiki", "alpha.md")); !os.IsNotExist(err) {
		t.Fatalf("active page remains: %v", err)
	}
	page, err := os.ReadFile(filepath.Join(workspace, "wiki", ".dormant", "alpha.md"))
	if err != nil || string(page) != "---\nid: stable-alpha\naliases:\n  - old-name\n---\nannotated history\n" {
		t.Fatalf("dormant page=%q err=%v", page, err)
	}
	cache, err := os.ReadFile(filepath.Join(workspace, "cache", "dormant_concepts.jsonl"))
	if err != nil || !strings.Contains(string(cache), `"stable-alpha"`) {
		t.Fatalf("dormant cache=%q err=%v", cache, err)
	}
}

func syntoIndexFixture(articleID, entityID, slug string, withSource bool) string {
	edges := "[]"
	if withSource {
		edges = `[{"source_path":"raw/source.md","content_hash":"` + strings.Repeat("0", 64) + `","concepts":[{"name":"` + slug + `","entity_id":"` + entityID + `"}]}]`
	}
	return `{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[{"id":"` + articleID + `","entity_id":"` + entityID + `","name":"` + slug + `","path":"wiki/` + slug + `.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":` + edges + `,"synthesis":[],"stats":{"article_count":1,"draft_count":0,"concept_count":1,"alias_count":0,"knowledge_item_count":0,"source_count":1,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`
}

func syntoCrossArtifactEntitylessIDPathFixture(entityID string) string {
	return `{"schema_version":1,"pack":{"id":"fixture","name":"fixture","version":"0","language":["en"],"capabilities":["articles","concepts"]},"articles":[{"id":"article-a","entity_id":null,"name":"Ordinary","path":"wiki/ordinary.md","summary":null,"tags":[],"aliases":[],"confidence":"high"},{"id":"article-b","entity_id":"` + entityID + `","name":"Alpha","path":"wiki/alpha.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":[{"source_path":"raw/alpha.md","content_hash":"` + strings.Repeat("0", 64) + `","concepts":[{"name":"Alpha","entity_id":"` + entityID + `"}]}],"synthesis":[],"stats":{"article_count":2,"draft_count":0,"concept_count":2,"alias_count":0,"knowledge_item_count":0,"source_count":1,"source_segment_count":0,"failed_note_count":0,"failed_concept_count":0}}`
}
