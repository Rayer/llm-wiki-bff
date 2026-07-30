package gcs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestGenerationRebuildReadsBackImmutableUploadAtReturnedGeneration(t *testing.T) {
	client, backend := newMemoryClient()
	seedManifest(t, backend, "old-generation", generationRebuildTestFiles())
	oldCurrent, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	backend.corruptGeneratedUploads = true

	_, err = client.RebuildIndexGeneration(context.Background(), func(_ context.Context, _ string) (store.GenerationRebuildPlan, error) {
		return store.GenerationRebuildPlan{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "manifest_stage") {
		t.Fatalf("corrupt upload error = %v, want manifest_stage", err)
	}
	current, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current.Data, oldCurrent.Data) {
		t.Fatal("corrupt immutable upload advanced or changed current pointer")
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	var uploaded backendObject
	for name, object := range backend.objects {
		if strings.Contains(name, "/"+generation.Prefix) {
			uploaded = object
			break
		}
	}
	if uploaded.Name == "" {
		t.Fatal("no immutable upload recorded")
	}
	foundReadback := false
	for i, request := range backend.requests {
		if request.Name == uploaded.Name {
			foundReadback = true
			if request.Generation != uploaded.Generation || backend.requestedLimits[i] != uploaded.Size {
				t.Fatalf("immutable readback request=%+v limit=%d, want generation=%d limit=%d", request, backend.requestedLimits[i], uploaded.Generation, uploaded.Size)
			}
		}
	}
	if !foundReadback {
		t.Fatalf("no exact-generation readback for %q", uploaded.Name)
	}
}

func TestGenerationRebuildPointerWritePreservesGenerationCAS(t *testing.T) {
	client, backend := newMemoryClient()
	seedManifest(t, backend, "old-generation", generationRebuildTestFiles())
	oldCurrent, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	backend.interfereCurrentWrite = true

	_, err = client.RebuildIndexGeneration(context.Background(), func(_ context.Context, _ string) (store.GenerationRebuildPlan, error) {
		return store.GenerationRebuildPlan{}, nil
	})
	if !errors.Is(err, store.ErrGenerationMismatch) {
		t.Fatalf("pointer interference error = %v, want ErrGenerationMismatch", err)
	}
	current, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current.Data, oldCurrent.Data) {
		t.Fatal("pointer write overwrote the concurrent current manifest")
	}
}

func TestGenerationRebuildPointerWriteClassifiesOnlyGenerationMismatchAsCASConflict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantCAS bool
	}{
		{name: "cancellation", err: context.Canceled},
		{name: "provider error", err: errors.New("permission denied")},
		{name: "generation mismatch", err: store.ErrGenerationMismatch, wantCAS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, backend := newMemoryClient()
			seedManifest(t, backend, "old-generation", generationRebuildTestFiles())
			backend.manifestWriteErr = tc.err
			_, err := client.RebuildIndexGeneration(context.Background(), func(_ context.Context, _ string) (store.GenerationRebuildPlan, error) {
				return store.GenerationRebuildPlan{}, nil
			})
			if err == nil || errors.Is(err, store.ErrGenerationMismatch) != tc.wantCAS || strings.Contains(err.Error(), "cas_conflict") != tc.wantCAS {
				t.Fatalf("pointer error = %v, wantCAS=%v", err, tc.wantCAS)
			}
		})
	}
}

