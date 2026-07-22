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
	EntityID  string
	Dormant   bool
	Page      []byte
	CacheRow  []byte
}

type reconciledConcept struct {
	CurrentID string
	StableID  string
	Slug      string
}

func validateConceptEntityMap(ids wikiindex.IDMap) error {
	seen := make(map[string]string, len(ids.ConceptEntityID))
	for conceptID, entityID := range ids.ConceptEntityID {
		if !annotation.ValidSourceID(conceptID) || !annotation.ValidSourceID(entityID) {
			return fmt.Errorf("unsafe concept entity mapping %q -> %q", conceptID, entityID)
		}
		if _, active := ids.Concept[conceptID]; !active {
			if _, dormant := ids.DormantConcept[conceptID]; !dormant {
				return fmt.Errorf("concept entity mapping has unknown LWC ID %q", conceptID)
			}
		}
		if old, exists := seen[entityID]; exists && old != conceptID {
			return fmt.Errorf("Synto entity_id %q maps to multiple LWC IDs", entityID)
		}
		seen[entityID] = conceptID
	}
	return nil
}

func snapshotConcepts(vault string) ([]conceptSnapshot, error) {
	data, err := readBoundedRegularFileWithin(vault, "cache/id_map.json")
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
	if err := validateConceptEntityMap(ids); err != nil {
		return nil, fmt.Errorf("validate prior concept map: %w", err)
	}
	if ids.Concept == nil {
		ids.Concept = map[string]string{}
	}
	if ids.DormantConcept == nil {
		ids.DormantConcept = map[string]string{}
	}
	conceptIDs := make([]string, 0, len(ids.Concept)+len(ids.DormantConcept))
	for conceptID := range ids.Concept {
		conceptIDs = append(conceptIDs, conceptID)
	}
	for conceptID := range ids.DormantConcept {
		if _, active := ids.Concept[conceptID]; active {
			return nil, fmt.Errorf("concept ID %q is both active and dormant", conceptID)
		}
		conceptIDs = append(conceptIDs, conceptID)
	}
	sort.Strings(conceptIDs)
	out := make([]conceptSnapshot, 0, len(conceptIDs))
	seenSlug := make(map[string]string, len(conceptIDs))
	for _, conceptID := range conceptIDs {
		slug, dormant := ids.Concept[conceptID], false
		if slug == "" {
			slug, dormant = ids.DormantConcept[conceptID], true
		}
		if !annotation.ValidSourceID(conceptID) || !safeConceptSlug(slug) {
			return nil, fmt.Errorf("unsafe prior concept mapping %q -> %q", conceptID, slug)
		}
		if priorID, exists := seenSlug[slug]; exists && priorID != conceptID {
			return nil, fmt.Errorf("duplicate prior concept slug %q", slug)
		}
		seenSlug[slug] = conceptID
		pagePath := filepath.Join("wiki", slug+".md")
		if dormant {
			pagePath = filepath.Join("wiki", ".dormant", slug+".md")
		}
		page, err := readBoundedRegularFileWithin(vault, filepath.ToSlash(pagePath))
		if errors.Is(err, os.ErrNotExist) {
			page = nil
		} else if err != nil {
			return nil, fmt.Errorf("read prior concept page %q: %w", slug, err)
		}
		row, err := readConceptCacheRow(vault, slug, dormant)
		if err != nil {
			return nil, err
		}
		out = append(out, conceptSnapshot{ConceptID: conceptID, Slug: slug, EntityID: ids.ConceptEntityID[conceptID], Dormant: dormant, Page: page, CacheRow: row})
	}
	return out, nil
}

func readConceptCacheRow(vault, slug string, dormant bool) ([]byte, error) {
	path := "cache/concepts.jsonl"
	if dormant {
		path = "cache/dormant_concepts.jsonl"
	}
	data, err := readBoundedRegularFileWithin(vault, path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read prior concept cache: %w", err)
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		row, err := decodeStrictConceptCacheRow(line)
		if err != nil {
			return nil, fmt.Errorf("decode prior concept cache: %w", err)
		}
		if value, ok := row["slug"].(string); ok && value == slug {
			return append([]byte(nil), line...), nil
		}
	}
	return nil, nil
}

