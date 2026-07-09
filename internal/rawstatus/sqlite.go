package rawstatus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	store "github.com/rayer/llm-wiki-bff/internal/storage"
	_ "modernc.org/sqlite"
)

func StateDBPath(vault string) string {
	return filepath.Join(vault, ".olw", "state.db")
}

func BuildFromStateDB(ctx context.Context, dbPath string, files []store.RawFile, now time.Time) (Artifact, error) {
	artifact := EmptyArtifact(now)
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return artifact, nil
		}
		return artifact, err
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return artifact, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `select path, content_hash, status, coalesce(ingested_at, ''), coalesce(error, '') from raw_notes`)
	if err != nil {
		return artifact, fmt.Errorf("query raw_notes: %w", err)
	}
	defer rows.Close()

	byPath := map[string]FileStatus{}
	for rows.Next() {
		var status FileStatus
		if err := rows.Scan(&status.Path, &status.SHA256, &status.OLWStatus, &status.IngestedAt, &status.Error); err != nil {
			return artifact, err
		}
		byPath[status.Path] = status
	}
	if err := rows.Err(); err != nil {
		return artifact, err
	}

	for _, raw := range files {
		status := byPath[raw.Path]
		status.Path = raw.Path
		status.Ingested = isIngested(raw, status)
		artifact.Files[raw.Name] = status
	}
	return artifact, nil
}
