package gcs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"cloud.google.com/go/storage"
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
	Path      string   `json:"path"`
	Status    string   `json:"status"` // "published" or "draft"
	Quality   string   `json:"quality,omitempty"`
	Concepts  []string `json:"concepts,omitempty"`
	SourceURL string   `json:"source_url,omitempty"`
	RawSource string   `json:"raw_source,omitempty"`
}

// NewClient creates a new GCS client for the given bucket/user/project.
func NewClient(bucket, userID, projectID string) (*Client, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage client: %w", err)
	}
	return &Client{
		bucket:    client.Bucket(bucket),
		userID:    userID,
		projectID: projectID,
	}, nil
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
		pages = append(pages, WikiPage{
			Slug:  slug,
			Title: slug,
			Path:  fmt.Sprintf("%s%s.md", dir, slug),
		})
	}
	return pages, nil
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

	// Detect content type
	contentType := "application/octet-stream"
	if strings.HasSuffix(gcsRelPath, ".md") {
		contentType = "text/markdown; charset=utf-8"
	} else if strings.HasSuffix(gcsRelPath, ".db") {
		contentType = "application/octet-stream"
	}

	w := obj.NewWriter(ctx)
	w.ContentType = contentType
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
		// Skip metadata files (TODO: move to wiki/.meta/)
		if slug == "index" || slug == "log" {
			continue
		}
		pages = append(pages, WikiPage{
			Slug:   slug,
			Title:  slug,
			Path:   fmt.Sprintf("%s%s.md", dir, slug),
			Status: status,
		})
	}
	return pages, nil
}
