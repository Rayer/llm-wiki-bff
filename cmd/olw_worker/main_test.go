package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/sourcestatus"
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

func TestCloudCommandValidationHappensBeforeDecode(t *testing.T) {
	cfg := workerConfig{Bucket: "bucket", UserID: "user", ProjectID: "project", Postprocess: true}
	for _, tc := range []struct {
		name string
		cfg  workerConfig
		raw  string
	}{
		{name: "empty execution id", cfg: cfg, raw: "not-json"},
		{name: "oversized raw command", cfg: workerConfig{Bucket: "bucket", UserID: "user", ProjectID: "project", ExecutionID: "exec-1", Postprocess: true}, raw: strings.Repeat("x", maxWorkerCommandBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := runWorkerBatch(context.Background(), tc.cfg, tc.raw); err == nil || err.Error() != "worker input is invalid" {
				t.Fatalf("runWorkerBatch() error = %v, want fixed pre-decode rejection", err)
			}
		})
	}
}

func TestCloudCommandStructuralLimits(t *testing.T) {
	var manyCommands strings.Builder
	manyCommands.WriteByte('[')
	for i := 0; i <= maxWorkerCommands; i++ {
		if i > 0 {
			manyCommands.WriteByte(',')
		}
		manyCommands.WriteString(`["run"]`)
	}
	manyCommands.WriteByte(']')
	if _, err := parseCommandBatch(manyCommands.String()); err == nil {
		t.Fatal("parseCommandBatch accepted command count overflow")
	}

	var manyArgs strings.Builder
	manyArgs.WriteString("[[\"run\"")
	for i := 0; i < maxWorkerArgs; i++ {
		manyArgs.WriteString(`,"arg"`)
	}
	manyArgs.WriteString(",\"overflow\"]]")
	if _, err := parseCommandBatch(manyArgs.String()); err == nil {
		t.Fatal("parseCommandBatch accepted argument count overflow")
	}

	if _, err := parseCommandBatch(`[["run","` + strings.Repeat("x", maxWorkerArgBytes+1) + `"]]`); err == nil {
		t.Fatal("parseCommandBatch accepted argument byte overflow")
	}
	var cumulative strings.Builder
	cumulative.WriteString("[[\"run\"")
	for i := 0; i < maxWorkerArgs; i++ {
		cumulative.WriteString(`,"`)
		cumulative.WriteString(strings.Repeat("x", maxWorkerArgBytes))
		cumulative.WriteByte('"')
	}
	cumulative.WriteString("]]")
	if _, err := parseCommandBatch(cumulative.String()); err == nil {
		t.Fatal("parseCommandBatch accepted cumulative argument byte overflow")
	}
}

