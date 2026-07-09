# LWC-77 Raw Files Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `GET /api/v1/raw` backed by raw file metadata and a postprocess-generated `cache/raw_status.json` artifact.

**Architecture:** Extend the storage abstraction with raw file metadata listing for GCS and localfs. Add a focused `internal/rawstatus` package that parses `cache/raw_status.json`, derives `ingested`, and generates the artifact from OLW `.olw/state.db` during worker postprocess. The BFF endpoint only reads storage metadata and JSON artifacts; it never opens SQLite at runtime.

**Tech Stack:** Go 1.26, Gin, Google Cloud Storage client, local filesystem storage, `database/sql`, `modernc.org/sqlite` for worker-side SQLite reads, standard Go tests.

## Global Constraints

- Runtime BFF handlers must not open or query `.olw/state.db`.
- `GET /api/v1/raw` returns `name`, `size`, `updated`, `sha256`, and `ingested`.
- Missing `raw/` returns `200` with an empty `files` array.
- Missing `cache/raw_status.json` returns `200`; all raw files use `ingested: false`.
- Malformed `cache/raw_status.json` returns `500`.
- GCS raw listing must not read object contents to backfill missing SHA256 metadata.
- GCS raw objects missing `sha256` metadata return `sha256: ""` and `ingested: false`.
- Raw listing is direct children only; recursive raw directory listing is out of scope.
- Frontend sidebar/table work is out of scope.

---

## File Structure

- Modify `internal/storage/storage.go`: add `RawFile` type and `ListRawFiles` to `Store`.
- Modify `internal/gcs/client.go`: implement GCS `ListRawFiles` and helper for direct raw object names.
- Modify `internal/gcs/list_test.go`: cover raw path filtering helper.
- Modify `internal/localfs/client.go`: implement local `ListRawFiles`.
- Modify `internal/localfs/client_test.go`: cover local raw listing metadata, direct-only behavior, and SHA256.
- Create `internal/rawstatus/rawstatus.go`: artifact types, JSON read/parse helpers, and ingested derivation.
- Create `internal/rawstatus/rawstatus_test.go`: cover missing/malformed artifact and status merge rules.
- Create `internal/rawstatus/sqlite.go`: worker-only state DB reader/generator helpers using `database/sql`.
- Create `internal/rawstatus/sqlite_test.go`: create temp SQLite DBs and verify artifact generation.
- Modify `cmd/olw_worker/main.go`: call raw status generation during postprocess.
- Modify `cmd/olw_worker/main_test.go`: assert postprocess/raw status behavior.
- Create `internal/handler/v1/raw_list.go`: `GET /api/v1/raw` handler and response types.
- Create `internal/handler/v1/raw_list_test.go`: handler behavior tests.
- Modify `main.go`: register `v1.GET("/raw", hV1.RawList)`.
- Add `demo/users/local-user/projects/demo/cache/raw_status.json`: empty demo artifact.
- Modify `go.mod` and `go.sum`: add `modernc.org/sqlite` dependency.

---

### Task 1: Storage Raw File Metadata Listing

**Files:**
- Modify: `internal/storage/storage.go`
- Modify: `internal/gcs/client.go`
- Modify: `internal/gcs/list_test.go`
- Modify: `internal/localfs/client.go`
- Modify: `internal/localfs/client_test.go`

**Interfaces:**
- Produces:
  - `type storage.RawFile struct { Name string; Path string; Size int64; Updated time.Time; SHA256 string }`
  - `Store.ListRawFiles(ctx context.Context) ([]RawFile, error)`
- Consumes: existing `Store` implementations and project-scoped prefixes.

- [ ] **Step 1: Write failing localfs raw listing test**

Add to `internal/localfs/client_test.go`:

