package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

const expectedSyntoWheelSHA256 = "4bc8dcf14b53f45fac32ce737ecf878f1a46d6d0b010c7decbe6c3b7b10afa77"

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
	for path, want := range map[string]string{"raw/source.md": "raw bytes", "wiki/alpha.md": "wiki bytes"} {
		data, err := os.ReadFile(filepath.Join(vault, filepath.FromSlash(path)))
		if err != nil || string(data) != want {
			t.Fatalf("migration changed %s=%q err=%v", path, data, err)
		}
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

func TestSyntoPipelineSafetyRejectsUnsafeMigratedConfig(t *testing.T) {
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
		mustWriteFile(t, filepath.Join(work, "synto.toml"), []byte("[pipeline]\nrelation_extraction = false\n"))
		writeValidSQLiteState(t, filepath.Join(work, ".synto", "state.db"))
		return nil
	}
	if err := ensureSyntoVault(context.Background(), vault, workerConfig{APIKey: "unused"}, nil); err == nil {
		t.Fatal("unsafe migrated Synto configuration was accepted")
	}
	if migrationCalls != 1 {
		t.Fatalf("migration calls = %d, want 1", migrationCalls)
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
	index := syntoIndexFixtureWithEntities([]string{"article-a:entity-a:alpha", "article-b:entity-b:beta"}, nil)
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

	valid := syntoIndexFixture("article", "entity", "alpha", true)
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
	data := strings.Replace(syntoIndexFixture("article", "entity", "alpha", true), `"path":"wiki/alpha.md"`, `"path":"articles/alpha.md"`, 1)
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
	if entities["article"] != "entity" {
		t.Fatalf("entity mapping = %#v, want article -> entity", entities)
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
	base := syntoIndexFixture("article", "entity", "alpha", true)
	for name, index := range map[string]string{
		"nested path":       strings.Replace(base, `"path":"wiki/alpha.md"`, `"path":"articles/nested/alpha.md"`, 1),
		"unexpected prefix": strings.Replace(base, `"path":"wiki/alpha.md"`, `"path":"exports/alpha.md"`, 1),
		"case collision":    strings.Replace(base, `],"terms":[]`, `,{"id":"article-b","entity_id":"entity-b","name":"Alpha","path":"articles/ALPHA.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[]`, 1),
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
	if run1Path == "" || run2Path == "" || rawPath == "" {
		t.Skip("set both exact INDEX paths and LWC195_RAW_SOURCE_PATH for the bridge")
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
	mustWriteFile(t, filepath.Join(workspace, "wiki", "Alpha.md"), []byte("---\nid: transient-alpha\n---\nsecond Alpha\n"))
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
	base := syntoIndexFixture("article", "entity", "alpha", true)
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

func TestSyntoEntityFirstMappingPreservesRenameAndFailsClosed(t *testing.T) {
	prior := []conceptSnapshot{{ConceptID: "stable-alpha", Slug: "alpha", EntityID: "entity-alpha"}}
	out, _, err := reconcileConceptIDMapWithEntities([]byte(`{"concept":{"generated":"renamed-alpha"},"concept_entity_id":{"generated":"entity-alpha"},"source":{},"redirects":{}}`), prior, true)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := wikiindex.DecodeIDMap(out)
	if err != nil {
		t.Fatal(err)
	}
	if ids.Concept["stable-alpha"] != "renamed-alpha" || ids.ConceptEntityID["stable-alpha"] != "entity-alpha" {
		t.Fatalf("rename mapping=%s", out)
	}

	cases := map[string]string{
		"collision":       `{"concept":{"a":"one","b":"two"},"concept_entity_id":{"a":"same","b":"same"},"source":{},"redirects":{}}`,
		"missing mapping": `{"concept":{"a":"renamed"},"source":{},"redirects":{}}`,
		"changed entity":  `{"concept":{"a":"alpha"},"concept_entity_id":{"a":"entity-other"},"source":{},"redirects":{}}`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := reconcileConceptIDMapWithEntities([]byte(data), prior, true); err == nil {
				t.Fatal("ambiguous entity mapping accepted")
			}
		})
	}
}

func TestSyntoIdentityLogRejectsMergeAndSplitLineage(t *testing.T) {
	for _, op := range []string{"merge", "split"} {
		workspace := t.TempDir()
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"generated":"alpha"},"concept_entity_id":{"generated":"entity-alpha"},"source":{},"redirects":{}}`))
		mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: generated\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"generated"}}`+"\n"))
		index := strings.Replace(syntoIndexFixture("generated", "entity-alpha", "alpha", true), `"terms":[]`, `"identity_log":[{"op":"`+op+`"}],"terms":[]`, 1)
		mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(index))
		if err := reconcileWorkspaceConcepts(workspace, nil); err == nil {
			t.Fatalf("%s lineage accepted", op)
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
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("article", "entity", "alpha", false)))
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
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha"},"concept_entity_id":{"stable-alpha":"entity-alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), priorRow)
	prior, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatal(err)
	}

	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-alpha\n---\nnew empty output\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-alpha":"alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"transient-alpha"}}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("transient-alpha", "entity-alpha", "alpha", false)))
	if err := reconcileWorkspaceConcepts(workspace, prior); err != nil {
		t.Fatalf("dormant reconcile: %v", err)
	}
	assertLifecycleState(t, workspace, true)
	dormantPrior := mustSnapshotConcepts(t, workspace)

	// The next Synto generation publishes the same entity again with a source;
	// the prior dormant mapping must reactivate the original LWC ID.
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-alpha\n---\nreactivated\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-alpha":"alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","sources":["stable-source"],"frontmatter":{"id":"transient-alpha"}}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("transient-alpha", "entity-alpha", "alpha", true)))
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
	if ids.Concept["stable-alpha"] != "alpha" || len(ids.DormantConcept) != 0 || ids.ConceptEntityID["stable-alpha"] != "entity-alpha" {
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
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha"},"dormant_concept":{"stable-beta":"beta"},"concept_entity_id":{"stable-alpha":"entity-alpha","stable-beta":"entity-beta"},"source":{},"redirects":{}}`))
	if err := runPostprocess(context.Background(), workspace); err != nil {
		t.Fatalf("postprocess: %v", err)
	}
	prior, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatalf("snapshot after postprocess: %v", err)
	}

	// Synto now emits the dormant entity under a new generated page ID. The
	// entity-aware reconciler, not postprocess, must restore stable-beta.
	mustWriteFile(t, filepath.Join(workspace, "wiki", "beta.md"), []byte("---\nid: transient-beta\n---\nBeta reactivated"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha","transient-beta":"beta"},"concept_entity_id":{"stable-alpha":"entity-alpha","transient-beta":"entity-beta"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte("{\"slug\":\"alpha\"}\n{\"slug\":\"beta\",\"sources\":[\"raw/source.md\"]}\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntities([]string{"stable-alpha:entity-alpha:alpha", "transient-beta:entity-beta:beta"}, []string{"entity-beta"})))
	if err := reconcileWorkspaceConcepts(workspace, prior, []sourceSnapshot{{RawPath: "raw/source.md", SyntoContentHash: strings.Repeat("0", 64)}}); err != nil {
		t.Fatalf("reactivation reconcile: %v", err)
	}
	ids := mustSnapshotIDMap(t, workspace)
	if ids.Concept["stable-beta"] != "beta" || ids.DormantConcept["stable-beta"] != "" || ids.ConceptEntityID["stable-beta"] != "entity-beta" {
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
			index:       syntoIndexFixture("transient-alpha", "entity-alpha", "alpha", true),
			wantDormant: true,
		},
		{
			name:        "tombstone-only source artifact",
			sourceMeta:  `,"source":{"s1":"source"},"source_meta":{"s1":{"slug":"source","source_file":"raw/source.md"}}`,
			index:       syntoIndexFixture("transient-alpha", "entity-alpha", "alpha", true),
			wantDormant: true,
		},
		{
			name:          "matching non-empty source artifact",
			sourceMeta:    `,"source":{"s1":"source"},"source_meta":{"s1":{"slug":"source","source_file":"raw/source.md"}}`,
			raw:           "current source",
			index:         syntoIndexFixtureWithEntitiesHash([]string{"transient-alpha:entity-alpha:alpha"}, []string{"entity-alpha"}, sha256Text("current source")),
			wantActiveRow: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := execOLW
			defer func() { execOLW = old }()
			vault := t.TempDir()
			mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
			mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: stable-alpha\n---\nHuman annotation and historical body\n"))
			mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha"},"concept_entity_id":{"stable-alpha":"entity-alpha"}`+tc.sourceMeta+`,"redirects":{}}`))
			mustWriteFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"stable-alpha"}}`+"\n"))
			if tc.raw != "" {
				mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte(tc.raw))
			}
			execOLW = func(_ context.Context, work string, command []string, _ []string, _, _ io.Writer) error {
				if strings.Join(command, " ") != "run --auto-approve" {
					return fmt.Errorf("unexpected command %v", command)
				}
				mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: transient-alpha\n---\nnew generated body\n"))
				mustWriteFile(t, filepath.Join(work, "cache", "id_map.json"), []byte(`{"concept":{"transient-alpha":"alpha"},"concept_entity_id":{"transient-alpha":"entity-alpha"},"source":{},"redirects":{}}`))
				mustWriteFile(t, filepath.Join(work, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","frontmatter":{"id":"transient-alpha"}}`+"\n"))
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
			if tc.wantDormant {
				if len(ids.Concept) != 0 || ids.DormantConcept["stable-alpha"] != "alpha" || ids.ConceptEntityID["stable-alpha"] != "entity-alpha" {
					t.Fatalf("empty/tombstone source set stayed active: %#v", ids)
				}
				page, err := os.ReadFile(filepath.Join(vault, "wiki", ".dormant", "alpha.md"))
				if err != nil || string(page) != "---\nid: stable-alpha\n---\nHuman annotation and historical body\n" {
					t.Fatalf("dormant identity/history = %q err=%v", page, err)
				}
				if _, err := os.Stat(filepath.Join(vault, "wiki", "alpha.md")); !os.IsNotExist(err) {
					t.Fatalf("active route remains: %v", err)
				}
				cache, err := os.ReadFile(filepath.Join(vault, "cache", "concepts.jsonl"))
				if err != nil || strings.TrimSpace(string(cache)) != "" {
					t.Fatalf("active cache row remains: %q err=%v", cache, err)
				}
			} else if !tc.wantActiveRow || ids.Concept["stable-alpha"] != "alpha" || ids.DormantConcept["stable-alpha"] != "" {
				t.Fatalf("matching source did not preserve active identity: %#v", ids)
			}
		})
	}
}

