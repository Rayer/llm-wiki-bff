package wikiindex

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/storage"
	fm "github.com/adrg/frontmatter"
	"github.com/rayer/llm-wiki-bff/internal/annotation"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/generation"
)

const (
	IDMapPath                 = "cache/id_map.json"
	IDMapTempPath             = "cache/id_map.json.tmp"
	ConceptsJSONLPath         = "cache/concepts.jsonl"
	maxJSONNormalizationDepth = 64
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
	Concept         map[string]string     `json:"concept"`
	DormantConcept  map[string]string     `json:"dormant_concept,omitempty"`
	ConceptEntityID map[string]string     `json:"concept_entity_id,omitempty"`
	Source          map[string]string     `json:"source"`
	SourceMeta      map[string]SourceMeta `json:"source_meta,omitempty"`
	Redirects       map[string][]string   `json:"redirects"`
}

type SourceMeta struct {
	Slug       string `json:"slug"`
	Title      string `json:"title,omitempty"`
	SourceFile string `json:"source_file,omitempty"`
}

// UnmarshalJSON keeps the nested source metadata bounded and rejects duplicate
// fields without changing the existing ignore-unknown-fields contract.
func (m *SourceMeta) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return errors.New("expected JSON object")
	}
	seen := make(map[string]struct{})
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return errors.New("expected JSON object key")
		}
		if _, exists := seen[name]; exists {
			return errors.New("duplicate JSON object key")
		}
		seen[name] = struct{}{}
		switch name {
		case "slug":
			err = dec.Decode(&m.Slug)
		case "title":
			err = dec.Decode(&m.Title)
		case "source_file":
			err = dec.Decode(&m.SourceFile)
		default:
			var ignored json.RawMessage
			err = dec.Decode(&ignored)
		}
		if err != nil {
			return err
		}
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') {
		if err != nil {
			return err
		}
		return errors.New("expected JSON object end")
	}
	return generation.EnsureJSONEOF(dec)
}

// DecodeIDMap bounds every collection while it is being decoded. Generated
// cache byte limits alone do not bound the number of map and slice entries.
func DecodeIDMap(data []byte) (IDMap, error) {
	result := IDMap{Concept: map[string]string{}, DormantConcept: map[string]string{}, ConceptEntityID: map[string]string{}, Source: map[string]string{}, SourceMeta: map[string]SourceMeta{}, Redirects: map[string][]string{}}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return result, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return result, errors.New("expected JSON object")
	}
	seen := make(map[string]struct{})
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return result, err
		}
		name, ok := key.(string)
		if !ok {
			return result, errors.New("expected JSON object key")
		}
		if _, exists := seen[name]; exists {
			return result, errors.New("duplicate JSON object key")
		}
		seen[name] = struct{}{}
		switch name {
		case "concept":
			result.Concept, err = generation.DecodeBoundedMap[string](dec)
		case "dormant_concept":
			result.DormantConcept, err = generation.DecodeBoundedMap[string](dec)
		case "concept_entity_id":
			result.ConceptEntityID, err = generation.DecodeBoundedMap[string](dec)
		case "source":
			result.Source, err = generation.DecodeBoundedMap[string](dec)
		case "source_meta":
			result.SourceMeta, err = generation.DecodeBoundedMap[SourceMeta](dec)
		case "redirects":
			result.Redirects, err = generation.DecodeBoundedStringLists(dec)
		default:
			var ignored json.RawMessage
			err = dec.Decode(&ignored)
		}
		if err != nil {
			return result, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return result, err
	}
	if err := generation.EnsureJSONEOF(dec); err != nil {
		return result, err
	}
	return result, nil
}

type markdownMatter struct {
	ID         string   `yaml:"id"`
	Title      string   `yaml:"title"`
	SourceFile string   `yaml:"source_file"`
	Sources    []string `yaml:"sources"`
	Source     string   `yaml:"source"`
}

func Rebuild(ctx context.Context, store Store) (IDMap, error) {
	next, err := BuildIDMap(ctx, store)
	if err != nil {
		return next, err
	}
	idMapData, err := encodeIDMap(next)
	if err != nil {
		return next, err
	}
	conceptsData, err := buildConceptsJSONL(ctx, store)
	if err != nil {
		return next, fmt.Errorf("build concepts jsonl: %w", err)
	}
	if err := writeIDMap(ctx, store, idMapData); err != nil {
		return next, err
	}
	if err := writeConceptsJSONL(ctx, store, conceptsData); err != nil {
		return next, err
	}
	return next, nil
}