func TestGenerationRebuildPreservesSourceInputFingerprint(t *testing.T) {
	client, backend := newMemoryClient()
	files := generationRebuildTestFiles()
	seedManifestWithInputFingerprint(t, backend, "old-generation", files, "source-fingerprint-v1")

	result, err := client.RebuildIndexGeneration(context.Background(), func(_ context.Context, workspace string) (store.GenerationRebuildPlan, error) {
		if err := os.WriteFile(filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"entity-alpha":"alpha"}}`), 0o600); err != nil {
			return store.GenerationRebuildPlan{}, err
		}
		return store.GenerationRebuildPlan{ConceptCount: 1}, nil
	})
	if err != nil {
		t.Fatalf("RebuildIndexGeneration: %v", err)
	}
	current, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := generation.Decode(current.Data)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.InputFingerprint != "source-fingerprint-v1" || manifest.PreviousGenerationID != "old-generation" {
		t.Fatalf("manifest provenance = %+v", manifest)
	}
	if result.NewGeneration != manifest.GenerationID {
		t.Fatalf("result generation = %q, manifest generation = %q", result.NewGeneration, manifest.GenerationID)
	}
}

func TestGenerationFilesFromWorkspaceRejectsSymlinkAndOversizedOutput(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		workspace := t.TempDir()
		writeGenerationRebuildTestFiles(t, workspace)
		if err := os.Symlink("ordinary.md", filepath.Join(workspace, "wiki", "link.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := generationFilesFromWorkspace(workspace); err == nil {
			t.Fatal("generationFilesFromWorkspace accepted a symlink")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		workspace := t.TempDir()
		writeGenerationRebuildTestFiles(t, workspace)
		large := filepath.Join(workspace, "wiki", "large.md")
		if err := os.WriteFile(large, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(large, generation.MaxFileBytes+1); err != nil {
			t.Fatal(err)
		}
		if _, err := generationFilesFromWorkspace(workspace); err == nil {
			t.Fatal("generationFilesFromWorkspace accepted an oversized output")
		}
	})

}

func TestGenerationRebuildRejectsPostPreflightMutationBeforeCAS(t *testing.T) {
	client, backend := newMemoryClient()
	files := generationRebuildTestFiles()
	seedManifest(t, backend, "old-generation", files)
	oldCurrent, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}

	previousHook := generationRebuildAfterPreflight
	defer func() { generationRebuildAfterPreflight = previousHook }()
	generationRebuildAfterPreflight = func(workspace string) {
		if err := os.WriteFile(filepath.Join(workspace, "cache", "id_map.json"), []byte(`{"concept":{"new":"alpha"}}`), 0o600); err != nil {
			t.Fatalf("mutate staged output: %v", err)
		}
	}
	_, err = client.RebuildIndexGeneration(context.Background(), func(_ context.Context, _ string) (store.GenerationRebuildPlan, error) {
		return store.GenerationRebuildPlan{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "manifest_stage") {
		t.Fatalf("mutation error = %v, want manifest_stage", err)
	}
	current, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(current.Data) != string(oldCurrent.Data) {
		t.Fatal("post-preflight mutation advanced or changed current manifest")
	}
}

func TestGenerationRebuildRejectsLstatOpenSwapBeforeCAS(t *testing.T) {
	client, backend := newMemoryClient()
	files := generationRebuildTestFiles()
	seedManifest(t, backend, "old-generation", files)
	oldCurrent, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}

	previousPreflightHook := generationRebuildAfterPreflight
	previousLstatHook := generationRebuildAfterFileLstat
	defer func() {
		generationRebuildAfterPreflight = previousPreflightHook
		generationRebuildAfterFileLstat = previousLstatHook
	}()
	armed := false
	generationRebuildAfterPreflight = func(string) { armed = true }
	generationRebuildAfterFileLstat = func(filePath string) {
		if !armed || !strings.HasSuffix(filepath.ToSlash(filePath), "cache/id_map.json") {
			return
		}
		armed = false
		outside := filepath.Join(t.TempDir(), "replacement.json")
		original, readErr := os.ReadFile(filePath)
		if readErr != nil {
			t.Fatalf("read staged file: %v", readErr)
		}
		if writeErr := os.WriteFile(outside, original, 0o600); writeErr != nil {
			t.Fatalf("write replacement: %v", writeErr)
		}
		if removeErr := os.Remove(filePath); removeErr != nil {
			t.Fatalf("remove staged file: %v", removeErr)
		}
		if linkErr := os.Symlink(outside, filePath); linkErr != nil {
			t.Fatalf("swap staged file: %v", linkErr)
		}
	}

	_, err = client.RebuildIndexGeneration(context.Background(), func(_ context.Context, _ string) (store.GenerationRebuildPlan, error) {
		return store.GenerationRebuildPlan{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "manifest_stage") {
		t.Fatalf("Lstat/Open swap error = %v, want manifest_stage", err)
	}
	current, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(current.Data) != string(oldCurrent.Data) {
		t.Fatal("Lstat/Open swap advanced or changed current manifest")
	}
}

func TestGenerationRebuildRejectsAncestorDirectorySwapBeforeCAS(t *testing.T) {
	client, backend := newMemoryClient()
	seedManifest(t, backend, "old-generation", generationRebuildTestFiles())
	oldCurrent, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}

	previousPreflightHook := generationRebuildAfterPreflight
	previousLstatHook := generationRebuildAfterFileLstat
	defer func() {
		generationRebuildAfterPreflight = previousPreflightHook
		generationRebuildAfterFileLstat = previousLstatHook
	}()
	generationRebuildAfterPreflight = func(workspace string) {
		replacement := filepath.Join(t.TempDir(), "cache")
		if err := os.MkdirAll(replacement, 0o700); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(workspace, "cache"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(workspace, "cache", entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(replacement, entry.Name()), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cacheDir := filepath.Join(workspace, "cache")
		if err := os.RemoveAll(cacheDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(replacement, cacheDir); err != nil {
			t.Fatal(err)
		}
	}
	generationRebuildAfterFileLstat = func(filePath string) {
		if !strings.HasSuffix(filepath.ToSlash(filePath), "cache/id_map.json") {
			return
		}
	}

	_, err = client.RebuildIndexGeneration(context.Background(), func(_ context.Context, _ string) (store.GenerationRebuildPlan, error) {
		return store.GenerationRebuildPlan{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "manifest_stage") {
		t.Fatalf("ancestor swap error = %v, want manifest_stage", err)
	}
	current, err := backend.Read(context.Background(), projectObject(generation.ManifestPath), 0, generation.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(current.Data) != string(oldCurrent.Data) {
		t.Fatal("ancestor swap advanced or changed current manifest")
	}
}

func TestGenerationWorkspacePathRejectsAmbiguousPaths(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{".", "..", "../outside", "/absolute", "wiki/../outside.md", `wiki\\escape.md`} {
		t.Run(rel, func(t *testing.T) {
			if _, err := generationWorkspacePath(root, rel); err == nil {
				t.Fatalf("generationWorkspacePath(%q) unexpectedly succeeded", rel)
			}
		})
	}
}

func generationRebuildTestFiles() map[string]backendObject {
	files := make(map[string]backendObject)
	for path, data := range map[string][]byte{
		"wiki/alpha.md":                []byte("alpha"),
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
	return files
}

func writeGenerationRebuildTestFiles(t *testing.T, workspace string) {
	t.Helper()
	for path, data := range map[string][]byte{
		"wiki/ordinary.md":             []byte("ordinary"),
		"synto.toml":                   []byte("[pipeline]\n"),
		"cache/id_map.json":            []byte(`{"concept":{}}`),
		"cache/concepts.jsonl":         []byte{},
		"cache/dormant_concepts.jsonl": []byte{},
		"cache/raw_status.json":        []byte(`{}`),
		"cache/suggested_queries.json": []byte(`{}`),
		".synto/state.db":              []byte("sqlite"),
		".synto/INDEX.json":            []byte(`{"schema_version":1}`),
	} {
		name := filepath.Join(workspace, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
