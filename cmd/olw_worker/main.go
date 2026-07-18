package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/rawstatus"
	"github.com/rayer/llm-wiki-bff/internal/sourcestatus"
	"github.com/rayer/llm-wiki-bff/internal/storage"
	"github.com/rayer/llm-wiki-bff/internal/suggestedqueries"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex/fsstore"
	"github.com/spf13/cobra"
)

type workerConfig struct {
	VaultPath      string
	Bucket         string
	DataDir        string
	UserID         string
	ProjectID      string
	ExecutionID    string
	APIKey         string
	InitVault      bool
	Postprocess    bool
	StopOnError    bool
	Workspace      bool
	WorkspaceDir   string
	SuppressOutput bool
	cloudMode      bool
	// These record Cobra presence, rather than a truthy value, so an explicit
	// false or empty local-routing flag cannot be replaced by inherited env.
	vaultSet, dataDirSet, workspaceSet         bool
	bucketSet, userIDSet, projectIDSet         bool
	executionIDSet, apiKeySet, workspaceDirSet bool
}

type execOLWFunc func(ctx context.Context, vault string, command []string, env []string, stdout, stderr io.Writer) error

var execOLW execOLWFunc = execOLWCommand

const (
	maxDiagnosticPending            = 8192
	maxDiagnosticBuffered           = maxDiagnosticPending + maxWorkerArgBytes
	maxWorkerKeyBytes               = 4096
	maxWorkerIDBytes                = 256
	maxWorkerPathBytes              = 4096
	maxWorkerCommands               = 64
	maxWorkerArgs                   = 64
	maxWorkerArgBytes               = 4096
	maxWorkerCommandBytes           = 1 << 20
	maxWorkerCommandCumulativeBytes = 256 << 10
)

func main() {
	if err := executeWorkerCommand(newRootCommand()); err != nil {
		log.Printf("worker: %v", err)
		os.Exit(1)
	}
}

func executeWorkerCommand(cmd *cobra.Command) error {
	if err := cmd.Execute(); err != nil {
		return errors.New("worker command rejected")
	}
	return nil
}

func newRootCommand() *cobra.Command {
	cfg := workerConfig{Postprocess: true, StopOnError: true}
	var noPostprocess bool

	rootCmd := &cobra.Command{
		Use:           "worker",
		Short:         "Run OLW commands against a local vault",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	rootCmd.SetFlagErrorFunc(func(*cobra.Command, error) error { return errors.New("worker command rejected") })
	rootCmd.PersistentFlags().StringVar(&cfg.VaultPath, "vault", "", "project vault path")
	rootCmd.PersistentFlags().StringVar(&cfg.Bucket, "bucket", "", "GCS bucket")
	rootCmd.PersistentFlags().StringVar(&cfg.DataDir, "data-dir", "", "local data root")
	rootCmd.PersistentFlags().StringVar(&cfg.UserID, "user-id", "", "user id")
	rootCmd.PersistentFlags().StringVar(&cfg.ProjectID, "project-id", "", "project id")
	rootCmd.PersistentFlags().StringVar(&cfg.ExecutionID, "execution-id", "", "pipeline execution id")
	rootCmd.PersistentFlags().StringVar(&cfg.APIKey, "api-key", "", "LLM API key")
	rootCmd.PersistentFlags().BoolVar(&cfg.StopOnError, "stop-on-error", true, "stop on first failed OLW command")
	rootCmd.PersistentFlags().BoolVar(&cfg.Workspace, "workspace", false, "run against a private copied workspace")
	rootCmd.PersistentFlags().StringVar(&cfg.WorkspaceDir, "workspace-dir", "", "parent directory for private workspaces")

	runCmd := &cobra.Command{
		Use:   "run <json array of arrays>",
		Short: "Execute a batch of OLW commands",
		Args:  fixedArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runCfg := cfg
			setWorkerFlagPresence(&runCfg, cmd)
			if noPostprocess {
				runCfg.Postprocess = false
			}
			return runWorkerBatch(cmd.Context(), runCfg, args[0])
		},
	}
	runCmd.Flags().BoolVar(&cfg.InitVault, "init", false, "run 'olw init .' before the command batch")
	runCmd.Flags().BoolVar(&cfg.Postprocess, "postprocess", true, "run postprocess after successful batch")
	runCmd.Flags().BoolVar(&noPostprocess, "no-postprocess", false, "skip postprocess after batch")

	postprocessCmd := &cobra.Command{
		Use:   "postprocess",
		Short: "Rebuild local BFF cache and index artifacts",
		Args:  fixedArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			postprocessCfg := cfg
			setWorkerFlagPresence(&postprocessCfg, cmd)
			return runPostprocessCommand(cmd.Context(), postprocessCfg)
		},
	}

	rootCmd.AddCommand(runCmd, postprocessCmd)
	return rootCmd
}

