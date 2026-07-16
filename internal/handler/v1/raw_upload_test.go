package v1

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestRawUploadDecision(t *testing.T) {
	tests := []struct {
		name                   string
		exists, overwrite      bool
		existingDigest, digest string
		wantStatus             int
		wantUploadStatus       string
		wantWrite              bool
	}{
		{name: "create", digest: "new", wantStatus: http.StatusCreated, wantUploadStatus: rawUploadStatusCreated, wantWrite: true},
		{name: "already exists", exists: true, existingDigest: "same", digest: "same", wantStatus: http.StatusOK, wantUploadStatus: rawUploadStatusAlreadyExists},
		{name: "conflict without overwrite", exists: true, existingDigest: "old", digest: "new", wantStatus: http.StatusConflict},
		{name: "replace with overwrite", exists: true, overwrite: true, existingDigest: "old", digest: "new", wantStatus: http.StatusOK, wantUploadStatus: rawUploadStatusReplaced, wantWrite: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, uploadStatus, write := rawUploadDecision(tt.exists, tt.existingDigest, tt.digest, tt.overwrite)
			if status != tt.wantStatus || uploadStatus != tt.wantUploadStatus || write != tt.wantWrite {
				t.Fatalf("got status=%d uploadStatus=%q write=%v; want status=%d uploadStatus=%q write=%v", status, uploadStatus, write, tt.wantStatus, tt.wantUploadStatus, tt.wantWrite)
			}
		})
	}
}

func TestValidateRawUploadFilenameAcceptsSafeMarkdownFilename(t *testing.T) {
	tests := []string{
		"notes-2026_06.27.md",
		"陽明山親子公園.md",
		"新北 景點 推薦.md",
		"新北市 特色公園 指南(完整版).md",
		"data.csv",
		"config.toml",
		"index.html",
	}
	for _, name := range tests {
		if err := validateRawUploadFilename(name); err != nil {
			t.Fatalf("validateRawUploadFilename(%q) returned error: %v", name, err)
		}
	}
}

func TestValidateRawUploadFilenameRejectsUnsafeNames(t *testing.T) {
	tests := []string{
		"",
		"notes.exe",
		"notes.MD",
		"../notes.md",
		".md",
		strings.Repeat("a", 510) + ".md",
	}

	for _, filename := range tests {
		t.Run(filename, func(t *testing.T) {
			if err := validateRawUploadFilename(filename); err == nil {
				t.Fatal("validateRawUploadFilename returned nil error")
			}
		})
	}
}

func TestReadRawUploadBodyReturnsBytesSizeAndSHA256(t *testing.T) {
	data, size, digest, err := readRawUploadBody(strings.NewReader("# Hello\n"))
	if err != nil {
		t.Fatalf("readRawUploadBody returned error: %v", err)
	}

	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("# Hello\n")))
	if string(data) != "# Hello\n" || size != int64(len(data)) || digest != wantDigest {
		t.Fatalf("data=%q size=%d digest=%q, want data %q size %d digest %q", data, size, digest, "# Hello\n", len(data), wantDigest)
	}
}

func TestReadRawUploadBodyRejectsEmptyAndOversizeFiles(t *testing.T) {
	if _, _, _, err := readRawUploadBody(strings.NewReader("")); err != errRawUploadEmptyFile {
		t.Fatalf("empty error = %v, want errRawUploadEmptyFile", err)
	}

	oversize := strings.NewReader(strings.Repeat("a", maxRawUploadSize+1))
	if _, _, _, err := readRawUploadBody(oversize); err != errRawUploadTooLarge {
		t.Fatalf("oversize error = %v, want errRawUploadTooLarge", err)
	}
}

