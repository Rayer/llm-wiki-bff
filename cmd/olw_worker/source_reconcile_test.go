package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/sourcestatus"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

func TestReconcileSourceIDMapPreservesIDsAddsNewAndRemovesMissing(t *testing.T) {
	prior := []sourceSnapshot{{SourceID: "stable-a", RawPath: "raw/a.md"}}
	data := []byte(`{"source":{"transient-a":"a","new-id":"new"},"source_meta":{"transient-a":{"slug":"a","source_file":"raw/a.md"},"new-id":{"slug":"new","source_file":"raw/new.md"}},"redirects":{"transient-a":["new-id"]}}`)
	out, sources, err := reconcileSourceIDMap(data, prior)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"stable-a": "a"`) || strings.Contains(string(out), `"transient-a"`) {
		t.Fatalf("reconciled source ID map = %s", out)
	}
	if !strings.Contains(string(out), `"new-id": "new"`) || !strings.Contains(string(out), `"stable-a": [`) {
		t.Fatalf("new source or redirect missing = %s", out)
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %#v", sources)
	}
}

func TestReconcileSourceIDMapRejectsDuplicateRawPathAndIDCollision(t *testing.T) {
	tests := []struct {
		name   string
		prior  []sourceSnapshot
		idMap  string
		needle string
	}{
		{"duplicate raw path", nil, `{"source":{"a":"a","b":"b"},"source_meta":{"a":{"slug":"a","source_file":"raw/same.md"},"b":{"slug":"b","source_file":"raw/same.md"}}}`, "duplicate generated source mapping"},
		{"stable ID collision", []sourceSnapshot{{SourceID: "stable-a", RawPath: "raw/a.md"}}, `{"source":{"transient-a":"a","stable-a":"b"},"source_meta":{"transient-a":{"slug":"a","source_file":"raw/a.md"},"stable-a":{"slug":"b","source_file":"raw/b.md"}}}`, "reserved"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := reconcileSourceIDMap([]byte(test.idMap), test.prior); err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("error = %v, want %q", err, test.needle)
			}
		})
	}
}

func TestReconcileSourceIDMapRedirectNormalizationIsDeterministic(t *testing.T) {
	prior := []sourceSnapshot{{SourceID: "stable-a", RawPath: "raw/a.md"}}
	data := []byte(`{"source":{"transient-a":"a"},"source_meta":{"transient-a":{"slug":"a","source_file":"raw/a.md"}},"redirects":{"transient-a":["x"],"stable-a":["y"]}}`)
	for i := 0; i < 20; i++ {
		if _, _, err := reconcileSourceIDMap(data, prior); err == nil || !strings.Contains(err.Error(), "redirect ID collision") {
			t.Fatalf("run %d error=%v, want deterministic redirect collision", i, err)
		}
	}

}

func TestReconcileSourceIDMapPreservesRedirectSlugTargets(t *testing.T) {
	data := []byte(`{"source":{"transient-a":"source","transient-b":"transient-a"},"source_meta":{"transient-a":{"slug":"source","source_file":"raw/source.md"},"transient-b":{"slug":"transient-a","source_file":"raw/other.md"}},"redirects":{"transient-a":["transient-a"]}}`)
	out, _, err := reconcileSourceIDMap(data, []sourceSnapshot{{SourceID: "stable-a", RawPath: "raw/source.md"}})
	if err != nil {
		t.Fatal(err)
	}
	var got wikiindex.IDMap
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if targets := got.Redirects["stable-a"]; len(targets) != 1 || targets[0] != "transient-a" {
		t.Fatalf("redirect targets=%#v, want exact slug target", targets)
	}
}

func TestDecodeIDMapRejectsDuplicateJSONKeys(t *testing.T) {
	for _, data := range []string{
		`{"source":{"a":"one","a":"two"}}`,
		`{"source_meta":{"a":{"source_file":"raw/a.md"},"a":{"source_file":"raw/b.md"}}}`,
		`{"redirects":{"a":["one"],"a":["two"]}}`,
	} {
		if _, err := wikiindex.DecodeIDMap([]byte(data)); err == nil {
			t.Fatalf("DecodeIDMap(%s) accepted duplicate key", data)
		}
	}
}

func TestDecodeRejectsNestedDuplicateJSONFields(t *testing.T) {
	for _, data := range []string{
		`{"source":{"a":"a"},"source_meta":{"a":{"slug":"a","slug":"b","source_file":"raw/a.md"}}}`,
		`{"source":{"a":"a"},"source_meta":{"a":{"slug":"a","source_file":"raw/a.md","source_file":"raw/b.md"}}}`,
	} {
		if _, err := wikiindex.DecodeIDMap([]byte(data)); err == nil {
			t.Fatalf("DecodeIDMap(%s) accepted nested duplicate field", data)
		}
	}
	if _, err := sourcestatus.Decode([]byte(`{"version":1,"sources":{"a":{"raw_path":"raw/a.md","raw_path":"raw/b.md"}}}`)); err == nil {
		t.Fatal("sourcestatus.Decode accepted duplicate receipt field")
	}
}

func TestRewriteSourcePageIDRejectsDuplicateFrontmatterIDs(t *testing.T) {
	data := []byte("---\nid: old-a\nid: old-b\nsource_file: raw/a.md\n---\nbody\n")
	if _, err := rewriteSourcePageID(data, "stable-a", "raw/a.md"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error=%v, want duplicate frontmatter id rejection", err)
	}
}

func TestRewriteSourcePageIDOnlyRewritesAuthoritativeTopLevelID(t *testing.T) {
	data := []byte("---\r\nid: transient\r\nnested:\r\n  id: transient\r\nblock: |\r\n  id: transient\r\n  prose transient\r\nsource_file: raw/a.md\r\n---\r\nbody id: transient\r\n")
	got, err := rewriteSourcePageID(data, "stable", "raw/a.md")
	if err != nil {
		t.Fatal(err)
	}
	want := bytesReplaceOnce(data, []byte("id: transient\r\n"), []byte("id: stable\r\n"))
	if string(got) != string(want) {
		t.Fatalf("rewritten page=%q, want only top-level id change=%q", got, want)
	}
}

func TestReconcileWorkspaceRewritesAuthoritativeSourceReferencesOnly(t *testing.T) {
	workspace := t.TempDir()
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"source":{"transient-a":"a"},"source_meta":{"transient-a":{"slug":"a","source_file":"raw/a.md"}}}`))
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), []byte(`{"slug":"concept","body":"prose transient-a","frontmatter":{"sources":["transient-a"],"source":"transient-a"},"sources":["transient-a"]}`+"\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "concept.md"), []byte("---\nid: concept\nsources:\n  - transient-a\nsource: transient-a\n---\nprose transient-a\n"))
	mustWriteFile(t, filepath.Join(workspace, "wiki", "sources", "a.md"), []byte("---\nid: transient-a\nsource_file: raw/a.md\n---\nsource prose transient-a\n"))
	if err := reconcileWorkspaceSources(workspace, []sourceSnapshot{{SourceID: "stable-a", RawPath: "raw/a.md"}}); err != nil {
		t.Fatal(err)
	}
	cache, err := os.ReadFile(filepath.Join(workspace, "cache", "concepts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytesTrimSpace(cache), &entry); err != nil {
		t.Fatal(err)
	}
	if got := entry["sources"].([]any)[0]; got != "stable-a" {
		t.Fatalf("top-level sources=%v", got)
	}
	frontmatter := entry["frontmatter"].(map[string]any)
	if frontmatter["source"] != "stable-a" || frontmatter["sources"].([]any)[0] != "stable-a" {
		t.Fatalf("cache frontmatter=%v", frontmatter)
	}
	concept, _ := os.ReadFile(filepath.Join(workspace, "wiki", "concept.md"))
	if !strings.Contains(string(concept), "sources:\n- stable-a\n") || !strings.Contains(string(concept), "source: stable-a") || strings.Count(string(concept), "prose transient-a") != 1 {
		t.Fatalf("concept page=%s", concept)
	}
}

func TestReconcileWorkspaceRejectsRemovedIDReuse(t *testing.T) {
	data := []byte(`{"source":{"removed":"new"},"source_meta":{"removed":{"slug":"new","source_file":"raw/new.md"}}}`)
	if _, _, err := reconcileSourceIDMap(data, []sourceSnapshot{{SourceID: "removed", RawPath: "raw/old.md"}}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("error=%v, want removed ID reservation rejection", err)
	}
}

func TestAnnotationTrailerRequiresOwnedTerminalDigest(t *testing.T) {
	valid := "body\n\n---\n\n## Human annotations (system)\n<!-- lwc-ann-v1 source_id=stable ann_sha256=" + annotation.Digest("note") + " -->\nnote\n"
	if got, err := stripSystemAnnotationTrailer([]byte(valid), "stable"); err != nil || string(got) != "body" {
		t.Fatalf("valid trailer strip=%q err=%v", got, err)
	}
	crlf := strings.ReplaceAll(valid, "\n", "\r\n")
	if got, err := stripSystemAnnotationTrailer([]byte(crlf), "stable"); err != nil || string(got) != "body" {
		t.Fatalf("CRLF trailer strip=%q err=%v", got, err)
	}
	markerLike := "body\n\n---\n\n## Human annotations (system)\n<!-- lwc-ann-v1 source_id=stable ann_sha256=fake -->\nuser text continues\n"
	if _, err := stripSystemAnnotationTrailer([]byte(markerLike), "stable"); err == nil {
		t.Fatal("malformed marker-like trailer was accepted")
	}
	wrongOwner := strings.Replace(valid, "source_id=stable", "source_id=other", 1)
	if _, err := stripSystemAnnotationTrailer([]byte(wrongOwner), "stable"); err == nil {
		t.Fatal("wrong-owner trailer was accepted")
	}
	workspace := t.TempDir()
	path := filepath.Join(workspace, "wiki", "sources", "a.md")
	mustWriteFile(t, path, []byte("---\nid: stable\nsource_file: raw/a.md\n---\n"+markerLike))
	if err := applyStableSourceAnnotations(workspace, []reconciledSource{{StableID: "stable", Slug: "a", RawPath: "raw/a.md"}}, []sourceSnapshot{{SourceID: "stable", RawPath: "raw/a.md"}}); err == nil || !strings.Contains(err.Error(), "annotation trailer") {
		t.Fatalf("malformed terminal trailer error=%v", err)
	}
}

func TestAnnotationTrailerRejectsDuplicateOwnedTrailers(t *testing.T) {
	trailer := "\n\n---\n\n## Human annotations (system)\n<!-- lwc-ann-v1 source_id=stable ann_sha256=" + annotation.Digest("note") + " -->\nnote\n"
	if _, err := stripSystemAnnotationTrailer([]byte("body"+trailer+trailer), "stable"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error=%v, want duplicate trailer rejection", err)
	}
}

func TestClearStableSourceAnnotationRemovesExactlyOneValidTrailer(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "wiki", "sources", "slug.md")
	trailer := "\n\n---\n\n## Human annotations (system)\n<!-- lwc-ann-v1 source_id=stable ann_sha256=" + annotation.Digest("note") + " -->\nnote\n"
	mustWriteFile(t, path, []byte("body"+trailer))
	source := []reconciledSource{{StableID: "stable", Slug: "slug", RawPath: "raw/a.md"}}
	if err := applyStableSourceAnnotations(workspace, source, []sourceSnapshot{{SourceID: "stable", RawPath: "raw/a.md"}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "body" {
		t.Fatalf("cleared page=%q err=%v", got, err)
	}
}

func TestRewriteConceptSourceReferencesRejectsBounds(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "cache", "concepts.jsonl")
	mustWriteFile(t, path, []byte(strings.Repeat(`{"slug":"x"}`+"\n", generation.MaxFiles+1)))
	if err := rewriteConceptSourceReferences(workspace, map[string]string{"old": "new"}); err == nil || !strings.Contains(err.Error(), "logical") {
		t.Fatalf("logical overflow error=%v", err)
	}
	mustWriteFile(t, path, make([]byte, generation.MaxFileBytes+1))
	if err := rewriteConceptSourceReferences(workspace, map[string]string{"old": "new"}); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("byte overflow error=%v", err)
	}
}

