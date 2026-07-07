package main

import "testing"

func TestAddIDToFrontmatterInsertsAfterOpeningMarker(t *testing.T) {
	input := []byte("---\ntitle: Example\n---\n# Example\n")

	got, changed, err := addIDToFrontmatter(input, "abc123def456")
	if err != nil {
		t.Fatalf("add id: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}

	want := "---\nid: abc123def456\ntitle: Example\n---\n# Example\n"
	if string(got) != want {
		t.Fatalf("content = %q, want %q", string(got), want)
	}
}

func TestAddIDToFrontmatterSkipsExistingID(t *testing.T) {
	input := []byte("---\ntitle: Example\nid: existing\n---\n# Example\n")

	got, changed, err := addIDToFrontmatter(input, "abc123def456")
	if err != nil {
		t.Fatalf("add id: %v", err)
	}
	if changed {
		t.Fatal("changed = true, want false")
	}
	if string(got) != string(input) {
		t.Fatalf("content changed: %q", string(got))
	}
}

func TestAddIDToFrontmatterRejectsMissingFrontmatter(t *testing.T) {
	_, changed, err := addIDToFrontmatter([]byte("# Example\n"), "abc123def456")
	if err == nil {
		t.Fatal("err = nil, want error")
	}
	if changed {
		t.Fatal("changed = true, want false")
	}
}
