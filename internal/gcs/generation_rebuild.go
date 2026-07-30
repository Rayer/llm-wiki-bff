package gcs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rayer/llm-wiki-bff/internal/generation"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

// RebuildIndexGeneration is the generation-managed admin rebuild seam. It
// materializes one pinned generation, rebuilds only derived artifacts in a
// private directory, uploads a complete new generation, validates its
// manifest, and CAS-advances current.json as the sole commit point.
func (c *Client) RebuildIndexGeneration(ctx context.Context, planner store.GenerationRebuildPlanner) (store.GenerationRebuildResult, error) {
	if planner == nil {
		return store.GenerationRebuildResult{}, errors.New("planner_missing")
	}
	old, oldObjectGeneration, exists, err := c.currentManifest(ctx)
	if err != nil {
		return store.GenerationRebuildResult{}, fmt.Errorf("manifest_read: %w", err)
	}
	if !exists {
		return store.GenerationRebuildResult{}, errors.New("generation_missing")
	}
	workspace, err := os.MkdirTemp("", "lwc-admin-rebuild-")
	if err != nil {
		return store.GenerationRebuildResult{}, errors.New("stage_create")
	}
	defer os.RemoveAll(workspace)

	for _, file := range old.Files {
		object, err := c.readObject(ctx, c.prefix()+"/"+old.ObjectPath(file), file.Generation, file.Size)
		if err != nil || int64(len(object.Data)) != file.Size || digestBytes(object.Data) != file.SHA256 {
			return store.GenerationRebuildResult{}, fmt.Errorf("manifest_input:%s", file.Path)
		}
		path := filepath.Join(workspace, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return store.GenerationRebuildResult{}, errors.New("stage_create")
		}
		if err := os.WriteFile(path, object.Data, 0o600); err != nil {
			return store.GenerationRebuildResult{}, errors.New("stage_write")
		}
	}

	planned, err := planner(ctx, workspace)
	if err != nil {
		return store.GenerationRebuildResult{}, fmt.Errorf("derived_rebuild: %w", err)
	}

	files, err := generationFilesFromWorkspace(workspace)
	if err != nil {
		return store.GenerationRebuildResult{}, errors.New("manifest_stage")
	}
	id, err := newAdminGenerationID()
	if err != nil {
		return store.GenerationRebuildResult{}, errors.New("generation_id")
	}
	manifest := generation.Manifest{
		Version:              generation.Version,
		GenerationID:         id,
		PreviousGenerationID: old.GenerationID,
		CreatedAt:            time.Now().UTC().Format(time.RFC3339),
		InputFingerprint:     adminInputFingerprint(old, files),
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(file.Path)))
		if err != nil {
			return store.GenerationRebuildResult{}, errors.New("manifest_stage")
		}
		a, err := c.writeObject(ctx, c.prefix()+"/"+generation.Prefix+id+"/"+file.Path, data, contentTypeForPath(file.Path), map[string]string{"sha256": file.SHA256}, writeCondition{DoesNotExist: true})
		if err != nil || a.Generation <= 0 || a.Size != file.Size {
			return store.GenerationRebuildResult{}, errors.New("generation_upload")
		}
		manifest.Files = append(manifest.Files, generation.File{Path: file.Path, Size: file.Size, SHA256: file.SHA256, Generation: a.Generation})
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	if err := manifest.Validate(); err != nil {
		return store.GenerationRebuildResult{}, errors.New("manifest_invalid")
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return store.GenerationRebuildResult{}, errors.New("manifest_encode")
	}
	_, err = c.writeObject(ctx, c.prefix()+"/"+generation.ManifestPath, manifestData, "application/json; charset=utf-8", map[string]string{"sha256": digestBytes(manifestData)}, writeCondition{GenerationMatch: &oldObjectGeneration})
	if err != nil {
		return store.GenerationRebuildResult{}, errors.New("cas_conflict")
	}
	return store.GenerationRebuildResult{
		Status:          "ok",
		OldGeneration:   old.GenerationID,
		NewGeneration:   manifest.GenerationID,
		ConceptCount:    planned.ConceptCount,
		SourceCount:     planned.SourceCount,
		MigratedOldIDs:  planned.MigratedOldIDs,
		RedirectCount:   planned.RedirectCount,
		AnnotationCount: 0,
	}, nil
}

type adminGenerationFile struct {
	Path   string
	Size   int64
	SHA256 string
}

func generationFilesFromWorkspace(root string) ([]adminGenerationFile, error) {
	var files []adminGenerationFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || !generation.GenerationOwned(filepath.ToSlash(rel)) {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, adminGenerationFile{Path: filepath.ToSlash(rel), Size: int64(len(data)), SHA256: digestBytes(data)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"synto.toml", "cache/id_map.json", "cache/concepts.jsonl", "cache/dormant_concepts.jsonl", "cache/raw_status.json", "cache/suggested_queries.json", ".synto/state.db", ".synto/INDEX.json"} {
		found := false
		for _, file := range files {
			if file.Path == required {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("incomplete generation")
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func newAdminGenerationID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "g_" + hex.EncodeToString(data), nil
}

func adminInputFingerprint(old generation.Manifest, files []adminGenerationFile) string {
	h := sha256.New()
	_, _ = h.Write([]byte(old.InputFingerprint))
	for _, file := range files {
		_, _ = h.Write([]byte(file.Path + "\x00" + file.SHA256 + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var _ store.GenerationRebuilder = (*Client)(nil)
