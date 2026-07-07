package wikiindex

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/storage"
	fm "github.com/adrg/frontmatter"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
)

const (
	IDMapPath         = "cache/id_map.json"
	IDMapTempPath     = "cache/id_map.json.tmp"
	ConceptsJSONLPath = "cache/concepts.jsonl"
)

var ErrNotFound = errors.New("wikiindex: not found")

type MarkdownFile struct {
	Slug string
	Path string
	Data []byte
}

type Store interface {
	ListMarkdownFiles(ctx context.Context, dir string) ([]MarkdownFile, error)
	ReadFile(ctx context.Context, relPath string) ([]byte, error)
	WriteBytesAtomic(ctx context.Context, data []byte, tmpPath, finalPath string) (string, error)
}

type IDMap struct {
	Concept   map[string]string   `json:"concept"`
	Source    map[string]string   `json:"source"`
	Redirects map[string][]string `json:"redirects"`
}

type markdownMatter struct {
	ID      string   `yaml:"id"`
	Title   string   `yaml:"title"`
	Sources []string `yaml:"sources"`
	Source  string   `yaml:"source"`
}

func Rebuild(ctx context.Context, store Store) (IDMap, error) {
	next, err := BuildIDMap(ctx, store)
	if err != nil {
		return next, err
	}
	if err := writeIDMap(ctx, store, next); err != nil {
		return next, err
	}
	if err := buildConceptsJSONL(ctx, store); err != nil {
		return next, fmt.Errorf("build concepts jsonl: %w", err)
	}
	return next, nil
}

func BuildIDMap(ctx context.Context, store Store) (IDMap, error) {
	next := IDMap{
		Concept:   map[string]string{},
		Source:    map[string]string{},
		Redirects: map[string][]string{},
	}

	if err := addIDMapEntries(ctx, store, "wiki/", next.Concept); err != nil {
		return next, err
	}
	if err := addIDMapEntries(ctx, store, "wiki/sources/", next.Source); err != nil {
		return next, err
	}

	old, err := readOldIDMap(ctx, store)
	if err != nil {
		return next, err
	}
	next.Redirects = cloneRedirects(old.Redirects)
	appendChangedRedirects(next.Redirects, old.Concept, next.Concept)
	appendChangedRedirects(next.Redirects, old.Source, next.Source)

	return next, nil
}

func addIDMapEntries(ctx context.Context, store Store, dir string, entries map[string]string) error {
	files, err := store.ListMarkdownFiles(ctx, dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", dir, err)
	}
	for _, file := range files {
		matter, err := parseMarkdownMatter(file.Data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", file.Path, err)
		}
		id := strings.TrimSpace(matter.ID)
		if id == "" {
			id = generateID(file.Data)
		}
		entries[id] = file.Slug
	}
	return nil
}

func parseMarkdownMatter(data []byte) (markdownMatter, error) {
	var matter markdownMatter
	if !strings.HasPrefix(string(data), "---") {
		return matter, nil
	}
	_, err := fm.MustParse(strings.NewReader(string(data)), &matter)
	return matter, err
}

func readOldIDMap(ctx context.Context, store Store) (IDMap, error) {
	old := IDMap{
		Concept:   map[string]string{},
		Source:    map[string]string{},
		Redirects: map[string][]string{},
	}
	data, err := store.ReadFile(ctx, IDMapPath)
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, storage.ErrObjectNotExist) {
			return old, nil
		}
		return old, fmt.Errorf("read old id map: %w", err)
	}
	if len(data) == 0 {
		return old, nil
	}
	if err := json.Unmarshal(data, &old); err != nil {
		return old, fmt.Errorf("decode old id map: %w", err)
	}
	if old.Concept == nil {
		old.Concept = map[string]string{}
	}
	if old.Source == nil {
		old.Source = map[string]string{}
	}
	if old.Redirects == nil {
		old.Redirects = map[string][]string{}
	}
	return old, nil
}

func writeIDMap(ctx context.Context, store Store, next IDMap) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode id map: %w", err)
	}
	if _, err := store.WriteBytesAtomic(ctx, data, IDMapTempPath, IDMapPath); err != nil {
		return fmt.Errorf("write id map: %w", err)
	}
	return nil
}

func buildConceptsJSONL(ctx context.Context, store Store) error {
	files, err := store.ListMarkdownFiles(ctx, "wiki/")
	if err != nil {
		return fmt.Errorf("list wiki for concepts jsonl: %w", err)
	}

	var builder strings.Builder
	for _, file := range files {
		entry := parseCacheEntry(file.Slug, string(file.Data))
		data, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode concepts jsonl %s: %w", file.Path, err)
		}
		builder.Write(data)
		builder.WriteByte('\n')
	}

	if _, err := store.WriteBytesAtomic(ctx, []byte(builder.String()), "cache/concepts.jsonl.tmp", ConceptsJSONLPath); err != nil {
		return fmt.Errorf("write concepts.jsonl: %w", err)
	}
	return nil
}

func parseCacheEntry(slug, raw string) conceptcache.Entry {
	frontmatter, body := parseFrontmatter(raw)
	title := slug
	if value := strings.TrimSpace(frontmatterString(frontmatter["title"])); value != "" {
		title = value
	}
	return conceptcache.Entry{
		Slug:        slug,
		Title:       title,
		Body:        body,
		Frontmatter: frontmatter,
		Sources:     frontmatterSources(frontmatter),
	}
}

func parseFrontmatter(raw string) (map[string]interface{}, string) {
	matter := make(map[string]interface{})
	if !strings.HasPrefix(raw, "---\n") {
		return matter, raw
	}
	body, err := fm.MustParse(strings.NewReader(raw), &matter)
	if err != nil {
		return make(map[string]interface{}), raw
	}
	return matter, string(body)
}

func frontmatterSources(frontmatter map[string]interface{}) []string {
	for _, key := range []string{"sources", "source"} {
		switch value := frontmatter[key].(type) {
		case []string:
			return append([]string(nil), value...)
		case []interface{}:
			sources := make([]string, 0, len(value))
			for _, item := range value {
				if source := strings.TrimSpace(fmt.Sprint(item)); source != "" {
					sources = append(sources, source)
				}
			}
			return sources
		case string:
			if value != "" {
				return []string{value}
			}
		}
	}
	return []string{}
}

func frontmatterString(value interface{}) string {
	text, _ := value.(string)
	return text
}

func cloneRedirects(src map[string][]string) map[string][]string {
	dst := make(map[string][]string, len(src))
	for id, redirects := range src {
		dst[id] = append([]string(nil), redirects...)
	}
	return dst
}

func appendChangedRedirects(redirects map[string][]string, oldEntries, newEntries map[string]string) {
	for id, newSlug := range newEntries {
		oldSlug := strings.TrimSpace(oldEntries[id])
		if oldSlug == "" || oldSlug == newSlug || containsString(redirects[id], oldSlug) {
			continue
		}
		redirects[id] = append(redirects[id], oldSlug)
	}
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func generateID(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])[:12]
}
