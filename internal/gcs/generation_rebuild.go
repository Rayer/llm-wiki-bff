package gcs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
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
	if err := old.Validate(); err != nil {
		return store.GenerationRebuildResult{}, errors.New("manifest_invalid")
	}
	workspace, err := os.MkdirTemp("", "lwc-admin-rebuild-")
	if err != nil {
		return store.GenerationRebuildResult{}, errors.New("stage_create")
	}
	defer os.RemoveAll(workspace)
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		return store.GenerationRebuildResult{}, errors.New("stage_create")
	}
	defer workspaceRoot.Close()

	for _, file := range old.Files {
		object, err := c.readObject(ctx, c.prefix()+"/"+old.ObjectPath(file), file.Generation, file.Size)
		if err != nil || int64(len(object.Data)) != file.Size || digestBytes(object.Data) != file.SHA256 {
			return store.GenerationRebuildResult{}, fmt.Errorf("manifest_input:%s", file.Path)
		}
		path, err := generationWorkspacePath(workspace, file.Path)
		if err != nil {
			return store.GenerationRebuildResult{}, errors.New("manifest_input")
		}
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

	files, err := generationFilesFromWorkspace(workspace, workspaceRoot)
	if err != nil {
		return store.GenerationRebuildResult{}, errors.New("manifest_stage")
	}
	generationRebuildAfterPreflight(workspace)
	id, err := newAdminGenerationID()
	if err != nil {
		return store.GenerationRebuildResult{}, errors.New("generation_id")
	}
	manifest := generation.Manifest{
		Version:              generation.Version,
		GenerationID:         id,
		PreviousGenerationID: old.GenerationID,
		CreatedAt:            time.Now().UTC().Format(time.RFC3339),
		InputFingerprint:     old.InputFingerprint,
	}
	for _, file := range files {
		path, err := generationWorkspacePath(workspace, file.Path)
		if err != nil {
			return store.GenerationRebuildResult{}, errors.New("manifest_stage")
		}
		rel, err := generationWorkspaceRelativePath(workspace, path)
		if err != nil {
			return store.GenerationRebuildResult{}, errors.New("manifest_stage")
		}
		data, digest, err := readGenerationWorkspaceFileRoot(workspaceRoot, rel, file.Size)
		if err != nil || digest != file.SHA256 {
			return store.GenerationRebuildResult{}, errors.New("manifest_stage")
		}
		a, err := c.writeObject(ctx, c.prefix()+"/"+generation.Prefix+id+"/"+file.Path, data, contentTypeForPath(file.Path), map[string]string{"sha256": file.SHA256}, writeCondition{DoesNotExist: true})
		if err != nil || a.Generation <= 0 || a.Size != file.Size {
			return store.GenerationRebuildResult{}, errors.New("generation_upload")
		}
		uploaded, err := c.readObject(ctx, c.prefix()+"/"+generation.Prefix+id+"/"+file.Path, a.Generation, file.Size)
		if err != nil || uploaded.Generation != a.Generation || uploaded.Size != file.Size || int64(len(uploaded.Data)) != file.Size || digestBytes(uploaded.Data) != file.SHA256 {
			return store.GenerationRebuildResult{}, errors.New("manifest_stage")
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
		if errors.Is(err, store.ErrGenerationMismatch) {
			return store.GenerationRebuildResult{}, fmt.Errorf("cas_conflict: %w", err)
		}
		return store.GenerationRebuildResult{}, fmt.Errorf("manifest_commit: %w", err)
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

var generationRebuildAfterPreflight = func(string) {}
var generationRebuildAfterFileLstat = func(string) {}

func generationFilesFromWorkspace(root string, anchored ...*os.Root) ([]adminGenerationFile, error) {
	workspaceRoot := (*os.Root)(nil)
	if len(anchored) > 0 {
		workspaceRoot = anchored[0]
	} else {
		var err error
		workspaceRoot, err = os.OpenRoot(root)
		if err != nil {
			return nil, err
		}
		defer workspaceRoot.Close()
	}
	var files []adminGenerationFile
	var total int64
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filePath == root {
			return nil
		}
		rel, err := generationWorkspaceRelativePath(root, filePath)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("generation contains symlink")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("generation contains special file")
		}
		if !generation.GenerationOwned(rel) {
			return nil
		}
		if info.Size() < 0 || info.Size() > generation.MaxFileBytes || total > generation.MaxTotalSize-info.Size() {
			return errors.New("generation output too large")
		}
		if len(files) >= generation.MaxFiles {
			return errors.New("too many generation files")
		}
		data, digest, err := readGenerationWorkspaceFileRoot(workspaceRoot, rel, info.Size())
		if err != nil {
			return err
		}
		total += int64(len(data))
		files = append(files, adminGenerationFile{Path: rel, Size: info.Size(), SHA256: digest})
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

func generationWorkspacePath(root, rel string) (string, error) {
	canonical, err := validateGenerationWorkspaceRelative(root, rel)
	if err != nil || canonical != rel {
		return "", errors.New("invalid generation workspace path")
	}
	return filepath.Join(root, filepath.FromSlash(canonical)), nil
}

func generationWorkspaceRelativePath(root, filePath string) (string, error) {
	rel, err := filepath.Rel(root, filePath)
	if err != nil {
		return "", errors.New("invalid generation workspace path")
	}
	return validateGenerationWorkspaceRelative(root, filepath.ToSlash(rel))
}

func validateGenerationWorkspaceRelative(root, rel string) (string, error) {
	if rel == "." || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || strings.Contains(rel, "\\") {
		return "", errors.New("invalid generation workspace path")
	}
	canonical := filepath.ToSlash(rel)
	if canonical == "" || canonical == "." || path.IsAbs(canonical) || path.Clean(canonical) != canonical || canonical == ".." || strings.HasPrefix(canonical, "../") || strings.Contains(canonical, "//") {
		return "", errors.New("invalid generation workspace path")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(canonical)))
	if err != nil {
		return "", err
	}
	confined, err := filepath.Rel(absRoot, absolute)
	if err != nil || confined == "." || filepath.IsAbs(confined) || confined == ".." || strings.HasPrefix(confined, ".."+string(filepath.Separator)) || filepath.ToSlash(confined) != canonical {
		return "", errors.New("invalid generation workspace path")
	}
	return canonical, nil
}

func readGenerationWorkspaceFile(filePath string, expectedSize int64) ([]byte, string, error) {
	root, err := os.OpenRoot(filepath.Dir(filePath))
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	return readGenerationWorkspaceFileRoot(root, filepath.Base(filePath), expectedSize)
}

func readGenerationWorkspaceFileRoot(root *os.Root, rel string, expectedSize int64) ([]byte, string, error) {
	if expectedSize < 0 || expectedSize > generation.MaxFileBytes {
		return nil, "", errors.New("generation output too large")
	}
	info, err := root.Lstat(filepath.FromSlash(rel))
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return nil, "", errors.New("generation output changed")
	}
	generationRebuildAfterFileLstat(filepath.Join(root.Name(), filepath.FromSlash(rel)))
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() != expectedSize || !os.SameFile(info, openedInfo) {
		return nil, "", errors.New("generation output changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, generation.MaxFileBytes+1))
	if err != nil || int64(len(data)) != expectedSize || int64(len(data)) > generation.MaxFileBytes {
		return nil, "", errors.New("generation output changed")
	}
	return data, digestBytes(data), nil
}

func newAdminGenerationID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "g_" + hex.EncodeToString(data), nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var _ store.GenerationRebuilder = (*Client)(nil)