func setWorkerFlagPresence(cfg *workerConfig, cmd *cobra.Command) {
	changed := func(name string) bool {
		return cmd.Flags().Changed(name) || cmd.InheritedFlags().Changed(name) || cmd.Root().PersistentFlags().Changed(name)
	}
	cfg.vaultSet = changed("vault")
	cfg.bucketSet = changed("bucket")
	cfg.dataDirSet = changed("data-dir")
	cfg.userIDSet = changed("user-id")
	cfg.projectIDSet = changed("project-id")
	cfg.executionIDSet = changed("execution-id")
	cfg.apiKeySet = changed("api-key")
	cfg.workspaceSet = changed("workspace")
	cfg.workspaceDirSet = changed("workspace-dir")
}

func fixedArgs(want int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != want {
			return errors.New("worker command rejected")
		}
		return nil
	}
}

func runWorkerBatch(ctx context.Context, cfg workerConfig, rawCommands string) error {
	cfg = configFromEnvironment(cfg)
	if cfg.Bucket != "" {
		cfg.cloudMode = true
		if err := validateWorkerConfigBounds(cfg); err != nil || len(rawCommands) > maxWorkerCommandBytes {
			return errors.New("worker input is invalid")
		}
	}
	commands, err := parseCommandBatch(rawCommands)
	if err != nil {
		return errors.New("invalid command batch")
	}
	if cfg.InitVault {
		commands = append([][]string{{"init", "."}}, commands...)
	}
	if err := validateWorkerInput(cfg, commands); err != nil {
		return errors.New("worker input is invalid")
	}
	if cfg.Bucket != "" {
		if cfg.VaultPath != "" || cfg.DataDir != "" || cfg.Workspace {
			return errors.New("worker configuration is invalid")
		}
		return runCloudWorkerBatch(ctx, cfg, commands, newCloudObjectStore(cfg.Bucket))
	}
	vault, err := resolveVaultPath(cfg)
	if err != nil {
		return err
	}
	vault, err = canonicalExistingDir(vault)
	if err != nil {
		return err
	}
	if cfg.Workspace {
		return runWorkerBatchWorkspace(ctx, cfg, commands, vault)
	}
	return runWorkerBatchAtVault(ctx, cfg, commands, vault)
}

func configFromEnvironment(cfg workerConfig) workerConfig {
	if cfg.Bucket == "" && !cfg.bucketSet {
		cfg.Bucket = envOr("BUCKET", "")
	}
	// A cloud worker is never routed through a mounted local filesystem. Ignore
	// inherited legacy env; only an explicitly supplied local setting is kept so
	// validation can reject it before storage or a child process is touched.
	cloud := cfg.Bucket != ""
	if cfg.VaultPath == "" && !cfg.vaultSet && !cloud {
		cfg.VaultPath = envOr("VAULT_PATH", "")
	}
	if cfg.DataDir == "" && !cfg.dataDirSet && !cloud {
		cfg.DataDir = envOr("DATA_DIR", "")
		if cfg.DataDir == "" && cfg.Bucket == "" {
			cfg.DataDir = "/data"
		}
	}
	if cfg.UserID == "" && !cfg.userIDSet {
		cfg.UserID = envOr("USER_ID", "")
	}
	if cfg.ProjectID == "" && !cfg.projectIDSet {
		cfg.ProjectID = envOr("PROJECT_ID", "")
	}
	if cfg.ExecutionID == "" && !cfg.executionIDSet {
		cfg.ExecutionID = envOr("EXECUTION_ID", envOr("CLOUD_RUN_EXECUTION", ""))
	}
	if cfg.APIKey == "" && !cfg.apiKeySet {
		cfg.APIKey = envOr("LLM_API_KEY", "")
	}
	if cfg.WorkspaceDir == "" && !cfg.workspaceDirSet {
		cfg.WorkspaceDir = envOr("WORKSPACE_DIR", "/tmp")
	}
	if !cfg.Workspace && !cfg.workspaceSet && !cloud {
		cfg.Workspace = envBool("WORKSPACE")
	}
	return cfg
}

func runWorkerBatchAtVault(ctx context.Context, cfg workerConfig, commands [][]string, vault string) error {
	if err := cleanStaleLock(vault, 5*time.Minute); err != nil {
		return err
	}
	if err := ensureWikiTOML(vault, cfg); err != nil {
		return err
	}
	olwEnv, err := prepareOLWEnvironment(cfg)
	if err != nil {
		return err
	}
	defer cleanupOLWEnvironment(olwEnv)

	stdout, stderr, closeLog, err := pipelineLogWriters(vault, cfg, commands, cfg.SuppressOutput)
	if err != nil {
		return err
	}
	runErr := runOLWBatch(ctx, vault, commands, cfg.StopOnError, olwEnv, stdout, stderr)
	if err := closeLog(); err != nil {
		return fmt.Errorf("close pipeline log: %w", err)
	}
	if runErr != nil {
		return runErr
	}
	if cfg.Postprocess {
		if err := runPostprocess(ctx, vault); err != nil {
			return err
		}
	}
	return nil
}