func TestExplicitAPIKeyExcludesInheritedDiagnosticSecrets(t *testing.T) {
	inherited := strings.Repeat("oversized-inherited-secret", maxWorkerKeyBytes)
	t.Setenv("LLM_API_KEY", inherited)
	t.Setenv("DEEPSEEK_API_KEY", inherited+"-deepseek")
	cfg := workerConfig{APIKey: "small-explicit-key", apiKeySet: true}
	if err := validateWorkerInput(cfg, [][]string{{"run"}}); err != nil {
		t.Fatalf("explicit key validation error = %v", err)
	}
	for _, secret := range diagnosticSecrets(cfg, [][]string{{"run"}}) {
		if strings.Contains(secret, inherited) {
			t.Fatalf("diagnostic secret collection retained inherited key %q", secret)
		}
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

func TestRunWorkerBatchPassesIsolatedOLWEnvironment(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	vault := t.TempDir()
	var gotEnv []string
	execOLW = func(_ context.Context, _ string, _ []string, env []string, _, _ io.Writer) error {
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
	execOLW = func(_ context.Context, _ string, command []string, _ []string, _, _ io.Writer) error {
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
	execOLW = func(_ context.Context, _ string, command []string, _ []string, _, _ io.Writer) error {
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

func TestRunOLWBatchStopsOnFirstFailure(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	failErr := errors.New("failed")
	var ran [][]string
	execOLW = func(_ context.Context, _ string, command []string, _ []string, _, _ io.Writer) error {
		ran = append(ran, append([]string(nil), command...))
		if command[0] == "fail" {
			return failErr
		}
		return nil
	}

	err := runOLWBatch(context.Background(), t.TempDir(), [][]string{{"fail"}, {"second"}}, true, nil, nil, nil)
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
	execOLW = func(_ context.Context, _ string, command []string, _ []string, _, _ io.Writer) error {
		ran = append(ran, append([]string(nil), command...))
		if command[0] == "fail" {
			return errors.New("failed")
		}
		return nil
	}

	err := runOLWBatch(context.Background(), t.TempDir(), [][]string{{"fail"}, {"second"}}, false, nil, nil, nil)
	if err == nil {
		t.Fatal("runOLWBatch() error = nil, want error")
	}
	if len(ran) != 2 {
		t.Fatalf("ran %d commands, want 2: %#v", len(ran), ran)
	}
}

func TestRunWorkerBatchWritesPipelineLogForExecution(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	vault := t.TempDir()
	execOLW = func(_ context.Context, _ string, _ []string, _ []string, stdout, stderr io.Writer) error {
		if _, err := stdout.Write([]byte("stdout line\n")); err != nil {
			t.Fatalf("write stdout: %v", err)
		}
		if _, err := stderr.Write([]byte("stderr line\n")); err != nil {
			t.Fatalf("write stderr: %v", err)
		}
		return nil
	}

	cfg := workerConfig{VaultPath: vault, APIKey: "secret", ExecutionID: "olw-pipeline-abc123", Postprocess: false, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
		t.Fatalf("runWorkerBatch() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(vault, "cache", "pipeline-olw-pipeline-abc123.log"))
	if err != nil {
		t.Fatalf("read pipeline log: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "stdout line\n") || !strings.Contains(text, "stderr line\n") {
		t.Fatalf("log = %q, want stdout and stderr", text)
	}
}

func TestPipelineLogPathRejectsUnsafeExecutionID(t *testing.T) {
	if _, err := pipelineLogPath(t.TempDir(), "../escape"); err == nil {
		t.Fatal("pipelineLogPath() error = nil, want error")
	}
}

func TestWorkspaceSuccessSanitizesSecretSplitAcrossWrites(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	execOLW = func(_ context.Context, _ string, _ []string, _ []string, stdout, _ io.Writer) error {
		if _, err := io.WriteString(stdout, "token=chunked-"); err != nil {
			return err
		}
		_, err := io.WriteString(stdout, "secret")
		return err
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "chunked-secret", ExecutionID: "success", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run"]]`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(vault, "cache", "pipeline-success.log"))
	if err != nil || strings.Contains(string(data), "chunked-secret") || !strings.Contains(string(data), "[REDACTED]") {
		t.Fatalf("success log=%q err=%v", data, err)
	}
}

func TestRunPostprocessWritesSuggestedQueriesFromConcepts(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: alpha-id\ntitle: Alpha\nupdated: 2026-07-01T00:00:00Z\n---\nAlpha"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "beta.md"), []byte("---\nid: beta-id\ntitle: Beta\nupdated: 2026-07-10T00:00:00Z\n---\nBeta"))

	if err := runPostprocess(context.Background(), vault); err != nil {
		t.Fatalf("runPostprocess() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(vault, "cache", "suggested_queries.json"))
	if err != nil {
		t.Fatalf("read suggested_queries.json: %v", err)
	}
	var artifact struct {
		Queries   []string `json:"queries"`
		UpdatedAt string   `json:"updated_at"`
	}
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode suggested_queries.json: %v", err)
	}
	if len(artifact.Queries) != 2 {
		t.Fatalf("queries = %#v, want 2 entries", artifact.Queries)
	}
	if artifact.Queries[0] != "Beta" {
		t.Fatalf("queries[0] = %q, want Beta", artifact.Queries[0])
	}
	if artifact.UpdatedAt == "" {
		t.Fatal("updated_at is empty")
	}
}

func TestRunPostprocessWritesEmptyRawStatusWhenStateDBMissing(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "seed.md"), []byte("seed"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: concept-id\ntitle: Alpha\n---\nAlpha"))

	if err := runPostprocess(context.Background(), vault); err != nil {
		t.Fatalf("runPostprocess() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(vault, "cache", "raw_status.json"))
	if err != nil {
		t.Fatalf("read raw_status.json: %v", err)
	}
	if !strings.Contains(string(data), `"files": {}`) {
		t.Fatalf("raw_status.json = %s, want empty files object", data)
	}
	if !strings.Contains(string(data), `"file_count": 1`) {
		t.Fatalf("raw_status.json = %s, want file_count 1 for seed.md", data)
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

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceMaterializesAnnotationWithoutChangingStoredRaw(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	writeWorkspaceAnnotation(t, vault, "s1", "raw/source.md", "A human note")
	var gotVault, gotRaw string
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		data, err := os.ReadFile(filepath.Join(work, "raw", "source.md"))
		if err != nil {
			return err
		}
		gotVault, gotRaw = work, string(data)
		return nil
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true, StopOnError: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run"]]`); err != nil {
		t.Fatal(err)
	}
	if gotVault == vault || gotRaw != "original\n\n---\n\n## Human annotations (system)\n<!-- lwc-ann-v1 source_id=s1 ann_sha256="+annotation.Digest("A human note")+" -->\nA human note\n" {
		t.Fatalf("OLW input vault=%q raw=%q", gotVault, gotRaw)
	}
	stored, err := os.ReadFile(filepath.Join(vault, "raw", "source.md"))
	if err != nil || string(stored) != "original" {
		t.Fatalf("stored raw=%q err=%v", stored, err)
	}
	artifact, err := readSourceStatus(vault)
	if err != nil {
		t.Fatal(err)
	}
	receipt := artifact.Sources["s1"]
	if !sourcestatus.ValidReceipt(receipt, "raw/source.md") || receipt.LastIngestedAnnSHA256 != annotation.Digest("A human note") {
		t.Fatalf("receipt=%+v", receipt)
	}
}

func TestWorkspaceEmptyAnnotationRemovesPriorInfluence(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	writeWorkspaceStatus(t, vault, sourcestatus.Receipt{RawPath: "raw/source.md", LastIngestedRawSHA256: sha256Text("original"), LastIngestedAnnSHA256: annotation.Digest("old"), LastIngestFingerprint: sourcestatus.Fingerprint(sha256Text("original"), annotation.Digest("old")), LastSuccessAt: time.Now().UTC().Format(time.RFC3339)})
	var got string
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		data, err := os.ReadFile(filepath.Join(work, "raw", "source.md"))
		got = string(data)
		return err
	}
	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run"]]`); err != nil {
		t.Fatal(err)
	}
	if got != "original" {
		t.Fatalf("empty annotation input=%q, want original raw only", got)
	}
	artifact, _ := readSourceStatus(vault)
	if artifact.Sources["s1"].LastIngestedAnnSHA256 != annotation.Digest("") {
		t.Fatalf("receipt=%+v", artifact.Sources["s1"])
	}
}

func TestWorkspaceMarkerLikeAnnotationOnlyGetsOneSystemTrailer(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	writeWorkspaceAnnotation(t, vault, "s1", "raw/source.md", "## Human annotations (system)\n<!-- lwc-ann-v1 source_id=s1 ann_sha256=fake -->")
	var got string
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		data, err := os.ReadFile(filepath.Join(work, "raw", "source.md"))
		got = string(data)
		return err
	}
	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run"]]`); err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "## Human annotations (system)") != 2 || strings.Count(got, "<!-- lwc-ann-v1 source_id=s1 ann_sha256=") != 2 {
		t.Fatalf("annotation text was not preserved literally or system trailer duplicated: %q", got)
	}
}

func TestWorkspaceMaterializesAnnotatedSourceIdenticallyOnSequentialRuns(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	writeWorkspaceAnnotation(t, vault, "s1", "raw/source.md", "note\n")
	var materialized []string
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		data, err := os.ReadFile(filepath.Join(work, "raw", "source.md"))
		materialized = append(materialized, string(data))
		return err
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}
	for i := 0; i < 2; i++ {
		if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"],["approve","--all"]]`); err != nil {
			t.Fatal(err)
		}
		// In production OLW regenerates id_map with its source mappings. The fake
		// executor above does not, so retain this mapped-source fixture between
		// the two independent workspace runs.
		mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"source_meta":{"s1":{"source_file":"raw/source.md"}}}`))
	}
	if len(materialized) != 4 || materialized[0] != materialized[2] {
		t.Fatalf("materialized runs differ: %#v", materialized)
	}
	if strings.Count(materialized[0], "<!-- lwc-ann-v1 source_id=s1 ") != 1 {
		t.Fatalf("materialized input has wrong trailer count: %q", materialized[0])
	}
	stored, err := os.ReadFile(filepath.Join(vault, "raw", "source.md"))
	if err != nil || string(stored) != "original" {
		t.Fatalf("stored raw changed: %q err=%v", stored, err)
	}
}

func TestWorkspaceRejectsDuplicateMappedRawPath(t *testing.T) {
	vault := workspaceVault(t, "original")
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"source_meta":{"s1":{"source_file":"raw/source.md"},"s2":{"source_file":"raw/source.md"}}}`))
	err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run"]]`)
	if err == nil || !strings.Contains(err.Error(), "duplicate source mapping") {
		t.Fatalf("error=%v", err)
	}
}

func TestWorkspaceRequiresFirstCommandToBeRunBeforeLeaseOrExecution(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()

	for _, commands := range []string{
		`[["clear"],["run","--auto-approve"]]`,
		`[["approve","--all"]]`,
		`[["clear"],["approve","--all"]]`,
	} {
		t.Run(commands, func(t *testing.T) {
			vault := workspaceVault(t, "original")
			executed := false
			execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
				executed = true
				return nil
			}

			err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", ExecutionID: "invalid", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, commands)
			if err == nil || !strings.Contains(err.Error(), "requires the first olw command to be run") {
				t.Fatalf("error=%v", err)
			}
			if executed {
				t.Fatal("OLW executed for an invalid workspace batch")
			}
			for _, path := range []string{"cache/source_status.json", ".olw/lwc-worker-lease.json"} {
				if _, err := os.Stat(filepath.Join(vault, path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid batch wrote %s: %v", path, err)
				}
			}
		})
	}
}

func TestWorkspaceAcceptsProductionCommandContract(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	var got [][]string
	execOLW = func(_ context.Context, _ string, command []string, _ []string, _, _ io.Writer) error {
		got = append(got, append([]string(nil), command...))
		return nil
	}

	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run","--auto-approve"],["approve","--all"]]`); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"run", "--auto-approve"}, {"approve", "--all"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands=%q, want %q", got, want)
	}
}

func TestWorkspaceLeaseRejectsOverlapBeforeSnapshotOrPublish(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	started := make(chan struct{})
	release := make(chan struct{})
	execOLW = func(_ context.Context, _ string, _ []string, _ []string, _, _ io.Writer) error {
		close(started)
		<-release
		return nil
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "secret", ExecutionID: "first", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}
	firstDone := make(chan error, 1)
	go func() { firstDone <- runWorkerBatch(context.Background(), cfg, `[["run"]]`) }()
	<-started
	// Model an independently queued later run with a different source mapping.
	// It must be denied before it can snapshot, publish, or write an s2 receipt.
	mustWriteFile(t, filepath.Join(vault, "raw", "second.md"), []byte("second"))
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"source_meta":{"s2":{"source_file":"raw/second.md"}}}`))
	err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", ExecutionID: "second", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run"]]`)
	if err == nil || !strings.Contains(err.Error(), "vault lease is held") {
		t.Fatalf("overlap error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(vault, "cache", "source_status.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overlapping execution published receipt: %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if _, err := readSourceStatus(vault); err != nil {
		t.Fatalf("first receipt missing: %v", err)
	}
	status, err := readSourceStatus(vault)
	if err != nil || status.Sources["s1"].LastIngestFingerprint == "" {
		t.Fatalf("first receipt=%+v err=%v", status.Sources, err)
	}
	if _, exists := status.Sources["s2"]; exists {
		t.Fatalf("overlapping later run wrote s2 receipt: %+v", status.Sources["s2"])
	}
}

func TestStagePublishMirrorsWikiAndExcludesUnownedFiles(t *testing.T) {
	vault := workspaceVault(t, "stored raw")
	mustWriteFile(t, filepath.Join(vault, "wiki", "stale.md"), []byte("stale"))
	mustWriteFile(t, filepath.Join(vault, "cache", "annotations", "s1.json"), []byte("keep annotation"))
	mustWriteFile(t, filepath.Join(vault, "cache", "unknown.json"), []byte("keep unknown"))
	mustWriteFile(t, filepath.Join(vault, ".olw", "other.db"), []byte("keep other"))
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "raw", "source.md"), []byte("workspace raw"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "current.md"), []byte("current"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte("id map"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "annotations", "s1.json"), []byte("must not copy"))
	mustWriteFile(t, filepath.Join(workspace, "cache", "unknown.json"), []byte("must not copy"))
	mustWriteFile(t, filepath.Join(workspace, ".olw", "state.db"), []byte("state"))
	mustWriteFile(t, filepath.Join(workspace, ".olw", "pipeline.lock"), []byte("must not copy"))
	if err := syncWorkspaceOutputs(workspace, vault, ""); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"wiki/stale.md", ".olw/pipeline.lock"} {
		if _, err := os.Stat(filepath.Join(vault, absent)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected synced file %s: %v", absent, err)
		}
	}
	for path, want := range map[string]string{"raw/source.md": "stored raw", "cache/annotations/s1.json": "keep annotation", "cache/unknown.json": "keep unknown", ".olw/other.db": "keep other", "wiki/current.md": "current", ".olw/state.db": "state"} {
		data, err := os.ReadFile(filepath.Join(vault, path))
		if err != nil || string(data) != want {
			t.Fatalf("%s=%q err=%v, want %q", path, data, err, want)
		}
	}
}

func TestPublishRollbackPreservesPriorGenerationOnRenameError(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki", "page.md"), []byte("old"))
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "wiki", "page.md"), []byte("new"))
	stage, err := stageWorkspaceOutputs(workspace, vault, "")
	if err != nil {
		t.Fatal(err)
	}
	oldRename := publishRename
	defer func() { publishRename = oldRename }()
	publishRename = func(root *os.Root, oldName, newName string) error {
		if strings.HasSuffix(oldName, "/wiki") && newName == "wiki" {
			return errors.New("injected rename failure")
		}
		return root.Rename(oldName, newName)
	}
	if err := publishStagedOutputs(vault, stage); err == nil {
		t.Fatal("publish succeeded")
	}
	data, err := os.ReadFile(filepath.Join(vault, "wiki", "page.md"))
	if err != nil || string(data) != "old" {
		t.Fatalf("prior generation not restored: %q err=%v", data, err)
	}
}

