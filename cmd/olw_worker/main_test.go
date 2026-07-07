package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCommandBatch(t *testing.T) {
	commands, err := parseCommandBatch(`[["clear"],["run","--auto-approve"]]`)
	if err != nil {
		t.Fatalf("parseCommandBatch() error = %v", err)
	}
	if len(commands) != 2 || commands[1][0] != "run" || commands[1][1] != "--auto-approve" {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestParseCommandBatchRejectsEmptyCommand(t *testing.T) {
	if _, err := parseCommandBatch(`[[]]`); err == nil {
		t.Fatal("parseCommandBatch() error = nil, want error")
	}
}

func TestParseCommandBatchRejectsEmptyCommandName(t *testing.T) {
	if _, err := parseCommandBatch(`[["","--flag"]]`); err == nil {
		t.Fatal("parseCommandBatch() error = nil, want error")
	}
}

func TestResolveVaultPathPrefersExplicitVault(t *testing.T) {
	cfg := workerConfig{VaultPath: "/tmp/explicit", DataDir: "/data", UserID: "u", ProjectID: "p"}
	got, err := resolveVaultPath(cfg)
	if err != nil {
		t.Fatalf("resolveVaultPath() error = %v", err)
	}
	if got != "/tmp/explicit" {
		t.Fatalf("vault = %q, want explicit", got)
	}
}

func TestResolveVaultPathFromUserProject(t *testing.T) {
	cfg := workerConfig{DataDir: "/data", UserID: "u", ProjectID: "p"}
	got, err := resolveVaultPath(cfg)
	if err != nil {
		t.Fatalf("resolveVaultPath() error = %v", err)
	}
	want := filepath.Join("/data", "users", "u", "projects", "p")
	if got != want {
		t.Fatalf("vault = %q, want %q", got, want)
	}
}

func TestResolveVaultPathErrorsWithoutEnoughConfig(t *testing.T) {
	if _, err := resolveVaultPath(workerConfig{DataDir: "/data", UserID: "u"}); err == nil {
		t.Fatal("resolveVaultPath() error = nil, want error")
	}
}

func TestEnsureWikiTOMLCreatesButDoesNotOverwrite(t *testing.T) {
	vault := t.TempDir()
	cfg := workerConfig{APIKey: "secret"}
	if err := ensureWikiTOML(vault, cfg); err != nil {
		t.Fatalf("ensureWikiTOML(create) error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(vault, "wiki.toml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		`name = "deepseek"`,
		`url = "https://api.deepseek.com/v1"`,
		`fast = "deepseek-chat"`,
		`heavy = "deepseek-reasoner"`,
		`auto_approve = true`,
		`article_max_tokens = 32768`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("wiki.toml missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "api_key") || strings.Contains(text, "secret") {
		t.Fatalf("wiki.toml should not persist API keys:\n%s", text)
	}

	if err := os.WriteFile(filepath.Join(vault, "wiki.toml"), []byte("custom"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureWikiTOML(vault, workerConfig{APIKey: "new"}); err != nil {
		t.Fatalf("ensureWikiTOML(existing) error = %v", err)
	}
	data, err = os.ReadFile(filepath.Join(vault, "wiki.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "custom" {
		t.Fatalf("existing wiki.toml overwritten: %q", data)
	}
}

func TestEnsureWikiTOMLRequiresAPIKeyWhenMissing(t *testing.T) {
	if err := ensureWikiTOML(t.TempDir(), workerConfig{}); err == nil {
		t.Fatal("ensureWikiTOML() error = nil, want error")
	}
}

func TestPrepareOLWEnvironmentIsolatesConfigAndMapsDeepSeekKey(t *testing.T) {
	env, err := prepareOLWEnvironment(workerConfig{APIKey: "secret"})
	if err != nil {
		t.Fatalf("prepareOLWEnvironment() error = %v", err)
	}

	values := envMap(env)
	configHome := values["XDG_CONFIG_HOME"]
	if configHome == "" {
		t.Fatalf("XDG_CONFIG_HOME missing from env: %#v", env)
	}
	if strings.Contains(configHome, ".config/olw") {
		t.Fatalf("XDG_CONFIG_HOME points at host/global olw config: %q", configHome)
	}
	if _, err := os.Stat(configHome); err != nil {
		t.Fatalf("XDG_CONFIG_HOME dir was not created: %v", err)
	}
	if values["DEEPSEEK_API_KEY"] != "secret" {
		t.Fatalf("DEEPSEEK_API_KEY = %q, want secret", values["DEEPSEEK_API_KEY"])
	}
}

func TestPrepareWorkspaceSymlinksRawAndCopiesWritableState(t *testing.T) {
	vault := newTestVault(t)
	parent := t.TempDir()

	workspace, err := prepareWorkspace(vault, parent)
	if err != nil {
		t.Fatalf("prepareWorkspace() error = %v", err)
	}
	defer workspace.cleanup()

	rawInfo, err := os.Lstat(filepath.Join(workspace.path, "raw"))
	if err != nil {
		t.Fatalf("lstat workspace raw: %v", err)
	}
	if rawInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("workspace raw mode = %v, want symlink", rawInfo.Mode())
	}
	rawTarget, err := os.Readlink(filepath.Join(workspace.path, "raw"))
	if err != nil {
		t.Fatalf("readlink workspace raw: %v", err)
	}
	if rawTarget != filepath.Join(vault, "raw") {
		t.Fatalf("raw symlink = %q, want %q", rawTarget, filepath.Join(vault, "raw"))
	}

	assertFileContent(t, filepath.Join(workspace.path, "wiki", "page.md"), "original wiki")
	assertFileContent(t, filepath.Join(workspace.path, "wiki", "sources", "source.md"), "original source")
	assertFileContent(t, filepath.Join(workspace.path, "cache", "id_map.json"), "{}")
	assertFileContent(t, filepath.Join(workspace.path, ".olw", "state.db"), "state")
	assertFileContent(t, filepath.Join(workspace.path, "wiki.toml"), "config")
}

func TestWorkspaceSyncBackCopiesDurableOutputsAndExcludesPipelineLock(t *testing.T) {
	vault := newTestVault(t)
	parent := t.TempDir()

	workspace, err := prepareWorkspace(vault, parent)
	if err != nil {
		t.Fatalf("prepareWorkspace() error = %v", err)
	}
	defer workspace.cleanup()

	writeFile(t, filepath.Join(workspace.path, "wiki", "page.md"), "updated wiki")
	writeFile(t, filepath.Join(workspace.path, "wiki", "new.md"), "new wiki")
	writeFile(t, filepath.Join(workspace.path, "cache", "concepts.jsonl"), "{}\n")
	writeFile(t, filepath.Join(workspace.path, ".olw", "state.db"), "updated state")
	writeFile(t, filepath.Join(workspace.path, ".olw", "pipeline.lock"), "do not sync")
	if err := os.Remove(filepath.Join(workspace.path, "wiki", "sources", "source.md")); err != nil {
		t.Fatalf("remove workspace source: %v", err)
	}

	if err := workspace.syncBack(); err != nil {
		t.Fatalf("syncBack() error = %v", err)
	}

	assertFileContent(t, filepath.Join(vault, "wiki", "page.md"), "updated wiki")
	assertFileContent(t, filepath.Join(vault, "wiki", "new.md"), "new wiki")
	assertMissing(t, filepath.Join(vault, "wiki", "sources", "source.md"))
	assertFileContent(t, filepath.Join(vault, "cache", "concepts.jsonl"), "{}\n")
	assertFileContent(t, filepath.Join(vault, ".olw", "state.db"), "updated state")
	assertMissing(t, filepath.Join(vault, ".olw", "pipeline.lock"))
}

func TestPrepareWorkspaceAllowsMissingOptionalDirectories(t *testing.T) {
	vault := t.TempDir()
	parent := t.TempDir()
	writeFile(t, filepath.Join(vault, "wiki.toml"), "config")

	workspace, err := prepareWorkspace(vault, parent)
	if err != nil {
		t.Fatalf("prepareWorkspace() error = %v", err)
	}
	defer workspace.cleanup()

	assertFileContent(t, filepath.Join(workspace.path, "wiki.toml"), "config")
	assertMissing(t, filepath.Join(workspace.path, "raw"))
	assertMissing(t, filepath.Join(workspace.path, "wiki"))
	assertMissing(t, filepath.Join(workspace.path, "cache"))
	assertMissing(t, filepath.Join(workspace.path, ".olw"))
}

func TestRunWorkerBatchPassesIsolatedOLWEnvironment(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	vault := t.TempDir()
	var gotEnv []string
	execOLW = func(_ context.Context, _ string, _ []string, env []string) error {
		gotEnv = append([]string(nil), env...)
		return nil
	}

	cfg := workerConfig{VaultPath: vault, APIKey: "secret", Postprocess: false, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatalf("runWorkerBatch() error = %v", err)
	}

	values := envMap(gotEnv)
	if values["XDG_CONFIG_HOME"] == "" {
		t.Fatalf("exec env missing XDG_CONFIG_HOME: %#v", gotEnv)
	}
	if values["DEEPSEEK_API_KEY"] != "secret" {
		t.Fatalf("DEEPSEEK_API_KEY = %q, want secret", values["DEEPSEEK_API_KEY"])
	}
}

func TestRunWorkerBatchCanInitializeVaultBeforeCommands(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	vault := t.TempDir()
	var ran [][]string
	execOLW = func(_ context.Context, _ string, command []string, _ []string) error {
		ran = append(ran, append([]string(nil), command...))
		return nil
	}

	cfg := workerConfig{VaultPath: vault, APIKey: "secret", InitVault: true, Postprocess: false, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatalf("runWorkerBatch() error = %v", err)
	}

	want := [][]string{{"init", "."}, {"run", "--auto-approve"}}
	if len(ran) != len(want) {
		t.Fatalf("ran = %#v, want %#v", ran, want)
	}
	for i := range want {
		if strings.Join(ran[i], "\x00") != strings.Join(want[i], "\x00") {
			t.Fatalf("ran = %#v, want %#v", ran, want)
		}
	}
}

func TestRunWorkerBatchDoesNotInitializeVaultByDefault(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	vault := t.TempDir()
	var ran [][]string
	execOLW = func(_ context.Context, _ string, command []string, _ []string) error {
		ran = append(ran, append([]string(nil), command...))
		return nil
	}

	cfg := workerConfig{VaultPath: vault, APIKey: "secret", Postprocess: false, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatalf("runWorkerBatch() error = %v", err)
	}
	if len(ran) != 1 || strings.Join(ran[0], "\x00") != "run\x00--auto-approve" {
		t.Fatalf("ran = %#v, want only run command", ran)
	}
}

func TestRunWorkerBatchUsesWorkspaceAndSyncsBackOnSuccess(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	vault := newTestVault(t)
	parent := t.TempDir()
	var execVault string
	execOLW = func(_ context.Context, vault string, _ []string, _ []string) error {
		execVault = vault
		writeFile(t, filepath.Join(vault, "wiki", "generated.md"), "generated")
		writeFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), "{}\n")
		writeFile(t, filepath.Join(vault, ".olw", "state.db"), "updated state")
		return nil
	}

	cfg := workerConfig{
		VaultPath:       vault,
		APIKey:          "secret",
		Postprocess:     false,
		StopOnError:     true,
		UseWorkspace:    true,
		WorkspaceParent: parent,
	}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatalf("runWorkerBatch() error = %v", err)
	}
	if execVault == "" {
		t.Fatal("execOLW was not called")
	}
	if execVault == vault {
		t.Fatalf("exec vault = original vault %q, want workspace", execVault)
	}
	if !strings.HasPrefix(execVault, parent) {
		t.Fatalf("exec vault = %q, want under %q", execVault, parent)
	}
	assertFileContent(t, filepath.Join(vault, "wiki", "generated.md"), "generated")
	assertFileContent(t, filepath.Join(vault, "cache", "concepts.jsonl"), "{}\n")
	assertFileContent(t, filepath.Join(vault, ".olw", "state.db"), "updated state")
}

func TestRunWorkerBatchDoesNotSyncWorkspaceBackOnOLWFailure(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	failErr := errors.New("olw failed")
	vault := newTestVault(t)
	execOLW = func(_ context.Context, vault string, _ []string, _ []string) error {
		writeFile(t, filepath.Join(vault, "wiki", "generated.md"), "generated")
		writeFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), "{}\n")
		return failErr
	}

	cfg := workerConfig{
		VaultPath:       vault,
		APIKey:          "secret",
		Postprocess:     false,
		StopOnError:     true,
		UseWorkspace:    true,
		WorkspaceParent: t.TempDir(),
	}
	err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`)
	if !errors.Is(err, failErr) {
		t.Fatalf("runWorkerBatch() error = %v, want %v", err, failErr)
	}
	assertMissing(t, filepath.Join(vault, "wiki", "generated.md"))
	assertMissing(t, filepath.Join(vault, "cache", "concepts.jsonl"))
}

func TestRunOLWBatchStopsOnFirstFailure(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	failErr := errors.New("failed")
	var ran [][]string
	execOLW = func(_ context.Context, _ string, command []string, _ []string) error {
		ran = append(ran, append([]string(nil), command...))
		if command[0] == "fail" {
			return failErr
		}
		return nil
	}

	err := runOLWBatch(context.Background(), t.TempDir(), [][]string{{"fail"}, {"second"}}, true, nil)
	if !errors.Is(err, failErr) {
		t.Fatalf("runOLWBatch() error = %v, want %v", err, failErr)
	}
	if len(ran) != 1 {
		t.Fatalf("ran %d commands, want 1: %#v", len(ran), ran)
	}
}

func TestRunOLWBatchContinuesWhenStopOnErrorFalse(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	var ran [][]string
	execOLW = func(_ context.Context, _ string, command []string, _ []string) error {
		ran = append(ran, append([]string(nil), command...))
		if command[0] == "fail" {
			return errors.New("failed")
		}
		return nil
	}

	err := runOLWBatch(context.Background(), t.TempDir(), [][]string{{"fail"}, {"second"}}, false, nil)
	if err == nil {
		t.Fatal("runOLWBatch() error = nil, want error")
	}
	if len(ran) != 2 {
		t.Fatalf("ran %d commands, want 2: %#v", len(ran), ran)
	}
}

func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func newTestVault(t *testing.T) string {
	t.Helper()

	vault := t.TempDir()
	writeFile(t, filepath.Join(vault, "raw", "seed.md"), "raw")
	writeFile(t, filepath.Join(vault, "wiki", "page.md"), "original wiki")
	writeFile(t, filepath.Join(vault, "wiki", "sources", "source.md"), "original source")
	writeFile(t, filepath.Join(vault, "cache", "id_map.json"), "{}")
	writeFile(t, filepath.Join(vault, ".olw", "state.db"), "state")
	writeFile(t, filepath.Join(vault, "wiki.toml"), "config")
	return vault
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertFileContent(t *testing.T, path string, want string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lstat %s error = %v, want not exist", path, err)
	}
}