// runWorkerBatchWorkspace keeps the mounted vault immutable while OLW runs.
// Receipts are written only after every durable output has been copied back.
func runWorkerBatchWorkspace(ctx context.Context, cfg workerConfig, commands [][]string, vault string) (runErr error) {
	if !cfg.Postprocess {
		return errors.New("workspace mode requires postprocess before recording ingestion receipts")
	}
	if !startsWithFullOLWRun(commands) {
		return errors.New("workspace mode requires the first olw command to be run before recording ingestion receipts")
	}
	lease, err := acquireVaultLease(vault, cfg.ExecutionID)
	if err != nil {
		return err
	}
	defer func() {
		if err := lease.Release(); err != nil && runErr == nil {
			runErr = err
		}
	}()
	if err := recoverInterruptedPublish(vault); err != nil {
		return err
	}
	snapshots, err := snapshotSources(vault)
	if err != nil {
		return err
	}
	workspace, err := createWorkspace(cfg.WorkspaceDir, vault)
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(workspace); err != nil && runErr == nil {
			runErr = fmt.Errorf("cleanup workspace: %w", err)
		}
	}()

	if err := materializeSnapshots(workspace, snapshots); err != nil {
		return err
	}
	if err := runWorkerBatchAtVault(ctx, cfg, commands, workspace); err != nil {
		logErr := publishWorkspaceFailureLog(workspace, vault, cfg)
		var recordErr error
		recordErr = recordFailure(vault, snapshots, err)
		if recordErr != nil || logErr != nil {
			return errors.Join(err, logErr, recordErr)
		}
		return err
	}
	if err := syncWorkspaceOutputs(workspace, vault, cfg.ExecutionID); err != nil {
		recordErr := recordFailure(vault, snapshots, err)
		if recordErr != nil {
			return errors.Join(err, recordErr)
		}
		return err
	}
	return recordSuccess(vault, snapshots, time.Now().UTC())
}

func runPostprocessCommand(ctx context.Context, cfg workerConfig) (runErr error) {
	vault, err := resolveVaultPath(cfg)
	if err != nil {
		return err
	}
	vault, err = canonicalExistingDir(vault)
	if err != nil {
		return err
	}
	if cfg.Workspace {
		lease, err := acquireVaultLease(vault, cfg.ExecutionID)
		if err != nil {
			return err
		}
		defer func() {
			if err := lease.Release(); err != nil && runErr == nil {
				runErr = err
			}
		}()
		if err := recoverInterruptedPublish(vault); err != nil {
			return err
		}
		workspace, err := createWorkspace(cfg.WorkspaceDir, vault)
		if err != nil {
			return err
		}
		defer os.RemoveAll(workspace)
		if err := runPostprocess(ctx, workspace); err != nil {
			return err
		}
		return syncWorkspaceOutputs(workspace, vault, cfg.ExecutionID)
	}
	return runPostprocess(ctx, vault)
}

