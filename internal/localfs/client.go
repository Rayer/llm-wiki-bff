package localfs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	fm "github.com/adrg/frontmatter"
	"github.com/rayer/llm-wiki-bff/internal/gcs"
	store "github.com/rayer/llm-wiki-bff/internal/storage"
)

type Client struct {
	root      string
	userID    string
	projectID string
}

type wikiPageFrontmatter struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
}

func New(root string) *Client {
	return &Client{root: filepath.Clean(root)}
}

func (c *Client) WithScope(userID, projectID string) *Client {
	return &Client{
		root:      c.root,
		userID:    userID,
		projectID: projectID,
	}
}

func (c *Client) Scope(userID, projectID string) store.Store {
	return c.WithScope(userID, projectID)
}

func (c *Client) Prefix() string {
	return store.ProjectPrefix(c.userID, c.projectID)
}

func (c *Client) ReadFile(ctx context.Context, relPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := c.fullPath(relPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrObjectNotExist
		}
		return nil, fmt.Errorf("read %s: %w", relPath, err)
	}
	return data, nil
}

func (c *Client) WriteBytes(ctx context.Context, data []byte, relPath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := c.fullPath(relPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", relPath, err)
	}
	return digest(data), nil
}

func (c *Client) WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tmp, err := c.fullPath(tmpPath)
	if err != nil {
		return "", err
	}
	final, err := c.fullPath(finalPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(tmp), err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(final), err)
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename %s to %s: %w", tmpPath, finalPath, err)
	}
	return digest(data), nil
}

func (c *Client) ListProjects(ctx context.Context, userID string) ([]store.Project, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !safeSegment(userID) {
		return nil, fmt.Errorf("invalid user ID")
	}
	base := filepath.Join(c.root, "users", userID, "projects")
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []store.Project{}, nil
		}
		return nil, fmt.Errorf("list projects: %w", err)
	}

	projects := make([]store.Project, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !safeSegment(entry.Name()) {
			continue
		}
		indexPath := filepath.Join(base, entry.Name(), "index.md")
		info, err := os.Stat(indexPath)
		if err != nil {
			continue
		}
		projects = append(projects, store.Project{
			ID:        entry.Name(),
			CreatedAt: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].ID < projects[j].ID
	})
	return projects, nil
}

func (c *Client) ListConcepts(ctx context.Context, includeDrafts bool) ([]store.WikiPage, error) {
	published, err := c.listConceptDir(ctx, "wiki", "published")
	if err != nil {
		return nil, err
	}
	if !includeDrafts {
		return published, nil
	}
	drafts, err := c.listConceptDir(ctx, filepath.Join("wiki", ".drafts"), "draft")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(published))
	for _, page := range published {
		seen[page.Slug] = struct{}{}
	}
	for _, page := range drafts {
		if _, ok := seen[page.Slug]; ok {
			continue
		}
		published = append(published, page)
	}
	return published, nil
}

func (c *Client) ListSources(ctx context.Context) ([]store.WikiPage, error) {
	return c.listMarkdownPages(ctx, filepath.Join("wiki", "sources"), "", true)
}

func (c *Client) ListConceptsFromCache(ctx context.Context) ([]store.WikiPage, error) {
	data, err := c.ReadFile(ctx, "cache/concepts.jsonl")
	if err != nil {
		return nil, err
	}
	return gcs.WikiPagesFromConceptsJSONL(data)
}

func (c *Client) ListSourcesFromCache(ctx context.Context) ([]store.WikiPage, error) {
	data, err := c.ReadFile(ctx, "cache/id_map.json")
	if err != nil {
		return nil, err
	}
	return gcs.WikiPagesFromSourceIDMap(data)
}

func (c *Client) GetPage(ctx context.Context, slug, category string) (*store.WikiPage, []byte, error) {
	if !safeSlug(slug) {
		return nil, nil, fmt.Errorf("invalid slug")
	}
	switch category {
	case "sources":
		return c.getPageFromPath(ctx, slug, filepath.Join("wiki", "sources", slug+".md"), "")
	case "concepts":
		page, data, err := c.getPageFromPath(ctx, slug, filepath.Join("wiki", slug+".md"), "published")
		if err == nil {
			return page, data, nil
		}
		if !errors.Is(err, storage.ErrObjectNotExist) {
			return nil, nil, err
		}
		return c.getPageFromPath(ctx, slug, filepath.Join("wiki", ".drafts", slug+".md"), "draft")
	default:
		return nil, nil, fmt.Errorf("unknown category: %s", category)
	}
}

func (c *Client) ListMarkdownFiles(ctx context.Context, dir string) ([]store.MarkdownFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fullDir, err := c.fullPath(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []store.MarkdownFile{}, nil
		}
		return nil, fmt.Errorf("list markdown files: %w", err)
	}

	files := make([]store.MarkdownFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(dir, entry.Name()))
		data, err := c.ReadFile(ctx, rel)
		if err != nil {
			return nil, err
		}
		files = append(files, store.MarkdownFile{
			Slug: strings.TrimSuffix(entry.Name(), ".md"),
			Path: rel,
			Data: data,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Slug < files[j].Slug
	})
	return files, nil
}

