package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	fm "github.com/adrg/frontmatter"
	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/generation"
	"github.com/rayer/llm-wiki-bff/internal/wikiindex"
)

type conceptSnapshot struct {
	ConceptID string
	Slug      string
}

type reconciledConcept struct {
	CurrentID string
	StableID  string
	Slug      string
}

func snapshotConcepts(vault string) ([]conceptSnapshot, error) {
	data, err := readFileWithin(vault, "cache/id_map.json")
	if errors.Is(err, os.ErrNotExist) {
		return []conceptSnapshot{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read prior concept map: %w", err)
	}
	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return nil, fmt.Errorf("decode prior concept map: %w", err)
	}
	if ids.Concept == nil {
		return []conceptSnapshot{}, nil
	}
	conceptIDs := make([]string, 0, len(ids.Concept))
	for conceptID := range ids.Concept {
		conceptIDs = append(conceptIDs, conceptID)
	}
	sort.Strings(conceptIDs)
	out := make([]conceptSnapshot, 0, len(conceptIDs))
	seenSlug := make(map[string]string, len(conceptIDs))
	for _, conceptID := range conceptIDs {
		slug := strings.TrimSpace(ids.Concept[conceptID])
		if !annotation.ValidSourceID(conceptID) || !safeConceptSlug(slug) {
			return nil, fmt.Errorf("unsafe prior concept mapping %q -> %q", conceptID, slug)
		}
		if priorID, exists := seenSlug[slug]; exists && priorID != conceptID {
			return nil, fmt.Errorf("duplicate prior concept slug %q", slug)
		}
		seenSlug[slug] = conceptID
		out = append(out, conceptSnapshot{ConceptID: conceptID, Slug: slug})
	}
	return out, nil
}

func safeConceptSlug(slug string) bool {
	return strings.TrimSpace(slug) != "" && slug == strings.TrimSpace(slug) &&
		!strings.ContainsAny(slug, "/\\") && slug != "." && slug != ".."
}

func reconcileConceptIDMap(data []byte, prior []conceptSnapshot) ([]byte, []reconciledConcept, error) {
	oldBySlug := make(map[string]string, len(prior))
	reservedByID := make(map[string]string, len(prior))
	for _, concept := range prior {
		if !annotation.ValidSourceID(concept.ConceptID) || !safeConceptSlug(concept.Slug) {
			return nil, nil, errors.New("unsafe prior concept mapping")
		}
		if old, exists := oldBySlug[concept.Slug]; exists && old != concept.ConceptID {
			return nil, nil, fmt.Errorf("duplicate prior concept slug %q", concept.Slug)
		}
		if oldSlug, exists := reservedByID[concept.ConceptID]; exists && oldSlug != concept.Slug {
			return nil, nil, fmt.Errorf("concept ID %q is reserved for %q", concept.ConceptID, oldSlug)
		}
		oldBySlug[concept.Slug] = concept.ConceptID
		reservedByID[concept.ConceptID] = concept.Slug
	}

	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return nil, nil, fmt.Errorf("decode generated concept map: %w", err)
	}
	if ids.Concept == nil {
		ids.Concept = map[string]string{}
	}
	if ids.Source == nil {
		ids.Source = map[string]string{}
	}
	if ids.SourceMeta == nil {
		ids.SourceMeta = map[string]wikiindex.SourceMeta{}
	}
	if ids.Redirects == nil {
		ids.Redirects = map[string][]string{}
	}

	currentIDs := make([]string, 0, len(ids.Concept))
	for currentID := range ids.Concept {
		currentIDs = append(currentIDs, currentID)
	}
	sort.Strings(currentIDs)

	reconciled := make([]reconciledConcept, 0, len(ids.Concept))
	translated := make(map[string]string, len(ids.Concept))
	used := make(map[string]string, len(ids.Concept))
	seenSlug := make(map[string]string, len(ids.Concept))
	for _, currentID := range currentIDs {
		slug := strings.TrimSpace(ids.Concept[currentID])
		if !annotation.ValidSourceID(currentID) || !safeConceptSlug(slug) || slug != ids.Concept[currentID] {
			return nil, nil, fmt.Errorf("unsafe generated concept mapping %q -> %q", currentID, ids.Concept[currentID])
		}
		if otherID, ok := seenSlug[slug]; ok {
			return nil, nil, fmt.Errorf("duplicate generated concept slug %q for %q and %q", slug, otherID, currentID)
		}
		seenSlug[slug] = currentID

		stableID := currentID
		if priorID, ok := oldBySlug[slug]; ok {
			stableID = priorID
		}
		if !annotation.ValidSourceID(stableID) {
			return nil, nil, fmt.Errorf("unsafe reconciled concept ID %q", stableID)
		}
		if otherSlug, ok := used[stableID]; ok && otherSlug != slug {
			return nil, nil, fmt.Errorf("concept ID collision %q for %q and %q", stableID, otherSlug, slug)
		}
		if reservedSlug, ok := reservedByID[stableID]; ok && reservedSlug != slug {
			return nil, nil, fmt.Errorf("concept ID %q is reserved for %q", stableID, reservedSlug)
		}
		used[stableID] = slug
		translated[currentID] = stableID
		reconciled = append(reconciled, reconciledConcept{CurrentID: currentID, StableID: stableID, Slug: slug})
	}

	nextConcept := make(map[string]string, len(ids.Concept))
	for _, concept := range reconciled {
		nextConcept[concept.StableID] = concept.Slug
	}
	ids.Concept = nextConcept

	normalizedRedirects := make(map[string][]string, len(ids.Redirects))
	redirectKeys := make([]string, 0, len(ids.Redirects))
	for from := range ids.Redirects {
		redirectKeys = append(redirectKeys, from)
	}
	sort.Strings(redirectKeys)
	for _, from := range redirectKeys {
		newFrom := normalizeTranslatedID(from, translated)
		targets := ids.Redirects[from]
		normalizedTargets := make([]string, len(targets))
		copy(normalizedTargets, targets)
		if existing, ok := normalizedRedirects[newFrom]; ok && !equalStrings(existing, normalizedTargets) {
			return nil, nil, fmt.Errorf("redirect ID collision %q", newFrom)
		}
		normalizedRedirects[newFrom] = normalizedTargets
	}
	ids.Redirects = normalizedRedirects

	sort.Slice(reconciled, func(i, j int) bool { return reconciled[i].Slug < reconciled[j].Slug })
	out, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return out, reconciled, nil
}