func TestRecoverCommittedPublishPreservesNewGeneration(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki", "page.md"), []byte("new"))
	mustWriteFile(t, filepath.Join(vault, ".lwc-worker-backup-crash", "wiki", "page.md"), []byte("old"))
	if err := os.Mkdir(filepath.Join(vault, ".lwc-worker-stage-crash"), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := publishJournalRecord{
		Stage: ".lwc-worker-stage-crash", Backup: ".lwc-worker-backup-crash", Phase: publishPhaseCommitted,
		Entries: []publishEntry{{Destination: "wiki", Stage: ".lwc-worker-stage-crash/wiki", Backup: ".lwc-worker-backup-crash/wiki", HadOld: true, Published: true}},
	}
	if err := writePublishJournal(vault, journal); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedPublish(vault); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(vault, "wiki", "page.md"))
	if err != nil || string(data) != "new" {
		t.Fatalf("published generation changed after committed recovery: %q err=%v", data, err)
	}
	for _, name := range []string{publishJournal, journal.Stage, journal.Backup} {
		if _, err := os.Stat(filepath.Join(vault, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("committed recovery left %s: %v", name, err)
		}
	}
}

func TestRecoverInterruptedPublishRejectsMalformedJournalWithoutChanges(t *testing.T) {
	base := publishJournalRecord{
		Stage:   ".lwc-worker-stage-crash",
		Backup:  ".lwc-worker-backup-crash",
		Entries: []publishEntry{{Destination: "wiki", Stage: ".lwc-worker-stage-crash/wiki", Backup: ".lwc-worker-backup-crash/wiki", HadOld: true, Published: true}},
	}
	cases := []struct {
		name   string
		mutate func(*publishJournalRecord)
	}{
		{"raw destination", func(j *publishJournalRecord) { j.Entries[0].Destination = "raw" }},
		{"raw backup path", func(j *publishJournalRecord) { j.Entries[0].Backup = "raw" }},
		{"traversal backup path", func(j *publishJournalRecord) { j.Entries[0].Backup = ".lwc-worker-backup-crash/../raw" }},
		{"bad stage", func(j *publishJournalRecord) { j.Stage = "raw" }},
		{"duplicate destination", func(j *publishJournalRecord) { j.Entries = append(j.Entries, j.Entries[0]) }},
		{"invalid phase", func(j *publishJournalRecord) { j.Phase = "rollback" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			for path, data := range map[string]string{
				"raw/source.md":                 "raw",
				"cache/annotations/source.json": "annotation",
				"wiki/page.md":                  "wiki",
				"cache/id_map.json":             "cache",
				".olw/state.db":                 "state",
			} {
				mustWriteFile(t, filepath.Join(vault, path), []byte(data))
			}
			journal := base
			journal.Entries = append([]publishEntry(nil), base.Entries...)
			tc.mutate(&journal)
			data, err := json.Marshal(journal)
			if err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, filepath.Join(vault, publishJournal), data)
			if err := recoverInterruptedPublish(vault); err == nil {
				t.Fatal("recovery succeeded")
			}
			for path, want := range map[string]string{
				"raw/source.md":                 "raw",
				"cache/annotations/source.json": "annotation",
				"wiki/page.md":                  "wiki",
				"cache/id_map.json":             "cache",
				".olw/state.db":                 "state",
			} {
				got, err := os.ReadFile(filepath.Join(vault, path))
				if err != nil || string(got) != want {
					t.Fatalf("%s=%q err=%v, want %q", path, got, err, want)
				}
			}
		})
	}
}

