package storage

import "testing"

func TestProjectPrefix(t *testing.T) {
	got := ProjectPrefix("alice", "demo")
	want := "users/alice/projects/demo"
	if got != want {
		t.Fatalf("ProjectPrefix = %q, want %q", got, want)
	}
}

func TestProjectPrefixWithSlash(t *testing.T) {
	got := ProjectPrefixWithSlash("alice", "demo")
	want := "users/alice/projects/demo/"
	if got != want {
		t.Fatalf("ProjectPrefixWithSlash = %q, want %q", got, want)
	}
}

func TestUserProjectsPrefix(t *testing.T) {
	got := UserProjectsPrefix("alice")
	want := "users/alice/projects/"
	if got != want {
		t.Fatalf("UserProjectsPrefix = %q, want %q", got, want)
	}
}

func TestProjectObjectPath(t *testing.T) {
	got := ProjectObjectPath("alice", "demo", "raw/article.md")
	want := "users/alice/projects/demo/raw/article.md"
	if got != want {
		t.Fatalf("ProjectObjectPath = %q, want %q", got, want)
	}
}

func TestSafeRawPath(t *testing.T) {
	for _, raw := range []string{"raw/a..b.md", "raw/nested/file.md"} {
		if !SafeRawPath(raw) {
			t.Fatalf("safe raw path rejected: %q", raw)
		}
	}
	for _, raw := range []string{"raw/", "raw/../secret", "raw/a/../../secret", "/raw/a.md", `raw\\a.md`, "raw//a.md"} {
		if SafeRawPath(raw) {
			t.Fatalf("unsafe raw path accepted: %q", raw)
		}
	}
}
