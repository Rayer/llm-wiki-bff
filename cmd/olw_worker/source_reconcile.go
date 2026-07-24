package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
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
	"gopkg.in/yaml.v2"
)

type reconciledSource struct {
	CurrentID string
	StableID  string
	Slug      string
	RawPath   string
}

const maxConceptJSONLLineBytes = 16 << 20

func reconcileSourceIDMap(data []byte, prior []sourceSnapshot) ([]byte, []reconciledSource, error) {
	oldByRaw := make(map[string]string, len(prior))
	reservedByID := make(map[string]string, len(prior))
	for _, source := range prior {
		if !annotation.ValidSourceID(source.SourceID) || !safeMappedRawPath(source.RawPath) {
			return nil, nil, errors.New("unsafe prior source mapping")
		}
		if old, exists := oldByRaw[source.RawPath]; exists && old != source.SourceID {
			return nil, nil, fmt.Errorf("duplicate prior source mapping for %q", source.RawPath)
		}
		if oldRaw, exists := reservedByID[source.SourceID]; exists && oldRaw != source.RawPath {
			return nil, nil, fmt.Errorf("source ID %q is reserved for %q", source.SourceID, oldRaw)
		}
		oldByRaw[source.RawPath] = source.SourceID
		reservedByID[source.SourceID] = source.RawPath
	}
	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return nil, nil, fmt.Errorf("decode generated source map: %w", err)
	}
	if ids.Source == nil {
		ids.Source = map[string]string{}
	}
	if ids.SourceMeta == nil {
		ids.SourceMeta = map[string]wikiindex.SourceMeta{}
	}
	for id := range ids.SourceMeta {
		if _, ok := ids.Source[id]; !ok {
			return nil, nil, fmt.Errorf("source metadata %q has no source mapping", id)
		}
	}
	currentIDs := make([]string, 0, len(ids.Source))
	for currentID := range ids.Source {
		currentIDs = append(currentIDs, currentID)
	}
	sort.Strings(currentIDs)
	reconciled := make([]reconciledSource, 0, len(ids.Source))
	translated := make(map[string]string, len(ids.Source))
	used := make(map[string]string, len(ids.Source))
	seenRaw := make(map[string]string, len(ids.Source))
	for _, currentID := range currentIDs {
		slug := ids.Source[currentID]
		if !annotation.ValidSourceID(currentID) || strings.TrimSpace(slug) == "" || strings.ContainsAny(slug, "/\\") || slug == "." || slug == ".." {
			return nil, nil, fmt.Errorf("unsafe generated source mapping %q -> %q", currentID, slug)
		}
		meta, ok := ids.SourceMeta[currentID]
		if !ok || strings.TrimSpace(meta.SourceFile) == "" || !safeMappedRawPath(meta.SourceFile) || meta.Slug != slug {
			return nil, nil, fmt.Errorf("missing or unsafe source metadata for %q", currentID)
		}
		if otherID, ok := seenRaw[meta.SourceFile]; ok {
			return nil, nil, fmt.Errorf("duplicate generated source mapping %q and %q -> %q", otherID, currentID, meta.SourceFile)
		}
		seenRaw[meta.SourceFile] = currentID
		stableID := currentID
		if priorID, ok := oldByRaw[meta.SourceFile]; ok {
			stableID = priorID
		}
		if !annotation.ValidSourceID(stableID) {
			return nil, nil, fmt.Errorf("unsafe reconciled source ID %q", stableID)
		}
		if otherPath, ok := used[stableID]; ok && otherPath != meta.SourceFile {
			return nil, nil, fmt.Errorf("source ID collision %q for %q and %q", stableID, otherPath, meta.SourceFile)
		}
		if reservedPath, ok := reservedByID[stableID]; ok && reservedPath != meta.SourceFile {
			return nil, nil, fmt.Errorf("source ID %q is reserved for %q", stableID, reservedPath)
		}
		used[stableID] = meta.SourceFile
		translated[currentID] = stableID
		reconciled = append(reconciled, reconciledSource{CurrentID: currentID, StableID: stableID, Slug: slug, RawPath: meta.SourceFile})
	}
	nextSource := make(map[string]string, len(ids.Source))
	nextMeta := make(map[string]wikiindex.SourceMeta, len(ids.SourceMeta))
	for _, source := range reconciled {
		nextSource[source.StableID] = source.Slug
		nextMeta[source.StableID] = ids.SourceMeta[source.CurrentID]
	}
	ids.Source, ids.SourceMeta = nextSource, nextMeta
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
		for i, target := range targets {
			// Redirect values are slugs. A slug can equal a transient source ID,
			// so translating it would corrupt the destination.
			normalizedTargets[i] = target
		}
		if existing, ok := normalizedRedirects[newFrom]; ok && !equalStrings(existing, normalizedTargets) {
			return nil, nil, fmt.Errorf("redirect ID collision %q", newFrom)
		}
		normalizedRedirects[newFrom] = normalizedTargets
	}
	ids.Redirects = normalizedRedirects
	sort.Slice(reconciled, func(i, j int) bool { return reconciled[i].RawPath < reconciled[j].RawPath })
	out, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return out, reconciled, nil
}

