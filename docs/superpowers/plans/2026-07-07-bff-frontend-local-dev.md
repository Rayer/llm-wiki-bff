# BFF + Frontend Local Development Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add BFF/frontend local development mode backed by filesystem data so the app can be tested locally without GCP credentials or worker pipeline execution.

**Architecture:** Introduce a narrow storage interface consumed by handlers, cache, search, and wikiindex adapters. Keep `gcs.Client` as the production implementation and add `internal/localfs.Client` for the same storage contract against `local-data`. Wire `main.go` to select local storage from `--local` or `LOCAL_DATA_DIR`, then add seed data, Makefile targets, compose wiring, and local docs.

**Tech Stack:** Go 1.26, Gin, Viper/config env, existing `internal/gcs` parsing helpers, filesystem tests with `t.TempDir`, Docker Compose, Makefile.

---

## File Map

- Create: `internal/storage/storage.go` defines the shared storage contract and type aliases for `WikiPage`, `Project`, and `MarkdownFile`.
- Modify: `internal/gcs/client.go` aliases storage types and exposes cache parsing helpers needed by localfs.
- Create: `internal/localfs/client.go` implements storage against local filesystem paths.
- Create: `internal/localfs/client_test.go` covers path safety, listings, cache reads, page reads, writes, and stats.
- Modify: `internal/cache/cache.go` consumes `storage.WikiPage` through storage-facing interfaces.
- Modify: `internal/search/search.go` accepts the storage interface instead of `*gcs.Client`.
- Modify: `internal/handler/v1/handler.go` stores the interface and returns scoped storage.
- Modify: `internal/handler/v1/endpoints.go`, `metrics.go`, `id_routing.go`, `raw_upload.go` replace concrete GCS usage where handler code only needs storage behavior.
- Modify: `main.go` parses local mode flags and injects a local rebuild function that does not require Firestore.
- Create: `demo/users/local-user/projects/demo/index.md`, `wiki.toml`, `cache/concepts.jsonl`, `cache/id_map.json`, `raw/seed.md`, `wiki/local-development.md`, `wiki/storage-contract.md`, and `wiki/sources/local-dev-source.md` provide prebuilt local demo data.
- Modify: `Makefile` adds local dev targets while preserving existing deploy targets.
- Modify: `docker-compose.yml` uses local mode for BFF and keeps worker outside first-phase scope.
- Create: `docs/LOCAL_DEV.md` documents quickstart, inner-loop commands, headers, and troubleshooting.

---

### Task 1: Add Storage Interface

**Files:**
- Create: `internal/storage/storage.go`
- Modify: `internal/gcs/client.go`

- [ ] **Step 1: Write the storage package**

Create `internal/storage/storage.go`:

```go
package storage

import "context"

type WikiPage struct {
	Slug      string   `json:"slug"`
	Title     string   `json:"title"`
	ID        string   `json:"id"`
	Path      string   `json:"path"`
	Status    string   `json:"status"`
	Quality   string   `json:"quality,omitempty"`
	Concepts  []string `json:"concepts,omitempty"`
	SourceURL string   `json:"source_url,omitempty"`
	RawSource string   `json:"raw_source,omitempty"`
}

type Project struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

type MarkdownFile struct {
	Slug string
	Path string
	Data []byte
}

type Store interface {
	WithScope(userID, projectID string) Store
	Prefix() string
	ReadFile(ctx context.Context, relPath string) ([]byte, error)
	WriteBytes(ctx context.Context, data []byte, relPath string) (string, error)
	WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error)
	ListProjects(ctx context.Context, userID string) ([]Project, error)
	ListConcepts(ctx context.Context, includeDrafts bool) ([]WikiPage, error)
	ListSources(ctx context.Context) ([]WikiPage, error)
	ListConceptsFromCache(ctx context.Context) ([]WikiPage, error)
	ListSourcesFromCache(ctx context.Context) ([]WikiPage, error)
	GetPage(ctx context.Context, slug, category string) (*WikiPage, []byte, error)
	ListMarkdownFiles(ctx context.Context, dir string) ([]MarkdownFile, error)
	BucketStats(ctx context.Context) (int64, int64, error)
	GetMetaSHA256(ctx context.Context, relPath string) (string, error)
}
```