func bytesTrimSpace(data []byte) []byte { return []byte(strings.TrimSpace(string(data))) }

func bytesReplaceOnce(data, old, replacement []byte) []byte {
	index := strings.Index(string(data), string(old))
	if index < 0 {
		return data
	}
	out := append([]byte(nil), data[:index]...)
	out = append(out, replacement...)
	return append(out, data[index+len(old):]...)
}

func TestSourceReconcileMalformedConceptsCacheLeavesAllBytesUnchanged(t *testing.T) {
	// Adversarial: duplicate-key concepts cache must fail closed with zero writes
	// (id_map, concepts cache, concept pages, Source pages).
	workspace := t.TempDir()
	idMap := []byte(`{
  "source": {"s-transient": "source"},
  "source_meta": {"s-transient": {"slug": "source", "source_file": "raw/a.md"}},
  "redirects": {}
}`)
	malformedCache := []byte(
		`{"slug":"alpha","slug":"evil","sources":["s-transient"],"frontmatter":{"sources":["s-transient"]}}` + "\n",
	)
	conceptPage := []byte("---\nid: c1\nsources:\n  - s-transient\n---\nbody\n")
	sourcePage := []byte("---\nid: s-transient\nsource_file: raw/a.md\n---\nsource body\n")
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), idMap)
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), malformedCache)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), conceptPage)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "sources", "source.md"), sourcePage)

	before := snapshotRelevantVaultBytes(t, workspace,
		"cache/id_map.json", "cache/concepts.jsonl", "wiki/alpha.md", "wiki/sources/source.md",
	)
	err := reconcileWorkspaceSources(workspace, []sourceSnapshot{{SourceID: "s-stable", RawPath: "raw/a.md"}})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error=%v, want duplicate-key rejection", err)
	}
	assertVaultBytesUnchanged(t, workspace, before)
}

