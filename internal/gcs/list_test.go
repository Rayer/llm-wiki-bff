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
