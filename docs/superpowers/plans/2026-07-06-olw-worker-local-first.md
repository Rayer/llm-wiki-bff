# OLW Worker Local-First Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `cmd/olw_worker` as a local/filesystem-first OLW command runner that can run command batches against a vault and generate BFF cache/index artifacts.

**Architecture:** Move index/cache artifact generation into `internal/wikiindex`, independent from HTTP handlers. Add a filesystem store for mounted/local vaults, then make `cmd/olw_worker` resolve config, run `olw` command batches, and call `wikiindex.Rebuild` after successful batches. Keep BFF rebuild endpoints, but change admin pipeline trigger so it only invokes the Cloud Run Job and does not rebuild stale data immediately.

**Tech Stack:** Go 1.26, Cobra, standard library `os/exec`, existing `github.com/adrg/frontmatter`, existing Gin handler tests, filesystem temp dirs for local postprocess tests.

---

## File Structure

- Create `internal/wikiindex/wikiindex.go`: exported rebuild core, ID map types, JSONL generation using the full `internal/cache.Entry` shape.
- Create `internal/wikiindex/wikiindex_test.go`: unit tests for ID map rebuild, redirects, and concepts JSONL output.
- Create `internal/wikiindex/fsstore/fsstore.go`: local filesystem implementation of the wikiindex store interface.
- Create `internal/wikiindex/fsstore/fsstore_test.go`: temp-vault tests for markdown listing, reads, and atomic writes.
- Modify `internal/handler/v1/idmap.go`: keep Firestore lock helpers, remove duplicated rebuild core, alias or adapt to `wikiindex.IDMap`.
- Modify `internal/handler/v1/endpoints.go`: call `wikiindex.Rebuild` and stop immediate rebuild in `AdminPipelineTrigger`.
- Modify `internal/handler/v1/handler.go`: update injected rebuild function type if needed.
- Modify `internal/handler/v1/handler_test.go` and/or `internal/handler/v1/admin_test.go`: update tests for rebuild and admin trigger behavior.
- Replace `cmd/olw_worker/main.go`: implement config resolution, command parsing, command execution, `wiki.toml` creation, `postprocess`, flags.
- Create `cmd/olw_worker/main_test.go`: tests for JSON parsing, config resolution, `wiki.toml`, and command failure strategy.

---

### Task 1: Extract Wiki Index Rebuild Core

**Files:**
- Create: `internal/wikiindex/wikiindex.go`
- Create: `internal/wikiindex/wikiindex_test.go`
- Modify: `internal/handler/v1/idmap.go`
- Modify: `internal/handler/v1/endpoints.go`
- Modify: `internal/handler/v1/handler.go`

- [ ] **Step 1: Write failing wikiindex tests**

Create `internal/wikiindex/wikiindex_test.go`:

