package gcs

import "testing"

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
