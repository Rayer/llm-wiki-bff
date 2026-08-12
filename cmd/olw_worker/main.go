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
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/llm"
	"github.com/rayer/llm-wiki-bff/internal/pipelinediagnostic"
	"github.com/rayer/llm-wiki-bff/internal/rawstatus"
	"github.com/rayer/llm-wiki-bff/internal/sourcestatus"
	"github.com/rayer/llm-wiki-bff/internal/storage"
	"github.com/rayer/llm-wiki-bff/internal/suggestedqueries"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex/fsstore"
	"github.com/spf13/cobra"
)

type workerConfig struct {
	VaultPath        string
	Bucket           string
	DataDir          string
	UserID           string
	ProjectID        string
	ExecutionID      string
	APIKey           string
	InitVault        bool
	Postprocess      bool
	StopOnError      bool
	Workspace        bool
	WorkspaceDir     string
	SuppressOutput   bool
	SuggestedQueries bool
	// CleanRebuild skips materializing prior generation outputs (wiki/.synto/
	// cache artifacts) so Synto cold-starts from raw only. Default false.
	CleanRebuild             bool
	cloudMode                bool
	suggestedQueriesProvider suggestedqueries.Provider
	// These record Cobra presence, rather than a truthy value, so an explicit
	// false or empty local-routing flag cannot be replaced by inherited env.
	vaultSet, dataDirSet, workspaceSet         bool
	bucketSet, userIDSet, projectIDSet         bool
	executionIDSet, apiKeySet, workspaceDirSet bool
}

type execOLWFunc func(ctx context.Context, vault string, command []string, env []string, stdout, stderr io.Writer) error

var execOLW execOLWFunc = execOLWCommand

var pipelineLiveDestination = func(stream pipelineStream) io.Writer {
	if stream == stderrStream {
		return os.Stderr
	}
	return os.Stdout
}

var pipelineDurableDestination = func(file *os.File) io.Writer { return file }
var pipelineControlDestination = func() io.Writer { return os.Stderr }

type workspaceBatchHooks struct {
	acquireVaultLease          func(string, string) (*vaultLease, error)
	recoverInterruptedPublish  func(string) error
	snapshotSources            func(string) ([]sourceSnapshot, error)
	snapshotConcepts           func(string, ...[]sourceSnapshot) ([]conceptSnapshot, error)
	createWorkspace            func(string, string) (string, error)
	materializeSnapshots       func(string, []sourceSnapshot) error
	runWorkerBatchAtVault      func(context.Context, workerConfig, [][]string, string) error
	reconcileWorkspaceSources  func(string, []sourceSnapshot) error
	reconcileWorkspaceConcepts func(string, []conceptSnapshot, ...[]sourceSnapshot) error
	syncWorkspaceOutputs       func(string, string, string) error
	runPostprocessWithProvider func(context.Context, string, suggestedqueries.Provider, io.Writer) error
	runSuggestedQueriesStage   func(context.Context, string, suggestedqueries.Provider) error
	recordSuccess              func(string, []sourceSnapshot, time.Time) error
	removeWorkspace            func(string) error
	releaseVaultLease          func(*vaultLease) error
}

var localWorkspaceBatchHooks = workspaceBatchHooks{
	acquireVaultLease:          acquireVaultLease,
	recoverInterruptedPublish:  recoverInterruptedPublish,
	snapshotSources:            snapshotSources,
	snapshotConcepts:           snapshotConcepts,
	createWorkspace:            createWorkspace,
	materializeSnapshots:       materializeSnapshots,
	runWorkerBatchAtVault:      runWorkerBatchAtVault,
	reconcileWorkspaceSources:  reconcileWorkspaceSources,
	reconcileWorkspaceConcepts: reconcileWorkspaceConcepts,
	syncWorkspaceOutputs:       syncWorkspaceOutputs,
	runPostprocessWithProvider: runPostprocessWithProvider,
	runSuggestedQueriesStage:   runSuggestedQueriesStage,
	recordSuccess:              recordSuccess,
	removeWorkspace:            os.RemoveAll,
	releaseVaultLease:          func(lease *vaultLease) error { return lease.Release() },
}

var buildNonce = "local"

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
	suggestedQueryModel             = "deepseek-chat"
)

const pipelineLogTruncationMarker = pipelinediagnostic.PipelineLogTruncationMarker

func main() {
	printBuildNonce(os.Stdout)
	if err := executeWorkerCommand(newRootCommand()); err != nil {
		log.Printf("worker: %s", formatWorkerExitLog(err))
		os.Exit(1)
	}
}

func printBuildNonce(w io.Writer) {
	fmt.Fprintf(w, "worker build_nonce=%s\n", buildNonce)
}

// errWorkerCommandRejected is the fixed CLI boundary for parse/usage failures.
// Operational errors keep their public messages and are not collapsed into this.
var errWorkerCommandRejected = errors.New("worker command rejected")
var errWorkerInputInvalid = errors.New("worker input is invalid")
var errWorkerConfigInvalid = errors.New("worker configuration is invalid")
var errInvalidCommandBatch = errors.New("invalid command batch")

func executeWorkerCommand(cmd *cobra.Command) error {
	if err := cmd.Execute(); err != nil {
		if isWorkerCLIRejection(err) {
			return errWorkerCommandRejected
		}
		return err
	}
	return nil
}