```go
package wikiindex

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakeStore struct {
	files  map[string][]MarkdownFile
	reads  map[string][]byte
	writes map[string][]byte
}

func (s *fakeStore) ListMarkdownFiles(_ context.Context, dir string) ([]MarkdownFile, error) {
	return append([]MarkdownFile(nil), s.files[dir]...), nil
}

func (s *fakeStore) ReadFile(_ context.Context, relPath string) ([]byte, error) {
	data, ok := s.reads[relPath]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeStore) WriteBytesAtomic(_ context.Context, data []byte, _, finalPath string) (string, error) {
	if s.writes == nil {
		s.writes = map[string][]byte{}
	}
	s.writes[finalPath] = append([]byte(nil), data...)
	return "digest", nil
}

func TestRebuildWritesIDMapAndConceptsJSONL(t *testing.T) {
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {
				{
					Slug: "alpha",
					Path: "wiki/alpha.md",
					Data: []byte("---\nid: concept-id\ntitle: Alpha\nsources:\n  - src-one\n---\nAlpha body"),
				},
			},
			"wiki/sources/": {
				{
					Slug: "src-one",
					Path: "wiki/sources/src-one.md",
					Data: []byte("---\nid: source-id\ntitle: Source One\n---\nSource body"),
				},
			},
		},
		reads: map[string][]byte{},
	}

	next, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if got := next.Concept["concept-id"]; got != "alpha" {
		t.Fatalf("concept id maps to %q, want alpha", got)
	}
	if got := next.Source["source-id"]; got != "src-one" {
		t.Fatalf("source id maps to %q, want src-one", got)
	}
	if _, ok := store.writes[IDMapPath]; !ok {
		t.Fatalf("missing write to %s", IDMapPath)
	}

	jsonl := strings.TrimSpace(string(store.writes[ConceptsJSONLPath]))
	var entry struct {
		Slug        string                 `json:"slug"`
		Title       string                 `json:"title"`
		Body        string                 `json:"body"`
		Frontmatter map[string]interface{} `json:"frontmatter"`
		Sources     []string               `json:"sources"`
	}
	if err := json.Unmarshal([]byte(jsonl), &entry); err != nil {
		t.Fatalf("concepts jsonl entry is not valid JSON: %v\n%s", err, jsonl)
	}
	if entry.Slug != "alpha" || entry.Title != "Alpha" || strings.TrimSpace(entry.Body) != "Alpha body" {
		t.Fatalf("entry = %+v, want alpha full cache entry", entry)
	}
	if len(entry.Sources) != 1 || entry.Sources[0] != "src-one" {
		t.Fatalf("sources = %#v, want [src-one]", entry.Sources)
	}
}

func TestRebuildPreservesRedirects(t *testing.T) {
	old := IDMap{
		Concept:   map[string]string{"same-id": "old-alpha"},
		Source:    map[string]string{},
		Redirects: map[string][]string{},
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{
		files: map[string][]MarkdownFile{
			"wiki/": {
				{Slug: "new-alpha", Path: "wiki/new-alpha.md", Data: []byte("---\nid: same-id\ntitle: Alpha\n---\nBody")},
			},
			"wiki/sources/": {},
		},
		reads: map[string][]byte{IDMapPath: oldJSON},
	}

	next, err := Rebuild(context.Background(), store)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if got := next.Redirects["same-id"]; len(got) != 1 || got[0] != "old-alpha" {
		t.Fatalf("redirects = %#v, want old-alpha", next.Redirects)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./internal/wikiindex
```

Expected: fail because `internal/wikiindex` does not exist.

- [ ] **Step 3: Create `internal/wikiindex/wikiindex.go`**

Implement:

