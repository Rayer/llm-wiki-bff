package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/rayert/llm-wiki-bff/internal/config"
	"github.com/rayert/llm-wiki-bff/internal/gcs"
	"github.com/rayert/llm-wiki-bff/internal/sync"
)

func main() {
	v := viper.New()
	v.SetDefault("dry-run", false)

	pflag.String("vault", "", "Path to Obsidian vault (or set VAULT env)")
	pflag.Bool("dry-run", false, "Report without uploading")
	pflag.StringSlice("dir", []string{"wiki", "raw", ".olw"}, "Vault subdirectories to sync (repeatable)")
	pflag.String("uid", "", "Override config user_id")
	pflag.String("pid", "", "Override config project_id")
	pflag.Parse()
	v.BindPFlags(pflag.CommandLine)

	v.SetConfigName("config")
	v.SetConfigType("toml")
	v.AddConfigPath(".")
	_ = v.ReadInConfig() // ignore file-not-found; vault can come from env

	vaultPath := v.GetString("vault")
	if vaultPath == "" {
		fmt.Fprintln(os.Stderr, "Error: --vault (or VAULT env) is required")
		os.Exit(1)
	}
	dryRun := v.GetBool("dry-run")

	// Vault validity check
	if info, err := os.Stat(vaultPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: vault path does not exist: %s\n", vaultPath)
		os.Exit(1)
	} else if err != nil {
		log.Fatalf("Error accessing vault: %v", err)
	} else if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Error: vault path is not a directory: %s\n", vaultPath)
		os.Exit(1)
	}

	// Warn if no expected subdirectories exist (might be a new vault)
	expectedDirs := []string{"wiki", "raw", ".olw"}
	found := false
	for _, d := range expectedDirs {
		if fi, err := os.Stat(filepath.Join(vaultPath, d)); err == nil && fi.IsDir() {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "Warning: vault at %s does not contain any of wiki/, raw/, or .olw/ — you may be syncing a new vault\n", vaultPath)
	}

	cfg, err := config.Load(".")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Override from CLI flags if set
	uid := v.GetString("uid")
	if uid != "" {
		cfg.UserID = uid
	}
	pid := v.GetString("pid")
	if pid != "" {
		cfg.ProjectID = pid
	}

	if cfg.Bucket == "" || cfg.UserID == "" || cfg.ProjectID == "" {
		log.Fatal("config.toml must set bucket, user_id, and project_id (use --uid and --pid to override)")
	}

	log.Printf("Vault: %s", vaultPath)
	log.Printf("GCS:   gs://%s/users/%s/projects/%s/", cfg.Bucket, cfg.UserID, cfg.ProjectID)
	if dryRun {
		log.Println("Mode:  DRY RUN (no uploads)")
	}

	// Get dirs from --dir flag
	dirs := v.GetStringSlice("dir")
	log.Printf("Dirs:  %v", dirs)

	// Create GCS client
	ctx := context.Background()
	gcsClient, err := gcs.NewClient(cfg.Bucket, cfg.UserID, cfg.ProjectID)
	if err != nil {
		log.Fatalf("Failed to create GCS client: %v", err)
	}

	// Run sync
	stats, err := sync.Sync(ctx, gcsClient, vaultPath, dryRun, dirs)
	if err != nil {
		log.Fatalf("Sync failed: %v", err)
	}

	// Print results
	fmt.Println()
	fmt.Println("── Sync Results ──")
	for _, r := range stats.Results {
		marker := ""
		switch r.Action {
		case "upload":
			marker = "↑"
		case "skip":
			marker = "="
		case "error":
			marker = "✗"
		}
		fmt.Printf("  %s %s → %s", marker, r.LocalPath, r.GCSRelPath)
		if r.Err != nil {
			fmt.Printf("  (error: %v)", r.Err)
		}
		fmt.Println()
	}

	fmt.Println()
	fmt.Printf("Uploaded: %d  Skipped: %d  Errors: %d\n", stats.Uploaded, stats.Skipped, stats.Errors)

	if stats.Errors > 0 {
		os.Exit(1)
	}
}
