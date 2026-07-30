package wikiindex

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"cloud.google.com/go/storage"
	fm "github.com/adrg/frontmatter"
	"github.com/rayer/llm-wiki-bff/internal/annotation"
	conceptcache "github.com/rayer/llm-wiki-bff/internal/cache"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	"gopkg.in/yaml.v3"
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
	IDRedirects     map[string]string     `json:"id_redirects,omitempty"`
}

// SyntoIdentityPlan is the validated identity authority for one Synto
// INDEX.json. ByPath contains only explicit entity-bound article rows; pages
// absent from the plan are intentionally not Concepts. ActiveEntities is the
// source-concept coverage set and every member must have one bound article.
type SyntoIdentityPlan struct {
	ByPath         map[string]string
	ActiveEntities map[string]bool
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
	result := IDMap{Concept: map[string]string{}, DormantConcept: map[string]string{}, ConceptEntityID: map[string]string{}, Source: map[string]string{}, SourceMeta: map[string]SourceMeta{}, Redirects: map[string][]string{}, IDRedirects: map[string]string{}}
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
		case "id_redirects":
			result.IDRedirects, err = generation.DecodeBoundedMap[string](dec)
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

func IsSyntoRootPage(path string) bool {
	return path == "wiki/index.md" || path == "wiki/log.md"
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

// RebuildWithSyntoIdentity builds the derived artifacts for a Synto
// generation. It is deliberately separate from Rebuild: legacy projects keep
// frontmatter/content-derived ID behavior, while Synto uses entity_id as the
// Concept ID authority and canonicalizes entity-bound page frontmatter.
func RebuildWithSyntoIdentity(ctx context.Context, store Store, plan SyntoIdentityPlan) (IDMap, error) {
	files, err := store.ListMarkdownFiles(ctx, "wiki/")
	if err != nil {
		return IDMap{}, fmt.Errorf("list wiki/: %w", err)
	}
	if err := validateSyntoIdentityPlan(files, plan); err != nil {
		return IDMap{}, err
	}
	plan = syntoPlanForAvailableFiles(files, plan)
	filesByPath := make(map[string]MarkdownFile, len(files))
	for _, file := range files {
		filesByPath[file.Path] = file
	}
	rewrittenPages := make(map[string][]byte, len(plan.ByPath))
	for path, entityID := range plan.ByPath {
		file := filesByPath[path]
		page, err := RewriteSyntoConceptPage(file.Data, entityID)
		if err != nil {
			return IDMap{}, fmt.Errorf("rewrite %s: %w", path, err)
		}
		rewrittenPages[path] = page
	}

	next := IDMap{
		Concept:         map[string]string{},
		DormantConcept:  map[string]string{},
		ConceptEntityID: map[string]string{},
		Source:          map[string]string{},
		SourceMeta:      map[string]SourceMeta{},
		Redirects:       map[string][]string{},
	}
	for _, file := range files {
		if IsSyntoRootPage(file.Path) {
			continue
		}
		if entityID := plan.ByPath[file.Path]; entityID != "" {
			next.Concept[entityID] = file.Slug
		}
	}
	if err := addSourceEntries(ctx, store, next.Source, next.SourceMeta); err != nil {
		return next, err
	}
	old, err := readOldIDMap(ctx, store)
	if err != nil {
		return next, err
	}
	for sourceID, redirects := range old.Redirects {
		if _, isSource := old.Source[sourceID]; isSource {
			next.Redirects[sourceID] = append([]string(nil), redirects...)
		}
	}
	next.IDRedirects = cloneStringMap(old.IDRedirects)
	if _, err := planSyntoIDRedirects(&next, old); err != nil {
		return next, err
	}
	appendChangedRedirects(next.Redirects, old.Source, next.Source)

	idMapData, err := encodeIDMap(next)
	if err != nil {
		return next, err
	}
	cacheFiles := make([]MarkdownFile, len(files))
	copy(cacheFiles, files)
	for i := range cacheFiles {
		if page, ok := rewrittenPages[cacheFiles[i].Path]; ok {
			cacheFiles[i].Data = page
		}
	}
	conceptsData, err := buildSyntoConceptsJSONL(cacheFiles, plan)
	if err != nil {
		return next, err
	}
	if err := writeIDMap(ctx, store, idMapData); err != nil {
		return next, err
	}
	for path, page := range rewrittenPages {
		if _, err := store.WriteBytesAtomic(ctx, page, path+".tmp", path); err != nil {
			return next, fmt.Errorf("write %s: %w", path, err)
		}
	}
	if err := writeConceptsJSONL(ctx, store, conceptsData); err != nil {
		return next, err
	}
	return next, nil
}

// RewriteSyntoConceptPage makes an entity-bound page's top-level frontmatter
// identity canonical while preserving its body and unrelated frontmatter.
func RewriteSyntoConceptPage(data []byte, entityID string) ([]byte, error) {
	if !ValidSyntoEntityID(entityID) {
		return nil, fmt.Errorf("invalid Synto entity ID %q", entityID)
	}
	lineEnding := []byte("\n")
	if newline := bytes.IndexByte(data, '\n'); newline > 0 && data[newline-1] == '\r' {
		lineEnding = []byte("\r\n")
	}
	if !bytes.HasPrefix(data, []byte("---")) {
		prefix := []byte("---" + string(lineEnding) + "id: " + entityID + string(lineEnding) + "---" + string(lineEnding))
		return append(prefix, data...), nil
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) == 0 || !syntoFrontmatterFence(lines[0]) {
		return nil, errors.New("concept frontmatter is malformed")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if syntoFrontmatterFence(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, errors.New("concept frontmatter is unterminated")
	}
	frontmatterBytes := bytes.Join(lines[1:end], nil)
	root, err := validateSyntoFrontmatterYAML(frontmatterBytes)
	if err != nil {
		return nil, fmt.Errorf("unsafe concept frontmatter: %w", err)
	}
	var idNode *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "id" {
			idNode = root.Content[i+1]
			break
		}
	}
	if idNode != nil && (idNode.Kind != yaml.ScalarNode || idNode.Tag != "!!str") {
		return nil, errors.New("concept frontmatter id must be a string")
	}
	if idNode != nil && (!ValidLegacyConceptID(strings.TrimSpace(idNode.Value)) && !ValidSyntoEntityID(strings.TrimSpace(idNode.Value))) {
		return nil, fmt.Errorf("invalid concept frontmatter id %q", strings.TrimSpace(idNode.Value))
	}
	found := 0
	for i := 1; i < end; i++ {
		line := strings.TrimSuffix(strings.TrimSuffix(string(lines[i]), "\n"), "\r")
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key, _, ok := strings.Cut(line, ":")
		key = strings.Trim(strings.TrimSpace(key), "\"'")
		if !ok || key != "id" {
			continue
		}
		found++
		if found > 1 {
			return nil, errors.New("duplicate concept frontmatter id")
		}
		lineEnding := "\n"
		if bytes.HasSuffix(lines[i], []byte("\r\n")) {
			lineEnding = "\r\n"
		}
		prefix := line[:strings.Index(line, ":")+1]
		lines[i] = []byte(prefix + " " + entityID + lineEnding)
	}
	if idNode != nil && found == 0 {
		return nil, errors.New("concept frontmatter id cannot be rewritten safely")
	}
	if found == 0 {
		closingLine := lines[end]
		lines = append(lines, nil)
		copy(lines[end+1:], lines[end:])
		ending := "\n"
		if bytes.HasSuffix(closingLine, []byte("\r\n")) {
			ending = "\r\n"
		}
		lines[end] = []byte("id: " + entityID + ending)
	}
	return bytes.Join(lines, nil), nil
}

func syntoFrontmatterFence(line []byte) bool {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	line = bytes.TrimRight(line, " \t")
	return bytes.Equal(line, []byte("---"))
}

func validateSyntoFrontmatterYAML(data []byte) (*yaml.Node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode || document.Content[0].Style&yaml.FlowStyle != 0 {
		return nil, errors.New("frontmatter must be a YAML mapping")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("frontmatter contains multiple YAML documents")
		}
		return nil, err
	}
	if err := validateSyntoYAMLNode(document.Content[0]); err != nil {
		return nil, err
	}
	return document.Content[0], nil
}

func validateSyntoYAMLNode(node *yaml.Node) error {
	switch node.Kind {
	case yaml.MappingNode:
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return errors.New("frontmatter mapping keys must be strings")
			}
			if _, exists := seen[key.Value]; exists {
				return fmt.Errorf("duplicate frontmatter key %q", key.Value)
			}
			seen[key.Value] = struct{}{}
			if err := validateSyntoYAMLNode(node.Content[i+1]); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := validateSyntoYAMLNode(child); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		if node.Alias == nil {
			return errors.New("frontmatter alias is invalid")
		}
	}
	return nil
}