- [ ] **Step 2: Update GCS types to alias storage types**

In `internal/gcs/client.go`, import storage with an alias and replace the three public type definitions:

```go
import (
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

type WikiPage = store.WikiPage
type Project = store.Project
type MarkdownFile = store.MarkdownFile
```

Keep private structs such as `wikiPageFrontmatter`, `conceptCacheEntry`, and `wikiIDMap` in `internal/gcs`.

- [ ] **Step 3: Export cache parsing helpers for reuse**

Rename the helper functions in `internal/gcs/client.go`:

Rename `wikiPagesFromConceptsJSONL` to `WikiPagesFromConceptsJSONL` and `wikiPagesFromSourceIDMap` to `WikiPagesFromSourceIDMap` without changing their bodies. The existing bodies already parse `cache/concepts.jsonl` and `cache/id_map.json`; only the function names change so `internal/localfs` can reuse them.

Update callers in `ListConceptsFromCache` and `ListSourcesFromCache`:

```go
return WikiPagesFromConceptsJSONL(data)
return WikiPagesFromSourceIDMap(data)
```

- [ ] **Step 4: Verify compile for GCS package**

Run:

```bash
go test ./internal/gcs
```

Expected: package compiles and tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/storage/storage.go internal/gcs/client.go
git commit -m "refactor: define wiki storage interface"
```

---

### Task 2: Implement Local Filesystem Store

**Files:**
- Create: `internal/localfs/client.go`
- Create: `internal/localfs/client_test.go`

- [ ] **Step 1: Write failing localfs tests**

Create `internal/localfs/client_test.go` with tests covering path safety and the main read paths:

```go
package localfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/storage"
)

func TestRejectsUnsafeRelativePaths(t *testing.T) {
	client := New(t.TempDir()).WithScope("u", "p")
	ctx := context.Background()

	if _, err := client.ReadFile(ctx, "../secret.md"); err == nil {
		t.Fatal("ReadFile allowed traversal path")
	}
	if _, err := client.ReadFile(ctx, "/tmp/secret.md"); err == nil {
		t.Fatal("ReadFile allowed absolute path")
	}
	if _, err := client.WriteBytes(ctx, []byte("x"), "wiki/../../secret.md"); err == nil {
		t.Fatal("WriteBytes allowed traversal path")
	}
}

func TestListProjectsScansIndexFiles(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "users/u/projects/demo/index.md", "# Demo")
	mustWrite(t, root, "users/u/projects/second/index.md", "# Second")
	mustWrite(t, root, "users/u/projects/no-index/wiki/a.md", "# A")

	projects, err := New(root).ListProjects(context.Background(), "u")
	if err != nil {
		t.Fatalf("ListProjects() error = %v", err)
	}
	if len(projects) != 2 || projects[0].ID != "demo" || projects[1].ID != "second" {
		t.Fatalf("projects = %#v, want demo and second", projects)
	}
}