func safeConceptSlug(slug string) bool {
	return strings.TrimSpace(slug) != "" && slug == strings.TrimSpace(slug) &&
		!strings.ContainsAny(slug, "/\\") && slug != "." && slug != ".."
}

func reconcileConceptIDMap(data []byte, prior []conceptSnapshot) ([]byte, []reconciledConcept, error) {
	return reconcileConceptIDMapWithEntities(data, prior, false)
}

func reconcileConceptIDMapWithEntities(data []byte, prior []conceptSnapshot, requireEntity bool) ([]byte, []reconciledConcept, error) {
	oldBySlug := make(map[string]string, len(prior))
	oldByEntity := make(map[string]string, len(prior))
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
		if concept.EntityID != "" {
			if !annotation.ValidSourceID(concept.EntityID) {
				return nil, nil, errors.New("unsafe prior concept entity mapping")
			}
			if old, exists := oldByEntity[concept.EntityID]; exists && old != concept.ConceptID {
				return nil, nil, fmt.Errorf("entity_id %q maps to multiple LWC IDs", concept.EntityID)
			}
			oldByEntity[concept.EntityID] = concept.ConceptID
		}
	}

	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return nil, nil, fmt.Errorf("decode generated concept map: %w", err)
	}
	if err := validateConceptEntityMap(ids); err != nil {
		return nil, nil, fmt.Errorf("validate generated concept map: %w", err)
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
	if ids.DormantConcept == nil {
		ids.DormantConcept = map[string]string{}
	}
	if ids.ConceptEntityID == nil {
		ids.ConceptEntityID = map[string]string{}
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

		entityID := strings.TrimSpace(ids.ConceptEntityID[currentID])
		if requireEntity && entityID == "" {
			return nil, nil, fmt.Errorf("missing Synto entity_id for concept %q", slug)
		}
		if entityID != "" && !annotation.ValidSourceID(entityID) {
			return nil, nil, fmt.Errorf("unsafe Synto entity_id %q", entityID)
		}
		stableID := currentID
		if priorID, ok := oldByEntity[entityID]; entityID != "" && ok {
			stableID = priorID
		} else if priorID, ok := oldBySlug[slug]; ok {
			prior := priorByID(prior, priorID)
			if requireEntity && prior.EntityID != "" && prior.EntityID != entityID {
				return nil, nil, fmt.Errorf("Synto entity mapping changed for slug %q", slug)
			}
			stableID = priorID
		}
		if !annotation.ValidSourceID(stableID) {
			return nil, nil, fmt.Errorf("unsafe reconciled concept ID %q", stableID)
		}
		if otherSlug, ok := used[stableID]; ok && otherSlug != slug {
			return nil, nil, fmt.Errorf("concept ID collision %q for %q and %q", stableID, otherSlug, slug)
		}
		if reservedSlug, ok := reservedByID[stableID]; ok && reservedSlug != slug {
			if oldByEntity[entityID] != stableID {
				return nil, nil, fmt.Errorf("concept ID %q is reserved for %q", stableID, reservedSlug)
			}
		}
		used[stableID] = slug
		translated[currentID] = stableID
		reconciled = append(reconciled, reconciledConcept{CurrentID: currentID, StableID: stableID, Slug: slug})
	}

	nextConcept := make(map[string]string, len(ids.Concept))
	nextEntity := make(map[string]string, len(ids.ConceptEntityID))
	priorEntityBySlug := make(map[string]string, len(prior))
	for _, old := range prior {
		if old.EntityID != "" {
			priorEntityBySlug[old.Slug] = old.EntityID
		}
	}
	for _, concept := range reconciled {
		nextConcept[concept.StableID] = concept.Slug
		entityID := ids.ConceptEntityID[concept.CurrentID]
		if entityID == "" {
			entityID = priorEntityBySlug[concept.Slug]
		}
		if entityID != "" {
			if !annotation.ValidSourceID(entityID) {
				return nil, nil, fmt.Errorf("unsafe Synto entity_id %q", entityID)
			}
			if priorID, exists := nextEntity[concept.StableID]; exists && priorID != entityID {
				return nil, nil, fmt.Errorf("Synto entity_id collision for concept %q", concept.StableID)
			}
			nextEntity[concept.StableID] = entityID
		}
	}
	ids.Concept = nextConcept
	ids.ConceptEntityID = nextEntity

	normalizedRedirects := make(map[string][]string, len(ids.Redirects))
	redirectKeys := make([]string, 0, len(ids.Redirects))
	for from := range ids.Redirects {
		redirectKeys = append(redirectKeys, from)
	}
	sort.Strings(redirectKeys)
	for _, from := range redirectKeys {
		// Validate before any normalization/persistence.
		if !annotation.ValidSourceID(from) {
			return nil, nil, fmt.Errorf("unsafe redirect key %q", from)
		}
		targets := ids.Redirects[from]
		for _, target := range targets {
			if !safeConceptSlug(target) {
				return nil, nil, fmt.Errorf("unsafe redirect target %q for %q", target, from)
			}
		}
		newFrom := normalizeTranslatedID(from, translated)
		if !annotation.ValidSourceID(newFrom) {
			return nil, nil, fmt.Errorf("unsafe redirect key %q", newFrom)
		}
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

func priorByID(prior []conceptSnapshot, id string) conceptSnapshot {
	for _, concept := range prior {
		if concept.ConceptID == id {
			return concept
		}
	}
	return conceptSnapshot{}
}

type plannedWrite struct {
	rel  string
	data []byte
}

func planConceptLifecycle(workspace string, data []byte, concepts []reconciledConcept, prior []conceptSnapshot, activeEntities map[string]bool) ([]byte, map[string]bool, []plannedWrite, []string, error) {
	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if ids.DormantConcept == nil {
		ids.DormantConcept = map[string]string{}
	}
	priorBySlug := make(map[string]conceptSnapshot, len(prior))
	for _, old := range prior {
		priorBySlug[old.Slug] = old
	}
	currentBySlug := make(map[string]reconciledConcept, len(concepts))
	dormant := make(map[string]bool)
	for _, concept := range concepts {
		currentBySlug[concept.Slug] = concept
		entityID := ids.ConceptEntityID[concept.StableID]
		if entityID == "" || !activeEntities[entityID] {
			dormant[concept.StableID] = true
		}
	}
	for _, old := range prior {
		if _, present := currentBySlug[old.Slug]; !present {
			dormant[old.ConceptID] = true
			ids.DormantConcept[old.ConceptID] = old.Slug
			if old.EntityID != "" {
				ids.ConceptEntityID[old.ConceptID] = old.EntityID
			}
		}
	}
	for id, slug := range ids.Concept {
		if dormant[id] {
			delete(ids.Concept, id)
			ids.DormantConcept[id] = slug
		}
	}
	for id := range ids.DormantConcept {
		if _, active := ids.Concept[id]; active {
			delete(ids.DormantConcept, id)
		}
	}

	var writes []plannedWrite
	var removals []string
	for id, slug := range ids.DormantConcept {
		old := priorBySlug[slug]
		page := old.Page
		if len(page) == 0 {
			page, err = readBoundedRegularFileWithin(workspace, filepath.ToSlash(filepath.Join("wiki", slug+".md")))
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("read dormant concept %q: %w", slug, err)
			}
		}
		writes = append(writes, plannedWrite{rel: filepath.ToSlash(filepath.Join("wiki", ".dormant", slug+".md")), data: page})
		removals = append(removals, filepath.ToSlash(filepath.Join("wiki", slug+".md")))
		_ = id
	}
	for _, old := range prior {
		if old.Dormant {
			if _, stillDormant := ids.DormantConcept[old.ConceptID]; !stillDormant {
				removals = append(removals, filepath.ToSlash(filepath.Join("wiki", ".dormant", old.Slug+".md")))
			}
		}
	}
	updated, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return updated, dormant, writes, removals, nil
}

func reconcileWorkspaceConcepts(workspace string, prior []conceptSnapshot, currentSources ...[]sourceSnapshot) error {
	// Plan/validate every output first, then apply. Input validation errors
	// must leave the live vault byte-identical (zero writes).
	data, err := readBoundedRegularFileWithin(workspace, "cache/id_map.json")
	if err != nil {
		return fmt.Errorf("read generated concept map: %w", err)
	}
	generated, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return fmt.Errorf("decode generated concept map: %w", err)
	}
	indexTruth, err := readSyntoIndexTruth(workspace)
	if err != nil {
		return err
	}
	if indexTruth.AmbiguousLineage {
		return errors.New("Synto merge/split lineage requires an owner-approved identity mapping")
	}
	entityIDs, err := readSyntoEntityIDs(workspace, generated.Concept)
	if err != nil {
		return err
	}
	data, err = mergeSyntoEntityIDs(data, entityIDs)
	if err != nil {
		return err
	}
	reconciledMap, concepts, err := reconcileConceptIDMapWithEntities(data, prior, entityIDs != nil)
	if err != nil {
		return err
	}

	translations := make(map[string]string, len(concepts))
	for _, concept := range concepts {
		if concept.CurrentID != concept.StableID {
			translations[concept.CurrentID] = concept.StableID
		}
	}

	dormant := map[string]bool{}
	lifecycleWrites := []plannedWrite(nil)
	removals := []string(nil)
	if entityIDs != nil {
		// An omitted source-truth argument is unavailable, not an instruction to
		// trust stale INDEX activity. Production callers pass the materialized
		// source slice, including an explicitly empty or tombstone-only slice.
		activeEntities := map[string]bool{}
		if len(currentSources) > 0 {
			activeEntities = activeSyntoEntities(indexTruth.SourceConcepts, currentSources[0])
		}
		reconciledMap, dormant, lifecycleWrites, removals, err = planConceptLifecycle(workspace, reconciledMap, concepts, prior, activeEntities)
		if err != nil {
			return err
		}
	}
	writes := make([]plannedWrite, 0, len(concepts)+len(lifecycleWrites)+3)
	writes = append(writes, plannedWrite{rel: "cache/id_map.json", data: reconciledMap})

	for _, concept := range concepts {
		if dormant[concept.StableID] {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("wiki", concept.Slug+".md"))
		page, err := readBoundedRegularFileWithin(workspace, rel)
		if err != nil {
			return fmt.Errorf("read generated concept %q: %w", concept.Slug, err)
		}
		page, err = rewriteConceptPageID(page, concept.CurrentID, concept.StableID)
		if err != nil {
			return fmt.Errorf("reconcile generated concept %q: %w", concept.Slug, err)
		}
		if len(translations) > 0 {
			page = rewriteConceptIDBearingWikilinks(page, concepts, translations)
		}
		writes = append(writes, plannedWrite{rel: rel, data: page})
	}

	otherWrites, err := planOtherConceptPageWikilinks(workspace, concepts, translations)
	if err != nil {
		return err
	}
	writes = append(writes, otherWrites...)

	cacheData, cachePresent, err := planConceptCacheIDs(workspace, concepts, translations)
	if err != nil {
		return err
	}
	if cachePresent {
		if entityIDs != nil {
			var dormantData []byte
			cacheData, dormantData, err = partitionConceptCacheRows(cacheData, dormant, concepts, prior)
			if err != nil {
				return err
			}
			writes = append(writes, plannedWrite{rel: "cache/dormant_concepts.jsonl", data: dormantData})
		}
		writes = append(writes, plannedWrite{rel: "cache/concepts.jsonl", data: cacheData})
	}
	writes = append(writes, lifecycleWrites...)

	// Deterministic apply order: id_map, concept pages (slug-sorted already),
	// other pages (walk order collected deterministically), then cache.
	for _, w := range writes {
		if err := writeFileAtomicWithin(workspace, w.rel, w.data); err != nil {
			return err
		}
	}
	for _, rel := range removals {
		if err := removeRegularFileWithin(workspace, rel); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func activeSyntoEntities(edges []syntoSourceConcept, sources []sourceSnapshot) map[string]bool {
	current := make(map[string]string, len(sources))
	for _, source := range sources {
		if source.Tombstone || source.RawPath == "" {
			continue
		}
		hash := source.SyntoContentHash
		if hash == "" {
			hash = syntoSourceContentHash(source)
		}
		current[source.RawPath] = hash
	}
	active := make(map[string]bool)
	for _, edge := range edges {
		if current[edge.SourcePath] == edge.ContentHash {
			active[edge.EntityID] = true
		}
	}
	return active
}

func partitionConceptCacheRows(data []byte, dormant map[string]bool, concepts []reconciledConcept, prior []conceptSnapshot) ([]byte, []byte, error) {
	bySlug := make(map[string]reconciledConcept, len(concepts))
	for _, concept := range concepts {
		bySlug[concept.Slug] = concept
	}
	priorBySlug := make(map[string]conceptSnapshot, len(prior))
	for _, old := range prior {
		priorBySlug[old.Slug] = old
	}
	var active, dormantRows bytes.Buffer
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		row, err := decodeStrictConceptCacheRow(line)
		if err != nil {
			return nil, nil, err
		}
		slug, ok := row["slug"].(string)
		if !ok || !safeConceptSlug(slug) {
			return nil, nil, errors.New("concept cache row has unsafe slug")
		}
		if concept, isCurrent := bySlug[slug]; isCurrent && dormant[concept.StableID] {
			if old := priorBySlug[slug]; len(old.CacheRow) > 0 {
				line = old.CacheRow
			}
			dormantRows.Write(line)
			dormantRows.WriteByte('\n')
			continue
		}
		active.Write(line)
		active.WriteByte('\n')
	}
	for _, old := range prior {
		if !dormant[old.ConceptID] || len(old.CacheRow) == 0 {
			continue
		}
		if !bytes.Contains(dormantRows.Bytes(), old.CacheRow) {
			dormantRows.Write(old.CacheRow)
			dormantRows.WriteByte('\n')
		}
	}
	return active.Bytes(), dormantRows.Bytes(), nil
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
	// Always scan for duplicate top-level id fields — even when the parsed
	// page ID already equals stableID (YAML keeps only one value).
	found := false
	for i := 1; i < end; i++ {
		line := lineWithoutEnding(lines[i])
		key, _, hasValue := strings.Cut(line, ":")
		if hasValue && !startsWithYAMLIndent(line) && strings.TrimSpace(key) == "id" {
			if found {
				return nil, errors.New("duplicate concept frontmatter id")
			}
			if pageID != stableID {
				lines[i] = rewriteTopLevelIDLine(lines[i], stableID)
			}
			found = true
		}
	}
	if !found {
		return nil, errors.New("concept frontmatter id is missing")
	}
	if pageID == stableID {
		return data, nil
	}
	return bytes.Join(lines, nil), nil
}

func rewriteOtherConceptPageWikilinks(workspace string, concepts []reconciledConcept, translations map[string]string) error {
	writes, err := planOtherConceptPageWikilinks(workspace, concepts, translations)
	if err != nil {
		return err
	}
	for _, w := range writes {
		if err := writeFileAtomicWithin(workspace, w.rel, w.data); err != nil {
			return err
		}
	}
	return nil
}

func planOtherConceptPageWikilinks(workspace string, concepts []reconciledConcept, translations map[string]string) ([]plannedWrite, error) {
	if len(translations) == 0 {
		return nil, nil
	}
	owned := make(map[string]struct{}, len(concepts))
	for _, concept := range concepts {
		owned[concept.Slug] = struct{}{}
	}
	root := filepath.Join(workspace, "wiki")
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var writes []plannedWrite
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		// Reject symlinks during walk before any content read/dereference.
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %q is not allowed", path)
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%q is not a regular file", path)
		}
		if info.Size() < 0 || info.Size() > generation.MaxFileBytes {
			return fmt.Errorf("%q exceeds generation size limit", path)
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if err := safeRelativePath(relSlash); err != nil {
			return err
		}
		slug := strings.TrimSuffix(entry.Name(), ".md")
		// Owned concept pages are rewritten separately. Nested paths such as
		// wiki/sources/<slug>.md are not concept pages and must still be scanned
		// for concept wikilinks (Source IDs/frontmatter are never rewritten here).
		if _, ok := owned[slug]; ok && relSlash == filepath.ToSlash(filepath.Join("wiki", slug+".md")) {
			return nil
		}
		data, err := readBoundedRegularFileWithin(workspace, relSlash)
		if err != nil {
			return err
		}
		updated := rewriteConceptIDBearingWikilinks(data, concepts, translations)
		if bytes.Equal(updated, data) {
			return nil
		}
		writes = append(writes, plannedWrite{rel: relSlash, data: updated})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return writes, nil
}

func rewriteConceptIDBearingWikilinks(data []byte, concepts []reconciledConcept, translations map[string]string) []byte {
	if len(translations) == 0 {
		return data
	}
	// Exact complete route identities only — never ID-prefix ReplaceAll.
	routes := make(map[string]string, len(concepts)*2)
	for _, concept := range concepts {
		stableID, ok := translations[concept.CurrentID]
		if !ok || stableID == concept.CurrentID {
			continue
		}
		fromBare := "concepts/" + concept.CurrentID
		toBare := "concepts/" + stableID
		routes[fromBare] = toBare
		if safeConceptSlug(concept.Slug) {
			routes[fromBare+"-"+concept.Slug] = toBare + "-" + concept.Slug
		}
	}
	if len(routes) == 0 {
		return data
	}
	return rewriteWikilinksOutsideMarkdownCode(data, func(inner string) (string, bool) {
		target, alias, hasAlias := strings.Cut(inner, "|")
		base, anchor, hasAnchor := strings.Cut(target, "#")
		to, ok := routes[strings.TrimSpace(base)]
		if !ok {
			return "", false
		}
		var b strings.Builder
		b.Grow(len(inner) + len(to) - len(strings.TrimSpace(base)) + 4)
		b.WriteString(to)
		if hasAnchor {
			b.WriteByte('#')
			b.WriteString(anchor)
		}
		if hasAlias {
			b.WriteByte('|')
			b.WriteString(alias)
		}
		return b.String(), true
	})
}

// rewriteWikilinksOutsideMarkdownCode rewrites [[...]] only outside fenced
// code blocks and inline code spans. Content inside code is left byte-identical.
func rewriteWikilinksOutsideMarkdownCode(data []byte, rewrite func(inner string) (string, bool)) []byte {
	var out bytes.Buffer
	out.Grow(len(data))
	i := 0
	lineStart := true
	for i < len(data) {
		if lineStart {
			if end, ok := scanFencedCodeBlock(data, i); ok {
				out.Write(data[i:end])
				i = end
				lineStart = i == 0 || (i > 0 && data[i-1] == '\n')
				continue
			}
		}
		if data[i] == '`' {
			end := scanInlineCodeSpan(data, i)
			out.Write(data[i:end])
			i = end
			lineStart = i > 0 && data[i-1] == '\n'
			continue
		}
		if i+1 < len(data) && data[i] == '[' && data[i+1] == '[' {
			closeAt := indexWikilinkClose(data, i+2)
			if closeAt >= 0 {
				inner := string(data[i+2 : closeAt])
				if replacement, ok := rewrite(inner); ok {
					out.WriteString("[[")
					out.WriteString(replacement)
					out.WriteString("]]")
				} else {
					out.Write(data[i : closeAt+2])
				}
				i = closeAt + 2
				lineStart = false
				continue
			}
		}
		b := data[i]
		out.WriteByte(b)
		i++
		lineStart = b == '\n'
	}
	return out.Bytes()
}

func scanFencedCodeBlock(data []byte, i int) (int, bool) {
	j := i
	// Optional CommonMark indent (≤3 spaces) then optional blockquote markers
	// (`>` with optional following space, including nested `>>`).
	j = skipMarkdownBlockquotePrefix(data, j)
	if j >= len(data) || (data[j] != '`' && data[j] != '~') {
		return 0, false
	}
	fenceChar := data[j]
	fenceLen := 0
	for j < len(data) && data[j] == fenceChar {
		j++
		fenceLen++
	}
	if fenceLen < 3 {
		return 0, false
	}
	// Consume the rest of the opening fence line.
	for j < len(data) && data[j] != '\n' {
		j++
	}
	if j < len(data) {
		j++
	}
	for j < len(data) {
		k := skipMarkdownBlockquotePrefix(data, j)
		closeLen := 0
		for k < len(data) && data[k] == fenceChar {
			k++
			closeLen++
		}
		if closeLen >= fenceLen {
			for k < len(data) && (data[k] == ' ' || data[k] == '\t') {
				k++
			}
			if k >= len(data) || data[k] == '\n' {
				if k < len(data) {
					k++
				}
				return k, true
			}
		}
		for j < len(data) && data[j] != '\n' {
			j++
		}
		if j < len(data) {
			j++
		}
	}
	// Unclosed fence: treat remainder as code so wikilink-like text is preserved.
	return len(data), true
}

// skipMarkdownBlockquotePrefix advances past optional ≤3 leading spaces and
// zero-or-more blockquote markers. Each nested `>` may have its own 0–3
// spaces of indentation before the next marker or fenced-code content.
func skipMarkdownBlockquotePrefix(data []byte, i int) int {
	j := i
	spaces := 0
	for j < len(data) && data[j] == ' ' && spaces < 3 {
		j++
		spaces++
	}
	for j < len(data) && data[j] == '>' {
		j++
		spaces = 0
		for j < len(data) && data[j] == ' ' && spaces < 3 {
			j++
			spaces++
		}
	}
	return j
}

func scanInlineCodeSpan(data []byte, i int) int {
	n := 0
	for i+n < len(data) && data[i+n] == '`' {
		n++
	}
	if n == 0 {
		return i + 1
	}
	j := i + n
	for j < len(data) {
		if data[j] != '`' {
			j++
			continue
		}
		m := 0
		for j+m < len(data) && data[j+m] == '`' {
			m++
		}
		if m == n {
			return j + m
		}
		j += m
	}
	// Unclosed: not a code span; consume only the first backtick.
	return i + 1
}

func indexWikilinkClose(data []byte, from int) int {
	for i := from; i+1 < len(data); i++ {
		if data[i] == '\n' {
			return -1
		}
		if data[i] == ']' && data[i+1] == ']' {
			return i
		}
	}
	return -1
}

func rewriteConceptCacheIDs(workspace string, concepts []reconciledConcept, translations map[string]string) error {
	data, present, err := planConceptCacheIDs(workspace, concepts, translations)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return writeFileAtomicWithin(workspace, "cache/concepts.jsonl", data)
}

func planConceptCacheIDs(workspace string, concepts []reconciledConcept, translations map[string]string) ([]byte, bool, error) {
	const rel = "cache/concepts.jsonl"
	info, err := lstatRegularWithin(workspace, rel)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Size() < 0 || info.Size() > generation.MaxFileBytes {
		return nil, false, errors.New("concepts cache size exceeds generation limit")
	}
	bySlug := make(map[string]reconciledConcept, len(concepts))
	for _, concept := range concepts {
		bySlug[concept.Slug] = concept
	}
	file, err := openRegularFileWithin(workspace, rel)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, generation.MaxFileBytes+1))
	scanner.Buffer(make([]byte, 64*1024), maxConceptJSONLLineBytes)
	var output bytes.Buffer
	rows := 0
	seenSlug := make(map[string]struct{}, len(concepts))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			output.WriteByte('\n')
			continue
		}
		if rows >= generation.MaxFiles {
			return nil, false, generation.ErrLogicalEntryLimit
		}
		rows++
		entry, err := decodeStrictConceptCacheRow(line)
		if err != nil {
			return nil, false, fmt.Errorf("decode concepts cache: %w", err)
		}
		slug, _ := entry["slug"].(string)
		if slug == "" {
			return nil, false, errors.New("concepts cache row is missing slug")
		}
		if _, dup := seenSlug[slug]; dup {
			return nil, false, fmt.Errorf("duplicate concepts cache slug %q", slug)
		}
		seenSlug[slug] = struct{}{}
		concept, ok := bySlug[slug]
		if !ok {
			return nil, false, fmt.Errorf("concepts cache slug %q is not declared in id map", slug)
		}
		if frontmatter, ok := entry["frontmatter"].(map[string]any); ok {
			if id, ok := frontmatter["id"].(string); ok {
				if id != concept.CurrentID && id != concept.StableID {
					return nil, false, fmt.Errorf("inconsistent concepts cache id %q for slug %q", id, slug)
				}
				frontmatter["id"] = concept.StableID
			}
		}
		if id, ok := entry["id"].(string); ok {
			if id != concept.CurrentID && id != concept.StableID {
				return nil, false, fmt.Errorf("inconsistent concepts cache top-level id %q for slug %q", id, slug)
			}
			entry["id"] = concept.StableID
		}
		if body, ok := entry["body"].(string); ok && len(translations) > 0 {
			entry["body"] = string(rewriteConceptIDBearingWikilinks([]byte(body), concepts, translations))
		}
		updated, err := json.Marshal(entry)
		if err != nil {
			return nil, false, err
		}
		if output.Len() > generation.MaxFileBytes-len(updated)-1 {
			return nil, false, errors.New("concepts cache size exceeds generation limit")
		}
		output.Write(updated)
		output.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("scan concepts cache: %w", err)
	}
	// Fail closed: when the cache artifact exists, every declared current
	// concept must appear exactly once.
	for _, concept := range concepts {
		if _, ok := seenSlug[concept.Slug]; !ok {
			return nil, false, fmt.Errorf("concepts cache missing slug %q declared in id map", concept.Slug)
		}
	}
	if len(seenSlug) != len(concepts) {
		return nil, false, errors.New("concepts cache rows do not match id map concepts")
	}
	return output.Bytes(), true, nil
}