func reconcileWorkspaceConcepts(workspace string, prior []conceptSnapshot) error {
	mapPath := filepath.Join(workspace, "cache", "id_map.json")
	data, err := os.ReadFile(mapPath)
	if err != nil {
		return fmt.Errorf("read generated concept map: %w", err)
	}
	reconciledMap, concepts, err := reconcileConceptIDMap(data, prior)
	if err != nil {
		return err
	}
	if err := writeFileAtomicWithin(workspace, "cache/id_map.json", reconciledMap); err != nil {
		return fmt.Errorf("write reconciled concept map: %w", err)
	}

	translations := make(map[string]string, len(concepts))
	for _, concept := range concepts {
		if concept.CurrentID != concept.StableID {
			translations[concept.CurrentID] = concept.StableID
		}
	}
	for _, concept := range concepts {
		path := filepath.Join(workspace, "wiki", concept.Slug+".md")
		page, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read generated concept %q: %w", concept.Slug, err)
		}
		page, err = rewriteConceptPageID(page, concept.CurrentID, concept.StableID)
		if err != nil {
			return fmt.Errorf("reconcile generated concept %q: %w", concept.Slug, err)
		}
		if len(translations) > 0 {
			page = rewriteConceptIDBearingWikilinks(page, translations)
		}
		if err := writeFileAtomicWithin(workspace, filepath.ToSlash(filepath.Join("wiki", concept.Slug+".md")), page); err != nil {
			return err
		}
	}
	if err := rewriteOtherConceptPageWikilinks(workspace, concepts, translations); err != nil {
		return err
	}
	return rewriteConceptCacheIDs(workspace, concepts, translations)
}

