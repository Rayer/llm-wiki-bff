package gcs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rayer/llm-wiki-bff/internal/generation"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

func TestQueryGenerationIdentityUsesPinnedManifestWithoutReread(t *testing.T) {
	client, backend := newMemoryClient()
	concepts := []byte(`{"slug":"one"}` + "\n")
	seedManifest(t, backend, "generation-one", map[string]backendObject{"cache/concepts.jsonl": {Data: concepts, Generation: 101}})
	pinnedStore, err := client.Pin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backend.put(projectObject(generation.ManifestPath), manifestBytes(t, "generation-two", map[string]backendObject{"cache/concepts.jsonl": {Data: []byte(`{"slug":"two"}`), Generation: 201}}), 8, nil)
	identity, err := pinnedStore.(*Client).QueryGenerationIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if identity.ProjectID != "project" || identity.GenerationID != "generation-one" || identity.ConceptsDigest != "sha256:"+digest(concepts) {
		t.Fatalf("identity=%+v", identity)
	}
	_, reads := backend.snapshots()
	if reads != 1 {
		t.Fatalf("manifest reads=%d, want 1", reads)
	}
}

func TestQueryGenerationIdentityFailsUnpinnedAndMissingOrInvalidConceptRow(t *testing.T) {
	client, backend := newMemoryClient()
	if _, err := client.QueryGenerationIdentity(context.Background()); !errors.Is(err, store.ErrQueryGenerationUnpinned) {
		t.Fatalf("unpinned err=%v", err)
	}
	seedManifest(t, backend, "generation-one", map[string]backendObject{"cache/id_map.json": {Data: []byte(`{}`), Generation: 101}})
	pinnedStore, err := client.Pin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pinnedStore.(*Client).QueryGenerationIdentity(context.Background()); !errors.Is(err, store.ErrQueryGenerationIdentityUnavailable) {
		t.Fatalf("missing row err=%v", err)
	}
	invalid := &Client{
		projectID: "project",
		view: &generationView{manifest: &generation.Manifest{
			GenerationID: "generation-one",
			Files:        []generation.File{{Path: conceptsCachePath, SHA256: strings.Repeat("z", 64)}},
		}},
	}
	if _, err := invalid.QueryGenerationIdentity(context.Background()); !errors.Is(err, store.ErrQueryGenerationIdentityUnavailable) {
		t.Fatalf("invalid digest err=%v", err)
	}
}
