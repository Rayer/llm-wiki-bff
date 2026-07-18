package gcs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

// memoryBackend is intentionally attached to one Client only. It models just
// the GCS read/list/write operations exercised by this adapter.
type memoryBackend struct {
	mu                      sync.Mutex
	objects                 map[string]backendObject
	requests                []backendObject
	manifestReads           int
	listVisits              int
	switchManifestOnRead    string
	commitManifestOnWrite   []byte
	manifestAfterLeaseWrite []byte
	manifestReadErr         error
	writeNames              []string
	writeConditions         []writeCondition
	nextGeneration          int64
}

type legacyLeaseProbeBackend struct {
	*memoryBackend
	deleteAttempts  int
	deleteErrs      []error
	deleteDeadlines []bool
	deleteFailures  int
	failGenerated   bool
}

func (b *legacyLeaseProbeBackend) Write(ctx context.Context, name string, data []byte, contentType string, metadata map[string]string, condition writeCondition) (backendObject, error) {
	if b.failGenerated && strings.HasSuffix(name, "/wiki/fail.md") {
		return backendObject{}, errors.New("primary write failed")
	}
	return b.memoryBackend.Write(ctx, name, data, contentType, metadata, condition)
}

func (b *legacyLeaseProbeBackend) Delete(ctx context.Context, name string, objectGeneration int64) error {
	b.deleteAttempts++
	b.deleteErrs = append(b.deleteErrs, ctx.Err())
	_, deadline := ctx.Deadline()
	b.deleteDeadlines = append(b.deleteDeadlines, deadline)
	if b.deleteAttempts <= b.deleteFailures {
		return context.DeadlineExceeded
	}
	return b.memoryBackend.Delete(ctx, name, objectGeneration)
}

func newMemoryClient() (*Client, *memoryBackend) {
	backend := &memoryBackend{objects: make(map[string]backendObject), nextGeneration: 1000}
	return &Client{userID: "user", projectID: "project", backend: backend}, backend
}

func (m *memoryBackend) Read(_ context.Context, name string, generation, limit int64) (backendObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, backendObject{Name: name, Generation: generation})
	object, ok := m.objects[name]
	if name == projectObject(generationManifestPath) {
		m.manifestReads++
		if m.switchManifestOnRead != "" && m.manifestReads == 1 {
			m.objects[name] = m.objects[m.switchManifestOnRead]
		}
	}
	if !ok || (generation > 0 && object.Generation != generation) {
		return backendObject{}, storage.ErrObjectNotExist
	}
	if object.Size > limit {
		return backendObject{}, errors.New("object exceeds input limit")
	}
	object.Data = append([]byte(nil), object.Data...)
	object.Metadata = cloneMetadata(object.Metadata)
	return object, nil
}

func (m *memoryBackend) Attrs(_ context.Context, name string, generation int64) (backendObject, error) {
	if name == projectObject(generationManifestPath) && m.manifestReadErr != nil {
		return backendObject{}, m.manifestReadErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[name]
	if !ok || (generation > 0 && object.Generation != generation) {
		return backendObject{}, storage.ErrObjectNotExist
	}
	object.Data = nil
	object.Metadata = cloneMetadata(object.Metadata)
	return object, nil
}

func (m *memoryBackend) List(_ context.Context, prefix string, directOnly bool, visit func(backendObject) error) error {
	m.mu.Lock()
	objects := make([]backendObject, 0)
	for name, object := range m.objects {
		if strings.HasPrefix(name, prefix) {
			if directOnly && strings.Contains(strings.TrimPrefix(name, prefix), "/") {
				continue
			}
			object.Name = name
			object.Data = nil
			object.Metadata = cloneMetadata(object.Metadata)
			objects = append(objects, object)
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Name < objects[j].Name })
	m.mu.Unlock()
	for _, object := range objects {
		m.mu.Lock()
		m.listVisits++
		m.mu.Unlock()
		if err := visit(object); err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryBackend) Write(_ context.Context, name string, data []byte, _ string, metadata map[string]string, condition writeCondition) (backendObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeNames = append(m.writeNames, name)
	m.writeConditions = append(m.writeConditions, condition)
	current, exists := m.objects[name]
	if condition.DoesNotExist && exists {
		return backendObject{}, store.ErrGenerationMismatch
	}
	if condition.GenerationMatch != nil && (!exists || current.Generation != *condition.GenerationMatch) {
		return backendObject{}, store.ErrGenerationMismatch
	}
	m.nextGeneration++
	object := backendObject{Name: name, Data: append([]byte(nil), data...), Generation: m.nextGeneration, Size: int64(len(data)), Metadata: cloneMetadata(metadata), Updated: time.Now().UTC()}
	m.objects[name] = object
	if strings.HasSuffix(name, "/"+generation.LeasePath) && m.manifestAfterLeaseWrite != nil {
		m.nextGeneration++
		m.objects[projectObject(generation.ManifestPath)] = backendObject{Name: projectObject(generation.ManifestPath), Data: append([]byte(nil), m.manifestAfterLeaseWrite...), Generation: m.nextGeneration, Size: int64(len(m.manifestAfterLeaseWrite)), Updated: time.Now().UTC()}
		m.manifestAfterLeaseWrite = nil
	}
	if m.commitManifestOnWrite != nil && generation.GenerationOwned(strings.TrimPrefix(name, projectObject(""))) {
		m.nextGeneration++
		m.objects[projectObject(generation.ManifestPath)] = backendObject{Name: projectObject(generation.ManifestPath), Data: append([]byte(nil), m.commitManifestOnWrite...), Generation: m.nextGeneration, Size: int64(len(m.commitManifestOnWrite)), Updated: time.Now().UTC()}
		m.commitManifestOnWrite = nil
	}
	return object, nil
}

func (m *memoryBackend) Delete(_ context.Context, name string, generation int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[name]
	if !ok || (generation > 0 && object.Generation != generation) {
		return store.ErrGenerationMismatch
	}
	delete(m.objects, name)
	return nil
}

func (m *memoryBackend) put(name string, data []byte, generation int64, metadata map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[name] = backendObject{Name: name, Data: append([]byte(nil), data...), Generation: generation, Size: int64(len(data)), Metadata: cloneMetadata(metadata), Updated: time.Now().UTC()}
}

func (m *memoryBackend) snapshots() ([]backendObject, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]backendObject(nil), m.requests...), m.manifestReads
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	copy := make(map[string]string, len(metadata))
	for key, value := range metadata {
		copy[key] = value
	}
	return copy
}

