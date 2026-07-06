package gcs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	fm "github.com/adrg/frontmatter"
	"google.golang.org/api/iterator"
)

// Client wraps GCS operations for a specific user/project.
type Client struct {
	bucket    *storage.BucketHandle
	userID    string
	projectID string
}

// WikiPage represents a wiki source or concept page.
type WikiPage struct {
	Slug      string   `json:"slug"`
	Title     string   `json:"title"`
	ID        string   `json:"id"`
	Path      string   `json:"path"`
	Status    string   `json:"status"` // "published" or "draft"
	Quality   string   `json:"quality,omitempty"`
	Concepts  []string `json:"concepts,omitempty"`
	SourceURL string   `json:"source_url,omitempty"`
	RawSource string   `json:"raw_source,omitempty"`
}

// Project represents a user project discovered in GCS.
type Project struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

// MarkdownFile is a direct markdown object under a project directory.
type MarkdownFile struct {
	Slug string
	Path string
	Data []byte
}

type wikiPageFrontmatter struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
}

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
		bucket:    c.bucket,
		userID:    userID,
		projectID: projectID,
	}
}

// NewScopedClient returns a client that shares the bucket connection but uses
// the supplied user/project prefix.
func (c *Client) NewScopedClient(userID, projectID string) *Client {
	return c.WithScope(userID, projectID)
}

func (c *Client) prefix() string {
	return fmt.Sprintf("users/%s/projects/%s", c.userID, c.projectID)
}

// ListSources returns all compiled wiki sources.
func (c *Client) ListSources(ctx context.Context) ([]WikiPage, error) {
	return c.listDir(ctx, "wiki/sources/")
}

// ListConcepts returns wiki concepts. Drafts are included when includeDrafts is true.
func (c *Client) ListConcepts(ctx context.Context, includeDrafts bool) ([]WikiPage, error) {
	published, err := c.listConceptDir(ctx, "wiki/", "published", true)
	if err != nil {
		return nil, err
	}
	if !includeDrafts {
		return published, nil
	}

	seen := make(map[string]struct{}, len(published))
	for _, page := range published {
		seen[page.Slug] = struct{}{}
	}

	drafts, err := c.listConceptDir(ctx, "wiki/.drafts/", "draft", false)
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
	return published, nil
}