func rewriteConceptPageID(data []byte, currentID, stableID string) ([]byte, error) {
	// Page identity is rewritten only where present. Postprocess may map a
	// content-hash concept ID for pages that still lack frontmatter ids.
	if !bytes.HasPrefix(data, []byte("---")) {
		return data, nil
	}
	var matter struct {
		ID string `yaml:"id"`
	}
	if _, err := fm.MustParse(strings.NewReader(string(data)), &matter); err != nil {
		return nil, err
	}
	pageID := strings.TrimSpace(matter.ID)
	if pageID == "" {
		return data, nil
	}
	if pageID != currentID && pageID != stableID {
		return nil, fmt.Errorf("inconsistent concept page id %q (want %q or %q)", pageID, currentID, stableID)
	}
	if pageID == stableID {
		return data, nil
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) == 0 || strings.TrimSpace(string(lines[0])) != "---" {
		return nil, errors.New("concept frontmatter is missing")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if isFrontmatterDelimiter(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, errors.New("concept frontmatter is unterminated")
	}
	found := false
	for i := 1; i < end; i++ {
		line := lineWithoutEnding(lines[i])
		key, _, hasValue := strings.Cut(line, ":")
		if hasValue && !startsWithYAMLIndent(line) && strings.TrimSpace(key) == "id" {
			if found {
				return nil, errors.New("duplicate concept frontmatter id")
			}
			lines[i] = rewriteTopLevelIDLine(lines[i], stableID)
			found = true
		}
	}
	if !found {
		return nil, errors.New("concept frontmatter id is missing")
	}
	return bytes.Join(lines, nil), nil
}

func rewriteOtherConceptPageWikilinks(workspace string, concepts []reconciledConcept, translations map[string]string) error {
	if len(translations) == 0 {
		return nil
	}
	owned := make(map[string]struct{}, len(concepts))
	for _, concept := range concepts {
		owned[concept.Slug] = struct{}{}
	}
	root := filepath.Join(workspace, "wiki")
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && filepath.Base(path) == "sources" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			return nil
		}
		slug := strings.TrimSuffix(entry.Name(), ".md")
		if _, ok := owned[slug]; ok {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		updated := rewriteConceptIDBearingWikilinks(data, translations)
		if bytes.Equal(updated, data) {
			return nil
		}
		rel := filepath.ToSlash(filepath.Join("wiki", strings.TrimPrefix(filepath.ToSlash(path), filepath.ToSlash(root)+"/")))
		return writeFileAtomicWithin(workspace, rel, updated)
	})
}

func rewriteConceptIDBearingWikilinks(data []byte, translations map[string]string) []byte {
	if len(translations) == 0 {
		return data
	}
	// Longest current IDs first so one ID cannot partially rewrite another.
	currentIDs := make([]string, 0, len(translations))
	for currentID := range translations {
		currentIDs = append(currentIDs, currentID)
	}
	sort.Slice(currentIDs, func(i, j int) bool {
		if len(currentIDs[i]) != len(currentIDs[j]) {
			return len(currentIDs[i]) > len(currentIDs[j])
		}
		return currentIDs[i] < currentIDs[j]
	})
	out := string(data)
	for _, currentID := range currentIDs {
		stableID := translations[currentID]
		// Canonical ID-bearing concept forms used by BFF routing.
		for _, sep := range []string{"-", "|", "#", "]"} {
			from := "concepts/" + currentID + sep
			to := "concepts/" + stableID + sep
			out = strings.ReplaceAll(out, from, to)
		}
	}
	return []byte(out)
}

func rewriteConceptCacheIDs(workspace string, concepts []reconciledConcept, translations map[string]string) error {
	path := filepath.Join(workspace, "cache", "concepts.jsonl")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() < 0 || info.Size() > generation.MaxFileBytes {
		return errors.New("concepts cache size exceeds generation limit")
	}
	bySlug := make(map[string]reconciledConcept, len(concepts))
	for _, concept := range concepts {
		bySlug[concept.Slug] = concept
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, generation.MaxFileBytes+1))
	scanner.Buffer(make([]byte, 64*1024), maxConceptJSONLLineBytes)
	var output bytes.Buffer
	rows := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			output.WriteByte('\n')
			continue
		}
		if rows >= generation.MaxFiles {
			return generation.ErrLogicalEntryLimit
		}
		rows++
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			return fmt.Errorf("decode concepts cache: %w", err)
		}
		slug, _ := entry["slug"].(string)
		concept, ok := bySlug[slug]
		if !ok {
			return fmt.Errorf("concepts cache slug %q is not declared in id map", slug)
		}
		if frontmatter, ok := entry["frontmatter"].(map[string]any); ok {
			if id, ok := frontmatter["id"].(string); ok {
				if id != concept.CurrentID && id != concept.StableID {
					return fmt.Errorf("inconsistent concepts cache id %q for slug %q", id, slug)
				}
				frontmatter["id"] = concept.StableID
			}
		}
		if id, ok := entry["id"].(string); ok {
			if id != concept.CurrentID && id != concept.StableID {
				return fmt.Errorf("inconsistent concepts cache top-level id %q for slug %q", id, slug)
			}
			entry["id"] = concept.StableID
		}
		if body, ok := entry["body"].(string); ok && len(translations) > 0 {
			entry["body"] = string(rewriteConceptIDBearingWikilinks([]byte(body), translations))
		}
		updated, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if output.Len() > generation.MaxFileBytes-len(updated)-1 {
			return errors.New("concepts cache size exceeds generation limit")
		}
		output.Write(updated)
		output.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan concepts cache: %w", err)
	}
	return writeFileAtomicWithin(workspace, "cache/concepts.jsonl", output.Bytes())
}
