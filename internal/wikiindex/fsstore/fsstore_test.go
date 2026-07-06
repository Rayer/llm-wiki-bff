package fsstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

func TestListMarkdownFilesListsDirectChildrenOnly(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "wiki", "beta.md"), []byte("beta"))
	mustWriteFile(t, filepath.Join(root, "wiki", "alpha.md"), []byte("alpha"))
	mustWriteFile(t, filepath.Join(root, "wiki", "notes.txt"), []byte("notes"))
	mustWriteFile(t, filepath.Join(root, "wiki", "nested", "gamma.md"), []byte("gamma"))

	files, err := New(root).ListMarkdownFiles(context.Background(), "wiki/")
	if err != nil {
		t.Fatalf("ListMarkdownFiles() error = %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2: %#v", len(files), files)
	}
	want := []wikiindex.MarkdownFile{
		{Slug: "alpha", Path: "wiki/alpha.md", Data: []byte("alpha")},
		{Slug: "beta", Path: "wiki/beta.md", Data: []byte("beta")},
	}
	for i := range want {
		if files[i].Slug != want[i].Slug || files[i].Path != want[i].Path || string(files[i].Data) != string(want[i].Data) {
			t.Fatalf("files[%d] = %#v, want %#v", i, files[i], want[i])
		}
	}
}

func TestListMarkdownFilesMissingDirectoryReturnsEmpty(t *testing.T) {
	files, err := New(t.TempDir()).ListMarkdownFiles(context.Background(), "wiki/missing")
	if err != nil {
		t.Fatalf("ListMarkdownFiles() error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("len(files) = %d, want 0", len(files))
	}
}

func TestReadFileMapsNotFound(t *testing.T) {
	_, err := New(t.TempDir()).ReadFile(context.Background(), "wiki/missing.md")
	if !errors.Is(err, wikiindex.ErrNotFound) {
		t.Fatalf("ReadFile() error = %v, want ErrNotFound", err)
	}
}

func TestWriteBytesAtomicReplacesFileAndReturnsSHA256(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	mustWriteFile(t, filepath.Join(root, "cache", "out.txt"), []byte("old"))

	data := []byte("replacement")
	digest, err := store.WriteBytesAtomic(context.Background(), data, "cache/out.txt.tmp", "cache/out.txt")
	if err != nil {
		t.Fatalf("WriteBytesAtomic() error = %v", err)
	}

	sum := sha256.Sum256(data)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %q, want %q", digest, hex.EncodeToString(sum[:]))
	}
	got, err := os.ReadFile(filepath.Join(root, "cache", "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("final data = %q, want %q", got, data)
	}
	if _, err := os.Stat(filepath.Join(root, "cache", "out.txt.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file stat error = %v, want not exist", err)
	}
}

func TestRejectsPathsEscapingRoot(t *testing.T) {
	store := New(t.TempDir())
	ctx := context.Background()

	if _, err := store.ListMarkdownFiles(ctx, "../wiki"); err == nil {
		t.Fatal("ListMarkdownFiles() error = nil, want traversal rejection")
	}
	if _, err := store.ReadFile(ctx, "wiki/../../outside"); err == nil {
		t.Fatal("ReadFile() error = nil, want normalized traversal rejection")
	}
	if _, err := store.ReadFile(ctx, "/tmp/outside.md"); err == nil {
		t.Fatal("ReadFile() error = nil, want absolute path rejection")
	}
	if _, err := store.WriteBytesAtomic(ctx, []byte("data"), "cache/tmp", "../cache/out"); err == nil {
		t.Fatal("WriteBytesAtomic() error = nil, want traversal rejection")
	}
}

func TestRejectsSymlinkEscapesUnderRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "secret.md"), []byte("secret"))
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "wiki", "secret.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	store := New(root)
	if _, err := store.ReadFile(context.Background(), "linked-dir/secret.md"); err == nil {
		t.Fatal("ReadFile() error = nil, want symlink dir escape rejection")
	}
	if _, err := store.ListMarkdownFiles(context.Background(), "linked-dir"); err == nil {
		t.Fatal("ListMarkdownFiles() error = nil, want symlink dir escape rejection")
	}
	if _, err := store.ListMarkdownFiles(context.Background(), "wiki"); err == nil {
		t.Fatal("ListMarkdownFiles() error = nil, want symlink file escape rejection")
	}
}

func TestWriteBytesAtomicDoesNotFollowSymlinkTempPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.txt")
	mustWriteFile(t, outsideFile, []byte("outside"))
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "cache", "out.txt.tmp")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, err := New(root).WriteBytesAtomic(context.Background(), []byte("replacement"), "cache/out.txt.tmp", "cache/out.txt"); err != nil {
		t.Fatalf("WriteBytesAtomic() error = %v", err)
	}
	got, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "outside" {
		t.Fatalf("outside file = %q, want untouched", got)
	}
}

func TestWriteBytesAtomicCleansUpTempFileOnCanceledContext(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := New(root).WriteBytesAtomic(ctx, []byte("data"), "cache/out.txt.tmp", "cache/out.txt"); err == nil {
		t.Fatal("WriteBytesAtomic() error = nil, want cancellation error")
	}
	entries, err := os.ReadDir(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cache entries after canceled write = %d, want 0", len(entries))
	}
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