func TestCommittedCleanupFailurePreservesNewGeneration(t *testing.T) {
	vault := t.TempDir()
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki", "page.md"), []byte("old"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "page.md"), []byte("new"))
	stage, err := stageWorkspaceOutputs(workspace, vault, "")
	if err != nil {
		t.Fatal(err)
	}
	oldRemoveAll := publishRemoveAll
	defer func() { publishRemoveAll = oldRemoveAll }()
	publishRemoveAll = func(root *os.Root, name string) error {
		if strings.HasPrefix(name, ".lwc-worker-backup-") {
			return errors.New("injected cleanup failure")
		}
		return root.RemoveAll(name)
	}
	err = publishStagedOutputs(vault, stage)
	publishRemoveAll = oldRemoveAll
	if err == nil || !strings.Contains(err.Error(), "injected cleanup failure") {
		t.Fatalf("publish error=%v", err)
	}
	data, readErr := os.ReadFile(filepath.Join(vault, "wiki", "page.md"))
	if readErr != nil || string(data) != "new" {
		t.Fatalf("cleanup error rolled back new generation: %q err=%v", data, readErr)
	}
	if err := recoverInterruptedPublish(vault); err != nil {
		t.Fatal(err)
	}
	data, readErr = os.ReadFile(filepath.Join(vault, "wiki", "page.md"))
	if readErr != nil || string(data) != "new" {
		t.Fatalf("committed recovery rolled back new generation: %q err=%v", data, readErr)
	}
}