func TestSyntoLifecycleFailsClosedWhenCurrentSourceTruthIsUnavailable(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: stable-alpha\n---\nhistory\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha"},"concept_entity_id":{"stable-alpha":"entity-alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha"}`+"\n"))
	prior, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-alpha\n---\nnew\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-alpha":"alpha"},"concept_entity_id":{"transient-alpha":"entity-alpha"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha"}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixture("transient-alpha", "entity-alpha", "alpha", true)))

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
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"stable-alpha":"alpha","stable-beta":"beta"},"concept_entity_id":{"stable-alpha":"entity-alpha","stable-beta":"entity-beta"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte("{\"slug\":\"alpha\",\"sources\":[\"raw/old.md\"]}\n{\"slug\":\"beta\",\"sources\":[\"raw/old.md\"]}\n"))
	prior, err := snapshotConcepts(workspace)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), []byte("---\nid: transient-alpha\n---\nnew stale article\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "beta.md"), []byte("---\nid: transient-beta\n---\nBeta\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "gamma.md"), []byte("---\nid: transient-gamma\n---\nGamma\n"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-alpha":"alpha","transient-beta":"beta","transient-gamma":"gamma"},"concept_entity_id":{"transient-alpha":"entity-alpha","transient-beta":"entity-beta","transient-gamma":"entity-gamma"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte("{\"slug\":\"alpha\",\"sources\":[\"raw/old.md\"]}\n{\"slug\":\"beta\",\"sources\":[\"raw/new.md\"]}\n{\"slug\":\"gamma\",\"sources\":[\"raw/new.md\"]}\n"))
	mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntities([]string{"transient-alpha:entity-alpha:alpha", "transient-beta:entity-beta:beta", "transient-gamma:entity-gamma:gamma"}, []string{"entity-beta", "entity-gamma"})))
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
		mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"transient-alpha":"alpha","transient-beta":"beta","transient-gamma":"gamma"},"concept_entity_id":{"transient-alpha":"entity-alpha","transient-beta":"entity-beta","transient-gamma":"entity-gamma"},"source":{},"redirects":{}}`))
		mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte("{\"slug\":\"alpha\"}\n{\"slug\":\"beta\"}\n{\"slug\":\"gamma\"}\n"))
		mustWriteFile(t, filepath.Join(workspace, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithSourceGroups([]string{"transient-alpha:entity-alpha:alpha", "transient-beta:entity-beta:beta", "transient-gamma:entity-gamma:gamma"}, groups)))
		prior := []conceptSnapshot{
			{ConceptID: "stable-alpha", Slug: "alpha", EntityID: "entity-alpha", Page: []byte("---\nid: stable-alpha\n---\nhistory\n")},
			{ConceptID: "stable-beta", Slug: "beta", EntityID: "entity-beta", Page: []byte("---\nid: stable-beta\n---\nhistory\n")},
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
		}, `[{"source_path":"raw/alpha.md","content_hash":"`+alphaOld+`","concepts":[{"name":"Alpha","entity_id":"entity-alpha"}]},{"source_path":"raw/beta.md","content_hash":"`+betaCurrent+`","concepts":[{"name":"Beta","entity_id":"entity-beta"}]},{"source_path":"raw/gamma.md","content_hash":"`+gammaCurrent+`","concepts":[{"name":"Gamma","entity_id":"entity-gamma"}]}]`, true)
	})
	t.Run("removed source tombstone", func(t *testing.T) {
		newCase(t, []sourceSnapshot{{RawPath: "raw/alpha.md", Tombstone: true}}, `[{"source_path":"raw/alpha.md","content_hash":"`+alphaOld+`","concepts":[{"name":"Alpha","entity_id":"entity-alpha"}]}]`, false)
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
		id := fmt.Sprintf("synto-transient-%d", gen)
		mustWriteFile(t, filepath.Join(work, "wiki", "alpha.md"), []byte("---\nid: "+id+"\nsources:\n  - source-1\n---\nbody\n"))
		mustWriteFile(t, filepath.Join(work, "wiki", "sources", "source.md"), []byte("---\nid: source-1\nsource_file: raw/source.md\n---\nsource\n"))
		mustWriteFile(t, filepath.Join(work, "cache", "id_map.json"), []byte(`{"concept":{"`+id+`":"alpha"},"source":{"source-1":"source"},"source_meta":{"source-1":{"slug":"source","source_file":"raw/source.md"}},"redirects":{}}`))
		mustWriteFile(t, filepath.Join(work, "cache", "concepts.jsonl"), []byte(`{"slug":"alpha","sources":["source-1"],"frontmatter":{"id":"`+id+`"}}`+"\n"))
		mustWriteFile(t, filepath.Join(work, "cache", "raw_status.json"), []byte("{}"))
		mustWriteFile(t, filepath.Join(work, "cache", "suggested_queries.json"), []byte("{}"))
		mustWriteFile(t, filepath.Join(work, ".synto", "INDEX.json"), []byte(syntoIndexFixtureWithEntitiesHash([]string{id + ":entity-alpha:alpha"}, []string{"entity-alpha"}, sha256Text("raw"))))
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
	if ids.Concept["synto-transient-1"] != "alpha" || ids.ConceptEntityID["synto-transient-1"] != "entity-alpha" {
		t.Fatalf("worker did not preserve canonical mapping: %s", data)
	}
	if _, err := os.Stat(filepath.Join(vault, "synto.toml")); err != nil {
		t.Fatalf("private workspace did not publish synto.toml: %v", err)
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
	if wantDormant && (len(ids.Concept) != 0 || ids.DormantConcept["stable-alpha"] != "alpha" || ids.ConceptEntityID["stable-alpha"] != "entity-alpha") {
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
