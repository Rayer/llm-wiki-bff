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
	"github.com/rayer/llm-wiki-bff/internal/suggestedqueries"
)

type testSuggestedQueryProvider struct {
	calls int
	user  string
	raw   string
	err   error
}

func (p *testSuggestedQueryProvider) Chat(_ context.Context, _, user string) (string, error) {
	p.calls++
	p.user = user
	return p.raw, p.err
}

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
	if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err == nil {
		t.Fatal("--init was accepted despite the single-command contract")
	}
	if len(ran) != 0 {
		t.Fatalf("unsafe --init reached child: %#v", ran)
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

	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
	err := runOLWBatch(context.Background(), vault, [][]string{{"fail"}, {"second"}}, true, nil, nil, nil)
	if err == nil {
		t.Fatal("unsafe batch was accepted")
	}
	if len(ran) != 0 {
		t.Fatalf("unsafe batch reached child: %#v", ran)
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

	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
	err := runOLWBatch(context.Background(), vault, [][]string{{"fail"}, {"second"}}, false, nil, nil, nil)
	if err == nil {
		t.Fatal("unsafe batch was accepted")
	}
	if len(ran) != 0 {
		t.Fatalf("unsafe batch reached child: %#v", ran)
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

	if _, err := os.Stat(filepath.Join(vault, "cache", "pipeline-olw-pipeline-abc123.log")); !os.IsNotExist(err) {
		t.Fatalf("no-postprocess private run published a pipeline log: %v", err)
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

	provider := &testSuggestedQueryProvider{raw: `{"candidates":[
{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["alpha-id","beta-id"]},
{"question":"如何探索這個主題的不同面向？","intent/use_case":"exploration","corpus_anchor_concept_ids":["alpha-id"]},
{"question":"哪些選擇適合進一步查找？","intent/use_case":"retrieval","corpus_anchor_concept_ids":["beta-id"]}
]}`}
	if err := runPostprocessWithProvider(context.Background(), vault, provider, nil); err != nil {
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
	if len(artifact.Queries) != 3 {
		t.Fatalf("queries = %#v, want 3 entries", artifact.Queries)
	}
	if artifact.Queries[0] != "哪些概念值得一起比較？" || provider.calls != 1 {
		t.Fatalf("queries[0] = %q, provider calls = %d", artifact.Queries[0], provider.calls)
	}
	if artifact.UpdatedAt == "" {
		t.Fatal("updated_at is empty")
	}
}

func TestSuggestedQueryGenerationDoesNotReadVaultRootIndexAsDescription(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "index.md"), []byte("SYSTEM_INDEX_MUST_NOT_REACH_SUGGESTED_QUERY_PROVIDER"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: alpha-id\ntitle: Alpha\n---\nAlpha"))
	provider := &testSuggestedQueryProvider{raw: `{"candidates":[
{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["alpha-id"]},
{"question":"如何探索這個主題的不同面向？","intent/use_case":"exploration","corpus_anchor_concept_ids":["alpha-id"]},
{"question":"哪些選擇適合進一步查找？","intent/use_case":"retrieval","corpus_anchor_concept_ids":["alpha-id"]}
]}`}
	if err := runPostprocessWithProvider(context.Background(), vault, provider, nil); err != nil {
		t.Fatalf("runPostprocessWithProvider() error = %v", err)
	}
	if strings.Contains(provider.user, "SYSTEM_INDEX_MUST_NOT_REACH_SUGGESTED_QUERY_PROVIDER") {
		t.Fatalf("provider user payload contains vault root index.md content: %q", provider.user)
	}
}

func TestSuggestedQueryGenerationFailurePreservesLastKnownGoodBytes(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: alpha-id\ntitle: Alpha\n---\nAlpha"))
	prior := []byte(`{"version":2,"queries":["哪些概念值得一起比較？","如何探索這個主題的不同面向？","哪些選擇適合進一步查找？"],"candidates":[{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["alpha-id"],"generation":{"model":"fixture","prompt_version":"v1"}},{"question":"如何探索這個主題的不同面向？","intent/use_case":"exploration","corpus_anchor_concept_ids":["alpha-id"],"generation":{"model":"fixture","prompt_version":"v1"}},{"question":"哪些選擇適合進一步查找？","intent/use_case":"retrieval","corpus_anchor_concept_ids":["alpha-id"],"generation":{"model":"fixture","prompt_version":"v1"}}],"updated_at":"2026-07-28T00:00:00Z"}`)
	mustWriteFile(t, filepath.Join(vault, "cache", "suggested_queries.json"), prior)
	provider := &testSuggestedQueryProvider{err: errors.New("provider unavailable")}
	if err := runPostprocessWithProvider(context.Background(), vault, provider, nil); err != nil {
		t.Fatalf("runPostprocessWithProvider() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(vault, "cache", "suggested_queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, prior) {
		t.Fatalf("suggested query artifact changed on provider failure: got %q, want byte-identical prior", got)
	}
}

func TestSuggestedQueryGenerationFailureWritesValidEmptyV2WhenAbsent(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: alpha-id\ntitle: Alpha\n---\nAlpha"))
	provider := &testSuggestedQueryProvider{err: errors.New("provider unavailable")}
	if err := runPostprocessWithProvider(context.Background(), vault, provider, nil); err != nil {
		t.Fatalf("runPostprocessWithProvider() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(vault, suggestedqueries.Path))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := suggestedqueries.Decode(data)
	if err != nil {
		t.Fatalf("empty fallback is not valid v2 JSON: %v", err)
	}
	if artifact.Version != 2 || artifact.Queries == nil || len(artifact.Queries) != 0 || artifact.Candidates == nil || len(artifact.Candidates) != 0 {
		t.Fatalf("empty fallback = %#v, want valid empty v2 artifact", artifact)
	}
}

func TestSuggestedQueriesStageOnlyRewritesQueryChips(t *testing.T) {
	vault := t.TempDir()
	idMap := []byte(`{"concept":{"alpha-id":"alpha"},"source":{},"redirects":{}}`)
	concepts := []byte(`{"id":"alpha-id","slug":"alpha","title":"Alpha","body":"Alpha","frontmatter":{"id":"alpha-id","title":"Alpha"}}` + "\n")
	priorQueries := []byte(`{"version":2,"queries":["舊的 query"],"candidates":[{"question":"舊的 query","intent/use_case":"exploration","corpus_anchor_concept_ids":["alpha-id"],"generation":{"model":"fixture","prompt_version":"v1"}}],"updated_at":"2026-07-01T00:00:00Z"}`)
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), idMap)
	mustWriteFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), concepts)
	mustWriteFile(t, filepath.Join(vault, "cache", "suggested_queries.json"), priorQueries)
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: alpha-id\ntitle: Alpha\n---\nAlpha"))

	provider := &testSuggestedQueryProvider{raw: `{"candidates":[
{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["alpha-id"]},
{"question":"如何探索這個主題的不同面向？","intent/use_case":"exploration","corpus_anchor_concept_ids":["alpha-id"]},
{"question":"哪些選擇適合進一步查找？","intent/use_case":"retrieval","corpus_anchor_concept_ids":["alpha-id"]}
]}`}
	if err := runSuggestedQueriesStage(context.Background(), vault, provider); err != nil {
		t.Fatalf("runSuggestedQueriesStage() error = %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}

	gotMap, err := os.ReadFile(filepath.Join(vault, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotMap, idMap) {
		t.Fatalf("id_map.json changed: got %s", gotMap)
	}
	gotConcepts, err := os.ReadFile(filepath.Join(vault, "cache", "concepts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotConcepts, concepts) {
		t.Fatalf("concepts.jsonl changed: got %s", gotConcepts)
	}
	gotQueries, err := os.ReadFile(filepath.Join(vault, "cache", "suggested_queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(gotQueries, priorQueries) {
		t.Fatal("suggested_queries.json was not regenerated")
	}
	artifact, err := suggestedqueries.Decode(gotQueries)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Queries) != 3 || artifact.Queries[0] != "哪些概念值得一起比較？" {
		t.Fatalf("queries = %#v", artifact.Queries)
	}
}

func TestCacheIndexStageDoesNotTouchSuggestedQueries(t *testing.T) {
	vault := t.TempDir()
	priorQueries := []byte(`{"version":2,"queries":["保留的 chip"],"candidates":[{"question":"保留的 chip","intent/use_case":"exploration","corpus_anchor_concept_ids":["alpha-id"],"generation":{"model":"fixture","prompt_version":"v1"}}],"updated_at":"2026-07-01T00:00:00Z"}`)
	mustWriteFile(t, filepath.Join(vault, "cache", "suggested_queries.json"), priorQueries)
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: a3f7b2c01d9d\ntitle: Alpha\n---\nAlpha"))
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\n"))
	mustWriteFile(t, filepath.Join(vault, ".synto", "INDEX.json"), []byte(syntoIndexFixture("a3f7b2c01d9d", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", false)))
	writeValidSQLiteState(t, filepath.Join(vault, ".synto", "state.db"))

	if err := runCacheIndexStage(context.Background(), vault, nil); err != nil {
		t.Fatalf("runCacheIndexStage() error = %v", err)
	}
	gotQueries, err := os.ReadFile(filepath.Join(vault, "cache", "suggested_queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotQueries, priorQueries) {
		t.Fatalf("suggested_queries.json changed during cache/index stage: %s", gotQueries)
	}
	if _, err := os.Stat(filepath.Join(vault, "cache", "id_map.json")); err != nil {
		t.Fatalf("id_map.json missing after cache/index stage: %v", err)
	}
}

func TestSuggestedQueriesCommandPublishesChipsWithoutIndexRebuild(t *testing.T) {
	vault := t.TempDir()
	// Use content-stable concept id so a full index rebuild would rewrite id_map;
	// the suggested-queries command must leave the hand-authored map untouched.
	idMap := []byte(`{"concept":{"a3f7b2c01d9d":"alpha"},"source":{},"redirects":{},"concept_entity_id":{"a3f7b2c01d9d":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}}`)
	concepts := []byte(`{"id":"a3f7b2c01d9d","slug":"alpha","title":"Alpha","body":"Alpha","frontmatter":{"id":"a3f7b2c01d9d","title":"Alpha"}}` + "\n")
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), idMap)
	mustWriteFile(t, filepath.Join(vault, "cache", "concepts.jsonl"), concepts)
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: a3f7b2c01d9d\ntitle: Alpha\n---\nAlpha"))
	mustWriteFile(t, filepath.Join(vault, "cache", "dormant_concepts.jsonl"), nil)
	mustWriteFile(t, filepath.Join(vault, "cache", "raw_status.json"), []byte(`{"version":1,"files":{},"file_count":0}`))
	mustWriteFile(t, filepath.Join(vault, "cache", "suggested_queries.json"), []byte(`{"version":2,"queries":[],"candidates":[],"updated_at":"2026-07-01T00:00:00Z"}`))
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
	mustWriteFile(t, filepath.Join(vault, ".synto", "INDEX.json"), []byte(syntoIndexFixture("a3f7b2c01d9d", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", false)))
	writeValidSQLiteState(t, filepath.Join(vault, ".synto", "state.db"))

	provider := &testSuggestedQueryProvider{raw: `{"candidates":[
{"question":"哪些概念值得一起比較？","intent/use_case":"comparison","corpus_anchor_concept_ids":["a3f7b2c01d9d"]},
{"question":"如何探索這個主題的不同面向？","intent/use_case":"exploration","corpus_anchor_concept_ids":["a3f7b2c01d9d"]},
{"question":"哪些選擇適合進一步查找？","intent/use_case":"retrieval","corpus_anchor_concept_ids":["a3f7b2c01d9d"]}
]}`}
	cfg := workerConfig{
		VaultPath:                vault,
		ExecutionID:              "suggest-only-1",
		SuggestedQueries:         true,
		suggestedQueriesProvider: provider,
	}
	if err := runSuggestedQueriesCommand(context.Background(), cfg); err != nil {
		t.Fatalf("runSuggestedQueriesCommand() error = %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	gotMap, err := os.ReadFile(filepath.Join(vault, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotMap, idMap) {
		t.Fatalf("command rewrote id_map.json: %s", gotMap)
	}
	gotQueries, err := os.ReadFile(filepath.Join(vault, "cache", "suggested_queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := suggestedqueries.Decode(gotQueries)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Queries) != 3 {
		t.Fatalf("queries = %#v, want 3", artifact.Queries)
	}
}

func TestSuggestedQueriesCommandRejectsCloudWithLocalRouting(t *testing.T) {
	err := runSuggestedQueriesCommand(context.Background(), workerConfig{
		Bucket:    "some-bucket",
		UserID:    "u",
		ProjectID: "p",
		VaultPath: "/tmp/vault",
		vaultSet:  true,
	})
	if err == nil || !errors.Is(err, errWorkerConfigInvalid) {
		t.Fatalf("error = %v, want errWorkerConfigInvalid", err)
	}
}

func TestWorkerPostprocessDirectPreservesDormantConceptAndEntityMappings(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: a3f7b2c01d9d\n---\nAlpha"))
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"a3f7b2c01d9d":"alpha"},"dormant_concept":{"stable-beta":"beta"},"concept_entity_id":{"a3f7b2c01d9d":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","stable-beta":"01JAZ5N7Y3K8M2Q4R6T9VWXABD","orphan":"01JAZ5N7Y3K8M2Q4R6T9VWXAC7"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
	mustWriteFile(t, filepath.Join(vault, ".synto", "INDEX.json"), []byte(syntoIndexFixture("a3f7b2c01d9d", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", false)))
	writeValidSQLiteState(t, filepath.Join(vault, ".synto", "state.db"))

	if err := runPostprocessCommand(context.Background(), workerConfig{VaultPath: vault, ExecutionID: "direct-r1"}); err != nil {
		t.Fatalf("runPostprocessCommand() error = %v", err)
	}
	ids := mustSnapshotIDMap(t, vault)
	if ids.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXABC"] != "alpha" || len(ids.Concept) != 1 || len(ids.DormantConcept) != 0 {
		t.Fatalf("postprocess direct entity maps = %#v", ids)
	}
	if len(ids.ConceptEntityID) != 0 {
		t.Fatalf("postprocess retained legacy entity map = %#v", ids.ConceptEntityID)
	}
	if _, ok := ids.ConceptEntityID["orphan"]; ok {
		t.Fatalf("postprocess retained orphan entity mapping: %#v", ids.ConceptEntityID)
	}
}

func TestWorkerPostprocessWorkspacePreservesDormantConceptAndEntityMappings(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: a3f7b2c01d9d\n---\nAlpha"))
	mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"concept":{"a3f7b2c01d9d":"alpha"},"dormant_concept":{"stable-beta":"beta"},"concept_entity_id":{"a3f7b2c01d9d":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","stable-beta":"01JAZ5N7Y3K8M2Q4R6T9VWXABD","orphan":"01JAZ5N7Y3K8M2Q4R6T9VWXAC7"},"source":{},"redirects":{}}`))
	mustWriteFile(t, filepath.Join(vault, "cache", "dormant_concepts.jsonl"), nil)
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\n"))
	mustWriteFile(t, filepath.Join(vault, ".synto", "INDEX.json"), []byte(syntoIndexFixture("a3f7b2c01d9d", "01JAZ5N7Y3K8M2Q4R6T9VWXABC", "alpha", true)))
	writeValidSQLiteState(t, filepath.Join(vault, ".synto", "state.db"))

	if err := runPostprocessCommand(context.Background(), workerConfig{VaultPath: vault, Workspace: true, WorkspaceDir: t.TempDir(), ExecutionID: "workspace-r1"}); err != nil {
		t.Fatalf("runPostprocessCommand(workspace) error = %v", err)
	}
	ids := mustSnapshotIDMap(t, vault)
	if ids.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXABC"] != "alpha" || len(ids.Concept) != 1 || len(ids.DormantConcept) != 0 {
		t.Fatalf("workspace postprocess direct entity maps = %#v", ids)
	}
	if len(ids.ConceptEntityID) != 0 {
		t.Fatalf("workspace postprocess retained legacy entity map = %#v", ids.ConceptEntityID)
	}
	if _, ok := ids.ConceptEntityID["orphan"]; ok {
		t.Fatalf("workspace postprocess retained orphan entity mapping: %#v", ids.ConceptEntityID)
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
		if err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`); err != nil {
			t.Fatal(err)
		}
		// In production OLW regenerates id_map with its source mappings. The fake
		// executor above does not, so retain this mapped-source fixture between
		// the two independent workspace runs.
		mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(`{"source_meta":{"s1":{"source_file":"raw/source.md"}}}`))
	}
	if len(materialized) != 2 || materialized[0] != materialized[1] {
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

func TestSnapshotSourcesLegacyFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		idMap     string
		pages     map[string]string
		raw       map[string]string
		wantError string
	}{
		{
			name:  "success uses authoritative source ID",
			idMap: `{"concept":{},"source":{"stable-source":"source"},"redirects":{}}`,
			pages: map[string]string{"source": "---\nid: not-the-stable-id\nsource_file: raw/source.md\n---\nsource\n"},
			raw:   map[string]string{"raw/source.md": "raw source"},
		},
		{
			name:      "missing page",
			idMap:     `{"source":{"stable-source":"source"}}`,
			wantError: "missing legacy source page",
		},
		{
			name:      "malformed frontmatter",
			idMap:     `{"source":{"stable-source":"source"}}`,
			pages:     map[string]string{"source": "not frontmatter\n"},
			wantError: "parse legacy source page",
		},
		{
			name:      "missing source_file",
			idMap:     `{"source":{"stable-source":"source"}}`,
			pages:     map[string]string{"source": "---\nid: source\n---\nsource\n"},
			wantError: "missing or unsafe legacy source_file",
		},
		{
			name:      "unsafe source_file",
			idMap:     `{"source":{"stable-source":"source"}}`,
			pages:     map[string]string{"source": "---\nsource_file: raw/../escape.md\n---\nsource\n"},
			wantError: "missing or unsafe legacy source_file",
		},
		{
			name:      "duplicate raw path",
			idMap:     `{"source":{"stable-a":"a","stable-b":"b"}}`,
			pages:     map[string]string{"a": "---\nsource_file: raw/same.md\n---\n", "b": "---\nsource_file: raw/same.md\n---\n"},
			wantError: "duplicate source mapping",
		},
		{
			name:      "duplicate source slug",
			idMap:     `{"source":{"stable-a":"same","stable-b":"same"}}`,
			pages:     map[string]string{"same": "---\nsource_file: raw/same.md\n---\n"},
			wantError: "duplicate legacy source slug",
		},
		{
			name:      "source and source metadata disagree",
			idMap:     `{"source":{"stable-source":"source"},"source_meta":{"stable-source":{"slug":"other","source_file":"raw/source.md"}}}`,
			wantError: "source metadata slug disagrees",
		},
		{
			name:      "mixed metadata duplicate raw path",
			idMap:     `{"source":{"stable-a":"a","stable-b":"b"},"source_meta":{"stable-a":{"slug":"a","source_file":"raw/same.md"}}}`,
			pages:     map[string]string{"b": "---\nsource_file: raw/same.md\n---\n"},
			wantError: "duplicate source mapping",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			for path, body := range tc.raw {
				mustWriteFile(t, filepath.Join(vault, filepath.FromSlash(path)), []byte(body))
			}
			for slug, page := range tc.pages {
				mustWriteFile(t, filepath.Join(vault, "wiki", "sources", slug+".md"), []byte(page))
			}
			mustWriteFile(t, filepath.Join(vault, "cache", "id_map.json"), []byte(tc.idMap))

			got, err := snapshotSources(vault)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("snapshotSources() error=%v, want substring %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("snapshotSources() error=%v", err)
			}
			if len(got) != 1 || got[0].SourceID != "stable-source" || got[0].RawPath != "raw/source.md" || string(got[0].RawBytes) != "raw source" || got[0].Tombstone || !got[0].Dirty {
				t.Fatalf("snapshotSources() = %#v", got)
			}
		})
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
			if err == nil || !strings.Contains(err.Error(), "worker input is invalid") {
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

	if err := runWorkerBatch(context.Background(), workerConfig{VaultPath: vault, APIKey: "secret", Workspace: true, WorkspaceDir: t.TempDir(), Postprocess: true}, `[["run","--auto-approve"]]`); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"run", "--auto-approve"}}
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
	writeValidSQLiteState(t, filepath.Join(workspace, ".olw", "state.db"))
	mustWriteFile(t, filepath.Join(workspace, ".olw", "pipeline.lock"), []byte("must not copy"))
	writeFreshSyntoRequiredOutputs(t, workspace)
	mustWriteFile(t, filepath.Join(workspace, "wiki.toml"), []byte("legacy"))
	writeValidSQLiteState(t, filepath.Join(workspace, ".olw", "state.db"))
	if err := syncWorkspaceOutputs(workspace, vault, ""); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"wiki/stale.md", ".olw/pipeline.lock"} {
		if _, err := os.Stat(filepath.Join(vault, absent)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected synced file %s: %v", absent, err)
		}
	}
	for path, want := range map[string]string{"raw/source.md": "stored raw", "cache/annotations/s1.json": "keep annotation", "cache/unknown.json": "keep unknown", ".olw/other.db": "keep other", "wiki/current.md": "current"} {
		data, err := os.ReadFile(filepath.Join(vault, path))
		if err != nil || string(data) != want {
			t.Fatalf("%s=%q err=%v, want %q", path, data, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(vault, ".olw", "state.db")); err != nil {
		t.Fatalf("legacy SQLite state missing: %v", err)
	}
}

func TestPublishRollbackPreservesPriorGenerationOnRenameError(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "wiki", "page.md"), []byte("old"))
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "wiki", "page.md"), []byte("new"))
	writeFreshSyntoRequiredOutputs(t, workspace)
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
	writeFreshSyntoRequiredOutputs(t, workspace)
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
	writeFreshSyntoRequiredOutputs(t, workspace)
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
	// Only credentials belong in the redaction set; identity/path stay visible.
	sink := newDiagnosticSink([]io.Writer{&output}, []string{"api-secret"})
	for _, part := range []string{strings.Repeat("safe", 3000), "api-", "secret user-id-ok /tmp/private --arg-ok"} {
		if _, err := sink.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "api-secret") {
		t.Fatalf("diagnostic leaked api credential: %q", text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("diagnostic was not redacted: %q", text)
	}
	if !strings.Contains(text, "user-id-ok") || !strings.Contains(text, "/tmp/private") || !strings.Contains(text, "--arg-ok") {
		t.Fatalf("diagnostic over-redacted control-plane context: %q", text)
	}
}

func TestDiagnosticSinkRetainsFullCapUntilOverflow(t *testing.T) {
	const capBytes = maxPipelineLog
	marker := []byte(pipelineLogTruncationMarker)

	makeInput := func(size int) []byte {
		data := bytes.Repeat([]byte{'x'}, size)
		copy(data, []byte("BEGIN-unique-diagnostic-sentinel"))
		middle := size / 2
		copy(data[middle:], []byte("MIDDLE-unique-diagnostic-sentinel"))
		copy(data[size-len("END-unique-diagnostic-sentinel"):], []byte("END-unique-diagnostic-sentinel"))
		return data
	}

	tests := []struct {
		name       string
		write      func(*testing.T, *diagnosticSink)
		want       []byte
		wantMarker bool
	}{
		{
			name: "exact cap",
			write: func(t *testing.T, sink *diagnosticSink) {
				t.Helper()
				if n, err := sink.Write(makeInput(capBytes)); err != nil || n != capBytes {
					t.Fatalf("Write() = %d, %v; want %d, nil", n, err, capBytes)
				}
			},
			want: makeInput(capBytes),
		},
		{
			name: "cap plus one",
			write: func(t *testing.T, sink *diagnosticSink) {
				t.Helper()
				if n, err := sink.Write(makeInput(capBytes + 1)); err != nil || n != capBytes+1 {
					t.Fatalf("Write() = %d, %v; want %d, nil", n, err, capBytes+1)
				}
			},
			want:       append(append([]byte(nil), makeInput(capBytes)[:capBytes-len(marker)]...), marker...),
			wantMarker: true,
		},
		{
			name: "late multi-write overflow",
			write: func(t *testing.T, sink *diagnosticSink) {
				t.Helper()
				input := makeInput(capBytes)
				for len(input) > 0 {
					n := 7777
					if n > len(input) {
						n = len(input)
					}
					if written, err := sink.Write(input[:n]); err != nil || written != n {
						t.Fatalf("Write() = %d, %v; want %d, nil", written, err, n)
					}
					input = input[n:]
				}
				if _, err := sink.Write([]byte("late-overflow")); err != nil {
					t.Fatal(err)
				}
				if _, err := sink.Write([]byte("subsequent-output-must-not-change")); err != nil {
					t.Fatal(err)
				}
			},
			want:       append(append([]byte(nil), makeInput(capBytes)[:capBytes-len(marker)]...), marker...),
			wantMarker: true,
		},
		{
			name: "far oversized",
			write: func(t *testing.T, sink *diagnosticSink) {
				t.Helper()
				if n, err := sink.Write(makeInput(capBytes + 100000)); err != nil || n != capBytes+100000 {
					t.Fatalf("Write() = %d, %v; want %d, nil", n, err, capBytes+100000)
				}
			},
			want:       append(append([]byte(nil), makeInput(capBytes + 100000)[:capBytes-len(marker)]...), marker...),
			wantMarker: true,
		},
		{
			name:  "empty",
			write: func(t *testing.T, _ *diagnosticSink) { t.Helper() },
			want:  []byte{},
		},
		{
			name: "ordinary",
			write: func(t *testing.T, sink *diagnosticSink) {
				t.Helper()
				if _, err := sink.Write([]byte("ordinary diagnostic output")); err != nil {
					t.Fatal(err)
				}
			},
			want: []byte("ordinary diagnostic output"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			sink := newDiagnosticSink([]io.Writer{&output}, nil)
			test.write(t, sink)
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(output.Bytes(), test.want) {
				t.Fatalf("output length=%d, want=%d; marker=%v, want marker=%v", output.Len(), len(test.want), bytes.Contains(output.Bytes(), marker), test.wantMarker)
			}
			if output.Len() != len(test.want) {
				t.Fatalf("output length=%d, want=%d", output.Len(), len(test.want))
			}
			if bytes.Contains(output.Bytes(), marker) != test.wantMarker {
				t.Fatalf("marker presence=%v, want %v", bytes.Contains(output.Bytes(), marker), test.wantMarker)
			}
		})
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

func TestWorkerOperationalExitLogKeepsLeaseCause(t *testing.T) {
	err := annotateError(errCloudLeaseUnavailable, errObjectGenerationConflict)
	if isWorkerCLIRejection(err) {
		t.Fatal("lease conflict must not be treated as CLI rejection")
	}
	got := formatWorkerExitLog(err)
	want := "pipeline publish lease unavailable: object generation conflict"
	if got != want {
		t.Fatalf("exit log=%q, want %q", got, want)
	}
	// Public Error() stays stable for callers/tests that match exact boundary text.
	if err.Error() != "pipeline publish lease unavailable" {
		t.Fatalf("public error changed: %q", err.Error())
	}
	if !errors.Is(err, errCloudLeaseUnavailable) || !errors.Is(err, errObjectGenerationConflict) {
		t.Fatalf("unwrap/Is broken: %v", err)
	}
}

func TestWorkerExitLogRedactsCredentialsInCause(t *testing.T) {
	t.Setenv("LLM_API_KEY", "sk-super-secret-value")
	err := annotateError(errCloudPipelineExecution, errors.New("provider rejected sk-super-secret-value"))
	got := formatWorkerExitLog(err)
	if strings.Contains(got, "sk-super-secret-value") {
		t.Fatalf("credential leaked in exit log: %q", got)
	}
	if !strings.Contains(got, "pipeline execution failed") || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("exit log missing boundary or redaction: %q", got)
	}
}

func TestWorkerCLIRejectionClassification(t *testing.T) {
	for _, msg := range []string{
		"unknown command \"payload-secret\" for \"worker\"",
		"invalid argument \"x\" for \"run\"",
		"worker command rejected",
	} {
		if !isWorkerCLIRejection(errors.New(msg)) {
			t.Fatalf("expected CLI rejection for %q", msg)
		}
	}
	for _, err := range []error{
		errCloudLeaseUnavailable,
		errCloudPipelineExecution,
		errors.New("worker input is invalid"),
		annotateError(errCloudMaterialization, errors.New("gcs timeout")),
	} {
		if isWorkerCLIRejection(err) {
			t.Fatalf("operational error misclassified as CLI: %v", err)
		}
	}
}

func TestWorkerRuntimeLeaseErrorIsNotCollapsedToRejected(t *testing.T) {
	// Runtime annotated lease errors must pass through executeWorkerCommand unchanged.
	// We exercise the classifier boundary used by executeWorkerCommand.
	err := annotateError(errCloudLeaseUnavailable, annotateError(errCloudLeaseHeld, errObjectGenerationConflict))
	if isWorkerCLIRejection(err) {
		t.Fatalf("runtime lease error treated as CLI rejection: %v", err)
	}
	if err.Error() != "pipeline publish lease unavailable" {
		t.Fatalf("public boundary changed: %q", err.Error())
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

func TestAPIKeyEnvironmentPrecedenceAndExplicitEmptySuppression(t *testing.T) {
	t.Setenv("LLM_API_KEY", "llm-key")
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-key")

	if got := configFromEnvironment(workerConfig{}).APIKey; got != "llm-key" {
		t.Fatalf("LLM_API_KEY precedence = %q, want llm-key", got)
	}
	t.Setenv("LLM_API_KEY", "")
	if got := configFromEnvironment(workerConfig{}).APIKey; got != "deepseek-key" {
		t.Fatalf("DEEPSEEK_API_KEY fallback = %q, want deepseek-key", got)
	}
	if got := configFromEnvironment(workerConfig{APIKey: "explicit-key", apiKeySet: true}).APIKey; got != "explicit-key" {
		t.Fatalf("explicit API key = %q, want explicit-key", got)
	}
	if got := configFromEnvironment(workerConfig{apiKeySet: true}).APIKey; got != "" {
		t.Fatalf("explicit empty API key = %q, want suppression", got)
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
	tmpRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if got == vault || !strings.HasPrefix(got, tmpRoot+string(filepath.Separator)) {
		t.Fatalf("Synto vault=%q, want a private workspace under %q", got, tmpRoot)
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
	mustWriteFile(t, filepath.Join(vault, "wiki", "new.md"), []byte("---\nid: 22af645d1859\n---\nnew article\n"))
	mustWriteFile(t, filepath.Join(vault, "synto.toml"), []byte("[pipeline]\nauto_commit = false\nauto_maintain = false\nrelation_extraction = false\n"))
	mustWriteFile(t, filepath.Join(vault, ".synto", "INDEX.json"), []byte(syntoIndexFixture("22af645d1859", "01JAZ5N7Y3K8M2Q4R6T9VWXAC8", "new", false)))
	writeValidSQLiteState(t, filepath.Join(vault, ".synto", "state.db"))
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

func TestWorkerInputInvalidPreservesCause(t *testing.T) {
	// Oversized API key is a bounds failure; public boundary stays stable.
	cfg := workerConfig{APIKey: strings.Repeat("k", maxWorkerKeyBytes+1), apiKeySet: true, Bucket: "b", bucketSet: true, UserID: "u", ProjectID: "p", ExecutionID: "exec-1", Postprocess: true}
	err := runWorkerBatch(context.Background(), cfg, `[["run","--auto-approve"]]`)
	if err == nil || err.Error() != "worker input is invalid" {
		t.Fatalf("error=%v", err)
	}
	if !errors.Is(err, errWorkerInputInvalid) {
		t.Fatalf("missing sentinel: %v", err)
	}
	if got := formatWorkerExitLog(err); !strings.Contains(got, "worker input is invalid") || !strings.Contains(got, ":") {
		t.Fatalf("cause concealed in exit log: %q", got)
	}
}

func TestPipelineLogWritersTeesConsoleUnlessSuppressed(t *testing.T) {
	vault := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := workerConfig{ExecutionID: "exec-console-tee"}
	stdout, stderr, closeLog, err := pipelineLogWriters(vault, cfg, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	if _, err := stdout.Write([]byte("hello-console-tee\n")); err != nil {
		t.Fatal(err)
	}
	if stderr == stdout {
		t.Fatal("stdout/stderr writers should remain independent")
	}
	if err := closeLog(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(vault, "cache", "pipeline-exec-console-tee.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello-console-tee") {
		t.Fatalf("log missing write: %q", data)
	}

	// Suppress path still writes the file.
	vault2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vault2, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout2, _, close2, err := pipelineLogWriters(vault2, workerConfig{ExecutionID: "exec-quiet"}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdout2.Write([]byte("quiet-only-file\n")); err != nil {
		t.Fatal(err)
	}
	if err := close2(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(vault2, "cache", "pipeline-exec-quiet.log"))
	if err != nil || !strings.Contains(string(data), "quiet-only-file") {
		t.Fatalf("suppressed path lost file log: %v %q", err, data)
	}
}

func TestDiagnosticSecretsAreCredentialsOnly(t *testing.T) {
	cfg := workerConfig{
		APIKey: "api-secret", apiKeySet: true,
		UserID: "user-id", ProjectID: "project-id", ExecutionID: "exec-id",
		VaultPath: "/vault/path", WorkspaceDir: "/ws", DataDir: "/data", Bucket: "bucket",
	}
	got := diagnosticSecrets(cfg, [][]string{{"run", "--arg", "/tmp/path"}})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "api-secret") {
		t.Fatalf("missing api key: %v", got)
	}
	for _, forbidden := range []string{"user-id", "project-id", "exec-id", "/vault/path", "/ws", "/data", "bucket", "--arg", "/tmp/path", "run"} {
		for _, value := range got {
			if value == forbidden {
				t.Fatalf("diagnosticSecrets scrubbed control-plane value %q: %v", forbidden, got)
			}
		}
	}
}