func TestSourceReconcileLateMalformedSourcePageLeavesAllBytesUnchanged(t *testing.T) {
	// Adversarial late failure: Source page with duplicate frontmatter ids after
	// id_map/concepts/concept pages would previously have been written.
	workspace := t.TempDir()
	idMap := []byte(`{
  "source": {"s-transient": "source"},
  "source_meta": {"s-transient": {"slug": "source", "source_file": "raw/a.md"}},
  "redirects": {}
}`)
	cache := []byte(`{"slug":"alpha","sources":["s-transient"],"frontmatter":{"sources":["s-transient"]}}` + "\n")
	conceptPage := []byte("---\nid: c1\nsources:\n  - s-transient\nsource: s-transient\n---\nbody\n")
	// Duplicate top-level id fields — rewriteSourcePageID rejects after earlier stages.
	sourcePage := []byte("---\nid: s-transient\nid: s-other\nsource_file: raw/a.md\n---\nsource body\n")
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), idMap)
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), cache)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), conceptPage)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "sources", "source.md"), sourcePage)

	before := snapshotRelevantVaultBytes(t, workspace,
		"cache/id_map.json", "cache/concepts.jsonl", "wiki/alpha.md", "wiki/sources/source.md",
	)
	err := reconcileWorkspaceSources(workspace, []sourceSnapshot{{SourceID: "s-stable", RawPath: "raw/a.md"}})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error=%v, want late duplicate source frontmatter rejection", err)
	}
	assertVaultBytesUnchanged(t, workspace, before)
}