func TestPublishRejectsDestinationSymlink(t *testing.T) {
	vault := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(vault, "wiki")); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "wiki", "page.md"), []byte("new"))
	if err := syncWorkspaceOutputs(workspace, vault, ""); err == nil || !strings.Contains(err.Error(), "destination symlink") {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(external, "page.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrote through destination symlink: %v", err)
	}
}

func TestWorkspaceFailurePublishesCappedRedactedLog(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	execOLW = func(_ context.Context, _ string, _ []string, _ []string, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, "key=very-secret")
		return errors.New("OLW failed")
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "very-secret", ExecutionID: "failed", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}
	if err := runWorkerBatch(context.Background(), cfg, `[["run"]]`); err == nil {
		t.Fatal("run succeeded")
	}
	data, err := os.ReadFile(filepath.Join(vault, "cache", "pipeline-failed.log"))
	if err != nil || strings.Contains(string(data), "very-secret") || !strings.Contains(string(data), "[REDACTED]") {
		t.Fatalf("failure log=%q err=%v", data, err)
	}
}

func TestWorkspaceFailureLogPublishesWhenOtherWorkspaceOutputIsInvalid(t *testing.T) {
	vault := t.TempDir()
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "cache", "pipeline-failed.log"), []byte("key=very-secret"))
	if err := os.Symlink(t.TempDir(), filepath.Join(workspace, "wiki")); err != nil {
		t.Fatal(err)
	}
	if err := publishWorkspaceFailureLog(workspace, vault, workerConfig{ExecutionID: "failed", APIKey: "very-secret"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(vault, "cache", "pipeline-failed.log"))
	if err != nil || string(data) != "key=[REDACTED]" {
		t.Fatalf("failure log=%q err=%v", data, err)
	}
}

