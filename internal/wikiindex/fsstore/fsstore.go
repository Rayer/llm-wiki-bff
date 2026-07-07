package fsstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

type Store struct {
	root string
}

func New(root string) *Store {
	return &Store{root: root}
}

func (s *Store) ListMarkdownFiles(ctx context.Context, dir string) ([]wikiindex.MarkdownFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fullDir, err := s.resolveExistingPath(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []wikiindex.MarkdownFile{}, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []wikiindex.MarkdownFile{}, nil
		}
		return nil, err
	}

	baseDir := cleanSlashPath(dir)
	files := make([]wikiindex.MarkdownFile, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || path.Ext(entry.Name()) != ".md" {
			continue
		}

		childPath := filepath.Join(fullDir, entry.Name())
		resolvedChild, err := s.verifyResolvedPath(childPath)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(resolvedChild)
		if err != nil {
			return nil, err
		}
		relPath := entry.Name()
		if baseDir != "" {
			relPath = path.Join(baseDir, entry.Name())
		}
		files = append(files, wikiindex.MarkdownFile{
			Slug: strings.TrimSuffix(entry.Name(), ".md"),
			Path: relPath,
			Data: data,
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

func (s *Store) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fullPath, err := s.resolveExistingPath(relPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, wikiindex.ErrNotFound
		}
		return nil, err
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, wikiindex.ErrNotFound
		}
		return nil, err
	}
	return data, nil
}

func (s *Store) WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := s.lexicalPath(tmpPath); err != nil {
		return "", err
	}
	finalFullPath, err := s.resolveWritablePath(finalPath)
	if err != nil {
		return "", err
	}

	tempFile, err := os.CreateTemp(filepath.Dir(finalFullPath), ".wikiindex-*")
	if err != nil {
		return "", err
	}
	tempPath := tempFile.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return "", err
	}
	if err := tempFile.Close(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(tempPath, finalFullPath); err != nil {
		return "", err
	}
	removeTemp = false

	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Store) resolveExistingPath(relPath string) (string, error) {
	fullPath, err := s.lexicalPath(relPath)
	if err != nil {
		return "", err
	}
	return s.verifyResolvedPath(fullPath)
}

func (s *Store) resolveWritablePath(relPath string) (string, error) {
	fullPath, err := s.lexicalPath(relPath)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(fullPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	resolvedParent, err := s.verifyResolvedPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(fullPath)), nil
}

func (s *Store) verifyResolvedPath(fullPath string) (string, error) {
	resolvedPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return "", err
	}
	root, err := s.canonicalRoot()
	if err != nil {
		return "", err
	}
	if !isWithinRoot(root, resolvedPath) {
		return "", fmt.Errorf("wikiindex fsstore: resolved path %q escapes root %q", resolvedPath, root)
	}
	return resolvedPath, nil
}

func (s *Store) canonicalRoot() (string, error) {
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absRoot)
}

func (s *Store) lexicalPath(relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("wikiindex fsstore: absolute path %q escapes root", relPath)
	}

	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	cleaned := filepath.Clean(filepath.FromSlash(relPath))
	if cleaned == "." {
		return absRoot, nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("wikiindex fsstore: path %q escapes root", relPath)
	}
	return filepath.Join(absRoot, cleaned), nil
}

func isWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func cleanSlashPath(relPath string) string {
	cleaned := path.Clean(strings.ReplaceAll(relPath, string(filepath.Separator), "/"))
	if cleaned == "." {
		return ""
	}
	return strings.TrimPrefix(cleaned, "/")
}