const generationManifestPath = ".lwc/publish/current.json"

func projectObject(rel string) string { return "users/user/projects/project/" + rel }

func manifestBytes(t *testing.T, generationID string, files map[string]backendObject) []byte {
	t.Helper()
	paths := make([]string, 0, len(files))
	for rel := range files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	manifest := generation.Manifest{Version: generation.Version, GenerationID: generationID, CreatedAt: time.Now().UTC().Format(time.RFC3339), InputFingerprint: "test-input", Files: make([]generation.File, 0, len(files))}
	for _, rel := range paths {
		object := files[rel]
		manifest.Files = append(manifest.Files, generation.File{Path: rel, Size: int64(len(object.Data)), SHA256: digest(object.Data), Generation: object.Generation})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return data
}

func seedManifest(t *testing.T, backend *memoryBackend, generationID string, files map[string]backendObject) {
	t.Helper()
	for rel, object := range files {
		backend.put(projectObject(path.Join(generation.Prefix, generationID, rel)), object.Data, object.Generation, map[string]string{"sha256": digest(object.Data)})
	}
	backend.put(projectObject(generation.ManifestPath), manifestBytes(t, generationID, files), 7, nil)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestGenerationViewLegacyOperationsRemainDirect(t *testing.T) {
	client, backend := newMemoryClient()
	for rel, data := range map[string][]byte{
		"wiki/alpha.md":                 []byte("---\ntitle: Alpha\n---\nalpha"),
		"wiki/.drafts/draft.md":         []byte("draft"),
		"wiki/sources/source.md":        []byte("source"),
		"cache/concepts.jsonl":          []byte(`{"slug":"alpha","title":"Alpha"}` + "\n"),
		"cache/id_map.json":             []byte(`{"source":{"source-id":"source"}}`),
		"meta/index.md":                 []byte("legacy index"),
		"raw/input.md":                  []byte("raw input"),
		"cache/source_status.json":      []byte("status"),
		"cache/pipeline-run.log":        []byte("log"),
		"cache/annotations/source.json": []byte("annotation"),
	} {
		backend.put(projectObject(rel), data, 11, map[string]string{"sha256": digest(data)})
	}
	ctx := context.Background()
	if data, err := client.ReadFile(ctx, "wiki/alpha.md"); err != nil || !bytes.Contains(data, []byte("alpha")) {
		t.Fatalf("ReadFile legacy = %q, %v", data, err)
	}
	if page, data, err := client.GetPage(ctx, "alpha", "concepts"); err != nil || page.Title != "alpha" || !bytes.Contains(data, []byte("alpha")) {
		t.Fatalf("GetPage legacy = %#v, %q, %v", page, data, err)
	}
	if pages, err := client.ListConcepts(ctx, true); err != nil || len(pages) != 3 {
		t.Fatalf("ListConcepts legacy = %#v, %v", pages, err)
	}
	if pages, err := client.ListSources(ctx); err != nil || len(pages) != 1 {
		t.Fatalf("ListSources legacy = %#v, %v", pages, err)
	}
	if files, err := client.ListMarkdownFiles(ctx, "wiki/"); err != nil || len(files) != 1 || files[0].Slug != "alpha" {
		t.Fatalf("ListMarkdownFiles legacy = %#v, %v", files, err)
	}
	if pages, err := client.ListConceptsFromCache(ctx); err != nil || len(pages) != 1 || pages[0].Slug != "alpha" {
		t.Fatalf("ListConceptsFromCache legacy = %#v, %v", pages, err)
	}
	if pages, err := client.ListSourcesFromCache(ctx); err != nil || len(pages) != 1 || pages[0].Slug != "source" {
		t.Fatalf("ListSourcesFromCache legacy = %#v, %v", pages, err)
	}
	if got, err := client.GetMetaSHA256(ctx, "wiki/alpha.md"); err != nil || got != digest([]byte("---\ntitle: Alpha\n---\nalpha")) {
		t.Fatalf("GetMetaSHA256 legacy = %q, %v", got, err)
	}
	if data, err := client.ReadRaw(ctx, "input.md"); err != nil || string(data) != "raw input" {
		t.Fatalf("ReadRaw legacy = %q, %v", data, err)
	}
}

func TestGenerationViewCapturesManifestOnceForWholeList(t *testing.T) {
	client, backend := newMemoryClient()
	first := map[string]backendObject{
		"wiki/alpha.md": {Data: []byte("---\ntitle: First Alpha\n---\nfirst"), Generation: 101},
		"wiki/beta.md":  {Data: []byte("---\ntitle: First Beta\n---\nfirst"), Generation: 102},
	}
	seedManifest(t, backend, "generation-one", first)
	second := map[string]backendObject{
		"wiki/alpha.md": {Data: []byte("second alpha"), Generation: 201},
		"wiki/beta.md":  {Data: []byte("second beta"), Generation: 202},
	}
	for rel, object := range second {
		backend.put(projectObject(path.Join(generation.Prefix, "generation-two", rel)), object.Data, object.Generation, map[string]string{"sha256": digest(object.Data)})
	}
	backend.put(projectObject(".test/next-current.json"), manifestBytes(t, "generation-two", second), 8, nil)
	backend.switchManifestOnRead = projectObject(".test/next-current.json")

	pages, err := client.ListConcepts(context.Background(), false)
	if err != nil {
		t.Fatalf("ListConcepts after manifest switch: %v", err)
	}
	if len(pages) != 2 || pages[0].Title != "First Alpha" || pages[1].Title != "First Beta" {
		t.Fatalf("ListConcepts mixed generations: %#v", pages)
	}
	requests, manifestReads := backend.snapshots()
	if manifestReads != 1 {
		t.Fatalf("manifest reads = %d, want exactly one", manifestReads)
	}
	for _, request := range requests {
		if request.Name == projectObject(generation.ManifestPath) {
			continue
		}
		if strings.HasPrefix(request.Name, projectObject(generation.Prefix)) && !strings.HasPrefix(request.Name, projectObject(generation.Prefix+"generation-one/")) {
			t.Fatalf("read mixed generation object: %#v", request)
		}
	}
}

func TestPinnedGenerationViewSurvivesManifestCommitAndNextPinSeesIt(t *testing.T) {
	client, backend := newMemoryClient()
	first := map[string]backendObject{"wiki/alpha.md": {Data: []byte("A"), Generation: 101}}
	seedManifest(t, backend, "generation-one", first)
	pinnedAStore, err := client.Pin(context.Background())
	if err != nil {
		t.Fatalf("Pin A: %v", err)
	}
	pinnedA := pinnedAStore.(*Client)
	second := map[string]backendObject{"wiki/alpha.md": {Data: []byte("B"), Generation: 201}}
	seedManifest(t, backend, "generation-two", second)
	backend.put(projectObject(generation.ManifestPath), manifestBytes(t, "generation-two", second), 8, nil)
	pinnedBStore, err := client.Pin(context.Background())
	if err != nil {
		t.Fatalf("Pin B: %v", err)
	}
	pinnedB := pinnedBStore.(*Client)
	for name, reader := range map[string]*Client{"A": pinnedA, "B": pinnedB} {
		data, err := reader.ReadFile(context.Background(), "wiki/alpha.md")
		if err != nil {
			t.Fatalf("read pinned %s: %v", name, err)
		}
		if got, want := string(data), name; got != want {
			t.Fatalf("pinned %s read = %q, want %q", name, got, want)
		}
	}
	if pinnedA.ViewToken() == pinnedB.ViewToken() || pinnedA.ViewToken() == "" {
		t.Fatalf("pinned view tokens = %q, %q", pinnedA.ViewToken(), pinnedB.ViewToken())
	}
}

func TestGenerationViewUsesOneManifestForEveryGeneratedOperation(t *testing.T) {
	operations := map[string]func(*Client) error{
		"ReadFile": func(client *Client) error {
			_, err := client.ReadFile(context.Background(), "wiki/alpha.md")
			return err
		},
		"ReadFileWithGeneration": func(client *Client) error {
			_, _, err := client.ReadFileWithGeneration(context.Background(), "wiki/alpha.md")
			return err
		},
		"GetPage": func(client *Client) error {
			_, _, err := client.GetPage(context.Background(), "alpha", "concepts")
			return err
		},
		"ListSources":  func(client *Client) error { _, err := client.ListSources(context.Background()); return err },
		"ListConcepts": func(client *Client) error { _, err := client.ListConcepts(context.Background(), true); return err },
		"ListMarkdownFiles": func(client *Client) error {
			_, err := client.ListMarkdownFiles(context.Background(), "wiki/")
			return err
		},
		"ListConceptsFromCache": func(client *Client) error { _, err := client.ListConceptsFromCache(context.Background()); return err },
		"ListSourcesFromCache":  func(client *Client) error { _, err := client.ListSourcesFromCache(context.Background()); return err },
		"GetMetaSHA256": func(client *Client) error {
			_, err := client.GetMetaSHA256(context.Background(), "wiki/alpha.md")
			return err
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			client, backend := newMemoryClient()
			files := map[string]backendObject{
				"wiki/alpha.md":          {Data: []byte("alpha"), Generation: 101},
				"wiki/sources/source.md": {Data: []byte("source"), Generation: 102},
				"cache/concepts.jsonl":   {Data: []byte(`{"slug":"alpha"}`), Generation: 103},
				"cache/id_map.json":      {Data: []byte(`{"source":{"id":"source"}}`), Generation: 104},
			}
			seedManifest(t, backend, "generation-one", files)
			if err := operation(client); err != nil {
				t.Fatalf("operation: %v", err)
			}
			requests, manifestReads := backend.snapshots()
			if manifestReads != 1 {
				t.Fatalf("manifest reads = %d, want 1", manifestReads)
			}
			for _, request := range requests {
				if request.Name == projectObject(generation.ManifestPath) {
					continue
				}
				if strings.HasPrefix(request.Name, projectObject(generation.Prefix)) && (!strings.HasPrefix(request.Name, projectObject(generation.Prefix+"generation-one/")) || request.Generation == 0) {
					t.Fatalf("generated operation did not pin manifest object: %#v", request)
				}
			}
		})
	}
}

func TestGenerationViewFailsClosedAndCanonicalPathsBypassIt(t *testing.T) {
	client, backend := newMemoryClient()
	generated := map[string]backendObject{"wiki/alpha.md": {Data: []byte("actual"), Generation: 101}}
	seedManifest(t, backend, "generation-one", generated)
	manifest := manifestBytes(t, "generation-one", generated)
	manifest = bytes.Replace(manifest, []byte(digest([]byte("actual"))), []byte(strings.Repeat("0", 64)), 1)
	backend.put(projectObject(generation.ManifestPath), manifest, 7, nil)
	backend.put(projectObject("wiki/alpha.md"), []byte("legacy must stay hidden"), 99, nil)
	for rel, data := range map[string][]byte{
		"raw/input.md":             []byte("raw"),
		"cache/annotations/a.json": []byte("annotation"),
		"cache/source_status.json": []byte("status"),
		"cache/pipeline-run.log":   []byte("log"),
	} {
		backend.put(projectObject(rel), data, 20, nil)
	}
	ctx := context.Background()
	if _, err := client.ReadFile(ctx, "wiki/alpha.md"); err == nil {
		t.Fatal("generation digest mismatch fell back to legacy object")
	}
	for rel, want := range map[string]string{
		"raw/input.md":             "raw",
		"cache/annotations/a.json": "annotation",
		"cache/source_status.json": "status",
		"cache/pipeline-run.log":   "log",
	} {
		got, err := client.ReadFile(ctx, rel)
		if err != nil || string(got) != want {
			t.Fatalf("canonical %s = %q, %v", rel, got, err)
		}
	}
}

func TestGenerationViewRejectsUnsafeGeneratedPathsWithoutLegacyFallback(t *testing.T) {
	client, backend := newMemoryClient()
	generated := map[string]backendObject{"wiki/alpha.md": {Data: []byte("current"), Generation: 101}}
	seedManifest(t, backend, "generation-current", generated)
	for _, rel := range []string{
		"wiki/a..b.md",
		"wiki/../legacy.md",
		"wiki//legacy.md",
		"cache/unknown.json",
		"cache/annotations/../legacy.json",
		".olw/other.db",
		"wiki.toml/extra",
	} {
		backend.put(projectObject(rel), []byte("stale legacy"), 99, nil)
	}
	for rel, data := range map[string][]byte{
		"raw/input.md":                  []byte("raw"),
		"cache/annotations/source.json": []byte("annotation"),
		"cache/source_status.json":      []byte("status"),
		"cache/pipeline-run.log":        []byte("log"),
	} {
		backend.put(projectObject(rel), data, 20, nil)
	}

	for _, rel := range []string{
		"wiki/a..b.md",
		"wiki/../legacy.md",
		"wiki//legacy.md",
		"cache/unknown.json",
		"cache/annotations/../legacy.json",
		".olw/other.db",
		"wiki.toml/extra",
	} {
		if _, err := client.ReadFile(context.Background(), rel); !errors.Is(err, store.ErrDeclaredObjectUnavailable) {
			t.Fatalf("ReadFile(%q) = %v, want declared-object-unavailable", rel, err)
		}
	}
	if _, _, err := client.GetPage(context.Background(), "a..b", "concepts"); !errors.Is(err, store.ErrDeclaredObjectUnavailable) {
		t.Fatalf("GetPage unsafe slug = %v, want declared-object-unavailable", err)
	}

	for rel, want := range map[string]string{
		"raw/input.md":                  "raw",
		"cache/annotations/source.json": "annotation",
		"cache/source_status.json":      "status",
		"cache/pipeline-run.log":        "log",
	} {
		got, err := client.ReadFile(context.Background(), rel)
		if err != nil || string(got) != want {
			t.Fatalf("canonical ReadFile(%q) = %q, %v", rel, got, err)
		}
	}
	if _, err := client.ReadFile(context.Background(), "wiki/missing.md"); !objectNotFound(err) || errors.Is(err, store.ErrDeclaredObjectUnavailable) {
		t.Fatalf("absent safe manifest path = %v, want object-not-exist", err)
	}
}

func TestGenerationWritesRequireLockedLegacySession(t *testing.T) {
	ctx := context.Background()
	legacy, _ := newMemoryClient()
	if _, err := legacy.WriteBytes(ctx, []byte("legacy"), "wiki/legacy.md"); err != nil {
		t.Fatalf("unlocked legacy WriteBytes: %v", err)
	}
	locked, release, err := legacy.BeginLegacyGenerationWrite(ctx)
	if err != nil {
		t.Fatalf("begin legacy session: %v", err)
	}
	defer func() { _ = release(context.Background()) }()
	if _, err := locked.WriteBytes(ctx, []byte("legacy"), "wiki/legacy.md"); err != nil {
		t.Fatalf("locked legacy WriteBytes: %v", err)
	}

	client, backend := newMemoryClient()
	seedManifest(t, backend, "generation-one", map[string]backendObject{"wiki/alpha.md": {Data: []byte("alpha"), Generation: 101}})
	local := path.Join(t.TempDir(), "page.md")
	if err := os.WriteFile(local, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, write := range []struct {
		name string
		run  func() error
	}{
		{"WriteBytes", func() error { _, err := client.WriteBytes(ctx, []byte("new"), "wiki/new.md"); return err }},
		{"WriteBytesAtomic", func() error {
			_, err := client.WriteBytesAtomic(ctx, []byte("new"), ".tmp/new", "wiki/new.md")
			return err
		}},
		{"UploadFile", func() error { _, err := client.UploadFile(ctx, local, "wiki/new.md"); return err }},
		{"UploadFileWithDigest", func() error {
			_, err := client.UploadFileWithDigest(ctx, local, "wiki/new.md", digest([]byte("new")))
			return err
		}},
		{"WriteFileIfGeneration", func() error { _, err := client.WriteFileIfGeneration(ctx, []byte("new"), "wiki/new.md", 0); return err }},
	} {
		if err := write.run(); !errors.Is(err, store.ErrGenerationManaged) {
			t.Fatalf("%s error = %v, want ErrGenerationManaged", write.name, err)
		}
	}
	for _, canonical := range []string{"raw/new.md", "cache/annotations/source.json", "cache/source_status.json", "cache/pipeline-run.log"} {
		if _, err := client.WriteBytes(ctx, []byte("canonical"), canonical); err != nil {
			t.Fatalf("canonical write %s rejected: %v", canonical, err)
		}
	}

	race, _ := newMemoryClient()
	_, releaseRace, err := race.BeginLegacyGenerationWrite(ctx)
	if err != nil {
		t.Fatalf("begin overlapping legacy session: %v", err)
	}
	defer func() { _ = releaseRace(context.Background()) }()
	if _, err := race.WriteBytes(ctx, []byte("race"), "wiki/race.md"); !errors.Is(err, store.ErrGenerationManaged) {
		t.Fatalf("overlapping WriteBytes error = %v, want ErrGenerationManaged", err)
	}
}

func TestLegacyGeneratedWriteEntrypointsPersistAndDoNotNestLease(t *testing.T) {
	client, backend := newMemoryClient()
	ctx := context.Background()
	local := path.Join(t.TempDir(), "page.md")
	if err := os.WriteFile(local, []byte("uploaded"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := client.WriteBytes(ctx, []byte("concepts"), conceptsCachePath); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	if _, err := client.WriteBytesAtomic(ctx, []byte("id-map"), "cache/id_map.json.tmp", idMapCachePath); err != nil {
		t.Fatalf("WriteBytesAtomic: %v", err)
	}
	if _, err := client.UploadFile(ctx, local, "wiki/upload.md"); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if _, err := client.UploadFileWithDigest(ctx, local, "wiki/upload-digest.md", digest([]byte("uploaded"))); err != nil {
		t.Fatalf("UploadFileWithDigest: %v", err)
	}
	if _, err := client.WriteFileIfGeneration(ctx, []byte("conditional"), "wiki/conditional.md", 0); err != nil {
		t.Fatalf("WriteFileIfGeneration: %v", err)
	}

	for rel, want := range map[string]string{
		conceptsCachePath:       "concepts",
		idMapCachePath:          "id-map",
		"wiki/upload.md":        "uploaded",
		"wiki/upload-digest.md": "uploaded",
		"wiki/conditional.md":   "conditional",
	} {
		data, err := client.ReadFile(ctx, rel)
		if err != nil || string(data) != want {
			t.Fatalf("persisted %s = %q, %v", rel, data, err)
		}
	}

	backend.mu.Lock()
	leaseWrites := 0
	tempWritten := false
	for _, name := range backend.writeNames {
		if name == projectObject(generation.LeasePath) {
			leaseWrites++
		}
		if name == projectObject("cache/id_map.json.tmp") {
			tempWritten = true
		}
	}
	_, leaseExists := backend.objects[projectObject(generation.LeasePath)]
	backend.mu.Unlock()
	if leaseWrites != 5 {
		t.Fatalf("shared lease write count = %d, want one per entrypoint", leaseWrites)
	}
	if leaseExists {
		t.Fatal("automatic generated writes left the shared lease behind")
	}
	if !tempWritten {
		t.Fatal("backend WriteBytesAtomic path skipped the temporary object")
	}

	nestedClient, nestedBackend := newMemoryClient()
	lockedStore, release, err := nestedClient.BeginLegacyGenerationWrite(ctx)
	if err != nil {
		t.Fatalf("begin attached legacy session: %v", err)
	}
	locked := lockedStore.(*Client)
	nestedBackend.mu.Lock()
	before := len(nestedBackend.writeNames)
	nestedBackend.mu.Unlock()
	if _, err := locked.WriteBytes(ctx, []byte("attached"), "wiki/attached.md"); err != nil {
		t.Fatalf("attached legacy WriteBytes: %v", err)
	}
	nestedBackend.mu.Lock()
	after := len(nestedBackend.writeNames)
	nestedBackend.mu.Unlock()
	_ = release(context.Background())
	if after != before+1 {
		t.Fatalf("attached lease write count = %d, want one generated write without nested lease", after-before)
	}
}

func TestLegacyWriteRechecksManifestAfterLeaseAcquisition(t *testing.T) {
	client, backend := newMemoryClient()
	backend.manifestAfterLeaseWrite = manifestBytes(t, "generation-one", map[string]backendObject{
		"wiki/current.md": {Data: []byte("current"), Generation: 101},
	})

	_, err := client.WriteBytes(context.Background(), []byte("rogue"), "wiki/rogue.md")
	if !errors.Is(err, store.ErrGenerationManaged) {
		t.Fatalf("WriteBytes() error = %v, want ErrGenerationManaged", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if _, ok := backend.objects[projectObject("wiki/rogue.md")]; ok {
		t.Fatal("legacy write occurred after manifest appeared during lease acquisition")
	}
	if _, ok := backend.objects[projectObject(generation.LeasePath)]; ok {
		t.Fatal("failed legacy write left lease behind")
	}
}

func TestLegacyLeaseReleaseUsesGenerationCondition(t *testing.T) {
	client, backend := newMemoryClient()
	_, release, err := client.BeginLegacyGenerationWrite(context.Background())
	if err != nil {
		t.Fatalf("begin legacy session: %v", err)
	}
	backend.put(projectObject(generation.LeasePath), []byte("replacement"), 999, nil)
	if err := release(context.Background()); err == nil || !strings.Contains(err.Error(), "lease cleanup failed") {
		t.Fatalf("release error = %v, want sanitized lease cleanup failure", err)
	}
}

func TestLegacyLeaseReleaseRetriesWithFreshContextAfterCallerCancellation(t *testing.T) {
	backend := &legacyLeaseProbeBackend{memoryBackend: &memoryBackend{objects: make(map[string]backendObject), nextGeneration: 1000}, deleteFailures: 1}
	client := &Client{userID: "user", projectID: "project", backend: backend}
	_, release, err := client.BeginLegacyGenerationWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	caller, cancel := context.WithCancel(context.Background())
	cancel()
	if err := release(caller); err != nil {
		t.Fatalf("release after caller cancellation = %v", err)
	}
	if backend.deleteAttempts != 2 {
		t.Fatalf("delete attempts = %d, want first timeout then success", backend.deleteAttempts)
	}
	for i, err := range backend.deleteErrs {
		if err != nil || !backend.deleteDeadlines[i] {
			t.Fatalf("attempt %d context err=%v deadline=%v", i+1, err, backend.deleteDeadlines[i])
		}
	}
}

func TestLegacyWriteJoinsPrimaryAndLeaseCleanupFailure(t *testing.T) {
	backend := &legacyLeaseProbeBackend{memoryBackend: &memoryBackend{objects: make(map[string]backendObject), nextGeneration: 1000}, deleteFailures: 3, failGenerated: true}
	client := &Client{userID: "user", projectID: "project", backend: backend}
	err := func() error {
		_, err := client.WriteBytes(context.Background(), []byte("new"), "wiki/fail.md")
		return err
	}()
	if err == nil || !strings.Contains(err.Error(), "primary write failed") || !strings.Contains(err.Error(), "lease cleanup failed") {
		t.Fatalf("joined legacy write error = %v", err)
	}
}

func TestLegacyManifestRaceJoinsLeaseCleanupFailure(t *testing.T) {
	backend := &legacyLeaseProbeBackend{memoryBackend: &memoryBackend{objects: make(map[string]backendObject), nextGeneration: 1000}, deleteFailures: 3, failGenerated: true}
	backend.manifestAfterLeaseWrite = manifestBytes(t, "generation-one", map[string]backendObject{"wiki/current.md": {Data: []byte("current"), Generation: 101}})
	client := &Client{userID: "user", projectID: "project", backend: backend}
	_, err := client.WriteBytes(context.Background(), []byte("new"), "wiki/race.md")
	if err == nil || !errors.Is(err, store.ErrGenerationManaged) || !strings.Contains(err.Error(), "lease cleanup failed") {
		t.Fatalf("manifest race error = %v", err)
	}
}

func TestLegacyManifestStateFailureIsNotManagedAndCleansLease(t *testing.T) {
	backend := &legacyLeaseProbeBackend{memoryBackend: &memoryBackend{
		objects: make(map[string]backendObject), nextGeneration: 1000,
		manifestReadErr: errors.New("provider state unavailable"),
	}}
	client := &Client{userID: "user", projectID: "project", backend: backend}
	_, err := client.WriteBytes(context.Background(), []byte("new"), "wiki/state.md")
	if !errors.Is(err, store.ErrGenerationStateUnavailable) || errors.Is(err, store.ErrGenerationManaged) {
		t.Fatalf("state failure error = %v, want state-unavailable only", err)
	}
	if _, err := backend.Read(context.Background(), projectObject(generation.LeasePath), 0, generation.MaxFileBytes); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Fatalf("lease remains after state failure: %v", err)
	}

	cleanupBackend := &legacyLeaseProbeBackend{memoryBackend: &memoryBackend{
		objects: make(map[string]backendObject), nextGeneration: 1000,
		manifestReadErr: errors.New("provider state unavailable"),
	}, deleteFailures: 3}
	client.backend = cleanupBackend
	_, err = client.WriteBytes(context.Background(), []byte("new"), "wiki/state-cleanup.md")
	if !errors.Is(err, store.ErrGenerationStateUnavailable) || !errors.Is(err, store.ErrLeaseCleanup) || errors.Is(err, store.ErrGenerationManaged) {
		t.Fatalf("state plus cleanup error = %v, want joined sanitized sentinels", err)
	}
}

func TestWriteBytesAtomicRejectsGeneratedTemporaryPathBeforeCanonicalWrite(t *testing.T) {
	client, backend := newMemoryClient()
	seedManifest(t, backend, "generation-one", map[string]backendObject{
		"wiki/current.md": {Data: []byte("current"), Generation: 101},
	})

	_, err := client.WriteBytesAtomic(context.Background(), []byte("rogue"), "wiki/rogue.md", "raw/allowed.md")
	if !errors.Is(err, store.ErrGenerationManaged) {
		t.Fatalf("WriteBytesAtomic() error = %v, want ErrGenerationManaged", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	for _, name := range []string{projectObject("wiki/rogue.md"), projectObject("raw/allowed.md")} {
		if _, ok := backend.objects[name]; ok {
			t.Fatalf("WriteBytesAtomic() wrote %q before rejecting generated temporary path", name)
		}
	}
}

func TestLegacyGeneratedWriteAutoAcquiresAndReleasesSharedLease(t *testing.T) {
	client, backend := newMemoryClient()
	ctx := context.Background()

	if _, err := client.WriteBytes(ctx, []byte("legacy"), "wiki/legacy.md"); err != nil {
		t.Fatalf("unleased legacy WriteBytes() error = %v, want success", err)
	}
	data, err := client.ReadFile(ctx, "wiki/legacy.md")
	if err != nil || string(data) != "legacy" {
		t.Fatalf("legacy generated cache = %q, %v", data, err)
	}
	if exists, err := client.HasCurrentManifest(ctx); err != nil || exists {
		t.Fatalf("legacy write manifest state = exists:%t err:%v, want absent", exists, err)
	}
	backend.mu.Lock()
	_, leaseExists := backend.objects[projectObject(generation.LeasePath)]
	backend.mu.Unlock()
	if leaseExists {
		t.Fatal("legacy write left the shared publish lease behind")
	}
}

func TestCreateOnlyCASUsesDoesNotExistAndNeverOverwrites(t *testing.T) {
	zero := createOrGenerationCondition(0)
	if !zero.DoesNotExist || zero.GenerationMatch != nil {
		t.Fatalf("zero condition = %#v, want DoesNotExist", zero)
	}
	if got := gcsConditions(zero); !got.DoesNotExist || got.GenerationMatch != 0 {
		t.Fatalf("GCS zero condition = %#v, want DoesNotExist", got)
	}
	matched := createOrGenerationCondition(42)
	if matched.DoesNotExist || matched.GenerationMatch == nil || *matched.GenerationMatch != 42 {
		t.Fatalf("nonzero condition = %#v, want GenerationMatch(42)", matched)
	}

	client, backend := newMemoryClient()
	if _, err := client.WriteFileIfGeneration(context.Background(), []byte("first"), "cache/annotations/a.json", 0); err != nil {
		t.Fatalf("create-only write: %v", err)
	}
	if _, err := client.WriteFileIfGeneration(context.Background(), []byte("second"), "cache/annotations/a.json", 0); !errors.Is(err, store.ErrGenerationMismatch) {
		t.Fatalf("concurrent create error = %v, want ErrGenerationMismatch", err)
	}
	object, err := backend.Read(context.Background(), projectObject("cache/annotations/a.json"), 0, generation.MaxFileBytes)
	if err != nil || string(object.Data) != "first" {
		t.Fatalf("concurrent create overwrote object = %q, %v", object.Data, err)
	}
	if _, err := client.WriteFileIfGeneration(context.Background(), []byte("updated"), "cache/annotations/a.json", object.Generation); err != nil {
		t.Fatalf("nonzero generation CAS: %v", err)
	}
}

func TestGenerationManifestFailuresCloseGeneratedReadsAndQuotaIgnoresHistory(t *testing.T) {
	client, backend := newMemoryClient()
	current := map[string]backendObject{"wiki/current.md": {Data: []byte("current"), Generation: 101}}
	seedManifest(t, backend, "generation-current", current)
	backend.put(projectObject("raw/input.md"), []byte("raw"), 1, nil)
	backend.put(projectObject("cache/source_status.json"), []byte("status"), 2, nil)
	backend.put(projectObject("wiki/current.md"), []byte("invisible legacy copy"), 3, nil)
	for i := 0; i < 3; i++ {
		backend.put(projectObject(path.Join(generation.Prefix, "generation-old-"+string(rune('a'+i)), "wiki/current.md")), []byte("historical duplicate"), int64(20+i), nil)
	}

	bytes, count, err := client.BucketStats(context.Background())
	if err != nil {
		t.Fatalf("BucketStats: %v", err)
	}
	if wantBytes, wantCount := int64(len("current")+len("raw")+len("status")), int64(3); bytes != wantBytes || count != wantCount {
		t.Fatalf("BucketStats = (%d, %d), want (%d, %d)", bytes, count, wantBytes, wantCount)
	}

	for _, invalid := range [][]byte{
		[]byte(`not json`),
		[]byte(`{"version":1,"generation_id":"unsafe","created_at":"2026-07-18T00:00:00Z","input_fingerprint":"x","files":[{"path":"wiki/../secret","size":1,"sha256":"` + strings.Repeat("a", 64) + `","generation":1}]}`),
		[]byte(`{"version":1,"generation_id":"generation-current","created_at":"2026-07-18T00:00:00Z","input_fingerprint":"x","files":[{"path":"wiki/a.md","size":1,"sha256":"` + strings.Repeat("a", 64) + `","generation":1},{"path":"wiki/a.md","size":1,"sha256":"` + strings.Repeat("a", 64) + `","generation":2}]}`),
	} {
		backend.put(projectObject(generation.ManifestPath), invalid, 7, nil)
		if _, err := client.ReadFile(context.Background(), "wiki/current.md"); err == nil {
			t.Fatalf("ReadFile accepted malformed manifest %q", invalid)
		}
	}
	for _, mutate := range []func(*generation.Manifest){
		func(manifest *generation.Manifest) { manifest.Files[0].Size++ },
		func(manifest *generation.Manifest) { manifest.Files[0].Generation++ },
		func(manifest *generation.Manifest) { manifest.Files[0].Size = generation.MaxFileBytes + 1 },
	} {
		var manifest generation.Manifest
		if err := json.Unmarshal(manifestBytes(t, "generation-current", current), &manifest); err != nil {
			t.Fatal(err)
		}
		mutate(&manifest)
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		backend.put(projectObject(generation.ManifestPath), data, 7, nil)
		if _, err := client.ReadFile(context.Background(), "wiki/current.md"); err == nil {
			t.Fatal("ReadFile accepted declared generation size or generation mismatch")
		}
	}

	seedManifest(t, backend, "generation-current", current)
	backend.mu.Lock()
	delete(backend.objects, projectObject(path.Join(generation.Prefix, "generation-current", "wiki/current.md")))
	backend.mu.Unlock()
	if _, err := client.ReadFile(context.Background(), "wiki/current.md"); err == nil || errors.Is(err, storage.ErrObjectNotExist) {
		t.Fatalf("missing declared generation object error = %v, want distinct unavailable category", err)
	}
}

func TestDeclaredGenerationMissingFailsClosedWithoutDraftFallback(t *testing.T) {
	client, backend := newMemoryClient()
	files := map[string]backendObject{
		"wiki/missing.md":         {Data: []byte("published"), Generation: 101},
		"wiki/.drafts/missing.md": {Data: []byte("draft"), Generation: 102},
	}
	seedManifest(t, backend, "generation-fail-closed", files)
	backend.mu.Lock()
	delete(backend.objects, projectObject(path.Join(generation.Prefix, "generation-fail-closed", "wiki/missing.md")))
	backend.mu.Unlock()
	_, _, err := client.GetPage(context.Background(), "missing", "concepts")
	if !errors.Is(err, store.ErrDeclaredObjectUnavailable) || errors.Is(err, storage.ErrObjectNotExist) {
		t.Fatalf("declared missing page error = %v, want fail-closed category", err)
	}
}

func TestManifestPathAbsentPreservesPublishedToDraftFallback(t *testing.T) {
	client, backend := newMemoryClient()
	files := map[string]backendObject{
		"wiki/.drafts/missing.md": {Data: []byte("draft"), Generation: 102},
	}
	seedManifest(t, backend, "generation-draft-fallback", files)
	page, data, err := client.GetPage(context.Background(), "missing", "concepts")
	if err != nil || page == nil || page.Status != "draft" || string(data) != "draft" {
		t.Fatalf("absent manifest path fallback page=%+v data=%q err=%v", page, data, err)
	}
}

func TestBucketStatsStopsManifestCanonicalIterationAtLimit(t *testing.T) {
	client, backend := newMemoryClient()
	seedManifest(t, backend, "generation-current", map[string]backendObject{
		"wiki/current.md": {Data: []byte("current"), Generation: 101},
	})
	for i := 0; i <= generation.MaxFiles; i++ {
		backend.put(projectObject(fmt.Sprintf("raw/%05d.md", i)), []byte("x"), int64(i+1), nil)
	}
	if _, _, err := client.BucketStats(context.Background()); err == nil || !strings.Contains(err.Error(), "bucket object limit") {
		t.Fatalf("BucketStats oversized canonical set error = %v", err)
	}
	backend.mu.Lock()
	visits := backend.listVisits
	backend.mu.Unlock()
	if visits != generation.MaxFiles+1 {
		t.Fatalf("BucketStats visits = %d, want hard stop at %d", visits, generation.MaxFiles+1)
	}
}

func TestManifestBucketStatsRejectsNegativeCanonicalSize(t *testing.T) {
	client, backend := newMemoryClient()
	seedManifest(t, backend, "generation-current", map[string]backendObject{
		"wiki/current.md": {Data: []byte("current"), Generation: 101},
	})
	name := projectObject("raw/input.md")
	backend.put(name, []byte("input"), 102, nil)
	backend.mu.Lock()
	object := backend.objects[name]
	object.Size = -1
	backend.objects[name] = object
	backend.mu.Unlock()
	if _, _, err := client.BucketStats(context.Background()); err == nil || !strings.Contains(err.Error(), "bucket byte limit") {
		t.Fatalf("negative canonical size error = %v", err)
	}
}

func TestWriteBytesAtomicBackendUsesProductionEquivalentFinalCAS(t *testing.T) {
	client, backend := newMemoryClient()
	if _, err := client.WriteBytesAtomic(context.Background(), []byte("first"), "cache/x.tmp", "raw/x"); err != nil {
		t.Fatalf("create atomic write: %v", err)
	}
	if got := backend.writeConditions[len(backend.writeConditions)-1]; !got.DoesNotExist || got.GenerationMatch != nil {
		t.Fatalf("create final condition = %#v, want DoesNotExist", got)
	}

	backend.put(projectObject("raw/x"), []byte("prior"), 77, nil)
	if _, err := client.WriteBytesAtomic(context.Background(), []byte("updated"), "cache/x.tmp", "raw/x"); err != nil {
		t.Fatalf("update atomic write: %v", err)
	}
	got := backend.writeConditions[len(backend.writeConditions)-1]
	if got.DoesNotExist || got.GenerationMatch == nil || *got.GenerationMatch != 77 {
		t.Fatalf("update final condition = %#v, want GenerationMatch(77)", got)
	}
}

type racingAttrsBackend struct {
	*memoryBackend
	mu       sync.Mutex
	attrs    int
	released chan struct{}
}

func (b *racingAttrsBackend) Attrs(ctx context.Context, name string, generation int64) (backendObject, error) {
	if strings.HasSuffix(name, "/raw/race") {
		object, err := b.memoryBackend.Attrs(ctx, name, generation)
		b.mu.Lock()
		b.attrs++
		if b.attrs == 2 {
			close(b.released)
		}
		b.mu.Unlock()
		<-b.released
		return object, err
	}
	return b.memoryBackend.Attrs(ctx, name, generation)
}

func TestWriteBytesAtomicBackendRejectsConcurrentCreateWithCAS(t *testing.T) {
	base := &memoryBackend{objects: make(map[string]backendObject), nextGeneration: 1000}
	backend := &racingAttrsBackend{memoryBackend: base, released: make(chan struct{})}
	clients := []*Client{
		{userID: "user", projectID: "project", backend: backend},
		{userID: "user", projectID: "project", backend: backend},
	}
	errs := make(chan error, len(clients))
	for i, client := range clients {
		go func(i int, client *Client) {
			_, err := client.WriteBytesAtomic(context.Background(), []byte(fmt.Sprintf("data-%d", i)), fmt.Sprintf("cache/race-%d.tmp", i), "raw/race")
			errs <- err
		}(i, client)
	}
	var successes, conflicts int
	for range clients {
		switch err := <-errs; {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrGenerationMismatch):
			conflicts++
		default:
			t.Fatalf("concurrent atomic write error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent atomic writes successes=%d conflicts=%d, want one each", successes, conflicts)
	}
}

func TestBackendReadRejectsTamperedObjectSize(t *testing.T) {
	client, backend := newMemoryClient()
	name := projectObject("raw/tampered.md")
	backend.put(name, []byte("content"), 1, nil)
	backend.mu.Lock()
	object := backend.objects[name]
	object.Size++
	backend.objects[name] = object
	backend.mu.Unlock()
	if _, err := client.ReadFile(context.Background(), "raw/tampered.md"); err == nil {
		t.Fatal("tampered object size was accepted")
	}
}

func TestLegacyBucketStatsFailsClosedAtObjectAndByteBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		bytes int64
	}{
		{name: "object limit", count: expectedLegacyStatsObjects + 1, bytes: int64(expectedLegacyStatsObjects + 1)},
		{name: "byte limit", count: 2, bytes: generation.MaxTotalSize + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, backend := newMemoryClient()
			for i := 0; i < tc.count; i++ {
				size := int64(1)
				backend.put(projectObject(fmt.Sprintf("raw/%05d", i)), make([]byte, size), int64(i+1), nil)
				if tc.name == "byte limit" && i == 1 {
					backend.mu.Lock()
					object := backend.objects[projectObject(fmt.Sprintf("raw/%05d", i))]
					object.Size = generation.MaxTotalSize
					backend.objects[projectObject(fmt.Sprintf("raw/%05d", i))] = object
					backend.mu.Unlock()
				}
			}
			if _, _, err := client.BucketStats(context.Background()); err == nil {
				t.Fatal("oversized legacy stats succeeded")
			}
			backend.mu.Lock()
			visits := backend.listVisits
			backend.mu.Unlock()
			if visits > tc.count || visits > expectedLegacyStatsObjects+1 {
				t.Fatalf("legacy stats visits=%d, want bounded streaming", visits)
			}
		})
	}
}

const expectedLegacyStatsObjects = 1000