// GetPage reads a wiki page by slug from sources or concepts.
func (c *Client) GetPage(ctx context.Context, slug, category string) (*WikiPage, []byte, error) {
	switch category {
	case "sources":
		return c.getPageFromDir(ctx, slug, "wiki/sources/", "")
	case "concepts":
		page, data, err := c.getPageFromDir(ctx, slug, "wiki/", "published")
		if err == nil {
			return page, data, nil
		}
		if !errors.Is(err, storage.ErrObjectNotExist) {
			return nil, nil, err
		}
		return c.getPageFromDir(ctx, slug, "wiki/.drafts/", "draft")
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

func (c *Client) getPageFromDir(ctx context.Context, slug, dir, status string) (*WikiPage, []byte, error) {
	path := fmt.Sprintf("%s/%s%s.md", c.prefix(), dir, slug)
	obj := c.bucket.Object(path)
	r, err := obj.NewReader(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer r.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, fmt.Errorf("read all %s: %w", path, err)
	}

	return &WikiPage{
		Slug:   slug,
		Title:  slug,
		Path:   fmt.Sprintf("%s%s.md", dir, slug),
		Status: status,
	}, data, nil
}

// ReadRaw reads a raw source file.
func (c *Client) ReadRaw(ctx context.Context, name string) ([]byte, error) {
	path := fmt.Sprintf("%s/raw/%s", c.prefix(), name)
	obj := c.bucket.Object(path)
	r, err := obj.NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("read raw %s: %w", path, err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

// ReadFile reads any file under the user/project prefix.
func (c *Client) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	path := fmt.Sprintf("%s/%s", c.prefix(), relPath)
	obj := c.bucket.Object(path)
	r, err := obj.NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

// ListMarkdownFiles reads direct .md files under dir, relative to the
// user/project prefix. Nested files are ignored.
func (c *Client) ListMarkdownFiles(ctx context.Context, dir string) ([]MarkdownFile, error) {
	prefix := fmt.Sprintf("%s/%s", c.prefix(), dir)
	it := c.bucket.Objects(ctx, &storage.Query{Prefix: prefix})

	var files []MarkdownFile
	for {
		attrs, err := it.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				break
			}
			return nil, err
		}
		if !strings.HasSuffix(attrs.Name, ".md") {
			continue
		}
		rel := strings.TrimPrefix(attrs.Name, prefix)
		if rel == attrs.Name || rel == "" || strings.Contains(rel, "/") {
			continue
		}

		data, err := c.ReadFile(ctx, dir+rel)
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

// listDir lists .md files under the given directory prefix.
func (c *Client) listDir(ctx context.Context, dir string) ([]WikiPage, error) {
	prefix := fmt.Sprintf("%s/%s", c.prefix(), dir)
	it := c.bucket.Objects(ctx, &storage.Query{Prefix: prefix})

	var pages []WikiPage
	for {
		attrs, err := it.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				break
			}
			return nil, err
		}
		if !strings.HasSuffix(attrs.Name, ".md") {
			continue
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(attrs.Name, prefix), ".md")
		page := WikiPage{
			Slug:  slug,
			Title: slug,
			Path:  fmt.Sprintf("%s%s.md", dir, slug),
		}
		data, err := c.ReadFile(ctx, page.Path)
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
	return page, nil
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
	path := fmt.Sprintf("%s/%s", c.prefix(), gcsRelPath)
	obj := c.bucket.Object(path)

	w := obj.NewWriter(ctx)
	w.ContentType = contentTypeForPath(gcsRelPath)
	w.Metadata = map[string]string{
		"sha256": digest,
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
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
	tmpFullPath := fmt.Sprintf("%s/%s", c.prefix(), tmpPath)
	finalFullPath := fmt.Sprintf("%s/%s", c.prefix(), finalPath)

	tmpObj := c.bucket.Object(tmpFullPath)
	w := tmpObj.NewWriter(ctx)
	w.ContentType = contentTypeForPath(finalPath)
	w.Metadata = map[string]string{
		"sha256": digest,
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return "", fmt.Errorf("write %s: %w", tmpFullPath, err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", tmpFullPath, err)
	}
	tmpAttrs := w.Attrs()
	tmpSource := tmpObj
	if tmpAttrs != nil && tmpAttrs.Generation > 0 {
		tmpSource = tmpObj.Generation(tmpAttrs.Generation)
	}
	defer func() {
		_ = tmpObj.Delete(context.Background())
	}()

	finalObj := c.bucket.Object(finalFullPath)
	attrs, err := finalObj.Attrs(ctx)
	if err != nil {
		if !errors.Is(err, storage.ErrObjectNotExist) {
			return "", fmt.Errorf("attrs %s: %w", finalFullPath, err)
		}
		finalObj = finalObj.If(storage.Conditions{DoesNotExist: true})
	} else {
		finalObj = finalObj.If(storage.Conditions{GenerationMatch: attrs.Generation})
	}
	if _, err := finalObj.CopierFrom(tmpSource).Run(ctx); err != nil {
		return "", fmt.Errorf("copy %s to %s: %w", tmpFullPath, finalFullPath, err)
	}
	return digest, nil
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
	path := fmt.Sprintf("%s/%s", c.prefix(), gcsRelPath)
	obj := c.bucket.Object(path)
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("attrs %s: %w", path, err)
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

	basePrefix := fmt.Sprintf("users/%s/projects/", userID)
	it := c.bucket.Objects(ctx, &storage.Query{
		Prefix:    basePrefix,
		Delimiter: "/",
	})

	seen := make(map[string]struct{})
	for {
		attrs, err := it.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				break
			}
			return nil, err
		}

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
	path := fmt.Sprintf("users/%s/projects/%s/index.md", userID, projectID)
	attrs, err := c.bucket.Object(path).Attrs(ctx)
	if err != nil || attrs.Created.IsZero() {
		return ""
	}
	return attrs.Created.UTC().Format(time.RFC3339)
}

// BucketStats returns object count for the user/project prefix.
// Does NOT iterate to sum bytes — uses a single paged listing.
func (c *Client) BucketStats(ctx context.Context) (int64, int64, error) {
	prefix := fmt.Sprintf("%s/", c.prefix())
	it := c.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	var count int64
	for i := 0; i < 1000; i++ {
		_, err := it.Next()
		if err != nil {
			break
		}
		count++
	}
	return 0, count, nil // bytes=0 (skip expensive size summation)
}

// listConceptDir lists concept markdown files from either wiki/ or wiki/.drafts/.
func (c *Client) listConceptDir(ctx context.Context, dir, status string, directOnly bool) ([]WikiPage, error) {
	prefix := fmt.Sprintf("%s/%s", c.prefix(), dir)
	it := c.bucket.Objects(ctx, &storage.Query{Prefix: prefix})

	var pages []WikiPage
	for {
		attrs, err := it.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				break
			}
			return nil, err
		}
		if !strings.HasSuffix(attrs.Name, ".md") {
			continue
		}

		rel := strings.TrimPrefix(attrs.Name, prefix)
		if rel == attrs.Name || rel == "" {
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
		data, err := c.ReadFile(ctx, page.Path)
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
