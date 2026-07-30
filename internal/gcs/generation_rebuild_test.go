package gcs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/generation"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestGenerationRebuildStagesDerivedArtifactsAndCASAdvancesPointer(t *testing.T) {
	client, backend := newMemoryClient()
	files := map[string]backendObject{}
	for path, data := range map[string][]byte{
		"wiki/alpha.md":                []byte("---\nid: old\n---\nalpha"),
		"synto.toml":                   []byte("[pipeline]\n"),
		"cache/id_map.json":            []byte(`{"concept":{"old":"alpha"}}`),
		"cache/concepts.jsonl":         []byte(`{"slug":"alpha"}`),
		"cache/dormant_concepts.jsonl": []byte{},
		"cache/raw_status.json":        []byte(`{}`),
		"cache/suggested_queries.json": []byte(`{}`),
		".synto/state.db":              []byte("sqlite"),
		".synto/INDEX.json":            []byte(`{"schema_version":1}`),
	} {
		files[path] = backendObject{Data: data, Generation: int64(len(files) + 10)}
	}
	seedManifest(t, backend, "old-generation", files)
	oldManifest, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}

	planner := func(_ context.Context, workspace string) (store.GenerationRebuildPlan, error) {
		if err := os.WriteFile(filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"entity-alpha":"alpha"}}`), 0o600); err != nil {
			return store.GenerationRebuildPlan{}, err
		}
		return store.GenerationRebuildPlan{ConceptCount: 1, SourceCount: 0}, nil
	}
	result, err := client.RebuildIndexGeneration(context.Background(), planner)
	if err != nil {
		t.Fatalf("RebuildIndexGeneration: %v", err)
	}
	if result.OldGeneration != "old-generation" || result.NewGeneration == "old-generation" || result.ConceptCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	current, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil || string(current.Data) == string(oldManifest.Data) {
		t.Fatalf("current manifest did not advance: err=%v", err)
	}
	oldMap, err := backend.Read(context.Background(), projectObject(generation.Prefix+"old-generation/cache/id_map.json"), 0, generation.MaxFileBytes)
	if err != nil || string(oldMap.Data) != `{"concept":{"old":"alpha"}}` {
		t.Fatalf("old generation changed: %q err=%v", oldMap.Data, err)
	}
	newMap, err := backend.Read(context.Background(), projectObject(generation.Prefix+result.NewGeneration+"/cache/id_map.json"), 0, generation.MaxFileBytes)
	if err != nil || string(newMap.Data) != `{"concept":{"entity-alpha":"alpha"}}` {
		t.Fatalf("new derived map=%q err=%v", newMap.Data, err)
	}
}