// isWorkerCLIRejection identifies cobra/parse failures that may echo raw user
// tokens (unknown commands, bad arity already normalized, flag parse). Those
// stay opaque. Runtime pipeline errors must not match this path.
func isWorkerCLIRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" || msg == errWorkerCommandRejected.Error() {
		return true
	}
	// Cobra echoes the rejected token; never forward that to logs as-is.
	if strings.HasPrefix(msg, "unknown command ") {
		return true
	}
	if strings.HasPrefix(msg, "invalid argument ") {
		return true
	}
	if strings.HasPrefix(msg, "accepts ") { // e.g. accepts 1 arg(s), received N
		return true
	}
	return false
}

// formatWorkerExitLog builds an operator-facing exit line: stable public
// boundary plus unwrapped cause chain, with credentials redacted.
func formatWorkerExitLog(err error) string {
	if err == nil {
		return ""
	}
	msg := formatWorkerExitMessage(err)
	if msg == "" {
		return errWorkerCommandRejected.Error()
	}
	secrets := []string{
		os.Getenv("LLM_API_KEY"),
		os.Getenv("DEEPSEEK_API_KEY"),
		os.Getenv("SYNTO_API_KEY"),
	}
	return string(redactDiagnosticBytes([]byte(msg), secrets))
}

func formatWorkerExitMessage(err error) string {
	if err == nil {
		return ""
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		parts := make([]string, 0, 4)
		seen := map[string]bool{}
		for _, sub := range multi.Unwrap() {
			if sub == nil {
				continue
			}
			part := formatWorkerExitMessage(sub)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			parts = append(parts, part)
		}
		return strings.Join(parts, "\n")
	}
	if ae, ok := err.(*annotatedError); ok && ae != nil {
		public := strings.TrimSpace(ae.Error())
		cause := formatWorkerExitMessage(ae.cause)
		switch {
		case public == "":
			return cause
		case cause == "" || cause == public:
			return public
		case strings.Contains(public, cause):
			return public
		default:
			return public + ": " + cause
		}
	}
	return strings.TrimSpace(err.Error())
}

func newRootCommand() *cobra.Command {
	cfg := workerConfig{Postprocess: true, StopOnError: true, SuggestedQueries: true}
	var noPostprocess bool
	var noSuggestedQueries bool

	rootCmd := &cobra.Command{
		Use:           "worker",
		Short:         "Run the pinned Synto pipeline against a local vault",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	rootCmd.SetFlagErrorFunc(func(*cobra.Command, error) error { return errWorkerCommandRejected })
	rootCmd.PersistentFlags().StringVar(&cfg.VaultPath, "vault", "", "project vault path")
	rootCmd.PersistentFlags().StringVar(&cfg.Bucket, "bucket", "", "GCS bucket")
	rootCmd.PersistentFlags().StringVar(&cfg.DataDir, "data-dir", "", "local data root")
	rootCmd.PersistentFlags().StringVar(&cfg.UserID, "user-id", "", "user id")
	rootCmd.PersistentFlags().StringVar(&cfg.ProjectID, "project-id", "", "project id")
	rootCmd.PersistentFlags().StringVar(&cfg.ExecutionID, "execution-id", "", "pipeline execution id")
	rootCmd.PersistentFlags().StringVar(&cfg.APIKey, "api-key", "", "LLM API key")
	rootCmd.PersistentFlags().BoolVar(&cfg.StopOnError, "stop-on-error", true, "stop on first failed Synto command")
	rootCmd.PersistentFlags().BoolVar(&cfg.Workspace, "workspace", false, "run against a private copied workspace")
	rootCmd.PersistentFlags().StringVar(&cfg.WorkspaceDir, "workspace-dir", "", "parent directory for private workspaces")
	rootCmd.PersistentFlags().BoolVar(&cfg.SuppressOutput, "suppress-output", false, "write child output only to the durable pipeline log (skip console tee)")

	runCmd := &cobra.Command{
		Use:   "run <json array of arrays>",
		Short: "Execute the accepted Synto run command",
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
	runCmd.Flags().BoolVar(&cfg.InitVault, "init", false, "deprecated; rejected because initialization is not part of the worker contract")
	runCmd.Flags().BoolVar(&cfg.Postprocess, "postprocess", true, "run postprocess after successful batch")
	runCmd.Flags().BoolVar(&noPostprocess, "no-postprocess", false, "skip postprocess after batch")

	postprocessCmd := &cobra.Command{
		Use:   "postprocess",
		Short: "Rebuild local BFF cache and index artifacts",
		Args:  fixedArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			postprocessCfg := cfg
			setWorkerFlagPresence(&postprocessCfg, cmd)
			if noSuggestedQueries {
				postprocessCfg.SuggestedQueries = false
			}
			return runPostprocessCommand(cmd.Context(), postprocessCfg)
		},
	}
	postprocessCmd.Flags().BoolVar(&noSuggestedQueries, "no-suggested-queries", false, "skip query-chip regeneration during postprocess")

	suggestedQueriesCmd := &cobra.Command{
		Use:   "suggested-queries",
		Short: "Regenerate cache/suggested_queries.json query chips only",
		Args:  fixedArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			suggestCfg := cfg
			setWorkerFlagPresence(&suggestCfg, cmd)
			suggestCfg.SuggestedQueries = true
			return runSuggestedQueriesCommand(cmd.Context(), suggestCfg)
		},
	}

	rootCmd.AddCommand(runCmd, postprocessCmd, suggestedQueriesCmd)
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
			return errWorkerCommandRejected
		}
		return nil
	}
}

