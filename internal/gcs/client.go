package gcs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	fm "github.com/adrg/frontmatter"
	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Client wraps GCS operations for a specific user/project.
type Client struct {
	bucket           *storage.BucketHandle
	userID           string
	projectID        string
	backend          clientBackend
	view             *generationView
	legacyWriteLease *legacyGenerationLease
}

type WikiPage = store.WikiPage
type Project = store.Project
type MarkdownFile = store.MarkdownFile

type wikiPageFrontmatter struct {
	ID         string `yaml:"id"`
	Title      string `yaml:"title"`
	SourceFile string `yaml:"source_file"`
}

const (
	conceptsCachePath = "cache/concepts.jsonl"
	idMapCachePath    = "cache/id_map.json"
)

type conceptCacheEntry struct {
	Slug        string                 `json:"slug"`
	Title       string                 `json:"title"`
	Frontmatter map[string]interface{} `json:"frontmatter"`
}

type wikiIDMap struct {
	Source     map[string]string     `json:"source"`
	SourceMeta map[string]sourceMeta `json:"source_meta"`
}

type sourceMeta struct {
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	SourceFile string `json:"source_file"`
}

type generationView struct {
	manifest *generation.Manifest
	token    string
}

type legacyGenerationLease struct{ generation int64 }

const maxLegacyStatsObjects = 1000
const legacyLeaseReleaseAttempts = 3

var errBucketObjectLimit = errors.New("bucket object limit exceeded")
var errBucketByteLimit = errors.New("bucket byte limit exceeded")

// NewClient creates a new GCS client for the given bucket.
func NewClient(bucket string) (*Client, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage client: %w", err)
	}
	return &Client{
		bucket: client.Bucket(bucket),
	}, nil
}

// WithScope returns a client that shares the bucket connection but uses the
// supplied user/project prefix.
func (c *Client) WithScope(userID, projectID string) *Client {
	return &Client{
		bucket:           c.bucket,
		userID:           userID,
		projectID:        projectID,
		backend:          c.backend,
		view:             c.view,
		legacyWriteLease: c.legacyWriteLease,
	}
}

// BeginLegacyGenerationWrite serializes pre-manifest generated writes with the
// worker using the same create-only lease object. The returned scope is the
// only GCS scope permitted to write generated canonical paths.
func (c *Client) BeginLegacyGenerationWrite(ctx context.Context) (store.Store, func(context.Context) error, error) {
	leaseName := c.prefix() + "/" + generation.LeasePath
	attrs, err := c.writeObject(ctx, leaseName, []byte(`{"started_at":"redacted"}`), "application/json", nil, writeCondition{DoesNotExist: true})
	if err != nil {
		return nil, nil, store.ErrGenerationManaged
	}
	lease := legacyGenerationLease{generation: attrs.Generation}
	if exists, err := c.HasCurrentManifest(ctx); err != nil || exists {
		release := func(context.Context) error {
			return store.RetryGenerationCleanup(lease.generation, generation.LeaseReleaseTimeout, legacyLeaseReleaseAttempts, func(releaseCtx context.Context, objectGeneration int64) error {
				return c.deleteObject(releaseCtx, leaseName, objectGeneration)
			})
		}
		if err != nil {
			return nil, nil, errors.Join(store.ErrGenerationStateUnavailable, release(context.Background()))
		}
		return nil, nil, errors.Join(store.ErrGenerationManaged, release(context.Background()))
	}
	scoped := *c
	scoped.legacyWriteLease = &lease
	release := func(context.Context) error {
		return store.RetryGenerationCleanup(lease.generation, generation.LeaseReleaseTimeout, legacyLeaseReleaseAttempts, func(releaseCtx context.Context, objectGeneration int64) error {
			return c.deleteObject(releaseCtx, leaseName, objectGeneration)
		})
	}
	return &scoped, release, nil
}

func (c *Client) withGenerationWriteLease(ctx context.Context, paths []string, write func(*Client) error) error {
	managed := false
	for _, path := range paths {
		if generation.GenerationOwnedWritePath(path) {
			managed = true
			break
		}
	}
	if !managed {
		return write(c)
	}

	if c.legacyWriteLease != nil {
		exists, err := c.HasCurrentManifest(ctx)
		if err != nil {
			return err
		}
		if exists {
			return store.ErrGenerationManaged
		}
		return write(c)
	}

	lockedStore, release, err := c.BeginLegacyGenerationWrite(ctx)
	if err != nil {
		return err
	}
	locked, ok := lockedStore.(*Client)
	if !ok {
		return errors.Join(errors.New("invalid legacy generation write scope"), release(context.Background()))
	}
	writeErr := write(locked)
	releaseErr := release(context.Background())
	return errors.Join(writeErr, releaseErr)
}

// Scope returns a project-scoped storage interface while preserving the
// concrete WithScope API used by GCS-specific tools.
func (c *Client) Scope(userID, projectID string) store.Store {
	return c.WithScope(userID, projectID)
}

// Pin resolves current.json once and returns a copy whose generated reads use
// exactly that manifest (or the explicit legacy view) for the operation.
func (c *Client) Pin(ctx context.Context) (store.Store, error) {
	if c.view != nil {
		return c, nil
	}
	// Keep the zero-value client usable as the storage-neutral test placeholder.
	if c.bucket == nil && c.backend == nil {
		return c, nil
	}
	view, err := c.operationView(ctx)
	if err != nil {
		return nil, err
	}
	pinned := *c
	pinned.view = &view
	return &pinned, nil
}

