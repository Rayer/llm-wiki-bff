package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/suggestedqueries"
)

func TestSnapshotReaderRejectsOversizedSupportedArtifactsAndPages(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name string
		path string
		size int64
	}{
		{name: "concepts", path: conceptsPath, size: generation.MaxFileBytes + 1},
		{name: "suggested queries", path: suggestedPath, size: suggestedqueries.MaxArtifactBytes + 1},
		{name: "page", path: "wiki/large.md", size: generation.MaxFileBytes + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, test.path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, test.size); err != nil {
				t.Fatal(err)
			}
			reader := newSnapshotReader(root)
			var err error
			if test.name == "page" {
				_, _, err = reader.GetPage(context.Background(), "large", "concepts")
			} else {
				_, err = reader.ReadFile(context.Background(), test.path)
			}
			if err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("read error = %v, want deterministic size rejection", err)
			}
		})
	}
}

func TestPreflightFreezesConceptsBeforeCacheLoad(t *testing.T) {
	root := t.TempDir()
	original := `{"slug":"original","title":"Original"}` + "\n"
	replacement := `{"slug":"replacement","title":"Replacement"}` + "\n"
	writeTestFile(t, filepath.Join(root, conceptsPath), original)
	prepared, err := preflightSnapshot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, conceptsPath), replacement)
	entries, err := prepared.cache.All(context.Background(), prepared.reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Slug != "original" || prepared.digest == "" {
		t.Fatalf("entries=%#v digest=%q, want frozen original corpus", entries, prepared.digest)
	}
}

func TestSnapshotReaderPreservesMissingOptionalSuggestedArtifact(t *testing.T) {
	reader := newSnapshotReader(t.TempDir())
	_, err := reader.ReadFile(context.Background(), suggestedPath)
	if !errors.Is(err, errSnapshotPathNotFound) {
		t.Fatalf("missing suggested artifact error = %v, want path-not-found", err)
	}
}