func runWorkerBatch(ctx context.Context, cfg workerConfig, rawCommands string) error {
	cfg = configFromEnvironment(cfg)
	if cfg.Bucket != "" {
		cfg.cloudMode = true
		if err := validateWorkerConfigBounds(cfg); err != nil {
			return annotateError(errWorkerInputInvalid, err)
		}
		if len(rawCommands) > maxWorkerCommandBytes {
			return annotateError(errWorkerInputInvalid, fmt.Errorf("command batch exceeds %d bytes", maxWorkerCommandBytes))
		}
	}
	commands, err := parseCommandBatch(rawCommands)
	if err != nil {
		return annotateError(errInvalidCommandBatch, err)
	}
	if cfg.InitVault {
		commands = append([][]string{{"init", "."}}, commands...)
	}
	if err := validateWorkerInput(cfg, commands); err != nil {
		return annotateError(errWorkerInputInvalid, err)
	}
	if cfg.Bucket != "" {
		if cfg.VaultPath != "" || cfg.DataDir != "" || cfg.Workspace {
			return errWorkerConfigInvalid
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
	// Generation always runs in the existing private-workspace transaction,
	// including local invocations. This keeps direct mode from letting Synto
	// migrate or mutate the mounted/live vault before validation and publish.
	return runWorkerBatchWorkspace(ctx, cfg, commands, vault)
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
		cfg.APIKey = envOr("LLM_API_KEY", envOr("DEEPSEEK_API_KEY", ""))
	}
	if cfg.WorkspaceDir == "" && !cfg.workspaceDirSet {
		cfg.WorkspaceDir = envOr("WORKSPACE_DIR", "/tmp")
	}
	if !cfg.Workspace && !cfg.workspaceSet && !cloud {
		cfg.Workspace = envBool("WORKSPACE")
	}
	// CLEAN_REBUILD is opt-in only (never implied by other flags). Explicit
	// true on the config struct wins; otherwise env may enable it.
	if !cfg.CleanRebuild {
		cfg.CleanRebuild = envBool("CLEAN_REBUILD")
	}
	return cfg
}

func runWorkerBatchAtVault(ctx context.Context, cfg workerConfig, commands [][]string, vault string) (runErr error) {
	var err error
	if err := cleanStaleLock(vault, 5*time.Minute); err != nil {
		return preserveWorkerFailure(err, failureStageLeaseCleanup, failureClassStateInvalid)
	}
	olwEnv, err := prepareOLWEnvironment(cfg)
	if err != nil {
		return preserveWorkerFailure(err, failureStageSyntoConfigValidation, failureClassIO)
	}
	defer cleanupOLWEnvironment(olwEnv)
	stdout, stderr, closeLog, err := pipelineLogWriters(vault, cfg, commands, cfg.SuppressOutput)
	if err != nil {
		return preserveWorkerFailure(err, failureStageReceiptRecording, failureClassIO)
	}
	defer func() {
		if err := closeLog(); err != nil {
			closeFailure := preserveWorkerFailure(fmt.Errorf("close pipeline log: %w", err), failureStageReceiptRecording, failureClassIO)
			if runErr == nil {
				runErr = closeFailure
			} else {
				runErr = errors.Join(runErr, closeFailure)
			}
		}
	}()
	if err := ensureSyntoVault(ctx, vault, cfg, olwEnv, stdout, stderr); err != nil {
		return preserveWorkerFailure(err, failureStageSyntoConfigValidation, failureClassUnknown)
	}

	runErr = runOLWBatch(ctx, vault, commands, cfg.StopOnError, olwEnv, stdout, stderr)
	if runErr != nil {
		return preserveWorkerFailure(runErr, failureStageSyntoRun, failureClassUnknown)
	}
	if cfg.Postprocess {
		if err := ensureSyntoIndex(ctx, vault, olwEnv, stdout, stderr); err != nil {
			return preserveWorkerFailure(err, failureStageSyntoIndexExport, failureClassStateInvalid)
		}
	}
	if err := validateSyntoPipelineSafety(filepath.Join(vault, "synto.toml")); err != nil {
		return preserveWorkerFailure(err, failureStageSyntoConfigValidation, failureClassValidation)
	}
	if cfg.Postprocess {
		if err := runPostprocessWithProvider(ctx, vault, suggestedQueryProvider(cfg), stderr); err != nil {
			return preserveWorkerFailure(err, failureStagePostprocess, failureClassIO)
		}
	}
	return nil
}

type localLeaseLifecycle struct {
	vault     string
	cfg       workerConfig
	workspace string
	published bool
	failure   error
	recorded  bool
}

func (l *localLeaseLifecycle) fail(err error, stage failureStage, class failureErrorClass) error {
	if err != nil {
		l.failure = preserveWorkerFailure(err, stage, class)
	}
	return err
}

func (l *localLeaseLifecycle) recordFailure() {
	if l.failure == nil || l.recorded {
		return
	}
	l.recorded = true
	workspace := l.workspace
	if l.published {
		workspace = ""
	}
	_ = recordLocalWorkerFailureLog(workspace, l.vault, l.cfg, l.failure, l.published)
}

func (l *localLeaseLifecycle) finish(runErr *error, lease *vaultLease, removeWorkspace func(string) error, releaseLease func(*vaultLease) error) {
	if l.failure == nil && *runErr != nil {
		l.failure = *runErr
	}
	l.recordFailure()

	var cleanupErr error
	if l.workspace != "" {
		cleanupErr = removeWorkspace(l.workspace)
		if cleanupErr == nil {
			l.workspace = ""
		} else if l.failure == nil {
			l.failure = newWorkerFailure(nil, failureStageLeaseCleanup, failureClassIO, "", fmt.Errorf("cleanup workspace: %w", cleanupErr))
			*runErr = l.failure
			l.recordFailure()
		}
	}

	if err := releaseLease(lease); err != nil {
		if *runErr == nil {
			*runErr = err
		}
		if l.failure == nil {
			l.failure = newWorkerFailure(nil, failureStageLeaseCleanup, failureClassIO, "", err)
			l.recordFailure()
		}
	}
}

// runWorkerBatchWorkspace keeps the mounted vault immutable while Synto runs.
// Receipts are written only after every durable output has been copied back.
func runWorkerBatchWorkspace(ctx context.Context, cfg workerConfig, commands [][]string, vault string) (runErr error) {
	if !cfg.Postprocess {
		workspace, err := localWorkspaceBatchHooks.createWorkspace(cfg.WorkspaceDir, vault)
		if err != nil {
			return err
		}
		defer localWorkspaceBatchHooks.removeWorkspace(workspace)
		return localWorkspaceBatchHooks.runWorkerBatchAtVault(ctx, cfg, commands, workspace)
	}
	if !startsWithFullOLWRun(commands) {
		return errors.New("workspace mode requires the Synto run command before recording ingestion receipts")
	}
	lease, err := localWorkspaceBatchHooks.acquireVaultLease(vault, cfg.ExecutionID)
	if err != nil {
		return err
	}
	workspace := ""
	terminalAttempted := false
	workspacePublished := false
	recordTerminal := func(failure error, published bool) {
		if failure == nil || terminalAttempted {
			return
		}
		terminalAttempted = true
		_ = recordLocalWorkerFailureLog(workspace, vault, cfg, failure, published)
	}
	defer func() {
		if err := localWorkspaceBatchHooks.releaseVaultLease(lease); err != nil && runErr == nil {
			runErr = newWorkerFailure(nil, failureStageLeaseCleanup, failureClassIO, "", err)
			recordTerminal(runErr, true)
		}
	}()
	defer func() {
		if runErr != nil {
			recordTerminal(runErr, workspacePublished)
		}
	}()
	if err := localWorkspaceBatchHooks.recoverInterruptedPublish(vault); err != nil {
		return preserveWorkerFailure(err, failureStageGenerationPublish, failureClassIO)
	}
	snapshots, err := localWorkspaceBatchHooks.snapshotSources(vault)
	if err != nil {
		return preserveWorkerFailure(err, failureStageInputMaterialization, failureClassUnknown)
	}
	priorConcepts, err := localWorkspaceBatchHooks.snapshotConcepts(vault, snapshots)
	if err != nil {
		return preserveWorkerFailure(err, failureStageInputMaterialization, failureClassUnknown)
	}
	workspace, err = localWorkspaceBatchHooks.createWorkspace(cfg.WorkspaceDir, vault)
	if err != nil {
		return preserveWorkerFailure(err, failureStageInputMaterialization, failureClassIO)
	}
	defer func() {
		if runErr != nil {
			recordTerminal(runErr, workspacePublished)
		}
		if err := localWorkspaceBatchHooks.removeWorkspace(workspace); err != nil && runErr == nil {
			runErr = newWorkerFailure(nil, failureStageLeaseCleanup, failureClassIO, "", fmt.Errorf("cleanup workspace: %w", err))
			recordTerminal(runErr, true)
		}
	}()

	if err := localWorkspaceBatchHooks.materializeSnapshots(workspace, snapshots); err != nil {
		return preserveWorkerFailure(err, failureStageInputMaterialization, failureClassUnknown)
	}
	err = localWorkspaceBatchHooks.runWorkerBatchAtVault(ctx, cfg, commands, workspace)
	if err != nil {
		failure := preserveWorkerFailure(err, failureStageSyntoRun, failureClassUnknown)
		if recordErr := recordFailure(vault, snapshots, failure); recordErr != nil {
			return errors.Join(failure, preserveWorkerFailure(recordErr, failureStageReceiptRecording, failureClassRecordingFailure))
		}
		return failure
	}
	if err := localWorkspaceBatchHooks.reconcileWorkspaceSources(workspace, snapshots); err != nil {
		failure := preserveWorkerFailure(err, failureStageSourceReconciliation, failureClassUnknown)
		recordErr := recordFailure(vault, snapshots, failure)
		if recordErr != nil {
			return errors.Join(failure, preserveWorkerFailure(recordErr, failureStageReceiptRecording, failureClassRecordingFailure))
		}
		return failure
	}
	if err := localWorkspaceBatchHooks.reconcileWorkspaceConcepts(workspace, priorConcepts, snapshots); err != nil {
		failure := preserveWorkerFailure(err, failureStageConceptReconciliation, failureClassUnknown)
		recordErr := recordFailure(vault, snapshots, failure)
		if recordErr != nil {
			return errors.Join(failure, preserveWorkerFailure(recordErr, failureStageReceiptRecording, failureClassRecordingFailure))
		}
		return failure
	}
	if err := localWorkspaceBatchHooks.syncWorkspaceOutputs(workspace, vault, cfg.ExecutionID); err != nil {
		workspacePublished = publishJournalCommitted(vault)
		failureClass := failureClassIO
		if errors.Is(err, errObjectGenerationConflict) {
			failureClass = failureClassPublishConflict
		}
		failure := preserveWorkerFailure(err, failureStageGenerationPublish, failureClass)
		recordErr := recordFailure(vault, snapshots, failure)
		if recordErr != nil {
			return errors.Join(failure, preserveWorkerFailure(recordErr, failureStageReceiptRecording, failureClassRecordingFailure))
		}
		return failure
	}
	workspacePublished = true
	if err := localWorkspaceBatchHooks.recordSuccess(vault, snapshots, time.Now().UTC()); err != nil {
		return preserveWorkerFailure(err, failureStageReceiptRecording, failureClassRecordingFailure)
	}
	return nil
}

func runPostprocessCommand(ctx context.Context, cfg workerConfig) (runErr error) {
	cfg = configFromEnvironment(cfg)
	vault, err := resolveVaultPath(cfg)
	if err != nil {
		return err
	}
	vault, err = canonicalExistingDir(vault)
	if err != nil {
		return err
	}
	lease, err := localWorkspaceBatchHooks.acquireVaultLease(vault, cfg.ExecutionID)
	if err != nil {
		return err
	}
	lifecycle := &localLeaseLifecycle{vault: vault, cfg: cfg}
	defer func() {
		lifecycle.finish(&runErr, lease, localWorkspaceBatchHooks.removeWorkspace, localWorkspaceBatchHooks.releaseVaultLease)
	}()
	if err := localWorkspaceBatchHooks.recoverInterruptedPublish(vault); err != nil {
		return lifecycle.fail(err, failureStageGenerationPublish, failureClassIO)
	}
	workspace, err := localWorkspaceBatchHooks.createWorkspace(cfg.WorkspaceDir, vault)
	lifecycle.workspace = workspace
	if err != nil {
		return lifecycle.fail(err, failureStageInputMaterialization, failureClassIO)
	}
	if err := localWorkspaceBatchHooks.runPostprocessWithProvider(ctx, workspace, suggestedQueryProvider(cfg), nil); err != nil {
		return lifecycle.fail(err, failureStagePostprocess, failureClassIO)
	}
	if err := localWorkspaceBatchHooks.syncWorkspaceOutputs(workspace, vault, cfg.ExecutionID); err != nil {
		lifecycle.published = publishJournalCommitted(vault)
		failureClass := failureClassIO
		if errors.Is(err, errObjectGenerationConflict) {
			failureClass = failureClassPublishConflict
		}
		return lifecycle.fail(err, failureStageGenerationPublish, failureClass)
	}
	lifecycle.published = true
	return nil
}

// runSuggestedQueriesCommand regenerates query chips only, then publishes.
// Cloud mode materializes the current generation, rewrites suggested_queries,
// and CAS-publishes a complete carry-forward generation. Local mode uses the
// vault workspace path. It never runs Synto, index rebuild, or reconcile.
func runSuggestedQueriesCommand(ctx context.Context, cfg workerConfig) (runErr error) {
	cfg = configFromEnvironment(cfg)
	if cfg.Bucket != "" {
		cfg.cloudMode = true
		if cfg.VaultPath != "" || cfg.DataDir != "" || cfg.Workspace {
			return errWorkerConfigInvalid
		}
		if err := validateWorkerConfigBounds(cfg); err != nil {
			return annotateError(errWorkerInputInvalid, err)
		}
		return runCloudSuggestedQueries(ctx, cfg, newCloudObjectStore(cfg.Bucket))
	}
	vault, err := resolveVaultPath(cfg)
	if err != nil {
		return err
	}
	vault, err = canonicalExistingDir(vault)
	if err != nil {
		return err
	}
	lease, err := localWorkspaceBatchHooks.acquireVaultLease(vault, cfg.ExecutionID)
	if err != nil {
		return err
	}
	lifecycle := &localLeaseLifecycle{vault: vault, cfg: cfg}
	defer func() {
		lifecycle.finish(&runErr, lease, localWorkspaceBatchHooks.removeWorkspace, localWorkspaceBatchHooks.releaseVaultLease)
	}()
	if err := localWorkspaceBatchHooks.recoverInterruptedPublish(vault); err != nil {
		return lifecycle.fail(err, failureStageGenerationPublish, failureClassIO)
	}
	workspace, err := localWorkspaceBatchHooks.createWorkspace(cfg.WorkspaceDir, vault)
	lifecycle.workspace = workspace
	if err != nil {
		return lifecycle.fail(err, failureStageInputMaterialization, failureClassIO)
	}
	if err := localWorkspaceBatchHooks.runSuggestedQueriesStage(ctx, workspace, suggestedQueryProvider(cfg)); err != nil {
		return lifecycle.fail(err, failureStagePostprocess, failureClassIO)
	}
	if err := localWorkspaceBatchHooks.syncWorkspaceOutputs(workspace, vault, cfg.ExecutionID); err != nil {
		lifecycle.published = publishJournalCommitted(vault)
		failureClass := failureClassIO
		if errors.Is(err, errObjectGenerationConflict) {
			failureClass = failureClassPublishConflict
		}
		return lifecycle.fail(err, failureStageGenerationPublish, failureClass)
	}
	lifecycle.published = true
	return nil
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
	return len(commands) == 1 && len(commands[0]) >= 1 && commands[0][0] == "run"
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
	configHome, err := os.MkdirTemp("", "synto-config-*")
	if err != nil {
		return nil, fmt.Errorf("create isolated Synto config dir: %w", err)
	}
	env := []string{"XDG_CONFIG_HOME=" + configHome}
	if strings.TrimSpace(cfg.APIKey) != "" {
		env = append(env, "SYNTO_API_KEY="+cfg.APIKey, "DEEPSEEK_API_KEY="+cfg.APIKey)
	}
	return env, nil
}

func runOLWBatch(ctx context.Context, vault string, commands [][]string, stopOnError bool, env []string, stdout, stderr io.Writer) error {
	if err := validateSyntoCommandBatch(commands); err != nil {
		return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassValidation, "", err)
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	var batchErr error
	for i, command := range commands {
		log.Printf("[%d/%d] synto command", i+1, len(commands))
		if err := validateSyntoPipelineSafety(filepath.Join(vault, "synto.toml")); err != nil {
			return newWorkerFailure(ctx, failureStageSyntoConfigValidation, failureClassValidation, "", err)
		}
		if err := execOLW(ctx, vault, command, env, stdout, stderr); err != nil {
			wrapped := fmt.Errorf("synto command failed: %w", newWorkerFailure(ctx, failureStageSyntoRun, failureClassChildExit, failureChildRun, err))
			if stopOnError {
				return wrapped
			}
			log.Printf("synto command failed (continuing)")
			batchErr = errors.Join(batchErr, wrapped)
		}
	}
	return batchErr
}

func execOLWCommand(ctx context.Context, vault string, command []string, env []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "synto", command...)
	cmd.Dir = vault
	cmd.Env = allowlistedSyntoEnvironment(env)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func allowlistedSyntoEnvironment(extra []string) []string {
	env := make([]string, 0, len(extra)+1)
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	for _, item := range extra {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		switch key {
		case "XDG_CONFIG_HOME", "SYNTO_API_KEY", "DEEPSEEK_API_KEY":
			env = append(env, item)
		}
	}
	return env
}

func pipelineLogWriters(vault string, cfg workerConfig, commands [][]string, suppressOutput bool) (io.Writer, io.Writer, func() error, error) {
	secrets := logSecrets(cfg)
	live := map[pipelineStream]io.Writer{}
	if !suppressOutput {
		live[stdoutStream] = pipelineLiveDestination(stdoutStream)
		live[stderrStream] = pipelineLiveDestination(stderrStream)
	}
	pipeline := newLivePipeline(nil, live, cfg, secrets)
	if strings.TrimSpace(cfg.ExecutionID) == "" {
		return pipeline.writer(stdoutStream), pipeline.writer(stderrStream), pipeline.Close, nil
	}
	path, err := pipelineLogPath(vault, cfg.ExecutionID)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		pipeline.recordDegraded("durable", fmt.Errorf("mkdir pipeline log dir: %w", err))
		return pipeline.writer(stdoutStream), pipeline.writer(stderrStream), pipeline.Close, nil
	}
	file, err := os.Create(path)
	if err != nil {
		pipeline.recordDegraded("durable", fmt.Errorf("create pipeline log: %w", err))
		return pipeline.writer(stdoutStream), pipeline.writer(stderrStream), pipeline.Close, nil
	}
	durable := newDiagnosticSink([]io.Writer{pipelineDurableDestination(file)}, secrets)
	pipeline = newLivePipeline(durable, live, cfg, secrets)
	return pipeline.writer(stdoutStream), pipeline.writer(stderrStream), func() error {
		pipelineErr := pipeline.Close()
		fileErr := file.Close()
		if fileErr != nil {
			pipeline.recordDegraded("durable", fileErr)
		}
		return pipelineErr
	}, nil
}

