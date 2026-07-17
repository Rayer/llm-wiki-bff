package localfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/storage"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
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

func TestMarkdownFallbackListing(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "users/u/projects/p/wiki/published.md", "---\nid: c1\ntitle: Published\n---\nBody")
	mustWrite(t, root, "users/u/projects/p/wiki/index.md", "# Index")
	mustWrite(t, root, "users/u/projects/p/wiki/sources/source-one.md", "---\nid: s1\ntitle: Source One\n---\nSource")
	mustWrite(t, root, "users/u/projects/p/wiki/.drafts/draft.md", "---\nid: d1\ntitle: Draft\n---\nDraft")

	client := New(root).WithScope("u", "p")
	concepts, err := client.ListConcepts(context.Background(), false)
	if err != nil {
		t.Fatalf("ListConcepts(false) error = %v", err)
	}
	if len(concepts) != 1 || concepts[0].Slug != "published" || concepts[0].Status != "published" {
		t.Fatalf("published concepts = %#v", concepts)
	}

	concepts, err = client.ListConcepts(context.Background(), true)
	if err != nil {
		t.Fatalf("ListConcepts(true) error = %v", err)
	}
	if len(concepts) != 2 || concepts[1].Slug != "draft" || concepts[1].Status != "draft" {
		t.Fatalf("all concepts = %#v", concepts)
	}

	sources, err := client.ListSources(context.Background())
	if err != nil {
		t.Fatalf("ListSources() error = %v", err)
	}
	if len(sources) != 1 || sources[0].Slug != "source-one" || sources[0].ID != "s1" {
		t.Fatalf("sources = %#v", sources)
	}
}

func TestWritesStatsAndDigest(t *testing.T) {
	client := New(t.TempDir()).WithScope("u", "p")
	ctx := context.Background()

	digest, err := client.WriteBytes(ctx, []byte("body"), "raw/note.md")
	if err != nil {
		t.Fatalf("WriteBytes() error = %v", err)
	}
	gotDigest, err := client.GetMetaSHA256(ctx, "raw/note.md")
	if err != nil {
		t.Fatalf("GetMetaSHA256() error = %v", err)
	}
	if gotDigest != digest {
		t.Fatalf("digest = %q, want %q", gotDigest, digest)
	}

	if _, err := client.WriteBytesAtomic(ctx, []byte("atomic"), "cache/out.tmp", "cache/out.txt"); err != nil {
		t.Fatalf("WriteBytesAtomic() error = %v", err)
	}
	bytes, files, err := client.BucketStats(ctx)
	if err != nil {
		t.Fatalf("BucketStats() error = %v", err)
	}
	if bytes == 0 || files != 2 {
		t.Fatalf("BucketStats() bytes=%d files=%d, want nonzero bytes and 2 files", bytes, files)
	}
}

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

func TestMissingFileUsesStorageNotFound(t *testing.T) {
	_, err := New(t.TempDir()).WithScope("u", "p").ReadFile(context.Background(), "wiki/missing.md")
	if !errors.Is(err, storage.ErrObjectNotExist) {
		t.Fatalf("ReadFile() error = %v, want storage.ErrObjectNotExist", err)
	}
}

func TestConditionalAnnotationWrites(t *testing.T) {
	client := New(t.TempDir()).WithScope("u", "p")
	ctx := context.Background()
	if _, err := client.WriteFileIfGeneration(ctx, []byte("one"), "cache/annotations/id.json", 0); err != nil {
		t.Fatal(err)
	}
	_, generation, err := client.ReadFileWithGeneration(ctx, "cache/annotations/id.json")
	if err != nil || generation == 0 {
		t.Fatalf("read generation: %d %v", generation, err)
	}
	if _, err := client.WriteFileIfGeneration(ctx, []byte("two"), "cache/annotations/id.json", 0); !errors.Is(err, store.ErrGenerationMismatch) {
		t.Fatalf("create conflict = %v", err)
	}
	if _, err := client.WriteFileIfGeneration(ctx, []byte("two"), "cache/annotations/id.json", generation+1); !errors.Is(err, store.ErrGenerationMismatch) {
		t.Fatalf("stale update = %v", err)
	}
	next, err := client.WriteFileIfGeneration(ctx, []byte("two"), "cache/annotations/id.json", generation)
	if err != nil {
		t.Fatal(err)
	}
	if next <= generation {
		t.Fatalf("next generation = %d, want greater than %d", next, generation)
	}
	if _, err := client.WriteFileIfGeneration(ctx, []byte("three"), "cache/annotations/id.json", generation); !errors.Is(err, store.ErrGenerationMismatch) {
		t.Fatalf("old generation accepted after rapid writes: %v", err)
	}
}

func TestConditionalWriteRecoversStaleSidecarBeforeAcceptingGeneration(t *testing.T) {
	client := New(t.TempDir()).WithScope("u", "p")
	ctx := context.Background()
	path := "cache/annotations/id.json"
	if _, err := client.WriteFileIfGeneration(ctx, []byte("one"), path, 0); err != nil {
		t.Fatal(err)
	}
	_, generation, err := client.ReadFileWithGeneration(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	full, err := client.fullPath(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after object rename but before the old sidecar changed.
	if err := os.WriteFile(full, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, recovered, err := client.ReadFileWithGeneration(ctx, path)
	if err != nil || string(data) != "two" || recovered <= generation {
		t.Fatalf("recovery data=%q generation=%d old=%d err=%v", data, recovered, generation, err)
	}
	if _, err := client.WriteFileIfGeneration(ctx, []byte("three"), path, generation); !errors.Is(err, store.ErrGenerationMismatch) {
		t.Fatalf("stale generation accepted after recovery: %v", err)
	}
}

func TestListObjectMetaDoesNotExposeOrReadAnnotationSidecars(t *testing.T) {
	client := New(t.TempDir()).WithScope("u", "p")
	ctx := context.Background()
	if _, err := client.WriteFileIfGeneration(ctx, []byte(`{"ann_sha256":"abc","raw_path":"raw/a.md","body":"note"}`), "cache/annotations/id.json", 0); err != nil {
		t.Fatal(err)
	}
	entries, err := client.ListObjectMeta(ctx, "cache/annotations/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "cache/annotations/id.json" || entries[0].SHA256 != "abc" || !entries[0].HasAnnotation {
		t.Fatalf("metadata entries = %#v", entries)
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
