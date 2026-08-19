package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
)

type snapshotReader struct {
	root string
}

var _ cache.Reader = (*snapshotReader)(nil)

func newSnapshotReader(root string) *snapshotReader { return &snapshotReader{root: root} }

func (r *snapshotReader) Prefix() string { return "query-experiment/" + filepath.Base(r.root) }

func (r *snapshotReader) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if relPath != "cache/concepts.jsonl" {
		return nil, errors.New("snapshot reader permits only cache/concepts.jsonl")
	}
	file, err := openSnapshotRegularFile(r.root, relPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
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
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return data, nil
}