```go
package wikiindex

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	fm "github.com/adrg/frontmatter"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
)

const (
	IDMapPath         = "cache/id_map.json"
	IDMapTempPath     = "cache/id_map.json.tmp"
	ConceptsJSONLPath = "cache/concepts.jsonl"
)

var ErrNotFound = errors.New("wikiindex: not found")

type MarkdownFile struct {
	Slug string
	Path string
	Data []byte
}

type Store interface {
	ListMarkdownFiles(ctx context.Context, dir string) ([]MarkdownFile, error)
	ReadFile(ctx context.Context, relPath string) ([]byte, error)
	WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error)
}

type IDMap struct {
	Concept   map[string]string   `json:"concept"`
	Source    map[string]string   `json:"source"`
	Redirects map[string][]string `json:"redirects"`
}

type markdownMatter struct {
	ID      string   `yaml:"id"`
	Title   string   `yaml:"title"`
	Sources []string `yaml:"sources"`
	Source  string   `yaml:"source"`
}

func Rebuild(ctx context.Context, store Store) (IDMap, error) {
	next, err := BuildIDMap(ctx, store)
	if err != nil {
		return next, err
	}
	if err := writeIDMap(ctx, store, next); err != nil {
		return next, err
	}
	if err := buildConceptsJSONL(ctx, store); err != nil {
		return next, fmt.Errorf("build concepts jsonl: %w", err)
	}
	return next, nil
}

func BuildIDMap(ctx context.Context, store Store) (IDMap, error) {
	next := IDMap{
		Concept:   map[string]string{},
		Source:    map[string]string{},
		Redirects: map[string][]string{},
	}
	if err := addIDMapEntries(ctx, store, "wiki/", next.Concept); err != nil {
		return next, err
	}
	if err := addIDMapEntries(ctx, store, "wiki/sources/", next.Source); err != nil {
		return next, err
	}
	old, err := readOldIDMap(ctx, store)
	if err != nil {
		return next, err
	}
	next.Redirects = cloneRedirects(old.Redirects)
	appendChangedRedirects(next.Redirects, old.Concept, next.Concept)
	appendChangedRedirects(next.Redirects, old.Source, next.Source)
	return next, nil
}

func addIDMapEntries(ctx context.Context, store Store, dir string, entries map[string]string) error {
	files, err := store.ListMarkdownFiles(ctx, dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, file := range files {
		matter, _, err := parseMatter(file.Data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", file.Path, err)
		}
		id := strings.TrimSpace(matter.ID)
		if id == "" {
			id = generateID(file.Data)
		}
		entries[id] = file.Slug
	}
	return nil
}

func readOldIDMap(ctx context.Context, store Store) (IDMap, error) {
	old := IDMap{Concept: map[string]string{}, Source: map[string]string{}, Redirects: map[string][]string{}}
	data, err := store.ReadFile(ctx, IDMapPath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return old, nil
		}
		return old, fmt.Errorf("read old id map: %w", err)
	}
	if len(data) == 0 {
		return old, nil
	}
	if err := json.Unmarshal(data, &old); err != nil {
		return old, fmt.Errorf("decode old id map: %w", err)
	}
	if old.Concept == nil {
		old.Concept = map[string]string{}
	}
	if old.Source == nil {
		old.Source = map[string]string{}
	}
	if old.Redirects == nil {
		old.Redirects = map[string][]string{}
	}
	return old, nil
}

func writeIDMap(ctx context.Context, store Store, next IDMap) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode id map: %w", err)
	}
	if _, err := store.WriteBytesAtomic(ctx, data, IDMapTempPath, IDMapPath); err != nil {
		return fmt.Errorf("write id map: %w", err)
	}
	return nil
}

func buildConceptsJSONL(ctx context.Context, store Store) error {
	files, err := store.ListMarkdownFiles(ctx, "wiki/")
	if err != nil {
		return fmt.Errorf("list wiki: %w", err)
	}
	var builder strings.Builder
	for _, file := range files {
		matter, body, err := parseMatter(file.Data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", file.Path, err)
		}
		title := strings.TrimSpace(matter.Title)
		if title == "" {
			title = file.Slug
		}
		entry := conceptcache.Entry{
			Slug:        file.Slug,
			Title:       title,
			Body:        body,
			Frontmatter: map[string]interface{}{},
			Sources:     matter.sources(),
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode %s: %w", file.Path, err)
		}
		builder.Write(data)
		builder.WriteByte('\n')
	}
	_, err = store.WriteBytesAtomic(ctx, []byte(builder.String()), "cache/concepts.jsonl.tmp", ConceptsJSONLPath)
	return err
}

func parseMatter(data []byte) (markdownMatter, string, error) {
	var matter markdownMatter
	raw := string(data)
	if !strings.HasPrefix(raw, "---") {
		return matter, raw, nil
	}
	body, err := fm.MustParse(strings.NewReader(raw), &matter)
	if err != nil {
		return matter, raw, err
	}
	return matter, string(body), nil
}

func (m markdownMatter) sources() []string {
	if len(m.Sources) > 0 {
		return append([]string(nil), m.Sources...)
	}
	if strings.TrimSpace(m.Source) != "" {
		return []string{strings.TrimSpace(m.Source)}
	}
	return []string{}
}

func generateID(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])[:12]
}

func cloneRedirects(src map[string][]string) map[string][]string {
	dst := make(map[string][]string, len(src))
	for id, redirects := range src {
		dst[id] = append([]string(nil), redirects...)
	}
	return dst
}

func appendChangedRedirects(redirects map[string][]string, oldEntries, newEntries map[string]string) {
	for id, newSlug := range newEntries {
		oldSlug := strings.TrimSpace(oldEntries[id])
		if oldSlug == "" || oldSlug == newSlug || containsString(redirects[id], oldSlug) {
			continue
		}
		redirects[id] = append(redirects[id], oldSlug)
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Adapt handler code to use `wikiindex`**

In `internal/handler/v1/handler.go`, change:

```go
rebuildIndex func(context.Context, string, string) (idMap, error)
```

to:

```go
rebuildIndex func(context.Context, string, string) (wikiindex.IDMap, error)
```

and import:

```go
"github.com/rayer/llm-wiki-bff/internal/wikiindex"
```

In `internal/handler/v1/idmap.go`, remove the old `idMap`, store interfaces, `buildIDMap`, `rebuildIndex`, `buildConceptsJSONL`, and related duplicate helpers. Keep Firestore lock helpers. If tests still reference `idMap`, add:

```go
type idMap = wikiindex.IDMap
```

and import `github.com/rayer/llm-wiki-bff/internal/wikiindex`.

In `internal/handler/v1/endpoints.go`, replace:

```go
next, err := rebuildIndex(ctx, gcsClient)
```

with:

```go
next, err := wikiindex.Rebuild(ctx, gcsClient)
```

and import `github.com/rayer/llm-wiki-bff/internal/wikiindex`.

- [ ] **Step 5: Add GCS adapter methods if needed**

If `*gcs.Client` does not satisfy `wikiindex.Store` because `ListMarkdownFiles` returns `[]gcs.MarkdownFile`, add an adapter in `internal/handler/v1/idmap.go`:

```go
type gcsWikiIndexStore struct {
	client *gcs.Client
}

