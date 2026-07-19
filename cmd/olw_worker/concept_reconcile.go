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
	"regexp"
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

// Matches Markdown wikilinks only. Plain URLs, inline code, and other prose
// that merely contain the same text are left byte-for-byte untouched.
var markdownWikilinkRE = regexp.MustCompile(`\[\[([^\[\]\n]+)\]\]`)

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
	data, err := readBoundedRegularFileWithin(workspace, "cache/id_map.json")
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
		if err := writeFileAtomicWithin(workspace, rel, page); err != nil {
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
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
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
		return writeFileAtomicWithin(workspace, relSlash, updated)
	})
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
	return markdownWikilinkRE.ReplaceAllFunc(data, func(match []byte) []byte {
		inner := match[2 : len(match)-2]
		target, alias, hasAlias := strings.Cut(string(inner), "|")
		base, anchor, hasAnchor := strings.Cut(target, "#")
		to, ok := routes[strings.TrimSpace(base)]
		if !ok {
			return match
		}
		var b strings.Builder
		b.Grow(len(match) + len(to) - len(strings.TrimSpace(base)))
		b.WriteString("[[")
		b.WriteString(to)
		if hasAnchor {
			b.WriteByte('#')
			b.WriteString(anchor)
		}
		if hasAlias {
			b.WriteByte('|')
			b.WriteString(alias)
		}
		b.WriteString("]]")
		return []byte(b.String())
	})
}

func rewriteConceptCacheIDs(workspace string, concepts []reconciledConcept, translations map[string]string) error {
	const rel = "cache/concepts.jsonl"
	info, err := lstatRegularWithin(workspace, rel)
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
	file, err := openRegularFileWithin(workspace, rel)
	if err != nil {
		return err
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
			return generation.ErrLogicalEntryLimit
		}
		rows++
		entry, err := decodeStrictConceptCacheRow(line)
		if err != nil {
			return fmt.Errorf("decode concepts cache: %w", err)
		}
		slug, _ := entry["slug"].(string)
		if slug == "" {
			return errors.New("concepts cache row is missing slug")
		}
		if _, dup := seenSlug[slug]; dup {
			return fmt.Errorf("duplicate concepts cache slug %q", slug)
		}
		seenSlug[slug] = struct{}{}
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
			entry["body"] = string(rewriteConceptIDBearingWikilinks([]byte(body), concepts, translations))
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
	// Fail closed: when the cache artifact exists, every declared current
	// concept must appear exactly once.
	for _, concept := range concepts {
		if _, ok := seenSlug[concept.Slug]; !ok {
			return fmt.Errorf("concepts cache missing slug %q declared in id map", concept.Slug)
		}
	}
	if len(seenSlug) != len(concepts) {
		return errors.New("concepts cache rows do not match id map concepts")
	}
	return writeFileAtomicWithin(workspace, rel, output.Bytes())
}

func decodeStrictConceptCacheRow(line []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
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
func decodeStrictJSONValue(dec *json.Decoder) (any, error) {
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
				val, err := decodeStrictJSONValue(dec)
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
				val, err := decodeStrictJSONValue(dec)
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
	if info.Size() < 0 || info.Size() > generation.MaxFileBytes {
		return nil, fmt.Errorf("%q exceeds generation size limit", rel)
	}
	file, err := r.Open(clean)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// Bound before allocation: never read more than MaxFileBytes+1.
	data, err := io.ReadAll(io.LimitReader(file, generation.MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > generation.MaxFileBytes {
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
