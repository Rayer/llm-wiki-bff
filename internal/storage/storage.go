package storage

import (
	"context"
	"time"
)

// WikiPage represents a wiki source or concept page.
type WikiPage struct {
	Slug      string   `json:"slug"`
	Title     string   `json:"title"`
	ID        string   `json:"id"`
	Path      string   `json:"path"`
	Status    string   `json:"status"`
	Quality   string   `json:"quality,omitempty"`
	Concepts  []string `json:"concepts,omitempty"`
	SourceURL string   `json:"source_url,omitempty"`
	RawSource string   `json:"raw_source,omitempty"`
}

// Project represents a user project discovered in wiki storage.
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

// RawFile is a direct file under a project's raw/ directory.
type RawFile struct {
	Name    string
	Path    string
	Size    int64
	Updated time.Time
	SHA256  string
}

// Store is the project-scoped wiki storage contract used by BFF read/write paths.
type Store interface {
	Prefix() string
	ReadFile(ctx context.Context, relPath string) ([]byte, error)
	WriteBytes(ctx context.Context, data []byte, relPath string) (string, error)
	WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error)
	ListProjects(ctx context.Context, userID string) ([]Project, error)
	ListConcepts(ctx context.Context, includeDrafts bool) ([]WikiPage, error)
	ListSources(ctx context.Context) ([]WikiPage, error)
	ListConceptsFromCache(ctx context.Context) ([]WikiPage, error)
	ListSourcesFromCache(ctx context.Context) ([]WikiPage, error)
	GetPage(ctx context.Context, slug, category string) (*WikiPage, []byte, error)
	ListMarkdownFiles(ctx context.Context, dir string) ([]MarkdownFile, error)
	ListRawFiles(ctx context.Context) ([]RawFile, error)
	BucketStats(ctx context.Context) (int64, int64, error)
	GetMetaSHA256(ctx context.Context, relPath string) (string, error)
}

// RootStore can derive project-scoped stores from request identity.
type RootStore interface {
	Store
	Scope(userID, projectID string) Store
}
