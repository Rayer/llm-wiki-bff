package v1

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

func TestPlanSyntoGenerationIsIdempotentAndPlansOneTimeRedirect(t *testing.T) {
	workspace := t.TempDir()
	for _, rel := range []string{"wiki", "wiki/sources", "cache", ".synto"} {
		if err := os.MkdirAll(filepath.Join(workspace, rel), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeGenerationRebuildTestFile(t, workspace, "wiki/alpha.md", []byte("---\nid: a3f7b2c01d9d\n---\nalpha\n"))
	writeGenerationRebuildTestFile(t, workspace, "cache/id_map.json", []byte(`{"concept":{"a3f7b2c01d9d":"alpha"},"source":{},"redirects":{}}`))
	writeGenerationRebuildTestFile(t, workspace, "cache/concepts.jsonl", []byte(`{"slug":"alpha","frontmatter":{"id":"a3f7b2c01d9d"}}`+"\n"))
	writeGenerationRebuildTestFile(t, workspace, ".synto/INDEX.json", []byte(`{"schema_version":1,"pack":{},"articles":[{"id":"generated","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC","name":"alpha","path":"wiki/alpha.md","summary":null,"tags":[],"aliases":[],"confidence":"high"}],"terms":[],"papers":[],"sources":[],"source_concepts":[{"source_path":"raw/source.md","content_hash":"0000000000000000000000000000000000000000000000000000000000000000","concepts":[{"name":"alpha","entity_id":"01JAZ5N7Y3K8M2Q4R6T9VWXABC"}]}],"synthesis":[],"stats":{}}`))

	first, err := planSyntoGeneration(context.Background(), workspace)
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	if first.MigratedOldIDs != 1 || first.RedirectCount != 1 {
		t.Fatalf("first plan=%+v, want one migration and redirect", first)
	}
	firstMap := readGenerationRebuildTestMap(t, workspace)
	if firstMap.Concept["01JAZ5N7Y3K8M2Q4R6T9VWXABC"] != "alpha" || firstMap.IDRedirects["a3f7b2c01d9d"] != "01JAZ5N7Y3K8M2Q4R6T9VWXABC" {
		t.Fatalf("first map=%+v", firstMap)
	}

	second, err := planSyntoGeneration(context.Background(), workspace)
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if second.MigratedOldIDs != 0 || second.RedirectCount != 1 {
		t.Fatalf("second plan=%+v, want zero migration and one stable redirect", second)
	}
	secondMap := readGenerationRebuildTestMap(t, workspace)
	firstJSON, _ := json.Marshal(firstMap)
	secondJSON, _ := json.Marshal(secondMap)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("second semantic map changed:\nfirst=%s\nsecond=%s", firstJSON, secondJSON)
	}
}

func writeGenerationRebuildTestFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readGenerationRebuildTestMap(t *testing.T, root string) wikiindex.IDMap {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "cache", "id_map.json"))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}