func TestCappedRedactingWriterCapsAndRedacts(t *testing.T) {
	var output bytes.Buffer
	writer := &cappedRedactingWriter{writer: &output, secrets: []string{"secret"}, limit: 11}
	if _, err := writer.Write([]byte("secret-123456789")); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "[REDACTED]-" {
		t.Fatalf("log=%q, want capped redaction", got)
	}
}

func TestDiagnosticSinkRedactsSplitAlternatingOutputAndArguments(t *testing.T) {
	var output bytes.Buffer
	sink := newDiagnosticSink([]io.Writer{&output}, []string{"api-secret", "user-secret", "project-secret", "/tmp/private", "--secret-arg"})
	for _, part := range []string{strings.Repeat("safe", 3000), "api-", "secret user-", "secret project-", "secret /tmp/", "private --secret-", "arg"} {
		if _, err := sink.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, raw := range []string{"api-secret", "user-secret", "project-secret", "/tmp/private", "--secret-arg"} {
		if strings.Contains(text, raw) {
			t.Fatalf("diagnostic leaked %q: %q", raw, text)
		}
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("diagnostic was not redacted: %q", text)
	}
}

func TestWorkerCommandErrorsAreFixedAndSilent(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"run", `[["run"]]`, "payload-secret"}, {"--stop-on-error=not-a-bool", "run", `[["run"]]`}, {"unknown-secret-command"}} {
		cmd := newRootCommand()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(args)
		err := executeWorkerCommand(cmd)
		if err == nil || err.Error() != "worker command rejected" {
			t.Fatalf("args=%q error=%v", args, err)
		}
		if output.Len() != 0 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("args=%q output=%q error=%v", args, output.String(), err)
		}
	}
}

func TestConfigEnvironmentDoesNotBecomeFlagDefaultAndExplicitWins(t *testing.T) {
	t.Setenv("BUCKET", "env-bucket")
	t.Setenv("USER_ID", "env-user")
	t.Setenv("PROJECT_ID", "env-project")
	cfg := configFromEnvironment(workerConfig{Bucket: "flag-bucket", UserID: "flag-user", ProjectID: "flag-project"})
	if cfg.Bucket != "flag-bucket" || cfg.UserID != "flag-user" || cfg.ProjectID != "flag-project" {
		t.Fatalf("explicit config lost: %+v", cfg)
	}
	cmd := newRootCommand()
	if f := cmd.PersistentFlags().Lookup("bucket"); f == nil || f.DefValue != "" {
		t.Fatalf("bucket default=%v", f)
	}
}

func TestCloudConfigIgnoresInheritedLocalRoutingAndHonorsExplicitFalse(t *testing.T) {
	t.Setenv("VAULT_PATH", "/mounted/vault")
	t.Setenv("DATA_DIR", "/data")
	t.Setenv("WORKSPACE", "true")
	got := configFromEnvironment(workerConfig{Bucket: "bucket"})
	if got.VaultPath != "" || got.DataDir != "" || got.Workspace {
		t.Fatalf("cloud inherited local routing: %+v", got)
	}
	got = configFromEnvironment(workerConfig{Bucket: "bucket", Workspace: false, workspaceSet: true})
	if got.Workspace {
		t.Fatalf("explicit workspace=false lost to env: %+v", got)
	}
	got = configFromEnvironment(workerConfig{Bucket: "bucket", VaultPath: "/explicit", vaultSet: true})
	if got.VaultPath != "/explicit" {
		t.Fatalf("explicit vault lost: %+v", got)
	}
}

func TestExplicitEmptyAndFalseFlagsSuppressInheritedEnvironment(t *testing.T) {
	t.Setenv("BUCKET", "inherited-bucket")
	t.Setenv("LLM_API_KEY", "inherited-api-key")
	t.Setenv("USER_ID", "inherited-user")
	t.Setenv("PROJECT_ID", "inherited-project")
	t.Setenv("EXECUTION_ID", "inherited-execution")
	t.Setenv("CLOUD_RUN_EXECUTION", "cloud-execution-sentinel")
	t.Setenv("WORKSPACE_DIR", "/inherited/workspace")
	t.Setenv("WORKSPACE", "true")
	t.Setenv("VAULT_PATH", "/inherited/vault")
	t.Setenv("DATA_DIR", "/inherited/data")

	got := configFromEnvironment(workerConfig{
		bucketSet: true, apiKeySet: true, userIDSet: true, projectIDSet: true,
		executionIDSet: true, workspaceDirSet: true, workspaceSet: true,
		vaultSet: true, dataDirSet: true,
	})
	if got.Bucket != "" || got.APIKey != "" || got.UserID != "" || got.ProjectID != "" || got.ExecutionID != "" || got.WorkspaceDir != "" || got.VaultPath != "" || got.DataDir != "" || got.Workspace {
		t.Fatalf("explicit empty/false flags were replaced by environment: %+v", got)
	}
}