func BuildIDMap(ctx context.Context, store Store) (IDMap, error) {
	next := IDMap{
		Concept:         map[string]string{},
		DormantConcept:  map[string]string{},
		ConceptEntityID: map[string]string{},
		Source:          map[string]string{},
		SourceMeta:      map[string]SourceMeta{},
		Redirects:       map[string][]string{},
	}

	if err := addIDMapEntries(ctx, store, "wiki/", next.Concept); err != nil {
		return next, err
	}
	if err := addSourceEntries(ctx, store, next.Source, next.SourceMeta); err != nil {
		return next, err
	}

	old, err := readOldIDMap(ctx, store)
	if err != nil {
		return next, err
	}
	if err := preserveConceptLifecycle(&next, old); err != nil {
		return next, err
	}
	next.Redirects = cloneRedirects(old.Redirects)
	appendChangedRedirects(next.Redirects, old.Concept, next.Concept)
	appendChangedRedirects(next.Redirects, old.Source, next.Source)

	return next, nil
}

// addSourceEntries intentionally parses the source collection once: the index
// needs both its stable ID map and source metadata from the same files.
func addSourceEntries(ctx context.Context, store Store, ids map[string]string, entries map[string]SourceMeta) error {
	files, err := store.ListMarkdownFiles(ctx, "wiki/sources/")
	if err != nil {
		return fmt.Errorf("list wiki/sources/: %w", err)
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
		ids[id] = file.Slug
		entries[id] = SourceMeta{Slug: file.Slug, Title: strings.TrimSpace(matter.Title), SourceFile: strings.TrimSpace(matter.SourceFile)}
	}
	return nil
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
		if !annotation.ValidSourceID(id) {
			return fmt.Errorf("unsafe ID %q in %s", id, file.Path)
		}
		if !validConceptSlug(file.Slug) {
			return fmt.Errorf("unsafe concept slug %q in %s", file.Slug, file.Path)
		}
		if oldSlug, exists := entries[id]; exists {
			return fmt.Errorf("duplicate concept ID %q for %q and %q", id, oldSlug, file.Slug)
		}
		for oldID, oldSlug := range entries {
			if oldSlug == file.Slug {
				return fmt.Errorf("duplicate concept slug %q for %q and %q", file.Slug, oldID, id)
			}
		}
		entries[id] = file.Slug
	}
	return nil
}

func validConceptSlug(slug string) bool {
	return slug != "" && slug == strings.TrimSpace(slug) &&
		!strings.ContainsAny(slug, "/\\") && slug != "." && slug != ".."
}

// preserveConceptLifecycle carries forward only lifecycle state that still
// belongs to a rebuilt active or dormant Concept. Entity rows for removed
// Concepts are deliberately pruned; malformed rows and collisions fail before
// Rebuild writes either generated artifact.
func preserveConceptLifecycle(next *IDMap, old IDMap) error {
	activeBySlug := make(map[string]string, len(next.Concept))
	for id, slug := range next.Concept {
		if !annotation.ValidSourceID(id) || !validConceptSlug(slug) {
			return fmt.Errorf("unsafe rebuilt concept mapping %q -> %q", id, slug)
		}
		if priorID, exists := activeBySlug[slug]; exists && priorID != id {
			return fmt.Errorf("duplicate rebuilt concept slug %q", slug)
		}
		activeBySlug[slug] = id
	}

	retainedDormant := make(map[string]string, len(old.DormantConcept))
	dormantBySlug := make(map[string]string, len(old.DormantConcept))
	for id, slug := range old.DormantConcept {
		if !annotation.ValidSourceID(id) || !validConceptSlug(slug) || id != strings.TrimSpace(id) {
			return fmt.Errorf("unsafe dormant concept mapping %q -> %q", id, slug)
		}
		if activeSlug, exists := next.Concept[id]; exists {
			return fmt.Errorf("concept ID %q is both active (%q) and dormant (%q)", id, activeSlug, slug)
		}
		if activeID, exists := activeBySlug[slug]; exists {
			return fmt.Errorf("concept slug %q is both active (%q) and dormant (%q)", slug, activeID, id)
		}
		if priorID, exists := dormantBySlug[slug]; exists && priorID != id {
			return fmt.Errorf("duplicate dormant concept slug %q", slug)
		}
		dormantBySlug[slug] = id
		retainedDormant[id] = slug
	}

	owned := make(map[string]struct{}, len(next.Concept)+len(retainedDormant))
	for id := range next.Concept {
		owned[id] = struct{}{}
	}
	for id := range retainedDormant {
		owned[id] = struct{}{}
	}
	entityOwners := make(map[string]string)
	nextEntities := make(map[string]string, len(old.ConceptEntityID))
	for id, entityID := range old.ConceptEntityID {
		if !annotation.ValidSourceID(id) || id != strings.TrimSpace(id) || !annotation.ValidSourceID(entityID) || entityID != strings.TrimSpace(entityID) {
			return fmt.Errorf("unsafe concept entity mapping %q -> %q", id, entityID)
		}
		if _, isOwned := owned[id]; !isOwned {
			// A valid row for an ID no longer present in either rebuilt active
			// or retained dormant state is unowned engine state and is pruned.
			continue
		}
		if priorID, exists := entityOwners[entityID]; exists && priorID != id {
			return fmt.Errorf("concept entity ID %q maps to multiple LWC IDs", entityID)
		}
		entityOwners[entityID] = id
		nextEntities[id] = entityID
	}

	next.DormantConcept = retainedDormant
	next.ConceptEntityID = nextEntities
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
		Concept:         map[string]string{},
		DormantConcept:  map[string]string{},
		ConceptEntityID: map[string]string{},
		Source:          map[string]string{},
		SourceMeta:      map[string]SourceMeta{},
		Redirects:       map[string][]string{},
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
	old, err = DecodeIDMap(data)
	if err != nil {
		return old, fmt.Errorf("decode old id map: %w", err)
	}
	if old.Concept == nil {
		old.Concept = map[string]string{}
	}
	if old.Source == nil {
		old.Source = map[string]string{}
	}
	if old.SourceMeta == nil {
		old.SourceMeta = map[string]SourceMeta{}
	}
	if old.DormantConcept == nil {
		old.DormantConcept = map[string]string{}
	}
	if old.ConceptEntityID == nil {
		old.ConceptEntityID = map[string]string{}
	}
	if old.Redirects == nil {
		old.Redirects = map[string][]string{}
	}
	return old, nil
}