func (s gcsWikiIndexStore) ListMarkdownFiles(ctx context.Context, dir string) ([]wikiindex.MarkdownFile, error) {
	files, err := s.client.ListMarkdownFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := make([]wikiindex.MarkdownFile, 0, len(files))
	for _, file := range files {
		out = append(out, wikiindex.MarkdownFile{Slug: file.Slug, Path: file.Path, Data: file.Data})
	}
	return out, nil
}

func (s gcsWikiIndexStore) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	return s.client.ReadFile(ctx, relPath)
}

func (s gcsWikiIndexStore) WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error) {
	return s.client.WriteBytesAtomic(ctx, data, tmpPath, finalPath)
}
```

Then call:

```go
next, err := wikiindex.Rebuild(ctx, gcsWikiIndexStore{client: gcsClient})
```

- [ ] **Step 6: Run focused tests**

Run:

```bash
go test ./internal/wikiindex ./internal/handler/v1
```

Expected: all tests pass.

- [ ] **Step 7: Commit**

Run:

```bash
git add internal/wikiindex internal/handler/v1/idmap.go internal/handler/v1/endpoints.go internal/handler/v1/handler.go internal/handler/v1/*_test.go
git commit -m "refactor: extract wiki index rebuild core"
```

---

### Task 2: Add Filesystem Wikiindex Store

**Files:**
- Create: `internal/wikiindex/fsstore/fsstore.go`
- Create: `internal/wikiindex/fsstore/fsstore_test.go`

- [ ] **Step 1: Write failing filesystem store tests**

Create `internal/wikiindex/fsstore/fsstore_test.go`:

```go
package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestListMarkdownFilesDirectChildrenOnly(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "wiki", "alpha.md"), []byte("alpha"))
	mustWrite(t, filepath.Join(root, "wiki", "beta.txt"), []byte("ignore"))
	mustWrite(t, filepath.Join(root, "wiki", "nested", "gamma.md"), []byte("ignore nested"))

	store := New(root)
	files, err := store.ListMarkdownFiles(context.Background(), "wiki/")
	if err != nil {
		t.Fatalf("ListMarkdownFiles() error = %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1: %#v", len(files), files)
	}
	if files[0].Slug != "alpha" || files[0].Path != "wiki/alpha.md" || string(files[0].Data) != "alpha" {
		t.Fatalf("file = %#v, want alpha markdown file", files[0])
	}
}

func TestReadFileNotFound(t *testing.T) {
	store := New(t.TempDir())
	_, err := store.ReadFile(context.Background(), "cache/id_map.json")
	if err == nil {
		t.Fatal("ReadFile() error = nil, want not found")
	}
}

func TestWriteBytesAtomicCreatesParentAndReplacesFinal(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	if _, err := store.WriteBytesAtomic(context.Background(), []byte("first"), "cache/tmp", "cache/final.txt"); err != nil {
		t.Fatalf("WriteBytesAtomic(first) error = %v", err)
	}
	if _, err := store.WriteBytesAtomic(context.Background(), []byte("second"), "cache/tmp", "cache/final.txt"); err != nil {
		t.Fatalf("WriteBytesAtomic(second) error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "cache", "final.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("final data = %q, want second", data)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./internal/wikiindex/fsstore
```

Expected: fail because package implementation does not exist.

- [ ] **Step 3: Implement filesystem store**

Create `internal/wikiindex/fsstore/fsstore.go`:

```go
package fsstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

type Store struct {
	root string
}

func New(root string) *Store {
	return &Store{root: filepath.Clean(root)}
}

func (s *Store) ListMarkdownFiles(_ context.Context, dir string) ([]wikiindex.MarkdownFile, error) {
	absDir, err := s.safePath(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(absDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []wikiindex.MarkdownFile{}, nil
		}
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	files := make([]wikiindex.MarkdownFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		relPath := filepath.ToSlash(filepath.Join(strings.Trim(dir, "/"), entry.Name()))
		data, err := os.ReadFile(filepath.Join(absDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", relPath, err)
		}
		slug := strings.TrimSuffix(entry.Name(), ".md")
		files = append(files, wikiindex.MarkdownFile{Slug: slug, Path: relPath, Data: data})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func (s *Store) ReadFile(_ context.Context, relPath string) ([]byte, error) {
	abs, err := s.safePath(relPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, wikiindex.ErrNotFound
		}
		return nil, fmt.Errorf("read %s: %w", relPath, err)
	}
	return data, nil
}

func (s *Store) WriteBytesAtomic(_ context.Context, data []byte, tmpPath, finalPath string) (string, error) {
	tmpAbs, err := s.safePath(tmpPath)
	if err != nil {
		return "", err
	}
	finalAbs, err := s.safePath(finalPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(tmpAbs), 0755); err != nil {
		return "", fmt.Errorf("mkdir tmp: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalAbs), 0755); err != nil {
		return "", fmt.Errorf("mkdir final: %w", err)
	}
	if err := os.WriteFile(tmpAbs, data, 0644); err != nil {
		return "", fmt.Errorf("write tmp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpAbs, finalAbs); err != nil {
		return "", fmt.Errorf("rename %s to %s: %w", tmpPath, finalPath, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (s *Store) safePath(relPath string) (string, error) {
	cleanRel := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(relPath, "/")))
	if cleanRel == "." {
		return s.root, nil
	}
	if strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) || cleanRel == ".." {
		return "", fmt.Errorf("path escapes root: %s", relPath)
	}
	abs := filepath.Join(s.root, cleanRel)
	if !strings.HasPrefix(abs, s.root+string(filepath.Separator)) && abs != s.root {
		return "", fmt.Errorf("path escapes root: %s", relPath)
	}
	return abs, nil
}
```

- [ ] **Step 4: Run filesystem store tests**

Run:

```bash
go test ./internal/wikiindex/fsstore
```

Expected: pass.

- [ ] **Step 5: Commit**

Run:

```bash
git add internal/wikiindex/fsstore
git commit -m "feat: add filesystem wiki index store"
```

---

### Task 3: Rebuild OLW Worker CLI

**Files:**
- Replace: `cmd/olw_worker/main.go`
- Create: `cmd/olw_worker/main_test.go`

- [ ] **Step 1: Write failing worker tests**

Create `cmd/olw_worker/main_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCommandBatch(t *testing.T) {
	commands, err := parseCommandBatch(`[["clear"],["run","--auto-approve"]]`)
	if err != nil {
		t.Fatalf("parseCommandBatch() error = %v", err)
	}
	if len(commands) != 2 || commands[1][0] != "run" || commands[1][1] != "--auto-approve" {
		t.Fatalf("commands = %#v", commands)
	}
}

func TestParseCommandBatchRejectsEmptyCommand(t *testing.T) {
	if _, err := parseCommandBatch(`[[]]`); err == nil {
		t.Fatal("parseCommandBatch() error = nil, want error")
	}
}

func TestResolveVaultPathPrefersFlag(t *testing.T) {
	cfg := workerConfig{VaultPath: "/tmp/explicit", DataDir: "/data", UserID: "u", ProjectID: "p"}
	got, err := resolveVaultPath(cfg)
	if err != nil {
		t.Fatalf("resolveVaultPath() error = %v", err)
	}
	if got != "/tmp/explicit" {
		t.Fatalf("vault = %q, want explicit", got)
	}
}

func TestResolveVaultPathFromUserProject(t *testing.T) {
	cfg := workerConfig{DataDir: "/data", UserID: "u", ProjectID: "p"}
	got, err := resolveVaultPath(cfg)
	if err != nil {
		t.Fatalf("resolveVaultPath() error = %v", err)
	}
	if got != filepath.Join("/data", "users", "u", "projects", "p") {
		t.Fatalf("vault = %q", got)
	}
}

func TestEnsureWikiTOMLCreatesButDoesNotOverwrite(t *testing.T) {
	vault := t.TempDir()
	cfg := workerConfig{APIKey: "secret"}
	if err := ensureWikiTOML(vault, cfg); err != nil {
		t.Fatalf("ensureWikiTOML(create) error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(vault, "wiki.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(string(data), `api_key = "secret"`) {
		t.Fatalf("wiki.toml missing api key:\n%s", data)
	}
	if err := os.WriteFile(filepath.Join(vault, "wiki.toml"), []byte("custom"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ensureWikiTOML(vault, workerConfig{APIKey: "new"}); err != nil {
		t.Fatalf("ensureWikiTOML(existing) error = %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(vault, "wiki.toml"))
	if string(data) != "custom" {
		t.Fatalf("existing wiki.toml overwritten: %q", data)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./cmd/olw_worker
```

Expected: fail because helpers are not implemented.

- [ ] **Step 3: Replace worker implementation**

Replace `cmd/olw_worker/main.go` with an implementation containing these units:

```go
type workerConfig struct {
	VaultPath   string
	DataDir     string
	UserID      string
	ProjectID   string
	APIKey      string
	Postprocess bool
	StopOnError bool
}
```

Required functions:

```go
func parseCommandBatch(raw string) ([][]string, error)
func resolveVaultPath(cfg workerConfig) (string, error)
func ensureWikiTOML(vault string, cfg workerConfig) error
func runOLWBatch(ctx context.Context, vault string, commands [][]string, stopOnError bool) error
func runPostprocess(ctx context.Context, vault string) error
```

CLI behavior:

```go
rootCmd.PersistentFlags().StringVar(&cfg.VaultPath, "vault", envOr("VAULT_PATH", ""), "project vault path")
rootCmd.PersistentFlags().StringVar(&cfg.DataDir, "data-dir", envOr("DATA_DIR", "/data"), "mounted data root")
rootCmd.PersistentFlags().StringVar(&cfg.UserID, "user-id", envOr("USER_ID", ""), "user id")
rootCmd.PersistentFlags().StringVar(&cfg.ProjectID, "project-id", envOr("PROJECT_ID", ""), "project id")
rootCmd.PersistentFlags().StringVar(&cfg.APIKey, "api-key", envOr("LLM_API_KEY", ""), "LLM API key")
rootCmd.PersistentFlags().BoolVar(&cfg.StopOnError, "stop-on-error", true, "stop on first failed OLW command")
runCmd.Flags().BoolVar(&cfg.Postprocess, "postprocess", true, "run postprocess after successful batch")
runCmd.Flags().BoolVar(&noPostprocess, "no-postprocess", false, "skip postprocess after batch")
```

`runPostprocess` should call:

```go
store := fsstore.New(vault)
_, err := wikiindex.Rebuild(ctx, store)
```

`runOLWBatch` should execute:

```go
exec.CommandContext(ctx, "olw", command...)
```

with `Dir = vault`, `Stdout = os.Stdout`, and `Stderr = os.Stderr`.

Keep stale lock cleanup:

```go
func cleanStaleLock(vault string, maxAge time.Duration) error
```

removing `.olw/pipeline.lock` only if older than five minutes.

- [ ] **Step 4: Run worker unit tests**

Run:

```bash
go test ./cmd/olw_worker
```

Expected: pass.

- [ ] **Step 5: Run worker build**

Run:

```bash
go build ./cmd/olw_worker
```

Expected: build succeeds.

- [ ] **Step 6: Commit**

Run:

```bash
git add cmd/olw_worker
git commit -m "feat: implement local-first olw worker"
```

---

### Task 4: Fix BFF Admin Pipeline Trigger

**Files:**
- Modify: `internal/handler/v1/endpoints.go`
- Modify: `internal/handler/v1/admin_test.go` or `internal/handler/v1/handler_test.go`

- [ ] **Step 1: Write failing admin trigger test**

Add or update a test that injects `h.rebuildIndex` with a function that fails the test if called:

```go
h.rebuildIndex = func(context.Context, string, string) (wikiindex.IDMap, error) {
	t.Fatal("AdminPipelineTrigger must not rebuild index immediately")
	return wikiindex.IDMap{}, nil
}
```

The test should call `AdminPipelineTrigger`, mock Cloud Run Jobs API success, and assert response status `200` with `status: "ok"` and an `execution_id`.

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
go test ./internal/handler/v1 -run TestAdminPipelineTrigger
```

Expected: fail because current handler rebuilds immediately after worker invocation.

- [ ] **Step 3: Update Cloud Run args**

In `AdminPipelineTrigger`, change container override args from:

```go
"args": []string{"run"},
```

to:

```go
"args": []string{"run", `[[\"init\"],[\"run\",\"--auto-approve\"]]`},
```

When constructing JSON with Go string literals, prefer:

```go
defaultWorkerCommands := `[["init"],["run","--auto-approve"]]`
"args": []string{"run", defaultWorkerCommands},
```

- [ ] **Step 4: Remove immediate rebuild block**

Delete the block beginning with:

```go
// Also run rebuild-index
```

and return:

```go
c.JSON(http.StatusOK, gin.H{
	"status":       "ok",
	"execution_id": executionID,
})
```

- [ ] **Step 5: Run focused handler tests**

Run:

```bash
go test ./internal/handler/v1 -run 'TestAdminPipelineTrigger|TestAdminRebuildIndex|TestRebuildIndex'
```

Expected: pass.

- [ ] **Step 6: Commit**

Run:

```bash
git add internal/handler/v1/endpoints.go internal/handler/v1/*_test.go
git commit -m "fix: avoid stale rebuild after pipeline trigger"
```

---

### Task 5: End-to-End Local Verification

**Files:**
- No required code files.
- Optional docs update: `cmd/olw_worker/DESIGN.md`

- [ ] **Step 1: Run all relevant Go tests**

Run:

```bash
go test ./cmd/olw_worker ./internal/wikiindex/... ./internal/handler/v1
```

Expected: pass.

- [ ] **Step 2: Run full repo tests**

Run:

```bash
go test ./...
```

Expected: pass.

- [ ] **Step 3: Build worker target**

Run:

```bash
go build -o /tmp/olw_worker ./cmd/olw_worker
```

Expected: build succeeds and `/tmp/olw_worker` exists.

- [ ] **Step 4: Verify postprocess on a temp vault**

Create a temp vault:

```bash
tmp=$(mktemp -d)
mkdir -p "$tmp/wiki/sources"
cat > "$tmp/wiki/alpha.md" <<'EOF'
---
id: concept-alpha
title: Alpha
sources:
  - source-alpha
---
Alpha body
EOF
cat > "$tmp/wiki/sources/source-alpha.md" <<'EOF'
---
id: source-alpha
title: Source Alpha
---
Source body
EOF
/tmp/olw_worker postprocess --vault "$tmp" --api-key local-test
test -f "$tmp/cache/id_map.json"
test -f "$tmp/cache/concepts.jsonl"
```

Expected: all commands exit `0`.

- [ ] **Step 5: Inspect generated files**

Run:

```bash
cat "$tmp/cache/id_map.json"
cat "$tmp/cache/concepts.jsonl"
```

Expected:

- `id_map.json` contains `"concept-alpha": "alpha"` and `"source-alpha": "source-alpha"`.
- `concepts.jsonl` contains `"slug":"alpha"`, `"title":"Alpha"`, and `"body":"Alpha body"`.

- [ ] **Step 6: Commit docs update if made**

If `cmd/olw_worker/DESIGN.md` was updated:

```bash
git add cmd/olw_worker/DESIGN.md
git commit -m "docs: update olw worker design notes"
```

---

## Self-Review

Spec coverage:

- JSON command batch is covered by Task 3.
- Default postprocess and `--no-postprocess` are covered by Task 3 and Task 5.
- `--stop-on-error` is covered by Task 3.
- Vault resolution is covered by Task 3.
- `wiki.toml` creation and non-overwrite behavior are covered by Task 3.
- Filesystem postprocess is covered by Tasks 1, 2, and 5.
- BFF admin trigger stale rebuild bug is covered by Task 4.
- Docker worker target is not changed because the spec says the current Dockerfile direction is already acceptable.

Completion scan:

- No unfinished markers or deferred work steps are intentionally left in this plan.

Type consistency:

- `wikiindex.IDMap`, `wikiindex.MarkdownFile`, `wikiindex.Store`, and `fsstore.New` are defined before later tasks use them.
- Handler injected rebuild functions use `wikiindex.IDMap`.