func parseCommandBatch(raw string) ([][]string, error) {
	if len(raw) > maxWorkerCommandBytes {
		return nil, errors.New("command batch exceeds byte limit")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	token, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("parse command batch: %w", err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return nil, errors.New("command batch is not an array")
	}
	commands := make([][]string, 0, 4)
	cumulativeBytes := 0
	for dec.More() {
		if len(commands) >= maxWorkerCommands {
			return nil, errors.New("command batch exceeds command limit")
		}
		commandToken, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if delim, ok := commandToken.(json.Delim); !ok || delim != '[' {
			return nil, errors.New("command is not an array")
		}
		command := make([]string, 0, 4)
		for dec.More() {
			if len(command) >= maxWorkerArgs {
				return nil, errors.New("command exceeds argument limit")
			}
			var arg string
			if err := dec.Decode(&arg); err != nil {
				return nil, err
			}
			if len(arg) > maxWorkerArgBytes {
				return nil, errors.New("command argument exceeds byte limit")
			}
			if cumulativeBytes > maxWorkerCommandCumulativeBytes-len(arg) {
				return nil, errors.New("command arguments exceed cumulative byte limit")
			}
			cumulativeBytes += len(arg)
			command = append(command, arg)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		if len(command) == 0 {
			return nil, fmt.Errorf("command %d is empty", len(commands))
		}
		if strings.TrimSpace(command[0]) == "" {
			return nil, fmt.Errorf("command %d has empty command name", len(commands))
		}
		commands = append(commands, command)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if len(commands) == 0 {
		return nil, errors.New("command batch is empty")
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("command batch has trailing data")
		}
		return nil, err
	}
	return commands, nil
}

func startsWithFullOLWRun(commands [][]string) bool {
	return len(commands) > 0 && len(commands[0]) > 0 && commands[0][0] == "run"
}

func resolveVaultPath(cfg workerConfig) (string, error) {
	if strings.TrimSpace(cfg.VaultPath) != "" {
		return filepath.Clean(cfg.VaultPath), nil
	}
	if strings.TrimSpace(cfg.DataDir) != "" && strings.TrimSpace(cfg.UserID) != "" && strings.TrimSpace(cfg.ProjectID) != "" {
		return filepath.Join(cfg.DataDir, "users", cfg.UserID, "projects", cfg.ProjectID), nil
	}
	return "", errors.New("cannot resolve vault path: set --vault or provide --data-dir, --user-id, and --project-id")
}

func ensureWikiTOML(vault string, cfg workerConfig) error {
	target := filepath.Join(vault, "wiki.toml")
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat wiki.toml: %w", err)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return errors.New("missing API key: set --api-key or LLM_API_KEY to create wiki.toml")
	}

	toml := `[provider]
name = "deepseek"
url = "https://api.deepseek.com/v1"

[models]
fast = "deepseek-chat"
heavy = "deepseek-reasoner"

[pipeline]
auto_approve = true
auto_commit = true
auto_maintain = true
article_max_tokens = 32768
max_concepts_per_source = 8
ingest_parallel = false
`

	if err := os.WriteFile(target, []byte(toml), 0o644); err != nil {
		return fmt.Errorf("write wiki.toml: %w", err)
	}
	return nil
}

func prepareOLWEnvironment(cfg workerConfig) ([]string, error) {
	configHome, err := os.MkdirTemp("", "olw-config-*")
	if err != nil {
		return nil, fmt.Errorf("create isolated OLW config dir: %w", err)
	}
	env := []string{"XDG_CONFIG_HOME=" + configHome}
	if strings.TrimSpace(cfg.APIKey) != "" {
		env = append(env, "LLM_API_KEY="+cfg.APIKey, "DEEPSEEK_API_KEY="+cfg.APIKey)
	}
	return env, nil
}

func runOLWBatch(ctx context.Context, vault string, commands [][]string, stopOnError bool, env []string, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	var batchErr error
	for i, command := range commands {
		log.Printf("[%d/%d] olw command", i+1, len(commands))
		if err := execOLW(ctx, vault, command, env, stdout, stderr); err != nil {
			wrapped := fmt.Errorf("olw command failed: %w", err)
			if stopOnError {
				return wrapped
			}
			log.Printf("olw command failed (continuing)")
			batchErr = errors.Join(batchErr, wrapped)
		}
	}
	return batchErr
}

func execOLWCommand(ctx context.Context, vault string, command []string, env []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "olw", command...)
	cmd.Dir = vault
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func pipelineLogWriters(vault string, cfg workerConfig, commands [][]string, suppressOutput bool) (io.Writer, io.Writer, func() error, error) {
	if cfg.cloudMode {
		return io.Discard, io.Discard, func() error { return nil }, nil
	}
	if strings.TrimSpace(cfg.ExecutionID) == "" {
		sink := newDiagnosticSink([]io.Writer{os.Stdout, os.Stderr}, diagnosticSecrets(cfg, commands))
		return sink, sink, sink.Close, nil
	}
	path, err := pipelineLogPath(vault, cfg.ExecutionID)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, nil, fmt.Errorf("mkdir pipeline log dir: %w", err)
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create pipeline log: %w", err)
	}
	destinations := []io.Writer{file}
	if !suppressOutput {
		destinations = append(destinations, os.Stdout, os.Stderr)
	}
	sink := newDiagnosticSink(destinations, diagnosticSecrets(cfg, commands))
	return sink, sink, func() error {
		if err := sink.Close(); err != nil {
			_ = file.Close()
			return err
		}
		return file.Close()
	}, nil
}

type diagnosticSink struct {
	writers []io.Writer
	secrets []string
	pending []byte
	written int
	mu      sync.Mutex
	closed  bool
}