func encodeIDMap(next IDMap) ([]byte, error) {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode id map: %w", err)
	}
	return data, nil
}

func writeIDMap(ctx context.Context, store Store, data []byte) error {
	if _, err := store.WriteBytesAtomic(ctx, data, IDMapTempPath, IDMapPath); err != nil {
		return fmt.Errorf("write id map: %w", err)
	}
	return nil
}

func buildConceptsJSONL(ctx context.Context, store Store) ([]byte, error) {
	files, err := store.ListMarkdownFiles(ctx, "wiki/")
	if err != nil {
		return nil, fmt.Errorf("list wiki for concepts jsonl: %w", err)
	}

	var builder strings.Builder
	for _, file := range files {
		entry := parseCacheEntry(file.Slug, string(file.Data))
		normalizedFrontmatter, err := normalizeJSONValue(entry.Frontmatter, 0)
		if err != nil {
			return nil, fmt.Errorf("normalize concepts jsonl %s: %w", file.Path, err)
		}
		entry.Frontmatter = normalizedFrontmatter.(map[string]interface{})
		data, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("encode concepts jsonl %s: %w", file.Path, err)
		}
		builder.Write(data)
		builder.WriteByte('\n')
	}

	return []byte(builder.String()), nil
}

func writeConceptsJSONL(ctx context.Context, store Store, data []byte) error {
	if _, err := store.WriteBytesAtomic(ctx, data, "cache/concepts.jsonl.tmp", ConceptsJSONLPath); err != nil {
		return fmt.Errorf("write concepts.jsonl: %w", err)
	}
	return nil
}

func normalizeJSONValue(value interface{}, depth int) (interface{}, error) {
	if depth > maxJSONNormalizationDepth {
		return nil, fmt.Errorf("maximum nesting depth %d exceeded", maxJSONNormalizationDepth)
	}

	switch value := value.(type) {
	case map[interface{}]interface{}:
		result := make(map[string]interface{}, len(value))
		for key, item := range value {
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("non-string map key type %T", key)
			}
			normalized, err := normalizeJSONValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			result[name] = normalized
		}
		return result, nil
	case map[string]interface{}:
		result := make(map[string]interface{}, len(value))
		for key, item := range value {
			normalized, err := normalizeJSONValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			result[key] = normalized
		}
		return result, nil
	case []interface{}:
		result := make([]interface{}, len(value))
		for i, item := range value {
			normalized, err := normalizeJSONValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			result[i] = normalized
		}
		return result, nil
	default:
		return value, nil
	}
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
	return ContentDerivedID(data)
}

// ContentDerivedID returns the ID Rebuild assigns to markdown without an explicit ID.
func ContentDerivedID(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])[:12]
}