func planSyntoIDRedirects(next *IDMap, old IDMap) (int, error) {
	redirects := make(map[string]string, len(old.IDRedirects)+len(old.Concept))
	currentBySlug := make(map[string]string, len(next.Concept))
	for entityID, slug := range next.Concept {
		if previous, exists := currentBySlug[slug]; exists && previous != entityID {
			return 0, fmt.Errorf("multiple current concepts use slug %q", slug)
		}
		currentBySlug[slug] = entityID
	}
	add := func(source, target string) error {
		if !ValidLegacyConceptID(source) || source == target {
			return fmt.Errorf("invalid ID redirect source %q", source)
		}
		if _, current := next.Concept[source]; current {
			return fmt.Errorf("ID redirect source %q is an active concept", source)
		}
		if !ValidSyntoEntityID(target) {
			return fmt.Errorf("invalid ID redirect target %q", target)
		}
		if _, current := next.Concept[target]; !current {
			return fmt.Errorf("ID redirect target %q not found", target)
		}
		if previous, exists := redirects[source]; exists && previous != target {
			return fmt.Errorf("ID redirect conflict for %q", source)
		}
		redirects[source] = target
		return nil
	}
	for source, target := range old.IDRedirects {
		if err := add(source, strings.TrimSpace(target)); err != nil {
			return 0, err
		}
	}
	for _, concepts := range []map[string]string{old.Concept, old.DormantConcept} {
		for oldID, slug := range concepts {
			target := currentBySlug[slug]
			if target == "" || oldID == target {
				continue
			}
			if ValidSyntoEntityID(oldID) {
				// A prior different released ULID is not migration evidence.
				// The current explicit article.entity_id remains authoritative.
				continue
			}
			if !ValidLegacyConceptID(oldID) {
				return 0, fmt.Errorf("non-legacy prior concept ID %q cannot be migrated", oldID)
			}
			if err := add(oldID, target); err != nil {
				return 0, err
			}
		}
	}
	for source, target := range redirects {
		if _, chained := redirects[target]; chained {
			return 0, fmt.Errorf("ID redirect chain detected for %q", source)
		}
		if source == target {
			return 0, fmt.Errorf("ID redirect cycle detected for %q", source)
		}
	}
	next.IDRedirects = redirects
	return len(redirects) - len(old.IDRedirects), nil
}

