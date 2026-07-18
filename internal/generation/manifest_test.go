package generation

import (
	"strings"
	"testing"
)

func TestDecodeRejectsUnsafeOrInvalidFileTables(t *testing.T) {
	valid := `{"version":1,"generation_id":"g_abc123","created_at":"2026-07-18T00:00:00Z","input_fingerprint":"x","files":[{"path":"wiki/a.md","size":1,"sha256":"` + strings.Repeat("a", 64) + `","generation":7}]}`
	if _, err := Decode([]byte(valid)); err != nil {
		t.Fatalf("Decode(valid): %v", err)
	}
	for _, replace := range []struct{ old, new string }{
		{"wiki/a.md", "raw/a.md"},
		{"wiki/a.md", "wiki/../secret"},
		{"wiki/a.md", "wiki//a.md"},
		{"\"generation\":7", "\"generation\":0"},
		{"\"size\":1", "\"size\":-1"},
		{"\"g_abc123\"", "\"../unsafe\""},
	} {
		if _, err := Decode([]byte(strings.Replace(valid, replace.old, replace.new, 1))); err == nil {
			t.Fatalf("Decode accepted %q -> %q", replace.old, replace.new)
		}
	}

	duplicate := strings.Replace(valid, `]}`, `,{"path":"wiki/a.md","size":1,"sha256":"`+strings.Repeat("a", 64)+`","generation":8}]}`, 1)
	if _, err := Decode([]byte(duplicate)); err == nil {
		t.Fatal("Decode accepted duplicate path")
	}
}

func TestGenerationOwnedAndCanonicalPaths(t *testing.T) {
	for _, path := range []string{"wiki/a.md", "wiki/.drafts/a.md", "wiki.toml", "cache/id_map.json", "cache/concepts.jsonl", "cache/raw_status.json", "cache/suggested_queries.json", ".olw/state.db"} {
		if !GenerationOwned(path) {
			t.Errorf("GenerationOwned(%q) = false", path)
		}
	}
	for _, path := range []string{"raw/a.md", "cache/annotations/a.json", "cache/source_status.json", "cache/pipeline-run.log", "wiki", "meta/index.md"} {
		if GenerationOwned(path) {
			t.Errorf("GenerationOwned(%q) = true", path)
		}
	}
}

func TestDecodeRejectsOversizedEncodingBeforeDecode(t *testing.T) {
	data := make([]byte, MaxManifestBytes+1)
	if _, err := Decode(data); err == nil || err.Error() != "generation manifest exceeds limit" {
		t.Fatalf("Decode oversized manifest error = %v", err)
	}
}