func TestCacheAndPageReads(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "users/u/projects/p/cache/concepts.jsonl", `{"slug":"local-concept","title":"Local Concept","frontmatter":{"id":"c1"}}`+"\n")
	mustWrite(t, root, "users/u/projects/p/cache/id_map.json", `{"source":{"s1":"source-one"}}`)
	mustWrite(t, root, "users/u/projects/p/wiki/local-concept.md", "---\nid: c1\ntitle: Local Concept\n---\nBody")
	mustWrite(t, root, "users/u/projects/p/wiki/sources/source-one.md", "---\nid: s1\ntitle: Source One\n---\nSource")

	client := New(root).WithScope("u", "p")
	concepts, err := client.ListConceptsFromCache(context.Background())
	if err != nil {
		t.Fatalf("ListConceptsFromCache() error = %v", err)
	}
	if len(concepts) != 1 || concepts[0].Slug != "local-concept" || concepts[0].ID != "c1" {
		t.Fatalf("concepts = %#v", concepts)
	}

	sources, err := client.ListSourcesFromCache(context.Background())
	if err != nil {
		t.Fatalf("ListSourcesFromCache() error = %v", err)
	}
	if len(sources) != 1 || sources[0].Slug != "source-one" || sources[0].ID != "s1" {
		t.Fatalf("sources = %#v", sources)
	}

	page, data, err := client.GetPage(context.Background(), "local-concept", "concepts")
	if err != nil {
		t.Fatalf("GetPage() error = %v", err)
	}
	if page.Title != "Local Concept" || string(data) == "" {
		t.Fatalf("page = %#v data=%q", page, string(data))
	}
}

func TestMissingFileUsesStorageNotFound(t *testing.T) {
	_, err := New(t.TempDir()).WithScope("u", "p").ReadFile(context.Background(), "wiki/missing.md")
	if !errors.Is(err, storage.ErrObjectNotExist) {
		t.Fatalf("ReadFile() error = %v, want storage.ErrObjectNotExist", err)
	}
}

func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
```

- [ ] **Step 2: Run localfs tests and verify failure**

Run:

```bash
go test ./internal/localfs
```

Expected: FAIL because package or methods do not exist.

- [ ] **Step 3: Implement localfs client**

Create `internal/localfs/client.go` with these units:

```go
package localfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

type Client struct {
	root      string
	userID    string
	projectID string
}

func New(root string) *Client {
	return &Client{root: filepath.Clean(root)}
}

func (c *Client) WithScope(userID, projectID string) store.Store {
	return &Client{root: c.root, userID: userID, projectID: projectID}
}

func (c *Client) Prefix() string {
	return fmt.Sprintf("users/%s/projects/%s", c.userID, c.projectID)
}

func (c *Client) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := c.fullPath(relPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrObjectNotExist
		}
		return nil, fmt.Errorf("read %s: %w", relPath, err)
	}
	return data, nil
}