func TestRawUploadResponseUsesProjectScopedPath(t *testing.T) {
	resp := newRawUploadResponse("user-1", "project-1", "note.md", 12, "abc123", rawUploadStatusCreated)

	if resp.Filename != "note.md" {
		t.Fatalf("filename = %q, want note.md", resp.Filename)
	}
	if resp.Path != "users/user-1/projects/project-1/raw/note.md" {
		t.Fatalf("path = %q", resp.Path)
	}
	if resp.Bytes != 12 || resp.SHA256 != "abc123" {
		t.Fatalf("bytes=%d sha256=%q, want bytes=12 sha256=abc123", resp.Bytes, resp.SHA256)
	}
	if resp.Status != rawUploadStatusCreated {
		t.Fatalf("status = %q, want %q", resp.Status, rawUploadStatusCreated)
	}
}

type fakeRawDigestStore struct {
	meta    map[string]string
	files   map[string][]byte
	metaErr error
	readErr error
}

func (f *fakeRawDigestStore) Prefix() string { return "users/u/projects/p" }
func (f *fakeRawDigestStore) ReadFile(_ context.Context, relPath string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	data, ok := f.files[relPath]
	if !ok {
		return nil, storage.ErrObjectNotExist
	}
	return data, nil
}
func (f *fakeRawDigestStore) WriteBytes(context.Context, []byte, string) (string, error) {
	return "", errors.New("not implemented")
}
func (f *fakeRawDigestStore) WriteBytesAtomic(context.Context, []byte, string, string) (string, error) {
	return "", errors.New("not implemented")
}
func (f *fakeRawDigestStore) ListProjects(context.Context, string) ([]store.Project, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeRawDigestStore) ListConcepts(context.Context, bool) ([]store.WikiPage, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeRawDigestStore) ListSources(context.Context) ([]store.WikiPage, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeRawDigestStore) ListConceptsFromCache(context.Context) ([]store.WikiPage, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeRawDigestStore) ListSourcesFromCache(context.Context) ([]store.WikiPage, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeRawDigestStore) GetPage(context.Context, string, string) (*store.WikiPage, []byte, error) {
	return nil, nil, errors.New("not implemented")
}
func (f *fakeRawDigestStore) ListMarkdownFiles(context.Context, string) ([]store.MarkdownFile, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeRawDigestStore) ListRawFiles(context.Context) ([]store.RawFile, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeRawDigestStore) BucketStats(context.Context) (int64, int64, error) {
	return 0, 0, errors.New("not implemented")
}
func (f *fakeRawDigestStore) GetMetaSHA256(_ context.Context, relPath string) (string, error) {
	if f.metaErr != nil {
		return "", f.metaErr
	}
	if f.meta == nil {
		return "", nil
	}
	return f.meta[relPath], nil
}

func TestResolveExistingRawDigestUsesMetadata(t *testing.T) {
	s := &fakeRawDigestStore{meta: map[string]string{"raw/note.md": "abc"}}
	digest, exists, err := resolveExistingRawDigest(context.Background(), s, "raw/note.md")
	if err != nil || !exists || digest != "abc" {
		t.Fatalf("got digest=%q exists=%v err=%v", digest, exists, err)
	}
}

func TestResolveExistingRawDigestFallsBackToRead(t *testing.T) {
	content := []byte("# hello\n")
	want := fmt.Sprintf("%x", sha256.Sum256(content))
	s := &fakeRawDigestStore{
		meta:  map[string]string{},
		files: map[string][]byte{"raw/note.md": content},
	}
	digest, exists, err := resolveExistingRawDigest(context.Background(), s, "raw/note.md")
	if err != nil || !exists || digest != want {
		t.Fatalf("got digest=%q exists=%v err=%v want %q", digest, exists, err, want)
	}
}

func TestResolveExistingRawDigestMissingFile(t *testing.T) {
	s := &fakeRawDigestStore{meta: map[string]string{}, files: map[string][]byte{}}
	digest, exists, err := resolveExistingRawDigest(context.Background(), s, "raw/note.md")
	if err != nil || exists || digest != "" {
		t.Fatalf("got digest=%q exists=%v err=%v", digest, exists, err)
	}
}
