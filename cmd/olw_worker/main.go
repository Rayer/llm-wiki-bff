package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex/fsstore"
	"github.com/spf13/cobra"
)

type workerConfig struct {
	VaultPath   string
	DataDir     string
	UserID      string
	ProjectID   string
	APIKey      string
	Postprocess bool
	StopOnError bool
}

var execOLW = execOLWCommand

func main() {
	if err := newRootCommand().Execute(); err != nil {
		log.Fatalf("worker: %v", err)
	}
}

func newRootCommand() *cobra.Command {
	cfg := workerConfig{Postprocess: true, StopOnError: true}
	var noPostprocess bool

	rootCmd := &cobra.Command{
		Use:   "worker",
		Short: "Run OLW commands against a local vault",
	}
	rootCmd.PersistentFlags().StringVar(&cfg.VaultPath, "vault", envOr("VAULT_PATH", ""), "project vault path")
	rootCmd.PersistentFlags().StringVar(&cfg.DataDir, "data-dir", envOr("DATA_DIR", "/data"), "mounted data root")
	rootCmd.PersistentFlags().StringVar(&cfg.UserID, "user-id", envOr("USER_ID", ""), "user id")
	rootCmd.PersistentFlags().StringVar(&cfg.ProjectID, "project-id", envOr("PROJECT_ID", ""), "project id")
	rootCmd.PersistentFlags().StringVar(&cfg.APIKey, "api-key", envOr("LLM_API_KEY", ""), "LLM API key")
	rootCmd.PersistentFlags().BoolVar(&cfg.StopOnError, "stop-on-error", true, "stop on first failed OLW command")

	runCmd := &cobra.Command{
		Use:   "run <json array of arrays>",
		Short: "Execute a batch of OLW commands",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runCfg := cfg
			if noPostprocess {
				runCfg.Postprocess = false
			}
			return runWorkerBatch(cmd.Context(), runCfg, args[0])
		},
	}
	runCmd.Flags().BoolVar(&cfg.Postprocess, "postprocess", true, "run postprocess after successful batch")
	runCmd.Flags().BoolVar(&noPostprocess, "no-postprocess", false, "skip postprocess after batch")

	postprocessCmd := &cobra.Command{
		Use:   "postprocess",
		Short: "Rebuild local BFF cache and index artifacts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPostprocessCommand(cmd.Context(), cfg)
		},
	}

	rootCmd.AddCommand(runCmd, postprocessCmd)
	return rootCmd
}

func runWorkerBatch(ctx context.Context, cfg workerConfig, rawCommands string) error {
	commands, err := parseCommandBatch(rawCommands)
	if err != nil {
		return err
	}
	vault, err := resolveVaultPath(cfg)
	if err != nil {
		return err
	}
	if err := requireExistingDir(vault); err != nil {
		return err
	}
	if err := cleanStaleLock(vault, 5*time.Minute); err != nil {
		return err
	}
	if err := ensureWikiTOML(vault, cfg); err != nil {
		return err
	}
	if err := runOLWBatch(ctx, vault, commands, cfg.StopOnError); err != nil {
		return err
	}
	if cfg.Postprocess {
		if err := runPostprocess(ctx, vault); err != nil {
			return err
		}
	}
	return nil
}

func runPostprocessCommand(ctx context.Context, cfg workerConfig) error {
	vault, err := resolveVaultPath(cfg)
	if err != nil {
		return err
	}
	if err := requireExistingDir(vault); err != nil {
		return err
	}
	return runPostprocess(ctx, vault)
}

func parseCommandBatch(raw string) ([][]string, error) {
	var commands [][]string
	if err := json.Unmarshal([]byte(raw), &commands); err != nil {
		return nil, fmt.Errorf("parse command batch: %w", err)
	}
	if len(commands) == 0 {
		return nil, errors.New("command batch is empty")
	}
	for i, command := range commands {
		if len(command) == 0 {
			return nil, fmt.Errorf("command %d is empty", i)
		}
		if strings.TrimSpace(command[0]) == "" {
			return nil, fmt.Errorf("command %d has empty command name", i)
		}
	}
	return commands, nil
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

func runOLWBatch(ctx context.Context, vault string, commands [][]string, stopOnError bool) error {
	var batchErr error
	for i, command := range commands {
		log.Printf("[%d/%d] olw %v", i+1, len(commands), command)
		if err := execOLW(ctx, vault, command); err != nil {
			wrapped := fmt.Errorf("olw %v: %w", command, err)
			if stopOnError {
				return wrapped
			}
			log.Printf("%v (continuing)", wrapped)
			batchErr = errors.Join(batchErr, wrapped)
		}
	}
	return batchErr
}

func execOLWCommand(ctx context.Context, vault string, command []string) error {
	cmd := exec.CommandContext(ctx, "olw", command...)
	cmd.Dir = vault
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
	return nil
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

func envOr(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}