func newDiagnosticSink(writers []io.Writer, secrets []string) *diagnosticSink {
	return &diagnosticSink{writers: writers, secrets: secrets}
}
func (w *diagnosticSink) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errors.New("diagnostic sink closed")
	}
	original := len(data)
	for len(data) > 0 {
		n := maxDiagnosticPending
		if room := maxDiagnosticBuffered - len(w.pending); room < n {
			n = room
		}
		if n > len(data) {
			n = len(data)
		}
		if n <= 0 {
			return 0, errors.New("diagnostic output rejected")
		}
		w.pending = append(w.pending, data[:n]...)
		data = data[n:]
		if len(w.pending) > maxDiagnosticPending {
			emit := len(w.pending) - maxDiagnosticPending
			// Do not release an incomplete sentinel which starts just before the
			// pending boundary.  Inputs are capped below this fixed tail bound.
			emit = safeDiagnosticEmit(w.pending, emit, w.secrets)
			if emit > 0 {
				if err := w.emitLocked(w.pending[:emit], false); err != nil {
					return 0, err
				}
			}
		}
	}
	return original, nil
}
func (w *diagnosticSink) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.emitLocked(w.pending, true)
}
func (w *diagnosticSink) emitLocked(data []byte, final bool) error {
	if len(data) == 0 {
		return nil
	}
	// Retain the fixed-size tail until close so a secret split across writes or
	// stdout/stderr cannot reach any destination before redaction.
	text := string(redactDiagnosticBytes(data, w.secrets))
	if w.written < maxPipelineLog {
		remaining := maxPipelineLog - w.written
		text = truncateDiagnostic(text, remaining)
		for _, dst := range w.writers {
			if _, err := io.WriteString(dst, text); err != nil {
				return err
			}
		}
		w.written += len(text)
	}
	if final {
		w.pending = nil
	} else {
		w.pending = append(w.pending[:0], w.pending[len(data):]...)
	}
	return nil
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func safeDiagnosticEmit(pending []byte, emit int, secrets []string) int {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		start := emit - len(secret) + 1
		if start < 0 {
			start = 0
		}
		for ; start < emit; start++ {
			n := minInt(len(secret), len(pending)-start)
			if n > 0 && bytes.Equal(pending[start:start+n], []byte(secret[:n])) && start+len(secret) > emit {
				emit = start
				break
			}
		}
	}
	return emit
}
func truncateDiagnostic(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := text[:limit]
	if i := strings.LastIndex(cut, "[REDACTED]"); i >= 0 && i+len("[REDACTED]") > limit {
		return cut[:i]
	}
	for n := 1; n < len("[REDACTED]") && n <= len(cut); n++ {
		if strings.HasSuffix(cut, "[REDACTED]"[:n]) {
			return cut[:len(cut)-n]
		}
	}
	return cut
}
func redactDiagnosticBytes(data []byte, secrets []string) []byte {
	text := string(data)
	ordered := append([]string(nil), secrets...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, secret := range ordered {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	return []byte(text)
}

type cappedRedactingWriter struct {
	writer  io.Writer
	secrets []string
	limit   int
	written int
	mu      sync.Mutex
}

func (w *cappedRedactingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(data)
	text := string(data)
	for _, secret := range w.secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	if w.written >= w.limit {
		return original, nil
	}
	remaining := w.limit - w.written
	if len(text) > remaining {
		text = text[:remaining]
	}
	n, err := io.WriteString(w.writer, text)
	w.written += n
	if err != nil {
		return 0, err
	}
	return original, nil
}

func logSecrets(cfg workerConfig) []string {
	values := []string{cfg.APIKey, os.Getenv("LLM_API_KEY"), os.Getenv("DEEPSEEK_API_KEY")}
	return values
}

func diagnosticSecrets(cfg workerConfig, commands [][]string) []string {
	values := []string{cfg.APIKey, cfg.UserID, cfg.ProjectID, cfg.ExecutionID, cfg.VaultPath, cfg.WorkspaceDir, cfg.DataDir, cfg.Bucket}
	if !cfg.apiKeySet {
		values = append(values, os.Getenv("LLM_API_KEY"), os.Getenv("DEEPSEEK_API_KEY"))
	}
	for _, command := range commands {
		values = append(values, command...)
	}
	return values
}

func validateWorkerInput(cfg workerConfig, commands [][]string) error {
	if err := validateWorkerConfigBounds(cfg); err != nil {
		return err
	}
	if len(commands) == 0 || len(commands) > maxWorkerCommands {
		return errors.New("oversize command batch")
	}
	for _, command := range commands {
		if len(command) == 0 || len(command) > maxWorkerArgs {
			return errors.New("oversize command")
		}
		for _, arg := range command {
			if len(arg) == 0 || len(arg) > maxWorkerArgBytes {
				return errors.New("oversize command argument")
			}
		}
	}
	var cumulativeBytes int
	for _, command := range commands {
		for _, arg := range command {
			if cumulativeBytes > maxWorkerCommandCumulativeBytes-len(arg) {
				return errors.New("oversize command batch")
			}
			cumulativeBytes += len(arg)
		}
	}
	return nil
}

func validateWorkerConfigBounds(cfg workerConfig) error {
	keyValues := []string{cfg.APIKey}
	if !cfg.apiKeySet {
		keyValues = append(keyValues, os.Getenv("LLM_API_KEY"), os.Getenv("DEEPSEEK_API_KEY"))
	}
	for _, value := range keyValues {
		if len(value) > maxWorkerKeyBytes {
			return errors.New("oversize key")
		}
	}
	for _, value := range []string{cfg.UserID, cfg.ProjectID, cfg.ExecutionID, cfg.Bucket} {
		if len(value) > maxWorkerIDBytes {
			return errors.New("oversize id")
		}
	}
	if cfg.cloudMode && !validPipelineExecutionID(cfg.ExecutionID) {
		return errors.New("invalid execution id")
	}
	for _, value := range []string{cfg.VaultPath, cfg.WorkspaceDir, cfg.DataDir} {
		if len(value) > maxWorkerPathBytes {
			return errors.New("oversize path")
		}
	}
	return nil
}

func pipelineLogPath(vault, executionID string) (string, error) {
	executionID = strings.TrimSpace(executionID)
	if !validPipelineExecutionID(executionID) {
		return "", fmt.Errorf("unsafe execution id: %s", executionID)
	}
	return filepath.Join(vault, "cache", "pipeline-"+executionID+".log"), nil
}

func validPipelineExecutionID(executionID string) bool {
	executionID = strings.TrimSpace(executionID)
	return executionID != "" && !strings.ContainsAny(executionID, `/\`+"\x00") && executionID != "." && executionID != ".." && !strings.Contains(executionID, "..")
}

func cleanStaleLock(vault string, maxAge time.Duration) error {
	lockFile := filepath.Join(vault, ".olw", "pipeline.lock")
	info, err := os.Stat(lockFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat pipeline lock: %w", err)
	}
	if time.Since(info.ModTime()) <= maxAge {
		return nil
	}
	if err := os.Remove(lockFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale pipeline lock: %w", err)
	}
	return nil
}

func runPostprocess(ctx context.Context, vault string) error {
	store := fsstore.New(vault)
	if _, err := wikiindex.Rebuild(ctx, store); err != nil {
		return fmt.Errorf("postprocess: %w", err)
	}
	if err := writeRawStatus(ctx, vault); err != nil {
		return fmt.Errorf("postprocess raw status: %w", err)
	}
	if err := writeSuggestedQueries(ctx, vault); err != nil {
		return fmt.Errorf("postprocess suggested queries: %w", err)
	}
	return nil
}

func writeSuggestedQueries(ctx context.Context, vault string) error {
	store := fsstore.New(vault)
	data, err := store.ReadFile(ctx, wikiindex.ConceptsJSONLPath)
	if err != nil {
		if errors.Is(err, wikiindex.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			data = nil
		} else {
			return fmt.Errorf("read concepts jsonl: %w", err)
		}
	}

	mtimes, err := listConceptMtTimes(vault)
	if err != nil {
		return fmt.Errorf("list concept mtimes: %w", err)
	}

	now := time.Now()
	var artifact suggestedqueries.Artifact
	if len(data) > 0 {
		artifact, err = suggestedqueries.BuildFromConceptsJSONL(data, mtimes, now)
		if err != nil {
			return fmt.Errorf("build suggested queries: %w", err)
		}
	} else {
		artifact = suggestedqueries.Build(nil, mtimes, now)
	}

	payload, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	if _, err := store.WriteBytesAtomic(ctx, payload, "cache/suggested_queries.json.tmp", suggestedqueries.Path); err != nil {
		return fmt.Errorf("write suggested queries: %w", err)
	}
	return nil
}

func listConceptMtTimes(vault string) (map[string]time.Time, error) {
	wikiDir := filepath.Join(vault, "wiki")
	entries, err := os.ReadDir(wikiDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]time.Time{}, nil
		}
		return nil, err
	}

	mtimes := make(map[string]time.Time, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		slug := strings.TrimSuffix(entry.Name(), ".md")
		mtimes[slug] = info.ModTime().UTC()
	}
	return mtimes, nil
}

func writeRawStatus(ctx context.Context, vault string) error {
	files, err := listVaultRawFiles(ctx, vault)
	if err != nil {
		return fmt.Errorf("list raw files: %w", err)
	}
	artifact, err := rawstatus.BuildFromStateDB(ctx, rawstatus.StateDBPath(vault), files, time.Now())
	if err != nil {
		return fmt.Errorf("build raw status: %w", err)
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	store := fsstore.New(vault)
	if _, err := store.WriteBytesAtomic(ctx, data, "cache/raw_status.json.tmp", rawstatus.Path); err != nil {
		return fmt.Errorf("write raw status: %w", err)
	}
	return nil
}

func listVaultRawFiles(ctx context.Context, vault string) ([]storage.RawFile, error) {
	rawDir := filepath.Join(vault, "raw")
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []storage.RawFile{}, nil
		}
		return nil, err
	}
	files := make([]storage.RawFile, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("raw", entry.Name()))
		data, err := os.ReadFile(filepath.Join(rawDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		files = append(files, storage.RawFile{
			Name:    entry.Name(),
			Path:    rel,
			Size:    info.Size(),
			Updated: info.ModTime().UTC(),
			SHA256:  fmt.Sprintf("%x", sum[:]),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})
	return files, nil
}

func requireExistingDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat vault %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("vault %q is not a directory", path)
	}
	return nil
}

type sourceSnapshot struct {
	SourceID       string
	RawPath        string
	RawBytes       []byte
	RawSHA256      string
	AnnotationBody string
	AnnotationSHA  string
	Fingerprint    string
	Dirty          bool
}

func canonicalExistingDir(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve vault %q: %w", dir, err)
	}
	if err := requireExistingDir(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func snapshotSources(vault string) ([]sourceSnapshot, error) {
	status, err := readSourceStatus(vault)
	if err != nil {
		return nil, err
	}
	data, err := readFileWithin(vault, "cache/id_map.json")
	if errors.Is(err, os.ErrNotExist) {
		return []sourceSnapshot{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read source map: %w", err)
	}
	var ids struct {
		SourceMeta map[string]struct {
			SourceFile string `json:"source_file"`
		} `json:"source_meta"`
	}
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("decode source map: %w", err)
	}

	snapshots := make([]sourceSnapshot, 0, len(ids.SourceMeta))
	mappedRawPaths := make(map[string]string, len(ids.SourceMeta))
	for sourceID, meta := range ids.SourceMeta {
		rawPath := strings.TrimSpace(meta.SourceFile)
		if !annotation.ValidSourceID(sourceID) || !safeMappedRawPath(rawPath) {
			return nil, fmt.Errorf("unsafe source mapping %q -> %q", sourceID, rawPath)
		}
		if prior, exists := mappedRawPaths[rawPath]; exists {
			return nil, fmt.Errorf("duplicate source mapping %q and %q -> %q", prior, sourceID, rawPath)
		}
		mappedRawPaths[rawPath] = sourceID
		raw, err := readRegularFileWithin(vault, rawPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read source raw %q: %w", rawPath, err)
		}
		ann, err := readAnnotation(vault, sourceID, rawPath)
		if err != nil {
			return nil, err
		}
		rawSum := sha256.Sum256(raw)
		rawSHA := fmt.Sprintf("%x", rawSum[:])
		fingerprint := sourcestatus.Fingerprint(rawSHA, ann.SHA256)
		receipt := status.Sources[sourceID]
		snapshots = append(snapshots, sourceSnapshot{
			SourceID: sourceID, RawPath: rawPath, RawBytes: raw, RawSHA256: rawSHA,
			AnnotationBody: ann.Body, AnnotationSHA: ann.SHA256, Fingerprint: fingerprint,
			Dirty: !sourcestatus.ValidReceipt(receipt, rawPath) || receipt.LastIngestFingerprint != fingerprint,
		})
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].SourceID < snapshots[j].SourceID })
	return snapshots, nil
}

func safeMappedRawPath(rawPath string) bool {
	if !storage.SafeRawPath(rawPath) {
		return false
	}
	name := strings.TrimPrefix(rawPath, "raw/")
	return name != "" && !strings.Contains(name, "/")
}

func readAnnotation(vault, sourceID, rawPath string) (annotation.Object, error) {
	data, err := readFileWithin(vault, annotation.Path(sourceID))
	if errors.Is(err, os.ErrNotExist) {
		return annotation.Object{SHA256: annotation.Digest("")}, nil
	}
	if err != nil {
		return annotation.Object{}, fmt.Errorf("read annotation %q: %w", sourceID, err)
	}
	var object annotation.Object
	if err := json.Unmarshal(data, &object); err != nil || object.Validate(sourceID, rawPath) != nil {
		return annotation.Object{}, fmt.Errorf("invalid annotation %q", sourceID)
	}
	return object, nil
}

func createWorkspace(parent, vault string) (string, error) {
	base, err := canonicalExistingDir(parent)
	if err != nil {
		return "", fmt.Errorf("workspace directory: %w", err)
	}
	workspace, err := os.MkdirTemp(base, "olw-workspace-*")
	if err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}
	vaultRoot, err := os.OpenRoot(vault)
	if err != nil {
		_ = os.RemoveAll(workspace)
		return "", err
	}
	defer vaultRoot.Close()
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		_ = os.RemoveAll(workspace)
		return "", err
	}
	defer workspaceRoot.Close()
	for _, dir := range []string{"raw", "wiki", "cache", ".olw"} {
		if err := copyTreeRoot(vaultRoot, workspaceRoot, dir, dir, nil); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.RemoveAll(workspace)
			return "", fmt.Errorf("copy %s into workspace: %w", dir, err)
		}
	}
	if err := copyOneIfExists(vaultRoot, workspaceRoot, "wiki.toml"); err != nil {
		_ = os.RemoveAll(workspace)
		return "", fmt.Errorf("copy wiki.toml into workspace: %w", err)
	}
	return workspace, nil
}

func materializeSnapshots(workspace string, snapshots []sourceSnapshot) error {
	for _, snapshot := range snapshots {
		data := append([]byte(nil), snapshot.RawBytes...)
		// Every non-empty annotation is materialized for every fresh workspace.
		// Receipts only determine BFF dirty state; they must never change the OLW
		// byte stream for otherwise identical source inputs.
		if strings.TrimSpace(snapshot.AnnotationBody) != "" {
			trailer := "\n\n---\n\n## Human annotations (system)\n<!-- lwc-ann-v1 source_id=" + snapshot.SourceID + " ann_sha256=" + snapshot.AnnotationSHA + " -->\n" + annotation.Normalize(snapshot.AnnotationBody) + "\n"
			data = append(data, []byte(trailer)...)
		}
		if err := writeFileAtomicWithin(workspace, snapshot.RawPath, data); err != nil {
			return fmt.Errorf("materialize %q: %w", snapshot.RawPath, err)
		}
	}
	return nil
}

func readSourceStatus(vault string) (sourcestatus.Artifact, error) {
	data, err := readFileWithin(vault, sourcestatus.Path)
	if errors.Is(err, os.ErrNotExist) {
		return sourcestatus.Artifact{Version: 1, Sources: map[string]sourcestatus.Receipt{}}, nil
	}
	if err != nil {
		return sourcestatus.Artifact{}, fmt.Errorf("read source status: %w", err)
	}
	artifact, err := sourcestatus.Decode(data)
	if err != nil || artifact.Version != 1 {
		return sourcestatus.Artifact{}, errors.New("invalid source status")
	}
	return artifact, nil
}

func recordSuccess(vault string, snapshots []sourceSnapshot, now time.Time) error {
	artifact, err := readSourceStatus(vault)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		artifact.Sources[snapshot.SourceID] = sourcestatus.Receipt{
			RawPath: snapshot.RawPath, LastIngestedRawSHA256: snapshot.RawSHA256,
			LastIngestedAnnSHA256: snapshot.AnnotationSHA, LastIngestFingerprint: snapshot.Fingerprint,
			LastSuccessAt: now.UTC().Format(time.RFC3339),
		}
	}
	return writeSourceStatus(vault, artifact)
}

func recordFailure(vault string, snapshots []sourceSnapshot, _ error) error {
	artifact, err := readSourceStatus(vault)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		receipt := artifact.Sources[snapshot.SourceID]
		receipt.RawPath = snapshot.RawPath
		receipt.FailedFingerprint = snapshot.Fingerprint
		receipt.Error = "pipeline failed"
		artifact.Sources[snapshot.SourceID] = receipt
	}
	return writeSourceStatus(vault, artifact)
}

func writeSourceStatus(vault string, artifact sourcestatus.Artifact) error {
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicWithin(vault, sourcestatus.Path, data)
}

func readRegularFileWithin(root, rel string) ([]byte, error) {
	if err := safeRelativePath(rel); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	info, err := r.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", rel)
	}
	// Root is descriptor-relative and rejects a symlink replacement which
	// escapes the original directory between Lstat and ReadFile.
	return r.ReadFile(filepath.FromSlash(rel))
}

func readFileWithin(root, rel string) ([]byte, error) {
	return readRegularFileWithin(root, rel)
}

func writeFileAtomicWithin(root, rel string, data []byte) error {
	if err := safeRelativePath(rel); err != nil {
		return err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	clean := filepath.Clean(filepath.FromSlash(rel))
	dir := filepath.Dir(clean)
	if err := r.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpName := filepath.Join(dir, ".atomic-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	file, err := r.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer r.Remove(tmpName)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return r.Rename(tmpName, clean)
}

func safeRelativePath(rel string) error {
	if filepath.IsAbs(rel) {
		return fmt.Errorf("absolute path %q is unsafe", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes root", rel)
	}
	return nil
}

func cleanupOLWEnvironment(env []string) {
	for _, entry := range env {
		key, dir, ok := strings.Cut(entry, "=")
		if ok && key == "XDG_CONFIG_HOME" && dir != "" {
			_ = os.RemoveAll(dir)
			return
		}
	}
}

func envOr(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}

func envBool(key string) bool {
	value, err := strconv.ParseBool(os.Getenv(key))
	return err == nil && value
}