func decodeStrictConceptCacheRow(line []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	value, err := decodeStrictJSONValue(dec)
	if err != nil {
		return nil, err
	}
	if err := generation.EnsureJSONEOF(dec); err != nil {
		return nil, err
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("concepts cache row must be a JSON object")
	}
	return obj, nil
}

// decodeStrictJSONValue recursively rejects duplicate object keys while
// streaming. Reuses the same EOF contract as generation/wikiindex decoders.
const maxStrictJSONDepth = 64

func decodeStrictJSONValue(dec *json.Decoder) (any, error) {
	return decodeStrictJSONValueAtDepth(dec, 0)
}

func decodeStrictJSONValueAtDepth(dec *json.Decoder, depth int) (any, error) {
	if depth > maxStrictJSONDepth {
		return nil, errors.New("JSON nesting depth exceeds limit")
	}
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			obj := make(map[string]any)
			seen := make(map[string]struct{})
			for dec.More() {
				if len(obj) >= generation.MaxFiles {
					return nil, generation.ErrLogicalEntryLimit
				}
				keyToken, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("expected JSON object key")
				}
				if _, exists := seen[key]; exists {
					return nil, errors.New("duplicate JSON object key")
				}
				seen[key] = struct{}{}
				val, err := decodeStrictJSONValueAtDepth(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := make([]any, 0)
			for dec.More() {
				if len(arr) >= generation.MaxFiles {
					return nil, generation.ErrLogicalEntryLimit
				}
				val, err := decodeStrictJSONValueAtDepth(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		default:
			return nil, fmt.Errorf("unexpected JSON delimiter %q", typed)
		}
	default:
		return typed, nil
	}
}

func readBoundedRegularFileWithin(root, rel string) ([]byte, error) {
	return readBoundedRegularFileWithinLimit(root, rel, generation.MaxFileBytes)
}

func readBoundedRegularFileWithinLimit(root, rel string, limit int64) ([]byte, error) {
	if err := safeRelativePath(rel); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	clean := filepath.FromSlash(rel)
	info, err := r.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", rel)
	}
	if limit < 0 || info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("%q exceeds generation size limit", rel)
	}
	file, err := r.Open(clean)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// Bound before allocation: never read more than MaxFileBytes+1.
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%q exceeds generation size limit", rel)
	}
	return data, nil
}

func lstatRegularWithin(root, rel string) (os.FileInfo, error) {
	if err := safeRelativePath(rel); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	info, err := r.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", rel)
	}
	return info, nil
}

func openRegularFileWithin(root, rel string) (*os.File, error) {
	if err := safeRelativePath(rel); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	clean := filepath.FromSlash(rel)
	info, err := r.Lstat(clean)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", rel)
	}
	return r.Open(clean)
}
