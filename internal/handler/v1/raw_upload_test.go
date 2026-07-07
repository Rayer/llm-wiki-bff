package v1

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

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
	resp := newRawUploadResponse("user-1", "project-1", "note.md", 12, "abc123")

	if resp.Filename != "note.md" {
		t.Fatalf("filename = %q, want note.md", resp.Filename)
	}
	if resp.Path != "users/user-1/projects/project-1/raw/note.md" {
		t.Fatalf("path = %q", resp.Path)
	}
	if resp.Bytes != 12 || resp.SHA256 != "abc123" {
		t.Fatalf("bytes=%d sha256=%q, want bytes=12 sha256=abc123", resp.Bytes, resp.SHA256)
	}
}