func (c *Client) ViewToken() string {
	if c.view == nil {
		return ""
	}
	return c.view.token
}

// NewScopedClient returns a client that shares the bucket connection but uses
// the supplied user/project prefix.
func (c *Client) NewScopedClient(userID, projectID string) *Client {
	return c.WithScope(userID, projectID)
}

func (c *Client) prefix() string {
	return store.ProjectPrefix(c.userID, c.projectID)
}

// ListSources returns all compiled wiki sources.
func (c *Client) ListSources(ctx context.Context) ([]WikiPage, error) {
	view, err := c.operationView(ctx)
	if err != nil {
		return nil, err
	}
	return c.listDir(ctx, view, "wiki/sources/")
}

// ListConcepts returns wiki concepts. Drafts are included when includeDrafts is true.
func (c *Client) ListConcepts(ctx context.Context, includeDrafts bool) ([]WikiPage, error) {
	view, err := c.operationView(ctx)
	if err != nil {
		return nil, err
	}
	// Always list both dirs to work around GCS iterator issue with directOnly
	published, err := c.listConceptDir(ctx, view, "wiki/", "published", false)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(published))
	for _, page := range published {
		seen[page.Slug] = struct{}{}
	}

	drafts, err := c.listConceptDir(ctx, view, "wiki/.drafts/", "draft", false)
	if err != nil {
		return nil, err
	}
	for _, page := range drafts {
		if _, ok := seen[page.Slug]; ok {
			continue
		}
		published = append(published, page)
		seen[page.Slug] = struct{}{}
	}

	if !includeDrafts {
		result := make([]WikiPage, 0, len(published))
		for _, p := range published {
			if p.Status == "published" {
				result = append(result, p)
			}
		}
		return result, nil
	}
	return published, nil
}

// ListConceptsFromCache returns wiki concepts from the generated JSONL cache.
func (c *Client) ListConceptsFromCache(ctx context.Context) ([]WikiPage, error) {
	view, err := c.operationView(ctx)
	if err != nil {
		return nil, err
	}
	data, err := c.readFileWithView(ctx, conceptsCachePath, view)
	if err != nil {
		return nil, err
	}
	return WikiPagesFromConceptsJSONL(data)
}

// ListSourcesFromCache returns wiki sources from the generated ID map cache.
func (c *Client) ListSourcesFromCache(ctx context.Context) ([]WikiPage, error) {
	view, err := c.operationView(ctx)
	if err != nil {
		return nil, err
	}
	data, err := c.readFileWithView(ctx, idMapCachePath, view)
	if err != nil {
		return nil, err
	}
	return WikiPagesFromSourceIDMap(data)
}

// GetPage reads a wiki page by slug from sources or concepts.
func (c *Client) GetPage(ctx context.Context, slug, category string) (*WikiPage, []byte, error) {
	view, err := c.operationView(ctx)
	if err != nil {
		return nil, nil, err
	}
	switch category {
	case "sources":
		return c.getPageFromDir(ctx, view, slug, "wiki/sources/", "")
	case "concepts":
		page, data, err := c.getPageFromDir(ctx, view, slug, "wiki/", "published")
		if err == nil {
			return page, data, nil
		}
		if !errors.Is(err, storage.ErrObjectNotExist) {
			return nil, nil, err
		}
		return c.getPageFromDir(ctx, view, slug, "wiki/.drafts/", "draft")
	default:
		return nil, nil, fmt.Errorf("unknown category: %s", category)
	}
}

// ReadMetaIndex reads the generated wiki metadata index.
func (c *Client) ReadMetaIndex(ctx context.Context) (string, error) {
	data, err := c.ReadFile(ctx, "meta/index.md")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *Client) getPageFromDir(ctx context.Context, view generationView, slug, dir, status string) (*WikiPage, []byte, error) {
	rel := fmt.Sprintf("%s%s.md", dir, slug)
	data, err := c.readFileWithView(ctx, rel, view)
	if err != nil {
		return nil, nil, err
	}

	return &WikiPage{
		Slug:   slug,
		Title:  slug,
		Path:   rel,
		Status: status,
	}, data, nil
}

// ReadRaw reads a raw source file.
func (c *Client) ReadRaw(ctx context.Context, name string) ([]byte, error) {
	path := fmt.Sprintf("%s/raw/%s", c.prefix(), name)
	object, err := c.readObject(ctx, path, 0, generation.MaxFileBytes)
	if err != nil {
		return nil, fmt.Errorf("read raw %s: %w", path, err)
	}
	return object.Data, nil
}

// ReadFile reads any file under the user/project prefix.
func (c *Client) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	if !generation.GenerationOwned(relPath) {
		return c.readFileWithView(ctx, relPath, generationView{})
	}
	view, err := c.operationView(ctx)
	if err != nil {
		return nil, err
	}
	return c.readFileWithView(ctx, relPath, view)
}