func normalizeTranslatedID(id string, translated map[string]string) string {
	for steps := 0; steps <= len(translated); steps++ {
		next, ok := translated[id]
		if !ok || next == id {
			return id
		}
		id = next
	}
	return id
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type syntoSourcePage struct {
	fields map[string]interface{}
}

func collapseSyntoSourceRegeneration(workspace string, data []byte, prior []sourceSnapshot) ([]byte, []string, error) {
	ids, err := wikiindex.DecodeIDMap(data)
	if err != nil {
		return nil, nil, fmt.Errorf("decode generated source map: %w", err)
	}
	oldByRaw := make(map[string]string, len(prior))
	for _, source := range prior {
		if !annotation.ValidSourceID(source.SourceID) || !safeMappedRawPath(source.RawPath) {
			return nil, nil, errors.New("unsafe prior source mapping")
		}
		oldByRaw[source.RawPath] = source.SourceID
	}

	byRaw := make(map[string][]string)
	for currentID, meta := range ids.SourceMeta {
		if _, ok := ids.Source[currentID]; !ok {
			return nil, nil, fmt.Errorf("source metadata %q has no source mapping", currentID)
		}
		byRaw[meta.SourceFile] = append(byRaw[meta.SourceFile], currentID)
	}

	rawPaths := make([]string, 0, len(byRaw))
	for rawPath := range byRaw {
		rawPaths = append(rawPaths, rawPath)
	}
	sort.Strings(rawPaths)

	var removals []string
	for _, rawPath := range rawPaths {
		currentIDs := byRaw[rawPath]
		sort.Strings(currentIDs)
		if len(currentIDs) != 2 {
			continue
		}
		priorID, ok := oldByRaw[rawPath]
		if !ok {
			continue
		}
		stableCurrent := ""
		transientCurrent := ""
		for _, currentID := range currentIDs {
			if currentID == priorID {
				if stableCurrent != "" {
					return nil, nil, errors.New("unprovable Synto source regeneration pair")
				}
				stableCurrent = currentID
			} else {
				if transientCurrent != "" {
					return nil, nil, errors.New("unprovable Synto source regeneration pair")
				}
				transientCurrent = currentID
			}
		}
		if stableCurrent == "" || transientCurrent == "" {
			return nil, nil, errors.New("unprovable Synto source regeneration pair")
		}

		stableSlug := ids.Source[stableCurrent]
		transientSlug := ids.Source[transientCurrent]
		stableRel, err := safeSourcePagePath(stableSlug)
		if err != nil {
			return nil, nil, err
		}
		transientRel, err := safeSourcePagePath(transientSlug)
		if err != nil || stableRel == transientRel {
			return nil, nil, errors.New("unprovable Synto source regeneration pair")
		}
		stablePage, err := readBoundedRegularFileWithin(workspace, stableRel)
		if err != nil {
			return nil, nil, errors.New("legacy Synto source page is unavailable")
		}
		transientPage, err := readBoundedRegularFileWithin(workspace, transientRel)
		if err != nil {
			return nil, nil, errors.New("generated Synto source page is unavailable")
		}
		stableMatter, err := parseSyntoSourcePage(stablePage)
		if err != nil || !sourcePageMatches(stableMatter, priorID, rawPath) {
			return nil, nil, errors.New("unprovable Synto source regeneration pair")
		}
		transientMatter, err := parseSyntoSourcePage(transientPage)
		if err != nil || !isSyntoSourceSummary(transientMatter, rawPath) {
			return nil, nil, errors.New("unprovable Synto source regeneration pair")
		}
		if transientCurrent != wikiindex.ContentDerivedID(transientPage) {
			return nil, nil, errors.New("unprovable Synto source regeneration pair")
		}

		for otherID, otherSlug := range ids.Source {
			if (otherID != stableCurrent && otherSlug == stableSlug) || (otherID != transientCurrent && otherSlug == transientSlug) {
				return nil, nil, errors.New("source page collision")
			}
		}
		redirected := false
		for _, redirect := range ids.Redirects[priorID] {
			if redirect == stableSlug {
				redirected = true
				break
			}
		}
		if stableSlug != "" && stableSlug != transientSlug && !redirected {
			ids.Redirects[priorID] = append(ids.Redirects[priorID], stableSlug)
		}
		delete(ids.Source, stableCurrent)
		delete(ids.SourceMeta, stableCurrent)
		removals = append(removals, stableRel)
	}
	if len(removals) == 0 {
		return data, nil, nil
	}
	collapsed, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return collapsed, removals, nil
}

func safeSourcePagePath(slug string) (string, error) {
	if strings.TrimSpace(slug) == "" || strings.ContainsAny(slug, "/\\") || slug == "." || slug == ".." || strings.IndexFunc(slug, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", errors.New("unsafe generated source page path")
	}
	return filepath.ToSlash(filepath.Join("wiki", "sources", slug+".md")), nil
}

func parseSyntoSourcePage(data []byte) (syntoSourcePage, error) {
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) == 0 || strings.TrimSpace(string(lines[0])) != "---" {
		return syntoSourcePage{}, errors.New("source frontmatter is missing")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if isFrontmatterDelimiter(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return syntoSourcePage{}, errors.New("source frontmatter is unterminated")
	}
	fields := make(map[string]interface{})
	if err := yaml.UnmarshalStrict(bytes.Join(lines[1:end], nil), &fields); err != nil {
		return syntoSourcePage{}, err
	}
	return syntoSourcePage{fields: fields}, nil
}

func sourcePageMatches(page syntoSourcePage, id, rawPath string) bool {
	value, ok := page.fields["id"].(string)
	if !ok || value != id {
		return false
	}
	return sourcePageRawPath(page) == rawPath
}

func sourcePageRawPath(page syntoSourcePage) string {
	value, _ := page.fields["source_file"].(string)
	return value
}

func isSyntoSourceSummary(page syntoSourcePage, rawPath string) bool {
	if _, hasID := page.fields["id"]; hasID {
		return false
	}
	allowed := map[string]bool{
		"title": true, "aliases": true, "tags": true, "status": true,
		"source_file": true, "quality": true, "created": true, "source_url": true,
	}
	for key := range page.fields {
		if !allowed[key] {
			return false
		}
	}
	title, ok := boundedSyntoString(page.fields["title"], true)
	if !ok || title == "" {
		return false
	}
	if sourceFile, ok := boundedSyntoString(page.fields["source_file"], true); !ok || sourceFile != rawPath || !safeMappedRawPath(sourceFile) {
		return false
	}
	if _, ok := boundedSyntoString(page.fields["status"], true); !ok {
		return false
	}
	if _, ok := boundedSyntoString(page.fields["quality"], true); !ok {
		return false
	}
	if _, ok := boundedSyntoString(page.fields["created"], true); !ok {
		return false
	}
	if _, ok := syntoStringList(page.fields["aliases"]); !ok {
		return false
	}
	if sourceURL, present := page.fields["source_url"]; present {
		if _, ok := boundedSyntoString(sourceURL, false); !ok {
			return false
		}
	}
	tags, ok := syntoStringList(page.fields["tags"])
	if !ok {
		return false
	}
	for _, tag := range tags {
		if tag == "source" {
			return true
		}
	}
	return false
}

func boundedSyntoString(value interface{}, nonEmpty bool) (string, bool) {
	text, ok := value.(string)
	if !ok || len(text) > 4096 || strings.IndexFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", false
	}
	if nonEmpty && strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

func syntoStringList(value interface{}) ([]string, bool) {
	items, ok := value.([]interface{})
	if !ok || len(items) > generation.MaxFiles {
		return nil, false
	}
	result := make([]string, len(items))
	for i, item := range items {
		text, ok := boundedSyntoString(item, false)
		if !ok {
			return nil, false
		}
		result[i] = text
	}
	return result, true
}

func reconcileWorkspaceSources(workspace string, prior []sourceSnapshot) error {
	// Plan/validate every output first, then apply. Input validation errors
	// must leave the live vault byte-identical (zero writes).
	data, err := readBoundedRegularFileWithin(workspace, "cache/id_map.json")
	if err != nil {
		return fmt.Errorf("read generated source map: %w", err)
	}
	collapsedData, sourceRemovals, err := collapseSyntoSourceRegeneration(workspace, data, prior)
	if err != nil {
		return err
	}
	for _, rel := range sourceRemovals {
		if !strings.HasPrefix(rel, "wiki/sources/") {
			return errors.New("unsafe source page removal")
		}
		if _, err := lstatRegularWithin(workspace, rel); err != nil {
			return err
		}
	}
	reconciledMap, sources, err := reconcileSourceIDMap(collapsedData, prior)
	if err != nil {
		return err
	}
	translations := make(map[string]string, len(sources))
	for _, source := range sources {
		if source.CurrentID != source.StableID {
			translations[source.CurrentID] = source.StableID
		}
	}

	writes := make([]plannedWrite, 0, len(sources)+3)
	writes = append(writes, plannedWrite{rel: "cache/id_map.json", data: reconciledMap})

	cacheData, cachePresent, err := planConceptSourceReferences(workspace, translations)
	if err != nil {
		return err
	}
	if cachePresent {
		writes = append(writes, plannedWrite{rel: "cache/concepts.jsonl", data: cacheData})
	}

	pageWrites, err := planConceptPageSourceReferences(workspace, translations)
	if err != nil {
		return err
	}
	writes = append(writes, pageWrites...)

	sourceWrites, err := planSourcePageIDAndAnnotations(workspace, sources, prior)
	if err != nil {
		return err
	}
	writes = append(writes, sourceWrites...)

	// Deterministic apply only after all validation succeeds.
	for _, w := range writes {
		if err := writeFileAtomicWithin(workspace, w.rel, w.data); err != nil {
			return err
		}
	}
	for _, rel := range sourceRemovals {
		if err := removeRegularFileWithin(workspace, rel); err != nil {
			return err
		}
	}
	return nil
}

func rewriteConceptSourceReferences(workspace string, translations map[string]string) error {
	data, present, err := planConceptSourceReferences(workspace, translations)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return writeFileAtomicWithin(workspace, "cache/concepts.jsonl", data)
}

func planConceptSourceReferences(workspace string, translations map[string]string) ([]byte, bool, error) {
	if len(translations) == 0 {
		return nil, false, nil
	}
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
	file, err := openRegularFileWithin(workspace, rel)
	if err != nil {
		return nil, false, err
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
			return nil, false, generation.ErrLogicalEntryLimit
		}
		rows++
		// Fail closed on duplicate keys / trailing data before Source rewrite can
		// sanitize the row into a form Concept reconciliation would accept.
		entry, err := decodeStrictConceptCacheRow(line)
		if err != nil {
			return nil, false, fmt.Errorf("decode concepts cache: %w", err)
		}
		rewriteSourceReferenceField(entry, "sources", translations)
		if frontmatter, ok := entry["frontmatter"].(map[string]any); ok {
			rewriteSourceReferenceField(frontmatter, "sources", translations)
			rewriteSourceReferenceField(frontmatter, "source", translations)
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
	return output.Bytes(), true, nil
}

func rewriteSourceReferenceField(fields map[string]any, key string, translations map[string]string) bool {
	value, ok := fields[key]
	if !ok {
		return false
	}
	changed := false
	switch typed := value.(type) {
	case string:
		if stable, exists := translations[typed]; exists {
			fields[key] = stable
			changed = true
		}
	case []any:
		for i, item := range typed {
			if id, ok := item.(string); ok {
				if stable, exists := translations[id]; exists {
					typed[i] = stable
					changed = true
				}
			}
		}
	}
	return changed
}

func rewriteConceptPageSourceReferences(workspace string, translations map[string]string) error {
	writes, err := planConceptPageSourceReferences(workspace, translations)
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

func planConceptPageSourceReferences(workspace string, translations map[string]string) ([]plannedWrite, error) {
	if len(translations) == 0 {
		return nil, nil
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
			if path != root && filepath.Base(path) == "sources" {
				return filepath.SkipDir
			}
			return nil
		}
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
		data, err := readBoundedRegularFileWithin(workspace, relSlash)
		if err != nil {
			return err
		}
		updated, changed, err := rewriteMarkdownFrontmatterSourceReferences(data, translations)
		if err != nil {
			return fmt.Errorf("rewrite concept page %q: %w", path, err)
		}
		if changed {
			writes = append(writes, plannedWrite{rel: relSlash, data: updated})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return writes, nil
}

func rewriteMarkdownFrontmatterSourceReferences(data []byte, translations map[string]string) ([]byte, bool, error) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return data, false, nil
	}
	var matter map[string]interface{}
	body, err := fm.MustParse(strings.NewReader(string(data)), &matter)
	if err != nil {
		return nil, false, err
	}
	changed := rewriteSourceReferenceField(matter, "sources", translations)
	changed = rewriteSourceReferenceField(matter, "source", translations) || changed
	if !changed {
		return data, false, nil
	}
	frontmatter, err := yaml.Marshal(matter)
	if err != nil {
		return nil, false, err
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(frontmatter)
	out.WriteString("---\n")
	out.Write(body)
	return out.Bytes(), true, nil
}

func rewriteSourcePageID(data []byte, id, rawPath string) ([]byte, error) {
	var matter struct {
		SourceFile string `yaml:"source_file"`
	}
	if _, err := fm.MustParse(strings.NewReader(string(data)), &matter); err != nil {
		return nil, err
	}
	if strings.TrimSpace(matter.SourceFile) != rawPath {
		return nil, fmt.Errorf("frontmatter source_file %q does not match %q", matter.SourceFile, rawPath)
	}
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) == 0 || strings.TrimSpace(string(lines[0])) != "---" {
		return nil, errors.New("source frontmatter is missing")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if isFrontmatterDelimiter(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, errors.New("source frontmatter is unterminated")
	}
	found := false
	for i := 1; i < end; i++ {
		line := lineWithoutEnding(lines[i])
		key, _, hasValue := strings.Cut(line, ":")
		if hasValue && !startsWithYAMLIndent(line) && strings.TrimSpace(key) == "id" {
			if found {
				return nil, errors.New("duplicate source frontmatter id")
			}
			lines[i] = rewriteTopLevelIDLine(lines[i], id)
			found = true
		}
	}
	if !found {
		eol := lineEnding(lines[end])
		if eol == "" {
			eol = "\n"
		}
		lines = append(lines[:end], append([][]byte{[]byte("id: " + id + eol)}, lines[end:]...)...)
	}
	return bytes.Join(lines, nil), nil
}

func lineWithoutEnding(line []byte) string {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return string(line)
}

func lineEnding(line []byte) string {
	if bytes.HasSuffix(line, []byte("\r\n")) {
		return "\r\n"
	}
	if bytes.HasSuffix(line, []byte("\n")) {
		return "\n"
	}
	return ""
}

func isFrontmatterDelimiter(line []byte) bool {
	content := lineWithoutEnding(line)
	return !startsWithYAMLIndent(content) && strings.TrimRight(content, " \t") == "---"
}

func startsWithYAMLIndent(line string) bool {
	return line != "" && (line[0] == ' ' || line[0] == '\t')
}

func rewriteTopLevelIDLine(line []byte, id string) []byte {
	content := lineWithoutEnding(line)
	eol := lineEnding(line)
	colon := strings.IndexByte(content, ':')
	if colon < 0 {
		return line
	}
	rest := content[colon+1:]
	leading := len(rest) - len(strings.TrimLeft(rest, " \t"))
	comment := -1
	for i := leading; i < len(rest); i++ {
		if rest[i] == '#' && (i == leading || rest[i-1] == ' ' || rest[i-1] == '\t') {
			comment = i
			break
		}
	}
	suffix := ""
	if comment >= 0 {
		before := rest[:comment]
		suffix = before[len(strings.TrimRight(before, " \t")):] + rest[comment:]
	} else {
		suffix = rest[len(strings.TrimRight(rest, " \t")):]
	}
	return []byte(content[:colon+1] + rest[:leading] + id + suffix + eol)
}

func planSourcePageIDAndAnnotations(workspace string, sources []reconciledSource, prior []sourceSnapshot) ([]plannedWrite, error) {
	byRaw := make(map[string]sourceSnapshot, len(prior))
	for _, snapshot := range prior {
		byRaw[snapshot.RawPath] = snapshot
	}
	writes := make([]plannedWrite, 0, len(sources))
	for _, source := range sources {
		rel := filepath.ToSlash(filepath.Join("wiki", "sources", source.Slug+".md"))
		page, err := readBoundedRegularFileWithin(workspace, rel)
		if err != nil {
			return nil, fmt.Errorf("read generated source %q: %w", source.RawPath, err)
		}
		page, err = rewriteSourcePageID(page, source.StableID, source.RawPath)
		if err != nil {
			return nil, fmt.Errorf("reconcile generated source %q: %w", source.RawPath, err)
		}
		// Combine Source page ID + annotation strip/append into one planned write.
		if snapshot, ok := byRaw[source.RawPath]; ok {
			page, err = planSourceAnnotationTransform(page, source.StableID, snapshot)
			if err != nil {
				return nil, err
			}
		}
		writes = append(writes, plannedWrite{rel: rel, data: page})
	}
	return writes, nil
}

func planSourceAnnotationTransform(page []byte, stableID string, snapshot sourceSnapshot) ([]byte, error) {
	page, err := stripSystemAnnotationTrailer(page, stableID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(snapshot.AnnotationBody) != "" {
		trailer := "\n\n---\n\n## Human annotations (system)\n<!-- lwc-ann-v1 source_id=" + stableID + " ann_sha256=" + snapshot.AnnotationSHA + " -->\n" + annotation.Normalize(snapshot.AnnotationBody) + "\n"
		page = append(page, []byte(trailer)...)
	}
	return page, nil
}

func applyStableSourceAnnotations(workspace string, sources []reconciledSource, prior []sourceSnapshot) error {
	// Wrapper retained for existing unit tests. Production path plans ID +
	// annotation transforms together via planSourcePageIDAndAnnotations.
	byRaw := make(map[string]sourceSnapshot, len(prior))
	for _, snapshot := range prior {
		byRaw[snapshot.RawPath] = snapshot
	}
	for _, source := range sources {
		snapshot, ok := byRaw[source.RawPath]
		if !ok {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("wiki", "sources", source.Slug+".md"))
		page, err := readBoundedRegularFileWithin(workspace, rel)
		if err != nil {
			return err
		}
		page, err = planSourceAnnotationTransform(page, source.StableID, snapshot)
		if err != nil {
			return err
		}
		if err := writeFileAtomicWithin(workspace, rel, page); err != nil {
			return err
		}
	}
	return nil
}

func stripSystemAnnotationTrailer(data []byte, owners ...string) ([]byte, error) {
	lines := splitSourceLines(data)
	headingCount := strings.Count(string(data), "Human annotations (system)")
	markerCount := strings.Count(string(data), "lwc-ann-v1")
	markerPresent := headingCount > 0 || markerCount > 0
	var candidate *annotationTrailer
	for i := range lines {
		if lines[i].content != "## Human annotations (system)" {
			continue
		}
		if candidate != nil {
			return nil, errors.New("duplicate annotation trailers")
		}
		if i < 4 || lines[i-1].content != "" || lines[i-2].content != "---" || lines[i-3].content != "" || i+2 >= len(lines) {
			continue
		}
		comment := lines[i+1].content
		prefix := "<!-- lwc-ann-v1 source_id="
		if !strings.HasPrefix(comment, prefix) || !strings.HasSuffix(comment, " -->") {
			continue
		}
		sourceID, digest, ok := parseAnnotationHeader(strings.TrimPrefix(strings.TrimSuffix(comment, " -->"), prefix))
		if !ok || !annotation.ValidSourceID(sourceID) {
			continue
		}
		if len(owners) > 0 && sourceID != owners[0] {
			return nil, errors.New("annotation trailer owner mismatch")
		}
		if !strings.HasSuffix(string(data), "\n") {
			continue
		}
		finalEOL := 1
		if strings.HasSuffix(string(data), "\r\n") {
			finalEOL = 2
		}
		body := data[lines[i+2].start : len(data)-finalEOL]
		if len(body) == 0 || annotation.Digest(string(body)) != digest {
			continue
		}
		start := lines[i-4].end - len(lines[i-4].eol)
		candidate = &annotationTrailer{start: start}
	}
	if !markerPresent {
		return append([]byte(nil), data...), nil
	}
	if candidate == nil {
		return nil, errors.New("malformed annotation trailer")
	}
	if headingCount != 1 || markerCount != 1 {
		return nil, errors.New("duplicate annotation trailers")
	}
	return append([]byte(nil), data[:candidate.start]...), nil
}

type sourceLine struct {
	start, end int
	content    string
	eol        string
}

type annotationTrailer struct{ start int }

func splitSourceLines(data []byte) []sourceLine {
	lines := make([]sourceLine, 0)
	for start := 0; start < len(data); {
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			lines = append(lines, sourceLine{start: start, end: len(data), content: lineWithoutEnding(data[start:])})
			break
		}
		end += start + 1
		lines = append(lines, sourceLine{start: start, end: end, content: lineWithoutEnding(data[start:end]), eol: lineEnding(data[start:end])})
		start = end
	}
	return lines
}

func parseAnnotationHeader(header string) (string, string, bool) {
	sourceID, digest, ok := strings.Cut(header, " ann_sha256=")
	if !ok || sourceID == "" || len(digest) != 64 {
		return "", "", false
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", "", false
	}
	return sourceID, digest, true
}