func TestWorkspaceConcurrentAnnotationRemainsDirtyAndIsNotSyncedBack(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	writeWorkspaceAnnotation(t, vault, "s1", "raw/source.md", "start")
	execOLW = func(_ context.Context, _ string, _ []string, _ []string, _, _ io.Writer) error {
		writeWorkspaceAnnotation(t, vault, "s1", "raw/source.md", "concurrent")
		return nil
	}
	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run"]]`); err != nil {
		t.Fatal(err)
	}
	artifact, _ := readSourceStatus(vault)
	if got := artifact.Sources["s1"].LastIngestedAnnSHA256; got != annotation.Digest("start") {
		t.Fatalf("receipt ann=%s, want start snapshot", got)
	}
	ann, err := readAnnotation(vault, "s1", "raw/source.md")
	if err != nil || ann.Body != "concurrent" {
		t.Fatalf("annotation=%+v err=%v", ann, err)
	}
}

func TestWorkspaceConcurrentRawChangeKeepsStartReceipt(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	writeWorkspaceAnnotation(t, vault, "s1", "raw/source.md", "note")
	var materialized string
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		data, err := os.ReadFile(filepath.Join(work, "raw", "source.md"))
		materialized = string(data)
		mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte("concurrent raw"))
		return err
	}
	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run"]]`); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(materialized, "original\n\n---") {
		t.Fatalf("workspace input=%q, want start raw", materialized)
	}
	artifact, _ := readSourceStatus(vault)
	if got := artifact.Sources["s1"].LastIngestedRawSHA256; got != sha256Text("original") {
		t.Fatalf("receipt raw=%s, want start raw", got)
	}
	if raw, _ := os.ReadFile(filepath.Join(vault, "raw", "source.md")); string(raw) != "concurrent raw" {
		t.Fatalf("concurrent raw was overwritten: %q", raw)
	}
}

func TestWorkspaceFailurePreservesLastSuccessAndRecordsFailure(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	prior := sourcestatus.Receipt{RawPath: "raw/source.md", LastIngestedRawSHA256: "oldraw", LastIngestedAnnSHA256: "oldann", LastIngestFingerprint: sourcestatus.Fingerprint("oldraw", "oldann"), LastSuccessAt: time.Now().UTC().Format(time.RFC3339)}
	writeWorkspaceStatus(t, vault, prior)
	execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
		return errors.New("OLW failed")
	}
	workspaceDir := t.TempDir()
	err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: workspaceDir, Postprocess: true}, `[["run"]]`)
	if err == nil {
		t.Fatal("run error=nil")
	}
	artifact, _ := readSourceStatus(vault)
	got := artifact.Sources["s1"]
	if got.LastIngestFingerprint != prior.LastIngestFingerprint || got.FailedFingerprint == "" || got.Error == "" {
		t.Fatalf("receipt=%+v", got)
	}
	if raw, _ := os.ReadFile(filepath.Join(vault, "raw", "source.md")); string(raw) != "original" {
		t.Fatalf("stored raw=%q", raw)
	}
	if entries, err := os.ReadDir(workspaceDir); err != nil || len(entries) != 0 {
		t.Fatalf("workspace entries=%v err=%v", entries, err)
	}
}

func TestWorkspaceRejectsUnsafeMappingsAndCleansUp(t *testing.T) {
	vault := workspaceVault(t, "original")
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"source_meta":{"s1":{"source_file":"raw/../escape.md"}}}`))
	workspaceDir := t.TempDir()
	err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: workspaceDir, Postprocess: true}, `[["run"]]`)
	if err == nil || !strings.Contains(err.Error(), "unsafe source mapping") {
		t.Fatalf("error=%v", err)
	}
	entries, err := os.ReadDir(workspaceDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace entries=%v err=%v", entries, err)
	}
}

func TestWorkspaceRejectsRawSymlinkEscapeAndMalformedAnnotation(t *testing.T) {
	vault := workspaceVault(t, "original")
	external := filepath.Join(t.TempDir(), "outside.md")
	mustWriteFile(t, external, []byte("outside"))
	if err := os.Remove(filepath.Join(vault, "raw", "source.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(vault, "raw", "source.md")); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run"]]`); err == nil {
		t.Fatal("symlink escape was accepted")
	}

	vault = workspaceVault(t, "original")
	mustWriteFile(t, filepath.Join(vault, "cache", "annotations", "s1.json"), []byte(`{"invalid":true}`))
	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run"]]`); err == nil || !strings.Contains(err.Error(), "invalid annotation") {
		t.Fatalf("malformed annotation error=%v", err)
	}

	vault = workspaceVault(t, "original")
	mustWriteFile(t, filepath.Join(vault, "cache", "source_status.json"), []byte(`{"sources":`))
	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run"]]`); err == nil || !strings.Contains(err.Error(), "invalid source status") {
		t.Fatalf("malformed source status error=%v", err)
	}
}

func TestNoWorkspaceRunsAgainstOriginalVault(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original")
	var got string
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		got = work
		return nil
	}
	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: false, Postprocess: false}, `[["run"]]`); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(vault)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("OLW vault=%q, want %q", got, want)
	}
}