type diagnosticSink struct {
	writers   []io.Writer
	secrets   []string
	pending   []byte
	output    []byte
	mu        sync.Mutex
	closed    bool
	truncated bool
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
	if w.truncated {
		return original, nil
	}
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
	if len(data) > 0 && !w.truncated {
		// Retain the full bounded output until close so an overflow can replace
		// the final marker-sized suffix without changing the total size.
		text := redactDiagnosticBytes(data, w.secrets)
		remaining := maxPipelineLog - len(w.output)
		if len(text) > remaining {
			w.output = append(w.output, text[:remaining]...)
			w.truncated = true
			copy(w.output[maxPipelineLog-len(pipelineLogTruncationMarker):], pipelineLogTruncationMarker)
		} else {
			w.output = append(w.output, text...)
		}
	}
	if final {
		for _, dst := range w.writers {
			if _, err := dst.Write(w.output); err != nil {
				return err
			}
		}
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
	// Only real credentials. Identity, paths, and command args stay visible.
	return diagnosticSecrets(cfg, nil)
}

// diagnosticSecrets returns only real credentials. Execution IDs, tenant IDs,
// paths, and command args are control-plane observability and must remain visible.
// When an explicit API key is set, oversized inherited env keys are ignored so
// they cannot disable redaction or validation by sheer size.
func diagnosticSecrets(cfg workerConfig, _ [][]string) []string {
	values := []string{cfg.APIKey}
	if cfg.apiKeySet {
		return values
	}
	for _, value := range []string{os.Getenv("LLM_API_KEY"), os.Getenv("DEEPSEEK_API_KEY")} {
		if value != "" && len(value) <= maxWorkerKeyBytes {
			values = append(values, value)
		}
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
	if err := validateSyntoCommandBatch(commands); err != nil {
		return err
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

// validateSyntoCommandBatch is intentionally stricter than JSON/argument
// validation. The Cloud contract is one unattended full Synto run; allowing a
// second command or a force-like flag would bypass Synto's manual-edit guard
// or expose mutation-capable maintenance/curation commands.
func validateSyntoCommandBatch(commands [][]string) error {
	if len(commands) != 1 || len(commands[0]) < 1 || commands[0][0] != "run" {
		return errors.New("only one Synto run command is allowed")
	}
	if len(commands[0]) == 1 {
		return nil
	}
	if len(commands[0]) == 2 && commands[0][1] == "--auto-approve" {
		return nil
	}
	return errors.New("unsafe Synto command or flag")
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
	return runPostprocessWithProvider(ctx, vault, nil, nil)
}

func suggestedQueryProvider(cfg workerConfig) suggestedqueries.Provider {
	if cfg.suggestedQueriesProvider != nil {
		return cfg.suggestedQueriesProvider
	}
	if !cfg.SuggestedQueries {
		return nil
	}
	return llm.NewClient(cfg.APIKey)
}

// runPostprocessWithProvider rebuilds cache/index artifacts, then regenerates
// query chips when provider is non-nil (or ensures empty/last-known-good when nil).
// Query-chip work is also available alone via runSuggestedQueriesStage / CLI.
func runPostprocessWithProvider(ctx context.Context, vault string, provider suggestedqueries.Provider, warn io.Writer) error {
	if err := runCacheIndexStage(ctx, vault, warn); err != nil {
		return err
	}
	if err := runSuggestedQueriesStage(ctx, vault, provider); err != nil {
		return err
	}
	return nil
}

// runCacheIndexStage rebuilds id_map/concepts, dormant cache, and raw_status.
// It does not touch suggested_queries.json.
func runCacheIndexStage(ctx context.Context, vault string, warn io.Writer) error {
	store := fsstore.New(vault)
	index, err := readSyntoIndexTruth(vault)
	if err != nil {
		return fmt.Errorf("read Synto identity authority: %w", err)
	}
	if index.Present {
		plan, err := syntoIdentityPlanFromIndex(index)
		if err != nil {
			return fmt.Errorf("build Synto identity authority: %w", err)
		}
		reportUnboundActiveEntities(plan, index, warn)
		if err := enforceActiveEntityCoverage(plan); err != nil {
			return fmt.Errorf("postprocess Synto index: %w", err)
		}
		if _, err := wikiindex.RebuildWithSyntoIdentity(ctx, store, plan); err != nil {
			return fmt.Errorf("postprocess Synto index: %w", err)
		}
	} else if _, err := wikiindex.Rebuild(ctx, store); err != nil {
		return fmt.Errorf("postprocess: %w", err)
	}
	if err := ensureDormantConceptCache(vault); err != nil {
		return fmt.Errorf("postprocess dormant concepts: %w", err)
	}
	if err := writeRawStatus(ctx, vault); err != nil {
		return fmt.Errorf("postprocess raw status: %w", err)
	}
	return nil
}

// runSuggestedQueriesStage regenerates cache/suggested_queries.json only.
// Provider nil preserves last-known-good or writes a valid empty v2 artifact.
func runSuggestedQueriesStage(ctx context.Context, vault string, provider suggestedqueries.Provider) error {
	if err := writeSuggestedQueries(ctx, vault, provider); err != nil {
		return fmt.Errorf("suggested queries: %w", err)
	}
	return nil
}

func ensureDormantConceptCache(vault string) error {
	path := "cache/dormant_concepts.jsonl"
	if _, err := readBoundedRegularFileWithin(vault, path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeFileAtomicWithin(vault, path, nil)
}

func writeSuggestedQueries(ctx context.Context, vault string, provider suggestedqueries.Provider) error {
	if provider == nil {
		return ensureEmptySuggestedQueries(ctx, vault)
	}
	store := fsstore.New(vault)
	data, err := readBoundedRegularFileWithin(vault, wikiindex.ConceptsJSONLPath)
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

	entries, err := decodeSuggestedQueryConcepts(data)
	if err != nil {
		return fmt.Errorf("decode suggested query concepts: %w", err)
	}
	artifact, err := suggestedqueries.Generate(ctx, provider, "", entries, mtimes, suggestedqueries.GenerationMetadata{
		Model:         suggestedQueryModel,
		PromptVersion: suggestedqueries.PromptVersion,
	}, time.Now())
	if err != nil {
		log.Printf("postprocess suggested queries: generation failed; preserving last-known-good artifact: %v", err)
		return ensureEmptySuggestedQueries(ctx, vault)
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

func ensureEmptySuggestedQueries(ctx context.Context, vault string) error {
	store := fsstore.New(vault)
	if _, err := store.ReadFile(ctx, suggestedqueries.Path); err == nil {
		return nil
	} else if !errors.Is(err, wikiindex.ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing suggested queries: %w", err)
	}
	artifact := suggestedqueries.Artifact{
		Version:    2,
		Queries:    []string{},
		Candidates: []suggestedqueries.Candidate{},
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	if _, err := store.WriteBytesAtomic(ctx, payload, "cache/suggested_queries.json.tmp", suggestedqueries.Path); err != nil {
		return fmt.Errorf("write empty suggested queries: %w", err)
	}
	return nil
}

func decodeSuggestedQueryConcepts(data []byte) ([]conceptcache.Entry, error) {
	if len(data) == 0 {
		return nil, nil
	}
	entries := make([]conceptcache.Entry, 0)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if len(entries) >= generation.MaxFiles {
			return nil, generation.ErrLogicalEntryLimit
		}
		var entry conceptcache.Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
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
	SourceID         string
	RawPath          string
	RawBytes         []byte
	RawSHA256        string
	SyntoContentHash string
	AnnotationBody   string
	AnnotationSHA    string
	Fingerprint      string
	Dirty            bool
	Tombstone        bool
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
	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return nil, fmt.Errorf("decode source map: %w", err)
	}

	capacity := len(ids.SourceMeta)
	if len(ids.Source) > capacity {
		capacity = len(ids.Source)
	}
	snapshots := make([]sourceSnapshot, 0, capacity)
	mappedRawPaths := make(map[string]string, capacity)
	addSnapshot := func(sourceID, rawPath string) error {
		if !annotation.ValidSourceID(sourceID) || !safeMappedRawPath(rawPath) {
			return fmt.Errorf("unsafe source mapping %q -> %q", sourceID, rawPath)
		}
		if prior, exists := mappedRawPaths[rawPath]; exists {
			return fmt.Errorf("duplicate source mapping %q and %q -> %q", prior, sourceID, rawPath)
		}
		mappedRawPaths[rawPath] = sourceID
		raw, err := readRegularFileWithin(vault, rawPath)
		if errors.Is(err, os.ErrNotExist) {
			snapshots = append(snapshots, sourceSnapshot{SourceID: sourceID, RawPath: rawPath, Tombstone: true})
			return nil
		}
		if err != nil {
			return fmt.Errorf("read source raw %q: %w", rawPath, err)
		}
		ann, err := readAnnotation(vault, sourceID, rawPath)
		if err != nil {
			return err
		}
		rawSum := sha256.Sum256(raw)
		rawSHA := fmt.Sprintf("%x", rawSum[:])
		fingerprint := sourcestatus.Fingerprint(rawSHA, ann.SHA256)
		receipt := status.Sources[sourceID]
		snapshot := sourceSnapshot{
			SourceID: sourceID, RawPath: rawPath, RawBytes: raw, RawSHA256: rawSHA,
			AnnotationBody: ann.Body, AnnotationSHA: ann.SHA256, Fingerprint: fingerprint,
			Dirty: !sourcestatus.ValidReceipt(receipt, rawPath) || receipt.LastIngestFingerprint != fingerprint,
		}
		snapshot.SyntoContentHash = syntoSourceContentHash(snapshot)
		snapshots = append(snapshots, snapshot)
		return nil
	}

	if len(ids.Source) == 0 {
		for sourceID, meta := range ids.SourceMeta {
			if err := addSnapshot(sourceID, strings.TrimSpace(meta.SourceFile)); err != nil {
				return nil, err
			}
		}
	} else {
		for sourceID := range ids.SourceMeta {
			if _, exists := ids.Source[sourceID]; !exists {
				return nil, fmt.Errorf("source metadata %q has no source mapping", sourceID)
			}
		}
		sourceIDs := make([]string, 0, len(ids.Source))
		for sourceID := range ids.Source {
			sourceIDs = append(sourceIDs, sourceID)
		}
		sort.Strings(sourceIDs)
		mappedSlugs := make(map[string]string, len(ids.Source))
		for _, sourceID := range sourceIDs {
			if !annotation.ValidSourceID(sourceID) {
				return nil, fmt.Errorf("unsafe source mapping %q -> %q", sourceID, ids.Source[sourceID])
			}
			slug := ids.Source[sourceID]
			pagePath, err := safeSourcePagePath(slug)
			if err != nil || strings.TrimSpace(slug) != slug {
				return nil, fmt.Errorf("unsafe source mapping %q -> %q", sourceID, slug)
			}
			if prior, exists := mappedSlugs[slug]; exists {
				return nil, fmt.Errorf("duplicate legacy source slug %q for %q and %q", slug, prior, sourceID)
			}
			mappedSlugs[slug] = sourceID

			meta, hasMeta := ids.SourceMeta[sourceID]
			rawPath := strings.TrimSpace(meta.SourceFile)
			if hasMeta {
				if meta.Slug != "" && meta.Slug != slug {
					return nil, fmt.Errorf("source metadata slug disagrees for %q: %q != %q", sourceID, meta.Slug, slug)
				}
			} else {
				page, readErr := readBoundedRegularFileWithin(vault, pagePath)
				if errors.Is(readErr, os.ErrNotExist) {
					return nil, fmt.Errorf("missing legacy source page %q: %w", pagePath, readErr)
				}
				if readErr != nil {
					return nil, fmt.Errorf("read legacy source page %q: %w", pagePath, readErr)
				}
				parsed, parseErr := parseSyntoSourcePage(page)
				if parseErr != nil {
					return nil, fmt.Errorf("parse legacy source page %q: %w", pagePath, parseErr)
				}
				value, ok := parsed.fields["source_file"].(string)
				rawPath = strings.TrimSpace(value)
				if !ok || !safeMappedRawPath(rawPath) {
					return nil, fmt.Errorf("missing or unsafe legacy source_file for %q", pagePath)
				}
			}
			if err := addSnapshot(sourceID, rawPath); err != nil {
				return nil, err
			}
		}
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
	for _, dir := range []string{"raw", "wiki", "cache", ".olw", ".synto"} {
		if err := copyTreeRoot(vaultRoot, workspaceRoot, dir, dir, nil); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.RemoveAll(workspace)
			return "", fmt.Errorf("copy %s into workspace: %w", dir, err)
		}
	}
	if err := copyOneIfExists(vaultRoot, workspaceRoot, "wiki.toml"); err != nil {
		_ = os.RemoveAll(workspace)
		return "", fmt.Errorf("copy wiki.toml into workspace: %w", err)
	}
	if err := copyOneIfExists(vaultRoot, workspaceRoot, "synto.toml"); err != nil {
		_ = os.RemoveAll(workspace)
		return "", fmt.Errorf("copy synto.toml into workspace: %w", err)
	}
	return workspace, nil
}

func materializeSnapshots(workspace string, snapshots []sourceSnapshot) error {
	for _, snapshot := range snapshots {
		if snapshot.Tombstone {
			continue
		}
		// Every non-empty annotation is materialized for every fresh workspace.
		// Receipts only determine BFF dirty state; they must never change the OLW
		// byte stream for otherwise identical source inputs.
		data := materializedSourceBytes(snapshot)
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
	if err != nil {
		return sourcestatus.Artifact{}, fmt.Errorf("invalid source status: %w", err)
	}
	if artifact.Version != 1 {
		return sourcestatus.Artifact{}, fmt.Errorf("invalid source status: unsupported version %d", artifact.Version)
	}
	return artifact, nil
}

func recordSuccess(vault string, snapshots []sourceSnapshot, now time.Time) error {
	artifact, err := readSourceStatus(vault)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		if snapshot.Tombstone {
			continue
		}
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
		if snapshot.Tombstone {
			continue
		}
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

func removeRegularFileWithin(root, rel string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	info, err := r.Lstat(rel)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to remove non-regular path %q", rel)
	}
	return r.Remove(rel)
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
