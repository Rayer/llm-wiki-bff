package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/suggestedqueries"
)

type snapshotReader struct {
	root            string
	concepts        []byte
	suggested       []byte
	conceptsFrozen  bool
	suggestedFrozen bool
}

var _ cache.Reader = (*snapshotReader)(nil)

func newSnapshotReader(root string) *snapshotReader { return &snapshotReader{root: root} }

func (r *snapshotReader) freezeConcepts(data []byte) {
	r.concepts = data
	r.conceptsFrozen = true
}

func (r *snapshotReader) freezeSuggested(data []byte) {
	r.suggested = data
	r.suggestedFrozen = true
}

func (r *snapshotReader) Prefix() string { return "query-experiment/" + filepath.Base(r.root) }

func (r *snapshotReader) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if relPath != "cache/concepts.jsonl" && relPath != "cache/suggested_queries.json" {
		return nil, errors.New("snapshot reader permits only supported cache artifacts")
	}
	if relPath == "cache/concepts.jsonl" && r.conceptsFrozen {
		return r.concepts, nil
	}
	if relPath == "cache/suggested_queries.json" && r.suggestedFrozen {
		return r.suggested, nil
	}
	file, err := openSnapshotRegularFile(r.root, relPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	limit := int64(generation.MaxFileBytes)
	if relPath == "cache/suggested_queries.json" {
		limit = suggestedqueries.MaxArtifactBytes
	}
	data, err := readBoundedSnapshotFile(file, limit, relPath)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (r *snapshotReader) ListConcepts(context.Context, bool) ([]gcs.WikiPage, error) {
	return nil, errors.New("snapshot reader does not enumerate wiki concepts")
}

func (r *snapshotReader) GetPage(ctx context.Context, slug, category string) (*gcs.WikiPage, []byte, error) {
	if category != "concepts" || slug == "" || strings.ContainsAny(slug, "/\\") || filepath.Base(slug) != slug {
		return nil, nil, errors.New("invalid snapshot page")
	}
	for _, relPath := range []string{"wiki/" + slug + ".md", "wiki/.drafts/" + slug + ".md"} {
		data, err := r.readPage(ctx, relPath)
		if err == nil {
			return &gcs.WikiPage{Slug: slug, Title: slug}, data, nil
		}
		if !errors.Is(err, errSnapshotNotFound) {
			return nil, nil, err
		}
	}
	return nil, nil, errSnapshotNotFound
}

var errSnapshotNotFound = errors.New("snapshot page not found")

func (r *snapshotReader) readPage(ctx context.Context, relPath string) ([]byte, error) {
	file, err := openSnapshotRegularFile(r.root, relPath)
	if err != nil {
		if errors.Is(err, errSnapshotPathNotFound) {
			return nil, errSnapshotNotFound
		}
		return nil, err
	}
	defer file.Close()
	data, err := readBoundedSnapshotFile(file, generation.MaxFileBytes, relPath)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func readBoundedSnapshotFile(file *os.File, limit int64, relPath string) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("snapshot file %s exceeds %d-byte limit", relPath, limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("snapshot file %s exceeds %d-byte limit", relPath, limit)
	}
	return data, nil
}