```go
func TestListRawFilesReturnsDirectRawMetadata(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "users/u/projects/p/raw/beta.md", "beta")
	mustWrite(t, root, "users/u/projects/p/raw/alpha.txt", "alpha")
	mustWrite(t, root, "users/u/projects/p/raw/nested/ignored.md", "ignored")

	client := New(root).WithScope("u", "p")
	files, err := client.ListRawFiles(context.Background())
	if err != nil {
		t.Fatalf("ListRawFiles() error = %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2: %#v", len(files), files)
	}
	if files[0].Name != "alpha.txt" || files[0].Path != "raw/alpha.txt" || files[0].Size != int64(len("alpha")) || files[0].SHA256 == "" || files[0].Updated.IsZero() {
		t.Fatalf("files[0] = %#v", files[0])
	}
	if files[1].Name != "beta.md" || files[1].Path != "raw/beta.md" || files[1].Size != int64(len("beta")) || files[1].SHA256 == "" || files[1].Updated.IsZero() {
		t.Fatalf("files[1] = %#v", files[1])
	}
}

func TestListRawFilesMissingDirectoryReturnsEmpty(t *testing.T) {
	files, err := New(t.TempDir()).WithScope("u", "p").ListRawFiles(context.Background())
	if err != nil {
		t.Fatalf("ListRawFiles() error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("len(files) = %d, want 0", len(files))
	}
}
```

- [ ] **Step 2: Write failing GCS raw path helper test**

Add to `internal/gcs/list_test.go`:

```go
func TestRawFileNameFromObjectKeepsDirectRawChildren(t *testing.T) {
	client := &Client{userID: "u1", projectID: "p1"}

	name, ok := client.rawFileNameFromObject("users/u1/projects/p1/raw/article.md")
	if !ok {
		t.Fatal("rawFileNameFromObject returned ok=false")
	}
	if name != "article.md" {
		t.Fatalf("name = %q, want article.md", name)
	}
}

func TestRawFileNameFromObjectRejectsNestedAndMarkers(t *testing.T) {
	client := &Client{userID: "u1", projectID: "p1"}

	tests := []string{
		"users/u1/projects/p1/raw/",
		"users/u1/projects/p1/raw/nested/article.md",
		"users/u1/projects/p1/wiki/article.md",
		"users/u2/projects/p1/raw/article.md",
	}
	for _, objectName := range tests {
		if name, ok := client.rawFileNameFromObject(objectName); ok {
			t.Fatalf("rawFileNameFromObject(%q) = %q, true; want false", objectName, name)
		}
	}
}
```

- [ ] **Step 3: Run tests to verify RED**

Run:

```bash
go test ./internal/localfs ./internal/gcs
```

Expected: fail because `ListRawFiles` and `rawFileNameFromObject` are undefined.

- [ ] **Step 4: Implement storage interface and localfs listing**

In `internal/storage/storage.go`, import `time` and add:

```go
type RawFile struct {
	Name    string
	Path    string
	Size    int64
	Updated time.Time
	SHA256  string
}
```

Add to `Store`:

```go
ListRawFiles(ctx context.Context) ([]RawFile, error)
```

In `internal/localfs/client.go`, add:

```go
func (c *Client) ListRawFiles(ctx context.Context) ([]store.RawFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fullDir, err := c.fullPath("raw")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []store.RawFile{}, nil
		}
		return nil, fmt.Errorf("list raw files: %w", err)
	}

	files := make([]store.RawFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat raw/%s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("raw", entry.Name()))
		sha256, err := c.GetMetaSHA256(ctx, rel)
		if err != nil {
			return nil, err
		}
		files = append(files, store.RawFile{
			Name:    entry.Name(),
			Path:    rel,
			Size:    info.Size(),
			Updated: info.ModTime().UTC(),
			SHA256:  sha256,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})
	return files, nil
}
```

- [ ] **Step 5: Implement GCS listing**

In `internal/gcs/client.go`, add:

```go
func (c *Client) ListRawFiles(ctx context.Context) ([]store.RawFile, error) {
	prefix := c.prefix() + "/raw/"
	it := c.bucket.Objects(ctx, &storage.Query{Prefix: prefix})

	files := make([]store.RawFile, 0)
	for {
		attrs, err := it.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				break
			}
			return nil, err
		}
		name, ok := c.rawFileNameFromObject(attrs.Name)
		if !ok {
			continue
		}
		files = append(files, store.RawFile{
			Name:    name,
			Path:    "raw/" + name,
			Size:    attrs.Size,
			Updated: attrs.Updated.UTC(),
			SHA256:  attrs.Metadata["sha256"],
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})
	return files, nil
}

func (c *Client) rawFileNameFromObject(objectName string) (string, bool) {
	prefix := c.prefix() + "/raw/"
	if !strings.HasPrefix(objectName, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(objectName, prefix)
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}
```

- [ ] **Step 6: Run tests to verify GREEN**

Run:

```bash
go test ./internal/localfs ./internal/gcs
```

Expected: pass.

- [ ] **Step 7: Commit**

```bash
git add internal/storage/storage.go internal/localfs/client.go internal/localfs/client_test.go internal/gcs/client.go internal/gcs/list_test.go
git commit -m "feat: list raw file metadata"
```

---

### Task 2: Raw Status Artifact Parsing and Merge Rules

**Files:**
- Create: `internal/rawstatus/rawstatus.go`
- Create: `internal/rawstatus/rawstatus_test.go`

**Interfaces:**
- Consumes: `storage.RawFile`
- Produces:
  - `const Path = "cache/raw_status.json"`
  - `type Artifact`
  - `type FileStatus`
  - `func Decode(data []byte) (Artifact, error)`
  - `func Apply(files []storage.RawFile, artifact Artifact) []File`
  - `func EmptyArtifact(now time.Time) Artifact`

- [ ] **Step 1: Write failing rawstatus tests**

Create `internal/rawstatus/rawstatus_test.go`:

```go
package rawstatus

import (
	"testing"
	"time"

	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestApplyMarksIngestedWhenHashStatusAndErrorMatch(t *testing.T) {
	files := []store.RawFile{{
		Name:    "seed.md",
		Path:    "raw/seed.md",
		Size:    4,
		Updated: time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC),
		SHA256:  "abc",
	}}
	artifact := Artifact{Files: map[string]FileStatus{
		"seed.md": {Path: "raw/seed.md", SHA256: "abc", OLWStatus: "ingested", Ingested: true},
	}}

	got := Apply(files, artifact)

	if len(got) != 1 || !got[0].Ingested {
		t.Fatalf("Apply() = %#v, want ingested seed.md", got)
	}
}

func TestApplyReturnsFalseForMissingChangedErrorAndUnsupportedStatus(t *testing.T) {
	files := []store.RawFile{
		{Name: "missing.md", Path: "raw/missing.md", SHA256: "same"},
		{Name: "changed.md", Path: "raw/changed.md", SHA256: "new"},
		{Name: "failed.md", Path: "raw/failed.md", SHA256: "same"},
		{Name: "new.md", Path: "raw/new.md", SHA256: "same"},
	}
	artifact := Artifact{Files: map[string]FileStatus{
		"changed.md": {Path: "raw/changed.md", SHA256: "old", OLWStatus: "ingested", Ingested: true},
		"failed.md":  {Path: "raw/failed.md", SHA256: "same", OLWStatus: "ingested", Ingested: true, Error: "boom"},
		"new.md":     {Path: "raw/new.md", SHA256: "same", OLWStatus: "new", Ingested: true},
	}}

	got := Apply(files, artifact)

	for _, file := range got {
		if file.Ingested {
			t.Fatalf("%s ingested=true, want false: %#v", file.Name, got)
		}
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	if _, err := Decode([]byte(`{"files":`)); err == nil {
		t.Fatal("Decode() error = nil, want malformed JSON error")
	}
}
```

- [ ] **Step 2: Run test to verify RED**

Run:

```bash
go test ./internal/rawstatus
```

Expected: fail because package does not exist.

- [ ] **Step 3: Implement rawstatus package**

Create `internal/rawstatus/rawstatus.go`:

```go
package rawstatus

import (
	"encoding/json"
	"time"

	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

const Path = "cache/raw_status.json"

type Artifact struct {
	Version     int                   `json:"version"`
	GeneratedAt string                `json:"generated_at"`
	Files       map[string]FileStatus `json:"files"`
}

type FileStatus struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	OLWStatus string `json:"olw_status"`
	Ingested  bool   `json:"ingested"`
	IngestedAt string `json:"ingested_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

type File struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Updated  string `json:"updated"`
	SHA256   string `json:"sha256"`
	Ingested bool   `json:"ingested"`
}

