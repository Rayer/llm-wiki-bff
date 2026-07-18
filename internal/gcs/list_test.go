package gcs

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/generation"
)

func TestObjectRelativePathTrimsProjectPrefix(t *testing.T) {
	client := &Client{userID: "u1", projectID: "p1"}

	got, ok := client.objectRelativePath("users/u1/projects/p1/wiki/page.md", "")

	if !ok {
		t.Fatal("objectRelativePath returned ok=false")
	}
	if got != "wiki/page.md" {
		t.Fatalf("relative path = %q, want %q", got, "wiki/page.md")
	}
}

func TestObjectRelativePathKeepsRequestedSubPrefix(t *testing.T) {
	client := &Client{userID: "u1", projectID: "p1"}

	got, ok := client.objectRelativePath("users/u1/projects/p1/wiki/page.md", "wiki")

	if !ok {
		t.Fatal("objectRelativePath returned ok=false")
	}
	if got != "wiki/page.md" {
		t.Fatalf("relative path = %q, want %q", got, "wiki/page.md")
	}
}

func TestObjectRelativePathRejectsProjectDirectoryMarker(t *testing.T) {
	client := &Client{userID: "u1", projectID: "p1"}

	if got, ok := client.objectRelativePath("users/u1/projects/p1/", ""); ok {
		t.Fatalf("objectRelativePath = %q, true; want false", got)
	}
}

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

func TestListObjectMetaEnforcesCountAndByteBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		bytes int64
	}{
		{name: "exact count", count: generation.MaxFiles, bytes: generation.MaxFiles},
		{name: "count plus one", count: generation.MaxFiles + 1, bytes: generation.MaxFiles + 1},
		{name: "exact bytes", count: 2, bytes: generation.MaxTotalSize},
		{name: "bytes plus one", count: 2, bytes: generation.MaxTotalSize + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, backend := newMemoryClient()
			for i := 0; i < tc.count; i++ {
				backend.put(projectObject(fmt.Sprintf("raw/%05d.md", i)), nil, int64(i+1), nil)
			}
			backend.mu.Lock()
			for i := 0; i < tc.count; i++ {
				name := projectObject(fmt.Sprintf("raw/%05d.md", i))
				object := backend.objects[name]
				object.Size = 1
				backend.objects[name] = object
			}
			first := backend.objects[projectObject("raw/00000.md")]
			first.Size = tc.bytes - int64(tc.count-1)
			backend.objects[first.Name] = first
			backend.mu.Unlock()
			_, err := client.ListObjectMeta(context.Background(), "raw/")
			if tc.name == "exact count" || tc.name == "exact bytes" {
				if err != nil {
					t.Fatalf("exact boundary error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "object list exceeds limit") {
				t.Fatalf("bounded listing error = %v", err)
			}
		})
	}
}

func TestListObjectMetaRejectsNegativeAndHugeSizes(t *testing.T) {
	for _, size := range []int64{-1, generation.MaxTotalSize + 1} {
		client, backend := newMemoryClient()
		name := projectObject("raw/bad.md")
		backend.put(name, nil, 1, nil)
		backend.mu.Lock()
		object := backend.objects[name]
		object.Size = size
		backend.objects[name] = object
		backend.mu.Unlock()
		if _, err := client.ListObjectMeta(context.Background(), "raw/"); err == nil || !strings.Contains(err.Error(), "object list exceeds limit") {
			t.Fatalf("size %d error = %v", size, err)
		}
	}
}