func validateSyntoIdentityPlan(files []MarkdownFile, plan SyntoIdentityPlan) error {
	fileByPath := make(map[string]MarkdownFile, len(files))
	for _, file := range files {
		if IsSyntoRootPage(file.Path) {
			continue
		}
		if !validSyntoArticlePath(file.Path) || !validConceptSlug(file.Slug) {
			return fmt.Errorf("unsafe Synto article path %q", file.Path)
		}
		if _, exists := fileByPath[file.Path]; exists {
			return fmt.Errorf("duplicate Synto article path %q", file.Path)
		}
		fileByPath[file.Path] = file
	}
	entityPaths := make(map[string]string, len(plan.ByPath))
	for path, entityID := range plan.ByPath {
		if !validSyntoArticlePath(path) || !ValidSyntoEntityID(entityID) || entityID != strings.TrimSpace(entityID) {
			return fmt.Errorf("unsafe Synto identity mapping %q -> %q", path, entityID)
		}
		if _, exists := fileByPath[path]; !exists {
			return fmt.Errorf("Synto identity path %q is absent from wiki", path)
		}
		if previous, exists := entityPaths[entityID]; exists {
			return fmt.Errorf("Synto entity_id %q maps to multiple articles %q and %q", entityID, previous, path)
		}
		entityPaths[entityID] = path
	}
	for entityID := range plan.ActiveEntities {
		if !ValidSyntoEntityID(entityID) || entityID != strings.TrimSpace(entityID) {
			return fmt.Errorf("unsafe active Synto entity_id %q", entityID)
		}
		if _, exists := entityPaths[entityID]; !exists {
			return fmt.Errorf("active Synto entity_id %q has no entity-bound article", entityID)
		}
	}
	return nil
}

func syntoPlanForAvailableFiles(files []MarkdownFile, plan SyntoIdentityPlan) SyntoIdentityPlan {
	available := make(map[string]bool, len(files))
	for _, file := range files {
		available[file.Path] = true
	}
	filtered := SyntoIdentityPlan{ByPath: make(map[string]string), ActiveEntities: plan.ActiveEntities}
	for path, entityID := range plan.ByPath {
		if available[path] {
			filtered.ByPath[path] = entityID
		}
	}
	return filtered
}

func validSyntoArticlePath(path string) bool {
	if !strings.HasPrefix(path, "wiki/") || strings.ContainsAny(path, "\\") || strings.Contains(path, "//") || strings.Contains(path, "..") || strings.Contains(path, "/./") {
		return false
	}
	rel := strings.TrimPrefix(path, "wiki/")
	return strings.HasSuffix(rel, ".md") && !strings.Contains(rel, "/") && validConceptSlug(strings.TrimSuffix(rel, ".md"))
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
		if IsSyntoRootPage(file.Path) {
			continue
		}
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

// EncodeIDMap exposes the bounded ID-map encoding for generation-aware
// planners that add one-time compatibility redirects after derivation.
func EncodeIDMap(next IDMap) ([]byte, error) { return encodeIDMap(next) }

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
		if IsSyntoRootPage(file.Path) {
			continue
		}
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

func buildSyntoConceptsJSONL(files []MarkdownFile, plan SyntoIdentityPlan) ([]byte, error) {
	var builder strings.Builder
	for _, file := range files {
		entityID := plan.ByPath[file.Path]
		if entityID == "" || IsSyntoRootPage(file.Path) {
			continue
		}
		entry := parseCacheEntry(file.Slug, string(file.Data))
		if entry.Frontmatter == nil {
			entry.Frontmatter = make(map[string]interface{})
		}
		entry.Frontmatter["id"] = entityID
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

func cloneStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
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