func EmptyArtifact(now time.Time) Artifact {
	return Artifact{
		Version:     1,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Files:       map[string]FileStatus{},
	}
}

func Decode(data []byte) (Artifact, error) {
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return Artifact{}, err
	}
	if artifact.Files == nil {
		artifact.Files = map[string]FileStatus{}
	}
	return artifact, nil
}

func Apply(files []store.RawFile, artifact Artifact) []File {
	out := make([]File, 0, len(files))
	for _, raw := range files {
		status := artifact.Files[raw.Name]
		out = append(out, File{
			Name:     raw.Name,
			Size:     raw.Size,
			Updated:  raw.Updated.UTC().Format(time.RFC3339),
			SHA256:   raw.SHA256,
			Ingested: isIngested(raw, status),
		})
	}
	return out
}

func isIngested(raw store.RawFile, status FileStatus) bool {
	if raw.SHA256 == "" || status.SHA256 != raw.SHA256 || status.Error != "" || !status.Ingested {
		return false
	}
	return status.OLWStatus == "ingested" || status.OLWStatus == "compiled"
}
```

- [ ] **Step 4: Run test to verify GREEN**

Run:

```bash
go test ./internal/rawstatus
```

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/rawstatus
git commit -m "feat: add raw status artifact merge rules"
```

---

### Task 3: Generate Raw Status from OLW SQLite State

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/rawstatus/sqlite.go`
- Create: `internal/rawstatus/sqlite_test.go`

**Interfaces:**
- Consumes: `rawstatus.Artifact`, `storage.RawFile`
- Produces:
  - `func BuildFromStateDB(ctx context.Context, dbPath string, files []storage.RawFile, now time.Time) (Artifact, error)`
  - `func StateDBPath(vault string) string`

- [ ] **Step 1: Add SQLite dependency**

Run:

```bash
go get modernc.org/sqlite@v1.53.0
```

Expected: `go.mod` and `go.sum` update.

- [ ] **Step 2: Write failing SQLite generation tests**

Create `internal/rawstatus/sqlite_test.go`:

```go
package rawstatus

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestBuildFromStateDBMarksMatchingIngestedRows(t *testing.T) {
	dbPath := createStateDB(t, `
insert into raw_notes(path, content_hash, status, ingested_at, error) values
('raw/seed.md', 'abc', 'ingested', '2026-06-21T16:25:25Z', ''),
('raw/changed.md', 'old', 'ingested', '2026-06-21T16:25:25Z', ''),
('raw/failed.md', 'same', 'ingested', '2026-06-21T16:25:25Z', 'boom');
`)
	files := []store.RawFile{
		{Name: "seed.md", Path: "raw/seed.md", SHA256: "abc"},
		{Name: "changed.md", Path: "raw/changed.md", SHA256: "new"},
		{Name: "failed.md", Path: "raw/failed.md", SHA256: "same"},
	}

	artifact, err := BuildFromStateDB(context.Background(), dbPath, files, time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildFromStateDB() error = %v", err)
	}

	if !artifact.Files["seed.md"].Ingested {
		t.Fatalf("seed status = %#v, want ingested", artifact.Files["seed.md"])
	}
	if artifact.Files["changed.md"].Ingested || artifact.Files["failed.md"].Ingested {
		t.Fatalf("changed/failed statuses = %#v", artifact.Files)
	}
}

func TestBuildFromStateDBMissingDBReturnsEmptyArtifact(t *testing.T) {
	artifact, err := BuildFromStateDB(context.Background(), filepath.Join(t.TempDir(), "missing.db"), nil, time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildFromStateDB() error = %v", err)
	}
	if len(artifact.Files) != 0 {
		t.Fatalf("files = %#v, want empty", artifact.Files)
	}
}

func TestBuildFromStateDBSchemaMismatchFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table raw_notes(path text primary key);`); err != nil {
		t.Fatal(err)
	}

	if _, err := BuildFromStateDB(context.Background(), path, nil, time.Now()); err == nil {
		t.Fatal("BuildFromStateDB() error = nil, want schema mismatch error")
	}
}

func createStateDB(t *testing.T, inserts string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
create table raw_notes (
	path text primary key,
	content_hash text not null,
	status text not null default 'new',
	ingested_at text,
	error text
);
` + inserts); err != nil {
		t.Fatal(err)
	}
	return path
}
```

- [ ] **Step 3: Run test to verify RED**

Run:

```bash
go test ./internal/rawstatus
```

Expected: fail because `BuildFromStateDB` is undefined.

- [ ] **Step 4: Implement SQLite generation**

Create `internal/rawstatus/sqlite.go`:

```go
package rawstatus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
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
```

- [ ] **Step 5: Run tests to verify GREEN**

Run:

```bash
go test ./internal/rawstatus
```

Expected: pass.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/rawstatus
git commit -m "feat: generate raw status from OLW state"
```

---

### Task 4: Wire Raw Status Generation into Worker Postprocess

**Files:**
- Modify: `cmd/olw_worker/main.go`
- Modify: `cmd/olw_worker/main_test.go`

**Interfaces:**
- Consumes:
  - `localfs.New(vault).WithScope(...)` is not suitable for bare vault paths.
  - Existing `fsstore.New(vault)` only supports wikiindex store methods.
- Produces:
  - Worker postprocess writes `cache/raw_status.json` after rebuilding wikiindex artifacts.

- [ ] **Step 1: Write failing worker test**

Add to `cmd/olw_worker/main_test.go`:

```go
func TestRunPostprocessWritesEmptyRawStatusWhenStateDBMissing(t *testing.T) {
	vault := t.TempDir()
	mustWriteFile(t, filepath.Join(vault, "raw", "seed.md"), []byte("seed"))
	mustWriteFile(t, filepath.Join(vault, "wiki", "alpha.md"), []byte("---\nid: concept-id\ntitle: Alpha\n---\nAlpha"))

	if err := runPostprocess(context.Background(), vault); err != nil {
		t.Fatalf("runPostprocess() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(vault, "cache", "raw_status.json"))
	if err != nil {
		t.Fatalf("read raw_status.json: %v", err)
	}
	if !strings.Contains(string(data), `"files": {}`) {
		t.Fatalf("raw_status.json = %s, want empty files object", data)
	}
}
```

- [ ] **Step 2: Run test to verify RED**

Run:

```bash
go test ./cmd/olw_worker -run TestRunPostprocessWritesEmptyRawStatusWhenStateDBMissing
```

Expected: fail because `raw_status.json` is not written.

- [ ] **Step 3: Implement vault raw file listing helper and postprocess write**

In `cmd/olw_worker/main.go`, add direct vault scanning. Do not use
`internal/localfs` here because `runPostprocess` receives a bare project vault
path, not a storage root with user/project scope.

```go
func listVaultRawFiles(ctx context.Context, vault string) ([]storage.RawFile, error) {
	rawDir := filepath.Join(vault, "raw")
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []storage.RawFile{}, nil
		}
		return nil, err
	}
	files := make([]storage.RawFile, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("raw", entry.Name()))
		data, err := os.ReadFile(filepath.Join(rawDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		files = append(files, storage.RawFile{
			Name:    entry.Name(),
			Path:    rel,
			Size:    info.Size(),
			Updated: info.ModTime().UTC(),
			SHA256:  fmt.Sprintf("%x", sum[:]),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})
	return files, nil
}
```

Add postprocess write:

```go
func writeRawStatus(ctx context.Context, vault string) error {
	files, err := listVaultRawFiles(ctx, vault)
	if err != nil {
		return fmt.Errorf("list raw files: %w", err)
	}
	artifact, err := rawstatus.BuildFromStateDB(ctx, rawstatus.StateDBPath(vault), files, time.Now())
	if err != nil {
		return fmt.Errorf("build raw status: %w", err)
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	store := fsstore.New(vault)
	if _, err := store.WriteBytesAtomic(ctx, data, "cache/raw_status.json.tmp", rawstatus.Path); err != nil {
		return fmt.Errorf("write raw status: %w", err)
	}
	return nil
}
```

Call `writeRawStatus(ctx, vault)` after successful `wikiindex.Rebuild`.

- [ ] **Step 4: Update imports**

Ensure `cmd/olw_worker/main.go` imports these packages in addition to the
imports it already needs for existing worker code:

```go
import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/rawstatus"
	"github.com/rayer/llm-wiki-bff/internal/storage"
)
```

- [ ] **Step 5: Run tests to verify GREEN**

Run:

```bash
go test ./cmd/olw_worker ./internal/rawstatus
```

Expected: pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/olw_worker/main.go cmd/olw_worker/main_test.go
git commit -m "feat: write raw status during worker postprocess"
```

---

### Task 5: Add `GET /api/v1/raw`

**Files:**
- Create: `internal/handler/v1/raw_list.go`
- Create: `internal/handler/v1/raw_list_test.go`
- Modify: `main.go`

**Interfaces:**
- Consumes:
  - `Store.ListRawFiles(ctx)`
  - `Store.ReadFile(ctx, rawstatus.Path)`
  - `rawstatus.Decode`
  - `rawstatus.Apply`
- Produces:
  - `func (h *Handler) RawList(c *gin.Context)`

- [ ] **Step 1: Write failing handler tests**

Create `internal/handler/v1/raw_list_test.go`:

```go
package v1

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/localfs"
	"github.com/rayer/llm-wiki-bff/internal/search"
)

func TestRawListReturnsFilesWithIngestedFromArtifact(t *testing.T) {
	root := t.TempDir()
	mustWriteHandlerFile(t, root, "users/request-user/projects/demo/raw/seed.md", "seed")
	sum := sha256Hex("seed")
	mustWriteHandlerFile(t, root, "users/request-user/projects/demo/cache/raw_status.json", `{"version":1,"files":{"seed.md":{"path":"raw/seed.md","sha256":"`+sum+`","olw_status":"ingested","ingested":true,"error":""}}}`)

	h := New(localfs.New(root), nil, search.NewIndex(), nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/raw", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo")

	h.RawList(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Files []struct {
			Name     string `json:"name"`
			SHA256   string `json:"sha256"`
			Ingested bool   `json:"ingested"`
		} `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Files) != 1 || body.Files[0].Name != "seed.md" || body.Files[0].SHA256 != sum || !body.Files[0].Ingested {
		t.Fatalf("body = %#v", body)
	}
}

func TestRawListMissingStatusMarksAllUningested(t *testing.T) {
	root := t.TempDir()
	mustWriteHandlerFile(t, root, "users/request-user/projects/demo/raw/seed.md", "seed")

	h := New(localfs.New(root), nil, search.NewIndex(), nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/raw", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo")

	h.RawList(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Files []struct {
			Ingested bool `json:"ingested"`
		} `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Files) != 1 || body.Files[0].Ingested {
		t.Fatalf("body = %#v, want one uningested file", body)
	}
}

func TestRawListMalformedStatusReturns500(t *testing.T) {
	root := t.TempDir()
	mustWriteHandlerFile(t, root, "users/request-user/projects/demo/raw/seed.md", "seed")
	mustWriteHandlerFile(t, root, "users/request-user/projects/demo/cache/raw_status.json", `{"files":`)

	h := New(localfs.New(root), nil, search.NewIndex(), nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/raw", nil)
	c.Set("userID", "request-user")
	c.Set("projectID", "demo")

	h.RawList(c)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func mustWriteHandlerFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
```

Append this helper at the bottom of `internal/handler/v1/raw_list_test.go`:

```go
func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:])
}
```

- [ ] **Step 2: Run test to verify RED**

Run:

```bash
go test ./internal/handler/v1 -run RawList
```

Expected: fail because `RawList` is undefined.

- [ ] **Step 3: Implement handler**

Create `internal/handler/v1/raw_list.go`:

```go
package v1