func (c *Client) ListRawFiles(ctx context.Context) ([]store.RawFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fullDir, err := c.fullPath("raw")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []store.RawFile{}, nil
		}
		return nil, fmt.Errorf("list raw files: %w", err)
	}

	files := make([]store.RawFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat raw/%s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("raw", entry.Name()))
		sha256, err := c.GetMetaSHA256(ctx, rel)
		if err != nil {
			return nil, err
		}
		files = append(files, store.RawFile{
			Name:    entry.Name(),
			Path:    rel,
			Size:    info.Size(),
			Updated: info.ModTime().UTC(),
			SHA256:  sha256,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})
	return files, nil
}

func (c *Client) BucketStats(ctx context.Context) (int64, int64, error) {
	root, err := c.projectRoot()
	if err != nil {
		return 0, 0, err
	}
	var bytes int64
	var files int64
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		bytes += info.Size()
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	return bytes, files, nil
}

func (c *Client) GetMetaSHA256(ctx context.Context, relPath string) (string, error) {
	data, err := c.ReadFile(ctx, relPath)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return "", nil
		}
		return "", err
	}
	return digest(data), nil
}

func (c *Client) getPageFromPath(ctx context.Context, slug, relPath, status string) (*store.WikiPage, []byte, error) {
	data, err := c.ReadFile(ctx, filepath.ToSlash(relPath))
	if err != nil {
		return nil, nil, err
	}
	page := store.WikiPage{
		Slug:   slug,
		Title:  slug,
		Path:   filepath.ToSlash(relPath),
		Status: status,
	}
	page, err = applyWikiPageFrontmatter(page, data)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", relPath, err)
	}
	return &page, data, nil
}

func (c *Client) listConceptDir(ctx context.Context, dir, status string) ([]store.WikiPage, error) {
	pages, err := c.listMarkdownPages(ctx, dir, status, true)
	if err != nil {
		return nil, err
	}
	filtered := pages[:0]
	for _, page := range pages {
		if page.Slug == "index" || page.Slug == "log" {
			continue
		}
		filtered = append(filtered, page)
	}
	return filtered, nil
}

func (c *Client) listMarkdownPages(ctx context.Context, dir, status string, directOnly bool) ([]store.WikiPage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fullDir, err := c.fullPath(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []store.WikiPage{}, nil
		}
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}

	pages := make([]store.WikiPage, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			if directOnly {
				continue
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		slug := strings.TrimSuffix(entry.Name(), ".md")
		relPath := filepath.ToSlash(filepath.Join(dir, entry.Name()))
		data, err := c.ReadFile(ctx, relPath)
		if err != nil {
			return nil, err
		}
		page := store.WikiPage{
			Slug:   slug,
			Title:  slug,
			Path:   relPath,
			Status: status,
		}
		page, err = applyWikiPageFrontmatter(page, data)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", relPath, err)
		}
		pages = append(pages, page)
	}
	sort.Slice(pages, func(i, j int) bool {
		return pages[i].Slug < pages[j].Slug
	})
	return pages, nil
}

func applyWikiPageFrontmatter(page store.WikiPage, data []byte) (store.WikiPage, error) {
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

func (c *Client) fullPath(relPath string) (string, error) {
	root, err := c.projectRoot()
	if err != nil {
		return "", err
	}
	cleanRel := filepath.Clean(filepath.FromSlash(relPath))
	if cleanRel == "." || filepath.IsAbs(cleanRel) || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) || cleanRel == ".." {
		return "", fmt.Errorf("unsafe relative path: %s", relPath)
	}
	target := filepath.Join(root, cleanRel)
	if err := ensureWithinExistingParent(root, target); err != nil {
		return "", err
	}
	return target, nil
}

func (c *Client) projectRoot() (string, error) {
	if !safeSegment(c.userID) || !safeSegment(c.projectID) {
		return "", fmt.Errorf("localfs scope is incomplete")
	}
	root, err := filepath.Abs(filepath.Join(c.root, "users", c.userID, "projects", c.projectID))
	if err != nil {
		return "", err
	}
	return root, nil
}

func ensureWithinExistingParent(root, target string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if !within(absRoot, absTarget) {
		return fmt.Errorf("path escapes project root")
	}

	if _, err := os.Lstat(absRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return err
	}

	parent := filepath.Dir(absTarget)
	for {
		if _, err := os.Lstat(parent); err == nil {
			break
		}
		next := filepath.Dir(parent)
		if next == parent || !within(absRoot, next) {
			parent = absRoot
			break
		}
		parent = next
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	if !within(resolvedRoot, resolvedParent) {
		return fmt.Errorf("path escapes project root through symlink")
	}
	return nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func safeSegment(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, `/\`+"\x00")
}

func safeSlug(value string) bool {
	return safeSegment(value) && !strings.Contains(value, "..")
}

func digest(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