func TestBucketConfigurationRejectsVaultAndMountedRoutingBeforeChild(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  workerConfig
		env  map[string]string
	}{
		{"explicit vault", workerConfig{VaultPath: t.TempDir(), APIKey: "secret", ExecutionID: "exec-1", Postprocess: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BUCKET", "bucket")
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			called := false
			old := execOLW
			defer func() { execOLW = old }()
			execOLW = func(context.Context, string, []string, []string, io.Writer, io.Writer) error {
				called = true
				return nil
			}
			err := runWorkerBatch(context.Background(), tc.cfg, `[["run"]]`)
			if err == nil || err.Error() != "worker configuration is invalid" || called {
				t.Fatalf("error=%v child=%v", err, called)
			}
		})
	}
}

func TestRecordFailurePersistsOnlySafeErrorAndKeepsSuccessFields(t *testing.T) {
	vault := workspaceVault(t, "raw")
	prior := sourcestatus.Receipt{RawPath: "raw/source.md", LastIngestFingerprint: "prior", LastSuccessAt: "2026-01-01T00:00:00Z"}
	writeWorkspaceStatus(t, vault, prior)
	snapshot := sourceSnapshot{SourceID: "s1", RawPath: "raw/source.md", Fingerprint: "failed"}
	sentinel := errors.New("tenant-secret /private/path object-key --argument")
	if err := recordFailure(vault, []sourceSnapshot{snapshot}, fmt.Errorf("wrapped: %w", sentinel)); err != nil {
		t.Fatal(err)
	}
	artifact, err := readSourceStatus(vault)
	if err != nil {
		t.Fatal(err)
	}
	got := artifact.Sources["s1"]
	if got.Error != "pipeline failed" || got.LastSuccessAt != prior.LastSuccessAt || got.LastIngestFingerprint != prior.LastIngestFingerprint {
		t.Fatalf("receipt=%+v", got)
	}
	data, err := os.ReadFile(filepath.Join(vault, filepath.FromSlash(sourcestatus.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), sentinel.Error()) {
		t.Fatalf("unsafe failure persisted: %s", data)
	}
}

func TestWorkspaceSuccessiveGenerationRunsPreserveSourceIDAndAnnotation(t *testing.T) {
	old := execOLW
	defer func() { execOLW = old }()
	vault := workspaceVault(t, "original raw")
	writeWorkspaceAnnotation(t, vault, "s1", "raw/source.md", "saved note")
	mustWriteFile(t, filepath.Join(vault, "wiki", "sources", "s1.md"), []byte("---\nid: s1\nsource_file: raw/source.md\n---\nbody\n"))
	var run int
	execOLW = func(_ context.Context, work string, _ []string, _ []string, _, _ io.Writer) error {
		run++
		transientID := fmt.Sprintf("transient-%d", run)
		if err := os.RemoveAll(filepath.Join(work, "wiki", "sources")); err != nil {
			return err
		}
		mustWriteFile(t, filepath.Join(work, "wiki", "sources", transientID+".md"), []byte("---\nid: "+transientID+"\nsource_file: raw/source.md\n---\nregenerated\n"))
		return nil
	}
	cfg := workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}
	for i := 0; i < 2; i++ {
		if err := runWorkerBatch(context.Background(), cfg, `[["run"]]`); err != nil {
			t.Fatal(err)
		}
	}
	idMap, err := os.ReadFile(filepath.Join(vault, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(idMap), `"s1"`) || strings.Contains(string(idMap), `"transient-2":`) {
		t.Fatalf("source ID drifted in id map: %s", idMap)
	}
	page, err := os.ReadFile(filepath.Join(vault, "wiki", "sources", "transient-2.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(page), "<!-- lwc-ann-v1 source_id=s1 ") != 1 || !strings.Contains(string(page), "saved note") {
		t.Fatalf("stable annotation was not materialized exactly once: %s", page)
	}
	annotationData, err := os.ReadFile(filepath.Join(vault, filepath.FromSlash(annotation.Path("s1"))))
	if err != nil || !strings.Contains(string(annotationData), `"source_id":"s1"`) {
		t.Fatalf("stable annotation object was not retained: %s err=%v", annotationData, err)
	}
	if _, err := os.Stat(filepath.Join(vault, filepath.FromSlash(annotation.Path("transient-2")))); !os.IsNotExist(err) {
		t.Fatalf("annotation was copied to transient source ID: %v", err)
	}
}

func workspaceVault(t *testing.T, raw string) string {
	t.Helper()
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "source.md"), []byte(raw))
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"source_meta":{"s1":{"source_file":"raw/source.md"}}}`))
	return vault
}

func writeWorkspaceAnnotation(t *testing.T, vault, sourceID, rawPath, body string) {
	t.Helper()
	object := annotation.Object{Version: 1, SourceID: sourceID, RawPath: rawPath, Body: body, SHA256: annotation.Digest(body), UpdatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedBy: "tester"}
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(vault, filepath.FromSlash(annotation.Path(sourceID))), data)
}

func writeWorkspaceStatus(t *testing.T, vault string, receipt sourcestatus.Receipt) {
	t.Helper()
	data, err := json.Marshal(sourcestatus.Artifact{Version: 1, Sources: map[string]sourcestatus.Receipt{"s1": receipt}})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(vault, filepath.FromSlash(sourcestatus.Path)), data)
}

func sha256Text(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", sum[:])
}