func (c *Client) readFileWithView(ctx context.Context, relPath string, view generationView) ([]byte, error) {
	path, expected, err := c.resolveReadPath(relPath, view)
	if err != nil {
		return nil, err
	}
	object, err := c.readObject(ctx, path, expected, readLimitForView(relPath, view))
	if err != nil {
		if objectNotFound(err) {
			if view.manifest != nil {
				return nil, store.ErrDeclaredObjectUnavailable
			}
			return nil, storage.ErrObjectNotExist
		}
		if view.manifest != nil {
			return nil, store.ErrDeclaredObjectUnavailable
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := c.verifyGenerationRead(relPath, view, object); err != nil {
		return nil, err
	}
	return object.Data, nil
}

func (c *Client) ReadFileWithGeneration(ctx context.Context, relPath string) ([]byte, int64, error) {
	view := generationView{}
	var err error
	if generation.GenerationOwned(relPath) {
		view, err = c.operationView(ctx)
		if err != nil {
			return nil, 0, err
		}
	}
	path, expected, err := c.resolveReadPath(relPath, view)
	if err != nil {
		return nil, 0, err
	}
	object, err := c.readObject(ctx, path, expected, readLimitForView(relPath, view))
	if err != nil {
		if objectNotFound(err) {
			if view.manifest != nil {
				return nil, 0, store.ErrDeclaredObjectUnavailable
			}
			return nil, 0, storage.ErrObjectNotExist
		}
		if view.manifest != nil {
			return nil, 0, store.ErrDeclaredObjectUnavailable
		}
		return nil, 0, fmt.Errorf("read %s: %w", path, err)
	}
	if err := c.verifyGenerationRead(relPath, view, object); err != nil {
		return nil, 0, err
	}
	return object.Data, object.Generation, nil
}

func (c *Client) verifyGenerationRead(relPath string, view generationView, object backendObject) error {
	if view.manifest == nil {
		return nil
	}
	file, ok := view.manifest.File(relPath)
	if !ok || file.Generation != object.Generation || file.Size != int64(len(object.Data)) {
		return store.ErrDeclaredObjectUnavailable
	}
	sum := sha256.Sum256(object.Data)
	if fmt.Sprintf("%x", sum[:]) != file.SHA256 {
		return store.ErrDeclaredObjectUnavailable
	}
	return nil
}

func readLimitForView(relPath string, view generationView) int64 {
	if view.manifest != nil {
		if file, ok := view.manifest.File(relPath); ok && file.Size < generation.MaxFileBytes {
			return file.Size
		}
	}
	return generation.MaxFileBytes
}

// HasCurrentManifest is deliberately a cheap existence check for write paths.
func (c *Client) HasCurrentManifest(ctx context.Context) (bool, error) {
	_, err := c.objectAttrs(ctx, c.prefix()+"/"+generation.ManifestPath, 0)
	if objectNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) currentManifest(ctx context.Context) (generation.Manifest, int64, bool, error) {
	object, err := c.readObject(ctx, c.prefix()+"/"+generation.ManifestPath, 0, generation.MaxManifestBytes)
	if objectNotFound(err) {
		return generation.Manifest{}, 0, false, nil
	}
	if err != nil {
		return generation.Manifest{}, 0, false, fmt.Errorf("read generation manifest: %w", err)
	}
	m, err := generation.Decode(object.Data)
	if err != nil {
		return generation.Manifest{}, 0, true, err
	}
	return m, object.Generation, true, nil
}

func (c *Client) captureGenerationView(ctx context.Context) (generationView, error) {
	m, manifestGeneration, exists, err := c.currentManifest(ctx)
	if err != nil {
		return generationView{}, err
	}
	if !exists {
		return generationView{token: "legacy"}, nil
	}
	return generationView{manifest: &m, token: fmt.Sprintf("manifest-%d", manifestGeneration)}, nil
}

func (c *Client) operationView(ctx context.Context) (generationView, error) {
	if c.view != nil {
		return *c.view, nil
	}
	return c.captureGenerationView(ctx)
}

func (c *Client) resolveReadPath(relPath string, view generationView) (string, int64, error) {
	if !generation.GenerationOwned(relPath) {
		if generationNamespacePath(relPath) && !canonicalReadPath(relPath) {
			return "", 0, store.ErrDeclaredObjectUnavailable
		}
		return c.prefix() + "/" + relPath, 0, nil
	}
	if view.manifest == nil {
		return c.prefix() + "/" + relPath, 0, nil
	}
	file, ok := view.manifest.File(relPath)
	if !ok {
		return "", 0, storage.ErrObjectNotExist
	}
	return c.prefix() + "/" + view.manifest.ObjectPath(file), file.Generation, nil
}

func generationNamespacePath(relPath string) bool {
	return relPath == "wiki" || strings.HasPrefix(relPath, "wiki/") ||
		relPath == "cache" || strings.HasPrefix(relPath, "cache/") ||
		relPath == ".olw" || strings.HasPrefix(relPath, ".olw/") ||
		relPath == "wiki.toml" || strings.HasPrefix(relPath, "wiki.toml/")
}

func canonicalReadPath(relPath string) bool {
	if store.SafeRawPath(relPath) || relPath == "cache/source_status.json" {
		return true
	}
	if strings.HasPrefix(relPath, "cache/annotations/") && strings.HasSuffix(relPath, ".json") {
		sourceID := strings.TrimSuffix(strings.TrimPrefix(relPath, "cache/annotations/"), ".json")
		return annotation.ValidSourceID(sourceID)
	}
	if strings.HasPrefix(relPath, "cache/pipeline-") && strings.HasSuffix(relPath, ".log") {
		executionID := strings.TrimSuffix(strings.TrimPrefix(relPath, "cache/pipeline-"), ".log")
		return executionID != "" && executionID != "." && executionID != ".." && path.Clean(relPath) == relPath && !strings.ContainsAny(executionID, `/\\`) && !strings.Contains(executionID, "..")
	}
	return false
}

func currentGenerationFiles(view generationView, relPrefix string) ([]generation.File, bool) {
	if view.manifest == nil {
		return nil, false
	}
	files := make([]generation.File, 0)
	for _, file := range view.manifest.Files {
		if strings.HasPrefix(file.Path, relPrefix) {
			files = append(files, file)
		}
	}
	return files, true
}

func objectNotFound(err error) bool {
	return errors.Is(err, storage.ErrObjectNotExist) || status.Code(err) == codes.NotFound
}

func (c *Client) WriteFileIfGeneration(ctx context.Context, data []byte, relPath string, expected int64) (int64, error) {
	var result int64
	err := c.withGenerationWriteLease(ctx, []string{relPath}, func(writer *Client) error {
		var annotationMeta struct {
			SHA256  string `json:"ann_sha256"`
			RawPath string `json:"raw_path"`
			Body    string `json:"body"`
		}
		_ = json.Unmarshal(data, &annotationMeta)
		metadata := map[string]string(nil)
		if annotationMeta.SHA256 != "" {
			metadata = map[string]string{"ann_sha256": annotationMeta.SHA256, "raw_path": annotationMeta.RawPath, "has_annotation": fmt.Sprintf("%t", annotationMeta.Body != "")}
		}
		attrs, err := writer.writeObject(ctx, fmt.Sprintf("%s/%s", writer.prefix(), relPath), data, contentTypeForPath(relPath), metadata, createOrGenerationCondition(expected))
		if err != nil {
			return conditionalWriteError(err)
		}
		result = attrs.Generation
		return nil
	})
	return result, err
}

// conditionalWriteError normalizes the two error surfaces used by GCS clients:
// gRPC transports return FailedPrecondition while storage.NewClient returns a
// googleapi HTTP 412 error for an ifGenerationMatch conflict.
func conditionalWriteError(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) == codes.FailedPrecondition {
		return store.ErrGenerationMismatch
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == 412 {
		return store.ErrGenerationMismatch
	}
	return err
}

func (c *Client) ListObjectMeta(ctx context.Context, relPrefix string) ([]store.ObjectMeta, error) {
	prefix := fmt.Sprintf("%s/%s", c.prefix(), relPrefix)
	objects, err := c.listObjects(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var out []store.ObjectMeta
	for _, attrs := range objects {
		out = append(out, store.ObjectMeta{Path: strings.TrimPrefix(attrs.Name, c.prefix()+"/"), Generation: attrs.Generation, Updated: attrs.Updated.UTC(), SHA256: attrs.Metadata["ann_sha256"], RawPath: attrs.Metadata["raw_path"], HasAnnotation: attrs.Metadata["has_annotation"] == "true"})
	}
	return out, nil
}

// ListMarkdownFiles reads direct .md files under dir, relative to the
// user/project prefix. Nested files are ignored.
func (c *Client) ListMarkdownFiles(ctx context.Context, dir string) ([]MarkdownFile, error) {
	view := generationView{}
	var err error
	if strings.HasPrefix(dir, "wiki/") {
		view, err = c.operationView(ctx)
		if err != nil {
			return nil, err
		}
	}
	if files, exists := currentGenerationFiles(view, dir); exists {
		out := make([]MarkdownFile, 0, len(files))
		for _, file := range files {
			rel := strings.TrimPrefix(file.Path, dir)
			if !strings.HasSuffix(file.Path, ".md") || rel == "" || strings.Contains(rel, "/") {
				continue
			}
			data, err := c.readFileWithView(ctx, file.Path, view)
			if err != nil {
				return nil, err
			}
			out = append(out, MarkdownFile{Slug: strings.TrimSuffix(rel, ".md"), Path: file.Path, Data: data})
		}
		return out, nil
	}
	prefix := fmt.Sprintf("%s/%s", c.prefix(), dir)
	objects, err := c.listObjects(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var files []MarkdownFile
	for _, attrs := range objects {
		if !strings.HasSuffix(attrs.Name, ".md") {
			continue
		}
		rel := strings.TrimPrefix(attrs.Name, prefix)
		if rel == attrs.Name || rel == "" || strings.Contains(rel, "/") {
			continue
		}

		data, err := c.readFileWithView(ctx, dir+rel, view)
		if err != nil {
			return nil, err
		}
		files = append(files, MarkdownFile{
			Slug: strings.TrimSuffix(rel, ".md"),
			Path: dir + rel,
			Data: data,
		})
	}
	return files, nil
}

func (c *Client) ListRawFiles(ctx context.Context) ([]store.RawFile, error) {
	prefix := c.prefix() + "/raw/"
	objects, err := c.listObjects(ctx, prefix)
	if err != nil {
		return nil, err
	}
	files := make([]store.RawFile, 0)
	for _, attrs := range objects {
		name, ok := c.rawFileNameFromObject(attrs.Name)
		if !ok {
			continue
		}
		files = append(files, store.RawFile{
			Name:    name,
			Path:    "raw/" + name,
			Size:    attrs.Size,
			Updated: attrs.Updated.UTC(),
			SHA256:  attrs.Metadata["sha256"],
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})
	return files, nil
}

func (c *Client) rawFileNameFromObject(objectName string) (string, bool) {
	prefix := c.prefix() + "/raw/"
	if !strings.HasPrefix(objectName, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(objectName, prefix)
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// listDir lists .md files under the given directory prefix.
func (c *Client) listDir(ctx context.Context, view generationView, dir string) ([]WikiPage, error) {
	if files, exists := currentGenerationFiles(view, dir); exists {
		pages := make([]WikiPage, 0, len(files))
		for _, file := range files {
			if !strings.HasSuffix(file.Path, ".md") {
				continue
			}
			slug := strings.TrimSuffix(strings.TrimPrefix(file.Path, dir), ".md")
			if slug == "" || strings.Contains(slug, "/") {
				continue
			}
			page := WikiPage{Slug: slug, Title: slug, Path: file.Path}
			data, err := c.readFileWithView(ctx, file.Path, view)
			if err != nil {
				return nil, err
			}
			page, err = applyWikiPageFrontmatter(page, data)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", page.Path, err)
			}
			pages = append(pages, page)
		}
		return pages, nil
	}
	prefix := fmt.Sprintf("%s/%s", c.prefix(), dir)
	objects, err := c.listObjects(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var pages []WikiPage
	for _, attrs := range objects {
		if !strings.HasSuffix(attrs.Name, ".md") {
			continue
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(attrs.Name, prefix), ".md")
		page := WikiPage{
			Slug:  slug,
			Title: slug,
			Path:  fmt.Sprintf("%s%s.md", dir, slug),
		}
		data, err := c.readFileWithView(ctx, page.Path, view)
		if err != nil {
			return nil, err
		}
		page, err = applyWikiPageFrontmatter(page, data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page.Path, err)
		}
		pages = append(pages, page)
	}
	return pages, nil
}

func applyWikiPageFrontmatter(page WikiPage, data []byte) (WikiPage, error) {
	if !strings.HasPrefix(string(data), "---") {
		return page, nil
	}

	var matter wikiPageFrontmatter
	if _, err := fm.MustParse(strings.NewReader(string(data)), &matter); err != nil {
		return page, err
	}
	if id := strings.TrimSpace(matter.ID); id != "" {
		page.ID = id
	}
	if title := strings.TrimSpace(matter.Title); title != "" {
		page.Title = title
	}
	if raw := strings.TrimSpace(matter.SourceFile); raw != "" {
		page.RawPath = raw
	}
	return page, nil
}

func WikiPagesFromConceptsJSONL(data []byte) ([]WikiPage, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	pages := make([]WikiPage, 0)
	lineNumber := 0
	rows := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if rows >= generation.MaxFiles {
			return nil, generation.ErrLogicalEntryLimit
		}
		rows++

		var entry conceptCacheEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("decode concepts cache line %d: %w", lineNumber, err)
		}
		slug := strings.TrimSpace(entry.Slug)
		if slug == "" {
			continue
		}
		title := strings.TrimSpace(entry.Title)
		if title == "" {
			title = slug
		}
		pages = append(pages, WikiPage{
			Slug:   slug,
			Title:  title,
			ID:     frontmatterStringValue(entry.Frontmatter, "id"),
			Path:   fmt.Sprintf("wiki/%s.md", slug),
			Status: "published",
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan concepts cache: %w", err)
	}
	return pages, nil
}

func WikiPagesFromSourceIDMap(data []byte) ([]WikiPage, error) {
	source, err := decodeWikiIDMap(data)
	if err != nil {
		return nil, fmt.Errorf("decode source id map: %w", err)
	}

	pages := make([]WikiPage, 0, len(source.Source))
	for id, slug := range source.Source {
		id = strings.TrimSpace(id)
		slug = strings.TrimSpace(slug)
		if id == "" || slug == "" {
			continue
		}
		meta := source.SourceMeta[id]
		title := strings.TrimSpace(meta.Title)
		if title == "" {
			title = slug
		}
		pages = append(pages, WikiPage{
			Slug:    slug,
			Title:   title,
			ID:      id,
			Path:    fmt.Sprintf("wiki/sources/%s.md", slug),
			RawPath: strings.TrimSpace(meta.SourceFile),
		})
	}
	sort.Slice(pages, func(i, j int) bool {
		return pages[i].Slug < pages[j].Slug
	})
	return pages, nil
}

func decodeWikiIDMap(data []byte) (wikiIDMap, error) {
	var source wikiIDMap
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return source, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return source, errors.New("expected JSON object")
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return source, err
		}
		name, ok := key.(string)
		if !ok {
			return source, errors.New("expected JSON object key")
		}
		switch name {
		case "source":
			source.Source, err = generation.DecodeBoundedMap[string](dec)
		case "source_meta":
			source.SourceMeta, err = generation.DecodeBoundedMap[sourceMeta](dec)
		case "concept":
			_, err = generation.DecodeBoundedMap[string](dec)
		case "redirects":
			_, err = decodeBoundedRedirects(dec)
		default:
			var ignored json.RawMessage
			err = dec.Decode(&ignored)
		}
		if err != nil {
			return source, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return source, err
	}
	if err := generation.EnsureJSONEOF(dec); err != nil {
		return source, err
	}
	return source, nil
}

func decodeBoundedRedirects(dec *json.Decoder) (map[string][]string, error) {
	return generation.DecodeBoundedStringLists(dec)
}

func frontmatterStringValue(frontmatter map[string]interface{}, key string) string {
	if len(frontmatter) == 0 {
		return ""
	}
	value, ok := frontmatter[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

// UploadFile uploads a local file to GCS under the user/project prefix.
// gcsRelPath is the path relative to the prefix (e.g., "wiki/陽明山.md").
// Returns the SHA256 hex digest of the uploaded content.
func (c *Client) UploadFile(ctx context.Context, localPath, gcsRelPath string) (string, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", localPath, err)
	}
	return c.uploadBytes(ctx, data, gcsRelPath)
}

// UploadFileWithDigest is like UploadFile but accepts pre-computed content + digest.
func (c *Client) UploadFileWithDigest(ctx context.Context, localPath, gcsRelPath, digest string) (string, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", localPath, err)
	}
	return digest, c.uploadBytesWithDigest(ctx, data, gcsRelPath, digest)
}

func (c *Client) uploadBytes(ctx context.Context, data []byte, gcsRelPath string) (string, error) {
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	return digest, c.uploadBytesWithDigest(ctx, data, gcsRelPath, digest)
}

func (c *Client) uploadBytesWithDigest(ctx context.Context, data []byte, gcsRelPath, digest string) error {
	return c.withGenerationWriteLease(ctx, []string{gcsRelPath}, func(writer *Client) error {
		path := fmt.Sprintf("%s/%s", writer.prefix(), gcsRelPath)
		_, err := writer.writeObject(ctx, path, data, contentTypeForPath(gcsRelPath), map[string]string{
			"sha256": digest,
		}, writeCondition{})
		if err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		return nil
	})
}

// WriteBytes uploads bytes to GCS under the user/project prefix and returns the
// SHA256 digest. gcsRelPath is relative to the prefix (e.g., "raw/my-note.md").
func (c *Client) WriteBytes(ctx context.Context, data []byte, gcsRelPath string) (string, error) {
	return c.uploadBytes(ctx, data, gcsRelPath)
}

// WriteBytesAtomic uploads bytes to a temporary object, then copies them to the
// final object with a generation precondition so the final replacement is atomic.
func (c *Client) WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error) {
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	err := c.withGenerationWriteLease(ctx, []string{tmpPath, finalPath}, func(writer *Client) error {
		if writer.backend != nil {
			tmpFullPath := fmt.Sprintf("%s/%s", writer.prefix(), tmpPath)
			tmpAttrs, err := writer.writeObject(ctx, tmpFullPath, data, contentTypeForPath(finalPath), map[string]string{"sha256": digest}, writeCondition{})
			if err != nil {
				return err
			}
			defer func() {
				bestEffortTemporaryCleanup(tmpAttrs.Generation, func(ctx context.Context) error {
					return writer.deleteObject(ctx, tmpFullPath, tmpAttrs.Generation)
				})
			}()
			finalFullPath := fmt.Sprintf("%s/%s", writer.prefix(), finalPath)
			finalAttrs, err := writer.objectAttrs(ctx, finalFullPath, 0)
			condition := writeCondition{DoesNotExist: true}
			if err == nil {
				condition = createOrGenerationCondition(finalAttrs.Generation)
			} else if !objectNotFound(err) {
				return err
			}
			_, err = writer.writeObject(ctx, finalFullPath, data, contentTypeForPath(finalPath), map[string]string{"sha256": digest}, condition)
			return err
		}
		tmpFullPath := fmt.Sprintf("%s/%s", writer.prefix(), tmpPath)
		finalFullPath := fmt.Sprintf("%s/%s", writer.prefix(), finalPath)

		tmpObj := writer.bucket.Object(tmpFullPath)
		w := tmpObj.NewWriter(ctx)
		w.ContentType = contentTypeForPath(finalPath)
		w.Metadata = map[string]string{
			"sha256": digest,
		}
		if _, err := w.Write(data); err != nil {
			w.Close()
			return conditionalWriteError(fmt.Errorf("write %s: %w", tmpFullPath, err))
		}
		if err := w.Close(); err != nil {
			return conditionalWriteError(fmt.Errorf("close %s: %w", tmpFullPath, err))
		}
		tmpAttrs := w.Attrs()
		tmpSource := tmpObj
		if tmpAttrs != nil && tmpAttrs.Generation > 0 {
			tmpSource = tmpObj.Generation(tmpAttrs.Generation)
		}
		if tmpAttrs != nil && tmpAttrs.Generation > 0 {
			defer func() {
				bestEffortTemporaryCleanup(tmpAttrs.Generation, func(ctx context.Context) error {
					return tmpObj.If(temporaryObjectDeleteConditions(tmpAttrs.Generation)).Delete(ctx)
				})
			}()
		}

		finalObj := writer.bucket.Object(finalFullPath)
		attrs, err := finalObj.Attrs(ctx)
		if err != nil {
			if !errors.Is(err, storage.ErrObjectNotExist) {
				return conditionalWriteError(fmt.Errorf("attrs %s: %w", finalFullPath, err))
			}
			finalObj = finalObj.If(storage.Conditions{DoesNotExist: true})
		} else {
			finalObj = finalObj.If(storage.Conditions{GenerationMatch: attrs.Generation})
		}
		if _, err := finalObj.CopierFrom(tmpSource).Run(ctx); err != nil {
			return conditionalWriteError(fmt.Errorf("copy %s to %s: %w", tmpFullPath, finalFullPath, err))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return digest, nil
}

func bestEffortTemporaryCleanup(objectGeneration int64, remove func(context.Context) error) {
	_ = store.RetryGenerationCleanup(objectGeneration, generation.LeaseReleaseTimeout, 1, func(ctx context.Context, _ int64) error {
		return remove(ctx)
	})
}

func temporaryObjectDeleteConditions(generation int64) storage.Conditions {
	return storage.Conditions{GenerationMatch: generation}
}

func contentTypeForPath(gcsRelPath string) string {
	if strings.HasSuffix(gcsRelPath, ".md") {
		return "text/markdown; charset=utf-8"
	}
	if strings.HasSuffix(gcsRelPath, ".json") {
		return "application/json; charset=utf-8"
	}
	return "application/octet-stream"
}

// GetMetaSHA256 returns the SHA256 digest from GCS object metadata, or "" if the
// object doesn't exist or has no sha256 metadata.
func (c *Client) GetMetaSHA256(ctx context.Context, gcsRelPath string) (string, error) {
	view := generationView{}
	var err error
	if generation.GenerationOwned(gcsRelPath) {
		view, err = c.operationView(ctx)
		if err != nil {
			return "", err
		}
	}
	path, expectedGeneration, err := c.resolveReadPath(gcsRelPath, view)
	if err != nil {
		return "", err
	}
	attrs, err := c.objectAttrs(ctx, path, expectedGeneration)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			if view.manifest != nil {
				return "", store.ErrDeclaredObjectUnavailable
			}
			return "", nil
		}
		if view.manifest != nil {
			return "", store.ErrDeclaredObjectUnavailable
		}
		return "", fmt.Errorf("attrs %s: %w", path, err)
	}
	if view.manifest != nil {
		file, ok := view.manifest.File(gcsRelPath)
		if !ok || file.Generation != attrs.Generation || file.Size != attrs.Size || attrs.Metadata["sha256"] != file.SHA256 {
			return "", store.ErrDeclaredObjectUnavailable
		}
	}
	return attrs.Metadata["sha256"], nil
}

// Prefix returns the GCS prefix for this client's user/project.
func (c *Client) Prefix() string {
	return c.prefix()
}

func (c *Client) objectRelativePath(objectName, requestedSubPrefix string) (string, bool) {
	basePrefix := c.prefix() + "/"
	if !strings.HasPrefix(objectName, basePrefix) {
		return "", false
	}

	rel := strings.TrimPrefix(objectName, basePrefix)
	if rel == "" {
		return "", false
	}

	subPrefix := strings.Trim(requestedSubPrefix, "/")
	if subPrefix != "" && rel != subPrefix && !strings.HasPrefix(rel, subPrefix+"/") {
		return "", false
	}
	return rel, true
}

// ListProjects returns project directories under users/{userID}/projects/.
func (c *Client) ListProjects(ctx context.Context, userID string) ([]Project, error) {
	if c == nil || c.bucket == nil {
		return nil, fmt.Errorf("GCS client is not configured")
	}

	basePrefix := store.UserProjectsPrefix(userID)
	it := c.bucket.Objects(ctx, &storage.Query{
		Prefix:    basePrefix,
		Delimiter: "/",
	})

	seen := make(map[string]struct{})
	var listedBytes int64
	listedObjects := 0
	for {
		attrs, err := it.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				break
			}
			return nil, err
		}
		if attrs.Size < 0 || attrs.Size > generation.MaxTotalSize || listedBytes > generation.MaxTotalSize-attrs.Size || listedObjects >= generation.MaxFiles {
			return nil, errors.New("object list exceeds limit")
		}
		listedObjects++
		listedBytes += attrs.Size

		prefix := attrs.Prefix
		if prefix == "" {
			name := strings.TrimPrefix(attrs.Name, basePrefix)
			if name == attrs.Name || name == "" || !strings.Contains(name, "/") {
				continue
			}
			prefix = basePrefix + strings.SplitN(name, "/", 2)[0] + "/"
		}

		projectID := strings.TrimSuffix(strings.TrimPrefix(prefix, basePrefix), "/")
		if projectID == "" || strings.Contains(projectID, "/") {
			continue
		}
		seen[projectID] = struct{}{}
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	projects := make([]Project, 0, len(ids))
	for _, id := range ids {
		projects = append(projects, Project{
			ID:        id,
			CreatedAt: c.projectCreatedAt(ctx, userID, id),
		})
	}
	return projects, nil
}

func (c *Client) projectCreatedAt(ctx context.Context, userID, projectID string) string {
	path := store.ProjectObjectPath(userID, projectID, "index.md")
	attrs, err := c.bucket.Object(path).Attrs(ctx)
	if err != nil || attrs.Created.IsZero() {
		return ""
	}
	return attrs.Created.UTC().Format(time.RFC3339)
}

// BucketStats counts canonical objects plus the current generation's logical
// files. Historical immutable generations are deliberately excluded.
func (c *Client) BucketStats(ctx context.Context) (int64, int64, error) {
	m, _, hasManifest, err := c.currentManifest(ctx)
	if err != nil {
		return 0, 0, err
	}
	if hasManifest {
		var bytes, count int64
		for _, file := range m.Files {
			if file.Size < 0 || file.Size > generation.MaxTotalSize || bytes > generation.MaxTotalSize-file.Size {
				return 0, 0, errBucketByteLimit
			}
			bytes += file.Size
			count++
		}
		prefix := c.prefix() + "/"
		canonical := int64(0)
		countCanonical := func(attrs backendObject) error {
			rel := strings.TrimPrefix(attrs.Name, prefix)
			if strings.HasPrefix(rel, generation.Prefix) || generation.GenerationOwned(rel) || rel == generation.ManifestPath || rel == generation.LeasePath {
				return nil
			}
			if attrs.Size < 0 || attrs.Size > generation.MaxTotalSize || bytes > generation.MaxTotalSize-attrs.Size {
				return errBucketByteLimit
			}
			canonical++
			if canonical > generation.MaxFiles {
				return errBucketObjectLimit
			}
			count++
			bytes += attrs.Size
			return nil
		}
		// Do not enumerate immutable generation history. Generation-owned
		// legacy paths are intentionally omitted, while canonical raw/cache
		// namespaces and direct root objects retain their quota semantics.
		for _, relPrefix := range []string{"raw/", "cache/"} {
			if err := c.visitObjectsWithBudget(ctx, prefix+relPrefix, false, generation.MaxFiles, generation.MaxTotalSize, errBucketObjectLimit, errBucketByteLimit, countCanonical); err != nil {
				return 0, 0, err
			}
		}
		err := c.visitObjectsWithBudget(ctx, prefix, true, generation.MaxFiles, generation.MaxTotalSize, errBucketObjectLimit, errBucketByteLimit, countCanonical)
		if err != nil {
			return 0, 0, err
		}
		return bytes, count, nil
	}
	var bytes, count int64
	visit := func(attrs backendObject) error {
		if count >= maxLegacyStatsObjects {
			return errors.New("bucket object limit exceeded")
		}
		if attrs.Size < 0 || attrs.Size > generation.MaxTotalSize || bytes > generation.MaxTotalSize-attrs.Size {
			return errors.New("bucket byte limit exceeded")
		}
		count++
		bytes += attrs.Size
		return nil
	}
	prefix := fmt.Sprintf("%s/", c.prefix())
	if err := c.visitObjectsWithBudget(ctx, prefix, false, maxLegacyStatsObjects, generation.MaxTotalSize, errBucketObjectLimit, errBucketByteLimit, visit); err != nil {
		return 0, 0, err
	}
	return bytes, count, nil
}

// listConceptDir lists concept markdown files from either wiki/ or wiki/.drafts/.
func (c *Client) listConceptDir(ctx context.Context, view generationView, dir, status string, directOnly bool) ([]WikiPage, error) {
	if files, exists := currentGenerationFiles(view, dir); exists {
		pages := make([]WikiPage, 0, len(files))
		for _, file := range files {
			if !strings.HasSuffix(file.Path, ".md") {
				continue
			}
			rel := strings.TrimPrefix(file.Path, dir)
			if rel == "" || (dir == "wiki/" && strings.Contains(rel, "sources/")) || (directOnly && strings.Contains(rel, "/")) {
				continue
			}
			slug := strings.TrimSuffix(rel, ".md")
			if slug == "index" || slug == "log" {
				continue
			}
			page := WikiPage{Slug: slug, Title: slug, Path: file.Path, Status: status}
			data, err := c.readFileWithView(ctx, page.Path, view)
			if err != nil {
				return nil, err
			}
			page, err = applyWikiPageFrontmatter(page, data)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", page.Path, err)
			}
			pages = append(pages, page)
		}
		return pages, nil
	}
	prefix := fmt.Sprintf("%s/%s", c.prefix(), dir)
	objects, err := c.listObjects(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var pages []WikiPage
	for _, attrs := range objects {
		if !strings.HasSuffix(attrs.Name, ".md") {
			continue
		}

		rel := strings.TrimPrefix(attrs.Name, prefix)
		if rel == attrs.Name || rel == "" {
			continue
		}
		// Skip source articles when listing concepts from wiki/
		if dir == "wiki/" && strings.Contains(rel, "sources/") {
			continue
		}
		if directOnly && strings.Contains(rel, "/") {
			continue
		}

		slug := strings.TrimSuffix(rel, ".md")
		if slug == "index" || slug == "log" {
			continue
		}
		page := WikiPage{
			Slug:   slug,
			Title:  slug,
			Path:   fmt.Sprintf("%s%s.md", dir, slug),
			Status: status,
		}
		data, err := c.readFileWithView(ctx, page.Path, view)
		if err != nil {
			return nil, err
		}
		page, err = applyWikiPageFrontmatter(page, data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page.Path, err)
		}
		pages = append(pages, page)
	}
	return pages, nil
}
