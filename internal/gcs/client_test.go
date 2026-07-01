package gcs

import "testing"

func TestNewScopedClientUsesRequestedPrefixWithoutChangingDefault(t *testing.T) {
	defaultClient := &Client{userID: "default-user", projectID: "default-project"}

	scopedClient := defaultClient.NewScopedClient("request-user", "request-project")

	if got := scopedClient.Prefix(); got != "users/request-user/projects/request-project" {
		t.Fatalf("scoped prefix = %q, want %q", got, "users/request-user/projects/request-project")
	}
	if got := defaultClient.Prefix(); got != "users/default-user/projects/default-project" {
		t.Fatalf("default prefix = %q, want %q", got, "users/default-user/projects/default-project")
	}
	if scopedClient.bucket != defaultClient.bucket {
		t.Fatal("scoped client does not share the default client's bucket handle")
	}
}

func TestApplyWikiPageFrontmatterIncludesID(t *testing.T) {
	page := WikiPage{
		Slug:  "alpha",
		Title: "alpha",
		Path:  "wiki/alpha.md",
	}
	data := []byte("---\nid: a3f7b2c01d9d\ntitle: Alpha Concept\n---\nBody")

	got, err := applyWikiPageFrontmatter(page, data)
	if err != nil {
		t.Fatalf("apply frontmatter: %v", err)
	}

	if got.ID != "a3f7b2c01d9d" {
		t.Fatalf("id = %q, want %q", got.ID, "a3f7b2c01d9d")
	}
	if got.Title != "Alpha Concept" {
		t.Fatalf("title = %q, want %q", got.Title, "Alpha Concept")
	}
	if got.Slug != page.Slug || got.Path != page.Path {
		t.Fatalf("page metadata changed unexpectedly: %#v", got)
	}
}
