package sync

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/gcs"
)

// Result records the outcome of syncing a single file.
type Result struct {
	LocalPath  string
	GCSRelPath string
	Action     string // "upload", "skip", "error"
	Err        error
}

// Stats accumulates sync results.
type Stats struct {
	Uploaded int
	Skipped  int
	Errors   int
	Results  []Result
}

// excludeDirs are directory names to skip entirely.
var excludeDirs = map[string]bool{
	".git":      true,
	".obsidian": true,
	".trash":    true,
	"chroma":    true, // .olw/chroma — large embeddings, not needed by BFF
}

// excludeFiles are file names to skip (matched against base name).
var excludeFiles = map[string]bool{
	".DS_Store":     true,
	"pipeline.lock": true, // runtime lock file
}

// Sync walks the vault and uploads new/changed files to GCS.
// dirs specifies which vault subdirectories to sync (e.g., ["wiki", "raw"]).
// If dryRun is true, it only reports what would happen without uploading.
func Sync(ctx context.Context, client *gcs.Client, vaultPath string, dryRun bool, dirs []string) (*Stats, error) {
	stats := &Stats{}

	for _, d := range dirs {
		absDir := filepath.Join(vaultPath, d)
		if _, err := os.Stat(absDir); os.IsNotExist(err) {
			continue
		}
		if err := walkDir(ctx, client, absDir, d, d, vaultPath, dryRun, stats); err != nil {
			return stats, fmt.Errorf("walk %s: %w", d, err)
		}
	}

	return stats, nil
}

func walkDir(ctx context.Context, client *gcs.Client, absDir, gcsDir, localPrefix, vaultPath string, dryRun bool, stats *Stats) error {
	return filepath.WalkDir(absDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip excluded directories
		if d.IsDir() {
			if excludeDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip excluded files
		if excludeFiles[d.Name()] {
			return nil
		}

		// Only sync .md + .db + .toml files
		if !strings.HasSuffix(d.Name(), ".md") && !strings.HasSuffix(d.Name(), ".db") && !strings.HasSuffix(d.Name(), ".toml") {
			return nil
		}
		// Skip pipeline.lock (runtime lock, not needed on cloud)
		if d.Name() == "pipeline.lock" {
			return nil
		}

		// Compute GCS relative path
		rel, err := filepath.Rel(filepath.Join(vaultPath, localPrefix), path)
		if err != nil {
			return err
		}
		// Fix Windows backslashes just in case
		rel = filepath.ToSlash(rel)
		gcsRelPath := gcsDir + "/" + rel

		syncFile(ctx, client, path, gcsRelPath, dryRun, stats)
		return nil
	})
}

func syncFile(ctx context.Context, client *gcs.Client, localPath, gcsRelPath string, dryRun bool, stats *Stats) {
	result := Result{LocalPath: localPath, GCSRelPath: gcsRelPath}

	// Compute local SHA256
	localDigest, err := fileSHA256(localPath)
	if err != nil {
		result.Action = "error"
		result.Err = err
		stats.Results = append(stats.Results, result)
		stats.Errors++
		return
	}

	// Check remote SHA256
	remoteDigest, err := client.GetMetaSHA256(ctx, gcsRelPath)
	if err != nil {
		result.Action = "error"
		result.Err = fmt.Errorf("check remote: %w", err)
		stats.Results = append(stats.Results, result)
		stats.Errors++
		return
	}

	if localDigest == remoteDigest && remoteDigest != "" {
		result.Action = "skip"
		stats.Results = append(stats.Results, result)
		stats.Skipped++
		return
	}

	if dryRun {
		result.Action = "upload"
		stats.Results = append(stats.Results, result)
		stats.Uploaded++
		return
	}

	// Upload
	if _, err := client.UploadFileWithDigest(ctx, localPath, gcsRelPath, localDigest); err != nil {
		result.Action = "error"
		result.Err = fmt.Errorf("upload: %w", err)
		stats.Errors++
	} else {
		result.Action = "upload"
		stats.Uploaded++
	}
	stats.Results = append(stats.Results, result)
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