import (
	"errors"
	"net/http"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gin-gonic/gin"
	"github.com/rayer/llm-wiki-bff/internal/handler"
	"github.com/rayer/llm-wiki-bff/internal/rawstatus"
)

type rawListResponse struct {
	Files []rawstatus.File `json:"files"`
}

func (h *Handler) RawList(c *gin.Context) {
	wikiStore, err := h.GetStore(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: err.Error()})
		return
	}
	files, err := wikiStore.ListRawFiles(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "list raw files: " + err.Error()})
		return
	}

	artifact := rawstatus.EmptyArtifact(time.Now())
	data, err := wikiStore.ReadFile(c.Request.Context(), rawstatus.Path)
	if err != nil {
		if !errors.Is(err, storage.ErrObjectNotExist) {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "read raw status: " + err.Error()})
			return
		}
	} else {
		artifact, err = rawstatus.Decode(data)
		if err != nil {
			c.JSON(http.StatusInternalServerError, handler.ErrorResponse{Error: "decode raw status: " + err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, rawListResponse{Files: rawstatus.Apply(files, artifact)})
}
```

- [ ] **Step 4: Register route**

In `main.go`, add inside the existing project-scoped route group:

```go
v1.GET("/raw", hV1.RawList)
```

Place it next to `v1.POST("/raw/upload", hV1.RawUpload)`.

- [ ] **Step 5: Run tests to verify GREEN**

Run:

```bash
go test ./internal/handler/v1
```

Expected: pass.

- [ ] **Step 6: Commit**

```bash
git add main.go internal/handler/v1/raw_list.go internal/handler/v1/raw_list_test.go
git commit -m "feat: add raw files API"
```

---

### Task 6: Demo Raw Status Artifact and Full Verification

**Files:**
- Create: `demo/users/local-user/projects/demo/cache/raw_status.json`

**Interfaces:**
- Consumes: all previous tasks.
- Produces: local demo has a status artifact with conservative empty status.

- [ ] **Step 1: Add demo artifact**

Create `demo/users/local-user/projects/demo/cache/raw_status.json`:

```json
{
  "version": 1,
  "generated_at": "2026-07-09T00:00:00Z",
  "files": {}
}
```

- [ ] **Step 2: Run targeted verification**

Run:

```bash
go test ./cmd/olw_worker ./internal/rawstatus ./internal/gcs ./internal/localfs ./internal/handler/v1
```

Expected: pass.

- [ ] **Step 3: Run broader verification**

Run:

```bash
go test ./...
```

Expected: pass.

- [ ] **Step 4: Commit**

```bash
git add demo/users/local-user/projects/demo/cache/raw_status.json
git commit -m "test: add demo raw status artifact"
```

---

## Self-Review Checklist

- Spec coverage:
  - `GET /api/v1/raw`: Task 5.
  - Storage-neutral raw metadata listing: Task 1.
  - Worker-generated `cache/raw_status.json`: Tasks 3 and 4.
  - Missing status returns 200 with `ingested: false`: Tasks 2 and 5.
  - Malformed status returns 500: Task 5.
  - No runtime SQLite reads: Task 5 only reads storage JSON.
  - GCS missing SHA256 limitation: Task 1 and Global Constraints.
  - Demo artifact: Task 6.
- Placeholder scan: no `TBD`, `TODO`, or unspecified edge handling remains.
- Type consistency:
  - `storage.RawFile` is consumed by `rawstatus.Apply` and `rawstatus.BuildFromStateDB`.
  - `rawstatus.Path` is used by worker and handler.
  - `RawList` route is registered as `GET /api/v1/raw`.