func TestSourceReconcileLateMalformedAnnotationTrailerLeavesAllBytesUnchanged(t *testing.T) {
	// Adversarial late failure: annotation trailer validation after Source ID rewrite.
	workspace := t.TempDir()
	idMap := []byte(`{
  "source": {"s-transient": "source"},
  "source_meta": {"s-transient": {"slug": "source", "source_file": "raw/a.md"}},
  "redirects": {}
}`)
	cache := []byte(`{"slug":"alpha","sources":["s-transient"],"frontmatter":{"sources":["s-transient"]}}` + "\n")
	conceptPage := []byte("---\nid: c1\nsources:\n  - s-transient\n---\nbody\n")
	// Valid frontmatter/id but malformed terminal annotation trailer.
	sourcePage := []byte("---\nid: s-transient\nsource_file: raw/a.md\n---\nbody\n\n---\n\n## Human annotations (system)\n<!-- lwc-ann-v1 source_id=s-transient ann_sha256=fake -->\nuser text continues\n")
	mustWriteFile(t, filepath.Join(workspace, "cache", "id_map.json"), idMap)
	mustWriteFile(t, filepath.Join(workspace, "cache", "concepts.jsonl"), cache)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "alpha.md"), conceptPage)
	mustWriteFile(t, filepath.Join(workspace, "wiki", "sources", "source.md"), sourcePage)

	before := snapshotRelevantVaultBytes(t, workspace,
		"cache/id_map.json", "cache/concepts.jsonl", "wiki/alpha.md", "wiki/sources/source.md",
	)
	err := reconcileWorkspaceSources(workspace, []sourceSnapshot{
		{SourceID: "s-stable", RawPath: "raw/a.md", AnnotationBody: "note", AnnotationSHA: annotation.Digest("note")},
	})
	if err == nil || !strings.Contains(err.Error(), "annotation trailer") {
		t.Fatalf("error=%v, want late malformed annotation trailer rejection", err)
	}
	assertVaultBytesUnchanged(t, workspace, before)
}

func TestApplyStableSourceAnnotationsIsExactlyOnceAndIdempotent(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "wiki", "sources", "slug.md")
	mustWriteFile(t, path, []byte("---\nid: stable\nsource_file: raw/a.md\n---\nbody\n\n---\n\n## Human annotations (system)\n<!-- lwc-ann-v1 source_id=stable ann_sha256="+annotation.Digest("old")+" -->\nold\n"))
	sources := []reconciledSource{{StableID: "stable", Slug: "slug", RawPath: "raw/a.md"}}
	prior := []sourceSnapshot{{SourceID: "stable", RawPath: "raw/a.md", AnnotationBody: "note", AnnotationSHA: annotation.Digest("note")}}
	if err := applyStableSourceAnnotations(workspace, sources, prior); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(want), "<!-- lwc-ann-v1 source_id=stable ") != 1 || strings.Contains(string(want), "source_id=transient") {
		t.Fatalf("annotation trailer = %s", want)
	}
	if err := applyStableSourceAnnotations(workspace, sources, prior); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != string(want) {
		t.Fatalf("rerun changed generated source:\nfirst=%s\nagain=%s", want, again)
	}
}