func (c *Client) WriteBytes(ctx context.Context, data []byte, relPath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := c.fullPath(relPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (c *Client) WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	final, err := c.fullPath(finalPath)
	if err != nil {
		return "", err
	}
	tmp, err := c.fullPath(tmpPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
```

Then add listing methods mirroring `internal/gcs/client.go`:

```go
func (c *Client) ListConceptsFromCache(ctx context.Context) ([]store.WikiPage, error) {
	data, err := c.ReadFile(ctx, "cache/concepts.jsonl")
	if err != nil {
		return nil, err
	}
	return gcs.WikiPagesFromConceptsJSONL(data)
}

func (c *Client) ListSourcesFromCache(ctx context.Context) ([]store.WikiPage, error) {
	data, err := c.ReadFile(ctx, "cache/id_map.json")
	if err != nil {
		return nil, err
	}
	return gcs.WikiPagesFromSourceIDMap(data)
}
```

Add these functions to `internal/localfs/client.go`:

```go
func (c *Client) ListProjects(ctx context.Context, userID string) ([]store.Project, error)
func (c *Client) ListConcepts(ctx context.Context, includeDrafts bool) ([]store.WikiPage, error)
func (c *Client) ListSources(ctx context.Context) ([]store.WikiPage, error)
func (c *Client) GetPage(ctx context.Context, slug, category string) (*store.WikiPage, []byte, error)
func (c *Client) ListMarkdownFiles(ctx context.Context, dir string) ([]store.MarkdownFile, error)
func (c *Client) BucketStats(ctx context.Context) (int64, int64, error)
func (c *Client) GetMetaSHA256(ctx context.Context, relPath string) (string, error)
func (c *Client) fullPath(relPath string) (string, error)
func (c *Client) projectRoot() (string, error)
func applyWikiPageFrontmatter(page store.WikiPage, data []byte) (store.WikiPage, error)
```

Implement the functions with these concrete rules:

- `fullPath` rejects empty scope, absolute paths, `..` segments, and paths escaping the project root after evaluating existing symlink parents.
- `ListProjects` scans `users/{userID}/projects/*/index.md`, sorts IDs, and uses the index file modification time as `CreatedAt`.
- `ListConcepts` lists `wiki/*.md` as published concepts, skips `index.md`, `log.md`, and `wiki/sources/*`, then adds `wiki/.drafts/*.md` as drafts only when `includeDrafts` is true.
- `ListSources` lists `wiki/sources/*.md`.
- `GetPage` reads `wiki/sources/{slug}.md` for sources, then `wiki/{slug}.md` and `wiki/.drafts/{slug}.md` for concepts.
- `BucketStats` walks the scoped project root and returns total bytes and file count.
- `GetMetaSHA256` computes SHA256 for existing files and returns an empty digest for missing files.

- [ ] **Step 4: Run localfs tests**

Run:

```bash
go test ./internal/localfs
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/localfs internal/gcs/client.go
git commit -m "feat: add local filesystem wiki store"
```

---

### Task 3: Convert Handler and Supporting Packages to Storage Interface

**Files:**
- Modify: `internal/cache/cache.go`
- Modify: `internal/search/search.go`
- Modify: `internal/handler/v1/handler.go`
- Modify: `internal/handler/v1/endpoints.go`
- Modify: `internal/handler/v1/metrics.go`
- Modify: `internal/handler/v1/id_routing.go`
- Modify: `internal/handler/v1/raw_upload.go`
- Modify tests under `internal/handler/v1/*_test.go` where compile errors identify concrete GCS-only fake types.

- [ ] **Step 1: Update cache interfaces**

Change `internal/cache/cache.go` imports and interfaces:

```go
import (
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

type conceptReader interface {
	ListConcepts(ctx context.Context, includeDrafts bool) ([]store.WikiPage, error)
	GetPage(ctx context.Context, slug, category string) (*store.WikiPage, []byte, error)
}

type Reader interface {
	conceptReader
	Prefix() string
}
```

- [ ] **Step 2: Update search methods**

Change `internal/search/search.go`:

```go
import store "github.com/rayer/llm-wiki-bff/internal/storage"

type Index struct {
	sources       []store.WikiPage
	concepts      []store.WikiPage
	entries       map[string]indexedPage
	conceptBodies map[string]string
}

func (idx *Index) Build(reader store.Store) error {
	ctx := context.Background()
	rawBytes, err := reader.ReadFile(ctx, "meta/index.md")
	if err != nil {
		return err
	}
	idx.sources, idx.concepts, idx.entries = parseMetaIndex(string(rawBytes))
	return nil
}

func (idx *Index) LoadConceptBodies(ctx context.Context, reader store.Store) error {
	if idx.conceptBodies == nil {
		idx.conceptBodies = make(map[string]string)
	}
	// Keep the current goroutine, semaphore, and result-channel implementation.
	// The only behavioral change is replacing gcsClient.GetPage with reader.GetPage.
}
```

Update `parseMetaIndex` to return `[]store.WikiPage`.

- [ ] **Step 3: Update V1 handler storage dependency**

Change `internal/handler/v1/handler.go`:

```go
import store "github.com/rayer/llm-wiki-bff/internal/storage"

type Handler struct {
	store     store.Store
	firestore *firestore.Client
	index     *search.Index
	cache     *conceptcache.Cache
	llm       *llm.Client
	expander  *llm.QueryExpander

	httpClient       *http.Client
	metadataTokenURL string
	cloudRunJobURL   string
	projectExists    func(context.Context, string) error
	rebuildIndex     func(context.Context, string, string) (wikiindex.IDMap, error)
	idRoutingMu      sync.Mutex
	idRoutingMaps    map[string]dualIDMap
	listCacheMu      sync.RWMutex
	listCache        map[string]cachedLists
}

type cachedLists struct {
	concepts []store.WikiPage
	sources  []store.WikiPage
}

func New(wikiStore store.Store, fs *firestore.Client, idx *search.Index, cache *conceptcache.Cache, llmClient *llm.Client, expander *llm.QueryExpander) *Handler {
	return &Handler{
		store:         wikiStore,
		firestore:     fs,
		index:         idx,
		cache:         cache,
		llm:           llmClient,
		expander:      expander,
		idRoutingMaps: make(map[string]dualIDMap),
		listCache:     make(map[string]cachedLists),
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (h *Handler) GetStore(c *gin.Context) (store.Store, error) {
	userID := c.GetString("userID")
	projectID := c.GetString("projectID")
	if userID == "" && projectID == "" {
		return h.store, nil
	}
	if userID == "" || projectID == "" {
		return nil, fmt.Errorf("incomplete storage request scope")
	}
	if h.store == nil {
		return nil, fmt.Errorf("wiki storage is not configured")
	}
	return h.store.WithScope(userID, projectID), nil
}
```

Keep a compatibility helper only if tests still need it:

```go
func (h *Handler) GetGCSClient(c *gin.Context) (store.Store, error) {
	return h.GetStore(c)
}
```

- [ ] **Step 4: Replace handler concrete GCS usage**

In handler files, replace:

```go
gcsClient, err := h.GetGCSClient(c)
```

with:

```go
wikiStore, err := h.GetStore(c)
```

Replace `h.gcs` field reads with `h.store`. Keep imports from `internal/gcs` only where needed for type aliases or tests; otherwise use `internal/storage`.

- [ ] **Step 5: Run affected package tests**

Run:

```bash
go test ./internal/cache ./internal/search ./internal/handler/v1
```

Expected: PASS after test type adjustments.

- [ ] **Step 6: Commit**

```bash
git add internal/cache internal/search internal/handler/v1
git commit -m "refactor: use wiki storage interface in handlers"
```

---

### Task 4: Wire BFF Local Mode and Local Rebuild

**Files:**
- Modify: `main.go`
- Modify: `internal/config/config.go`
- Modify: `main_test.go` or create focused startup helper tests if startup is factored

- [ ] **Step 1: Add config field for local data**

In `internal/config/config.go`, add:

```go
LocalDataDir string
```

and load it:

```go
LocalDataDir: v.GetString("local_data_dir"),
```

- [ ] **Step 2: Parse `--local` in main**

In `main.go`, import `flag`, `os`, `internal/localfs`, `internal/storage`, and `internal/wikiindex`.

Add before config load or immediately after it:

```go
localFlag := flag.String("local", "", "local data directory")
flag.Parse()
```

Select storage:

```go
var wikiStore storage.Store
localDataDir := strings.TrimSpace(*localFlag)
if localDataDir == "" {
	localDataDir = strings.TrimSpace(cfg.LocalDataDir)
}
if localDataDir == "" {
	localDataDir = strings.TrimSpace(os.Getenv("LOCAL_DATA_DIR"))
}

localMode := localDataDir != ""
if localMode {
	wikiStore = localfs.New(localDataDir)
	log.Printf("Local wiki storage ready: %s", localDataDir)
} else {
	gcsClient, err := gcs.NewClient(cfg.Bucket)
	if err != nil {
		log.Fatalf("Failed to create GCS client: %v", err)
	}
	wikiStore = gcsClient
	log.Printf("GCS client ready: gs://%s/", cfg.Bucket)
}
```

- [ ] **Step 3: Make Firestore optional in local mode**

Change Firestore startup:

```go
var fsClient *firestore.Client
if localMode {
	log.Printf("Local mode: Firestore client disabled")
} else {
	var err error
	fsClient, err = firestore.NewClient(cfg.GCPProject, "", "")
	if err != nil {
		log.Printf("WARNING: Firestore client not available: %v", err)
	} else {
		auth.CreateTestUser(context.Background(), fsClient.Raw())
	}
}
```

- [ ] **Step 4: Inject local rebuild function**

After handler creation:

```go
hV1 := handlerv1.New(wikiStore, fsClient, idx, conceptCache, llmClient, expander)
if localMode {
	hV1.SetRebuildIndexFunc(func(ctx context.Context, userID, projectID string) (wikiindex.IDMap, error) {
		scoped := wikiStore.WithScope(userID, projectID)
		return wikiindex.Rebuild(ctx, newStorageWikiIndexStore(scoped))
	})
}
```

If `newGCSWikiIndexStore` is currently private to `endpoints.go`, add a storage-backed equivalent in the handler package:

```go
type storageWikiIndexStore struct {
	store storage.Store
}

func newStorageWikiIndexStore(store storage.Store) storageWikiIndexStore {
	return storageWikiIndexStore{store: store}
}

func (s storageWikiIndexStore) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	return s.store.ReadFile(ctx, relPath)
}

func (s storageWikiIndexStore) WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error) {
	return s.store.WriteBytesAtomic(ctx, data, tmpPath, finalPath)
}
```

- [ ] **Step 5: Run compile tests**

Run:

```bash
go test ./...
```

Expected: PASS or failures only from existing external integration assumptions. Fix compile and unit failures caused by this task.

- [ ] **Step 6: Commit**

```bash
git add main.go internal/config/config.go internal/handler/v1
git commit -m "feat: wire bff local storage mode"
```

---

### Task 5: Add Demo Data

**Files:**
- Create: `demo/users/local-user/projects/demo/index.md`
- Create: `demo/users/local-user/projects/demo/wiki.toml`
- Create: `demo/users/local-user/projects/demo/cache/concepts.jsonl`
- Create: `demo/users/local-user/projects/demo/cache/id_map.json`
- Create: `demo/users/local-user/projects/demo/raw/seed.md`
- Create: `demo/users/local-user/projects/demo/wiki/local-development.md`
- Create: `demo/users/local-user/projects/demo/wiki/storage-contract.md`
- Create: `demo/users/local-user/projects/demo/wiki/sources/local-dev-source.md`

- [ ] **Step 1: Add markdown demo pages**

Create files with stable IDs and wikilinks:

```markdown
---
id: concept-local-dev
title: Local Development
---

# Local Development

Local development validates [[Storage Contract]] and frontend routing without a deployment.
```

```markdown
---
id: concept-storage-contract
title: Storage Contract
---

# Storage Contract

The storage contract keeps local filesystem data compatible with production GCS object layout.
```

```markdown
---
id: source-local-dev
title: Local Dev Source
---

# Local Dev Source

This source explains why the demo includes prebuilt cache artifacts.
```

- [ ] **Step 2: Add cache artifacts**

Create `cache/concepts.jsonl`:

```jsonl
{"slug":"local-development","title":"Local Development","frontmatter":{"id":"concept-local-dev","title":"Local Development"}}
{"slug":"storage-contract","title":"Storage Contract","frontmatter":{"id":"concept-storage-contract","title":"Storage Contract"}}
```

Create `cache/id_map.json`:

```json
{
  "concept": {
    "concept-local-dev": "local-development",
    "concept-storage-contract": "storage-contract"
  },
  "source": {
    "source-local-dev": "local-dev-source"
  }
}
```

- [ ] **Step 3: Verify demo with localfs tests**

Run:

```bash
go test ./internal/localfs
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add demo
git commit -m "testdata: add local development demo project"
```

---

### Task 6: Makefile, Compose, and Docs

**Files:**
- Modify: `Makefile`
- Modify: `docker-compose.yml`
- Create: `docs/LOCAL_DEV.md`

- [ ] **Step 1: Update Makefile targets**

Add local targets while keeping existing deploy targets:

```makefile
.PHONY: seed dev bff-local clean-local

seed:
	rm -rf local-data
	cp -R demo local-data

dev:
	docker compose up --build

bff-local:
	LOCAL_DATA_DIR=./local-data DEV_JWT=true JWT_SECRET=dev-secret go run . --local ./local-data

clean-local:
	rm -rf local-data
```

- [ ] **Step 2: Update docker-compose.yml**

Use local mode for BFF:

```yaml
services:
  bff:
    build:
      context: .
      dockerfile: Dockerfile
    ports:
      - "8080:8080"
    volumes:
      - ./local-data:/data
    environment:
      DEV_JWT: "true"
      JWT_SECRET: "dev-secret"
      LOCAL_DATA_DIR: "/data"
    command: ["/bff", "--local", "/data"]
```

Keep frontend service, but use relative `../llm-wiki-frontend` context. Remove worker from first-phase compose unless another service depends on it.

- [ ] **Step 3: Add local dev docs**

Create `docs/LOCAL_DEV.md` with:

```markdown
# Local Development

## Quickstart

```sh
make seed
make dev
```

BFF listens on `http://localhost:8080`. Frontend listens on `http://localhost:3000`.

## Inner Loop

```sh
make seed
make bff-local
```

Local scoped API calls use:

```text
X-User-ID: local-user
X-Project-ID: demo
```

## Scope

This flow uses prebuilt demo artifacts under `demo/`. Worker and OLW regeneration are separate from this local app development flow.

## Troubleshooting

- Port `8080` is busy: run `lsof -i :8080`.
- Demo data is empty: run `make seed`.
- Projects return unauthorized: include `X-User-ID: local-user`.
- Scoped endpoints fail: include `X-Project-ID: demo`.
```

- [ ] **Step 4: Run docs-adjacent verification**

Run:

```bash
make seed
go test ./...
```

Expected: tests pass and `local-data/users/local-user/projects/demo` exists.

- [ ] **Step 5: Commit**

```bash
git add Makefile docker-compose.yml docs/LOCAL_DEV.md
git commit -m "docs: add local development quickstart"
```

---

### Task 7: Smoke Verification

**Files:**
- No required source edits unless smoke testing exposes a defect.

- [ ] **Step 1: Start BFF locally**

Run:

```bash
make seed
LOCAL_DATA_DIR=./local-data DEV_JWT=true JWT_SECRET=dev-secret go run . --local ./local-data
```

Expected: logs include local wiki storage and BFF listening on `:8080`.

- [ ] **Step 2: Verify local API endpoints**

In a second shell, run:

```bash
curl -sS -H 'X-User-ID: local-user' http://localhost:8080/api/v1/projects
curl -sS -H 'X-User-ID: local-user' -H 'X-Project-ID: demo' http://localhost:8080/api/v1/concepts
curl -sS -H 'X-User-ID: local-user' -H 'X-Project-ID: demo' http://localhost:8080/api/v1/concepts/concept-local-dev
curl -sS -H 'X-User-ID: local-user' -H 'X-Project-ID: demo' http://localhost:8080/api/v1/sources
curl -sS -H 'X-User-ID: local-user' -H 'X-Project-ID: demo' http://localhost:8080/api/v1/sources/source-local-dev
```

Expected:

- projects response includes `demo`
- concepts response includes `local-development`
- concept detail includes `Local Development`
- sources response includes `local-dev-source`
- source detail includes `Local Dev Source`

- [ ] **Step 3: Verify rebuild endpoint locally**

Run:

```bash
curl -sS -X POST -H 'X-User-ID: local-user' -H 'X-Project-ID: demo' http://localhost:8080/api/v1/pipeline/rebuild-index
```

Expected: response has `"status":"ok"` and nonzero concept/source entry counts.

- [ ] **Step 4: Final test suite**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 5: Final status check**

Run:

```bash
git status --short
```

Expected: only intentional local runtime artifacts such as `local-data/` are untracked or ignored. Source changes should be committed.
