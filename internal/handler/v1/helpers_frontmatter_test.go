package v1

import (
	"testing"
)

func TestParseFrontmatterKeepsSourceFile(t *testing.T) {
	md := "---\nid: abc\ntitle: Demo\nsource_file: raw/demo-note.md\nstatus: published\n---\n\n# Body\n"
	fm, body := parseFrontmatter(md)
	got, _ := fm["source_file"].(string)
	if got != "raw/demo-note.md" {
		t.Fatalf("source_file = %#v, want raw/demo-note.md; fm=%#v", fm["source_file"], fm)
	}
	if body == "" || body == md {
		t.Fatalf("body not stripped of frontmatter: %q", body)
	}
}

func TestParseFrontmatterKeepsSourcesArray(t *testing.T) {
	md := "---\ntitle: Demo\nsources:\n  - raw/a.md\n  - raw/b.md\n---\nBody\n"
	fm, _ := parseFrontmatter(md)
	sources, ok := fm["sources"].([]interface{})
	if !ok || len(sources) != 2 {
		t.Fatalf("sources = %#v, want 2-item slice", fm["sources"])
	}
	if sources[0] != "raw/a.md" {
		t.Fatalf("sources[0] = %#v", sources[0])
	}
}
