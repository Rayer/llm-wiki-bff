package wikiindex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rayer/llm-wiki-bff/internal/annotation"
	"github.com/rayer/llm-wiki-bff/internal/generation"
)

// DecodeSyntoIdentityPlan decodes the bounded, schema-v1 Synto INDEX artifact
// used by both worker and admin rebuild paths. Only explicit article.entity_id
// values become Concept identity; source-concept entities are coverage
// requirements, never an inference source for entity-less articles.
func DecodeSyntoIdentityPlan(data []byte) (SyntoIdentityPlan, error) {
	if len(data) > generation.MaxFileBytes {
		return SyntoIdentityPlan{}, errors.New("Synto INDEX exceeds limit")
	}
	if err := validateReleasedSyntoIndex(data); err != nil {
		return SyntoIdentityPlan{}, fmt.Errorf("validate Synto INDEX: %w", err)
	}
	document, err := decodeStrictObject(data, map[string]bool{
		"schema_version": true, "pack": true, "articles": true, "terms": true,
		"papers": true, "sources": true, "source_concepts": true,
		"synthesis": true, "stats": true, "identity_log": true,
		"entity_aliases": true, "alias_denials": true,
	})
	if err != nil {
		return SyntoIdentityPlan{}, fmt.Errorf("decode Synto INDEX: %w", err)
	}
	for _, key := range []string{"schema_version", "pack", "articles", "terms", "papers", "sources", "source_concepts", "synthesis", "stats"} {
		if _, ok := document[key]; !ok {
			return SyntoIdentityPlan{}, fmt.Errorf("missing Synto INDEX field %q", key)
		}
	}
	var version int
	if err := json.Unmarshal(document["schema_version"], &version); err != nil || version != 1 {
		return SyntoIdentityPlan{}, errors.New("Synto INDEX schema_version must be 1")
	}
	for _, key := range []string{"articles", "terms", "papers", "sources", "source_concepts", "synthesis"} {
		if !jsonContainer(document[key], '[') {
			return SyntoIdentityPlan{}, fmt.Errorf("Synto INDEX field %q must be an array", key)
		}
	}
	for _, key := range []string{"pack", "stats"} {
		if !jsonContainer(document[key], '{') {
			return SyntoIdentityPlan{}, fmt.Errorf("Synto INDEX field %q must be an object", key)
		}
	}

	articles, err := decodeSyntoIdentityArticles(document["articles"])
	if err != nil {
		return SyntoIdentityPlan{}, err
	}
	active, err := decodeSyntoIdentitySourceConcepts(document["source_concepts"])
	if err != nil {
		return SyntoIdentityPlan{}, err
	}

	plan := SyntoIdentityPlan{ByPath: make(map[string]string), ActiveEntities: active}
	seenIDs := make(map[string]string, len(articles))
	seenSlugs := make(map[string]string, len(articles))
	seenEntities := make(map[string]string, len(articles))
	for _, article := range articles {
		if article.ID == "" || !annotation.ValidSourceID(article.ID) {
			return SyntoIdentityPlan{}, fmt.Errorf("invalid Synto article ID for %q", article.Path)
		}
		if previous, ok := seenIDs[article.ID]; ok {
			return SyntoIdentityPlan{}, fmt.Errorf("duplicate Synto article ID %q for %q and %q", article.ID, previous, article.Path)
		}
		seenIDs[article.ID] = article.Path
		slug, err := syntoIdentitySlug(article.Path)
		if err != nil {
			return SyntoIdentityPlan{}, err
		}
		key := strings.ToLower(slug)
		if previous, ok := seenSlugs[key]; ok {
			return SyntoIdentityPlan{}, fmt.Errorf("duplicate Synto article slug %q for %q and %q", slug, previous, article.Path)
		}
		seenSlugs[key] = article.Path
		canonicalPath := "wiki/" + slug + ".md"
		if article.EntityID != "" {
			if !ValidSyntoEntityID(article.EntityID) {
				return SyntoIdentityPlan{}, fmt.Errorf("unsafe Synto article entity_id %q", article.EntityID)
			}
			if previous, ok := seenEntities[article.EntityID]; ok {
				return SyntoIdentityPlan{}, fmt.Errorf("Synto entity_id %q maps to multiple articles %q and %q", article.EntityID, previous, article.Path)
			}
			seenEntities[article.EntityID] = canonicalPath
		}
		if IsSyntoRootPage(canonicalPath) {
			continue
		}
		if article.EntityID == "" {
			continue
		}
		path := canonicalPath
		plan.ByPath[path] = article.EntityID
	}
	return plan, nil
}

func validateReleasedSyntoIndex(data []byte) error {
	document, err := decodeStrictObject(data, map[string]bool{
		"schema_version": true, "pack": true, "articles": true, "terms": true,
		"papers": true, "sources": true, "source_concepts": true,
		"synthesis": true, "stats": true, "identity_log": true,
		"entity_aliases": true, "alias_denials": true,
	})
	if err != nil {
		return err
	}
	for _, key := range []string{"schema_version", "pack", "articles", "terms", "papers", "sources", "source_concepts", "synthesis", "stats"} {
		if _, ok := document[key]; !ok {
			return fmt.Errorf("missing Synto INDEX field %q", key)
		}
	}
	if string(bytes.TrimSpace(document["schema_version"])) != "1" {
		return errors.New("Synto INDEX schema_version must be 1")
	}
	if err := validateReleasedSyntoPack(document["pack"]); err != nil {
		return err
	}
	if err := validateReleasedSyntoStats(document["stats"]); err != nil {
		return err
	}
	if err := validateReleasedSyntoArticles(document["articles"]); err != nil {
		return err
	}
	for _, key := range []string{"terms", "papers"} {
		if _, err := releasedSyntoArray(document[key], key); err != nil {
			return err
		}
	}
	if err := validateReleasedSyntoSources(document["sources"]); err != nil {
		return err
	}
	if err := validateReleasedSyntoSourceConcepts(document["source_concepts"]); err != nil {
		return err
	}
	if err := validateReleasedSyntoSynthesis(document["synthesis"]); err != nil {
		return err
	}
	for _, key := range []string{"identity_log", "entity_aliases", "alias_denials"} {
		if raw, ok := document[key]; ok {
			if _, err := releasedSyntoArray(raw, key); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateReleasedSyntoPack(data []byte) error {
	object, err := decodeStrictObject(data, map[string]bool{"id": true, "name": true, "version": true, "language": true, "capabilities": true})
	if err != nil {
		return fmt.Errorf("invalid Synto pack: %w", err)
	}
	for _, key := range []string{"id", "name", "version", "language", "capabilities"} {
		if _, ok := object[key]; !ok {
			return fmt.Errorf("missing pack field %q", key)
		}
	}
	for _, key := range []string{"id", "name", "version"} {
		if value, err := releasedSyntoString(object[key], 1024, true); err != nil {
			return fmt.Errorf("invalid pack field %q: %w", key, err)
		} else if value == "" {
			return fmt.Errorf("invalid pack field %q: empty string", key)
		}
	}
	for _, key := range []string{"language", "capabilities"} {
		values, err := releasedSyntoStringArray(object[key], 1024)
		if err != nil {
			return fmt.Errorf("invalid pack field %q: %w", key, err)
		}
		if len(values) == 0 {
			return fmt.Errorf("invalid pack field %q: empty array", key)
		}
		if key == "capabilities" {
			for _, value := range values {
				switch value {
				case "articles", "concepts", "segments", "lifecycle":
				default:
					return fmt.Errorf("invalid pack capability %q", value)
				}
			}
		}
	}
	return nil
}

func validateReleasedSyntoStats(data []byte) error {
	allowed := map[string]bool{"article_count": true, "draft_count": true, "concept_count": true, "alias_count": true, "knowledge_item_count": true, "source_count": true, "source_segment_count": true, "failed_note_count": true, "failed_concept_count": true}
	object, err := decodeStrictObject(data, allowed)
	if err != nil {
		return fmt.Errorf("invalid Synto stats: %w", err)
	}
	for key := range allowed {
		raw, ok := object[key]
		if !ok {
			return fmt.Errorf("missing stats field %q", key)
		}
		trimmed := bytes.TrimSpace(raw)
		if bytes.Equal(trimmed, []byte("null")) {
			return fmt.Errorf("invalid Synto stats value for %q", key)
		}
		var value int64
		if err := json.Unmarshal(trimmed, &value); err != nil || value < 0 {
			return fmt.Errorf("invalid Synto stats value for %q", key)
		}
	}
	return nil
}

func validateReleasedSyntoArticles(data []byte) error {
	articles, err := releasedSyntoArray(data, "articles")
	if err != nil {
		return err
	}
	for _, article := range articles {
		object, err := decodeStrictObject(article, map[string]bool{"id": true, "entity_id": true, "name": true, "path": true, "summary": true, "tags": true, "aliases": true, "confidence": true})
		if err != nil {
			return fmt.Errorf("invalid Synto article: %w", err)
		}
		for _, key := range []string{"id", "name", "path", "summary", "tags", "aliases", "confidence"} {
			if _, ok := object[key]; !ok {
				return fmt.Errorf("missing Synto article field %q", key)
			}
		}
		for _, key := range []string{"id", "name", "path"} {
			if value, err := releasedSyntoString(object[key], generation.MaxPathBytes, true); err != nil || value == "" {
				return fmt.Errorf("invalid Synto article %q", key)
			}
		}
		if raw, ok := object["entity_id"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if value, err := releasedSyntoString(raw, 1024, true); err != nil || value == "" || !ValidSyntoEntityID(value) {
				return errors.New("invalid Synto article entity_id")
			}
		}
		if err := releasedSyntoNullableString(object["summary"], 1<<20); err != nil {
			return err
		}
		for _, key := range []string{"tags", "aliases"} {
			if _, err := releasedSyntoStringArray(object[key], 4096); err != nil {
				return fmt.Errorf("invalid Synto article %q: %w", key, err)
			}
		}
		if err := validateReleasedSyntoConfidence(object["confidence"]); err != nil {
			return err
		}
	}
	return nil
}

func validateReleasedSyntoSources(data []byte) error {
	items, err := releasedSyntoArray(data, "sources")
	if err != nil {
		return err
	}
	for _, item := range items {
		object, err := decodeStrictObject(item, map[string]bool{"id": true, "title": true, "source_type": true})
		if err != nil {
			return fmt.Errorf("invalid Synto source entry: %w", err)
		}
		for _, key := range []string{"id", "title", "source_type"} {
			if _, ok := object[key]; !ok {
				return fmt.Errorf("missing Synto source field %q", key)
			}
		}
		id, err := releasedSyntoString(object["id"], 1024, false)
		if err != nil || !safeReleasedSyntoSourceID(id) {
			return errors.New("invalid Synto source entry id")
		}
		if err := releasedSyntoNullableString(object["title"], 4096); err != nil {
			return err
		}
		if sourceType, err := releasedSyntoString(object["source_type"], 256, true); err != nil || sourceType == "" {
			return errors.New("invalid Synto source entry type")
		}
	}
	return nil
}

func safeReleasedSyntoSourceID(value string) bool {
	if strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false
	}
	if len(value) >= 2 && value[1] == ':' {
		first := value[0]
		if ('A' <= first && first <= 'Z') || ('a' <= first && first <= 'z') {
			return false
		}
	}
	return value != "." && safeSyntoSourcePath(value)
}

func validateReleasedSyntoSourceConcepts(data []byte) error {
	groups, err := releasedSyntoArray(data, "source_concepts")
	if err != nil {
		return err
	}
	seenGroups := make(map[string]struct{}, len(groups))
	for _, groupData := range groups {
		group, err := decodeStrictObject(groupData, map[string]bool{"source_path": true, "content_hash": true, "concepts": true})
		if err != nil {
			return fmt.Errorf("invalid Synto source_concepts group: %w", err)
		}
		for _, key := range []string{"source_path", "content_hash", "concepts"} {
			if _, ok := group[key]; !ok {
				return fmt.Errorf("missing Synto source_concepts field %q", key)
			}
		}
		sourcePath, err := releasedSyntoString(group["source_path"], generation.MaxPathBytes, true)
		if err != nil || !safeSyntoSourcePath(sourcePath) {
			return errors.New("invalid Synto source concept source_path")
		}
		if _, exists := seenGroups[sourcePath]; exists {
			return fmt.Errorf("duplicate Synto source_concepts group %q", sourcePath)
		}
		seenGroups[sourcePath] = struct{}{}
		hash, err := releasedSyntoString(group["content_hash"], 256, true)
		if err != nil || !validSyntoHash(hash) {
			return errors.New("invalid Synto source concept content_hash")
		}
		items, err := releasedSyntoArray(group["concepts"], "source concepts")
		if err != nil {
			return err
		}
		seenItems := make(map[string]struct{}, len(items))
		for _, itemData := range items {
			item, err := decodeStrictObject(itemData, map[string]bool{"name": true, "entity_id": true})
			if err != nil {
				return fmt.Errorf("invalid Synto source concept: %w", err)
			}
			name, err := releasedSyntoString(item["name"], 4096, true)
			entityID, entityErr := releasedSyntoString(item["entity_id"], 1024, true)
			if err != nil || entityErr != nil || name == "" || !ValidSyntoEntityID(entityID) {
				return errors.New("invalid Synto source concept identity")
			}
			semantic := name + "\x00" + entityID
			if _, exists := seenItems[semantic]; exists {
				return fmt.Errorf("duplicate Synto source concept %q", name)
			}
			seenItems[semantic] = struct{}{}
		}
	}
	return nil
}

func validateReleasedSyntoSynthesis(data []byte) error {
	items, err := releasedSyntoArray(data, "synthesis")
	if err != nil {
		return err
	}
	for _, item := range items {
		object, err := decodeStrictObject(item, map[string]bool{"path": true, "title": true})
		if err != nil {
			return fmt.Errorf("invalid Synto synthesis entry: %w", err)
		}
		pathValue, pathErr := releasedSyntoString(object["path"], generation.MaxPathBytes, true)
		title, titleErr := releasedSyntoString(object["title"], 4096, true)
		if pathErr != nil || titleErr != nil || !safeSyntoSourcePath(pathValue) || title == "" {
			return errors.New("invalid Synto synthesis entry")
		}
	}
	return nil
}

func releasedSyntoArray(data []byte, name string) ([]json.RawMessage, error) {
	if !jsonContainer(data, '[') {
		return nil, fmt.Errorf("Synto INDEX field %q must be an array", name)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil || len(items) > generation.MaxFiles {
		return nil, fmt.Errorf("Synto INDEX field %q must be a bounded array", name)
	}
	for _, item := range items {
		if err := validateReleasedSyntoJSONValue(item); err != nil {
			return nil, fmt.Errorf("invalid Synto INDEX field %q: %w", name, err)
		}
	}
	return items, nil
}

const maxReleasedSyntoJSONDepth = 64

func validateReleasedSyntoJSONValue(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decodeReleasedSyntoJSONValue(decoder, 0); err != nil {
		return err
	}
	return generation.EnsureJSONEOF(decoder)
}

func decodeReleasedSyntoJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxReleasedSyntoJSONDepth {
		return errors.New("JSON nesting depth exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			if len(seen) >= generation.MaxFiles {
				return generation.ErrLogicalEntryLimit
			}
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("expected JSON object key")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			seen[key] = struct{}{}
			if err := decodeReleasedSyntoJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		count := 0
		for decoder.More() {
			if count >= generation.MaxFiles {
				return generation.ErrLogicalEntryLimit
			}
			if err := decodeReleasedSyntoJSONValue(decoder, depth+1); err != nil {
				return err
			}
			count++
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func releasedSyntoString(data []byte, max int, trim bool) (string, error) {
	var value string
	if err := json.Unmarshal(data, &value); err != nil || len(value) > max {
		return "", errors.New("expected bounded string")
	}
	if trim {
		value = strings.TrimSpace(value)
	}
	return value, nil
}

func releasedSyntoStringArray(data []byte, maxString int) ([]string, error) {
	items, err := releasedSyntoArray(data, "string array")
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		value, err := releasedSyntoString(item, maxString, false)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func releasedSyntoNullableString(data []byte, max int) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	_, err := releasedSyntoString(data, max, false)
	return err
}

func validateReleasedSyntoConfidence(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var value interface{}
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return err
	}
	switch value.(type) {
	case string, float64:
		return nil
	default:
		return errors.New("invalid article confidence")
	}
}

type syntoIdentityArticle struct {
	ID       string
	EntityID string
	Name     string
	Path     string
}

func decodeSyntoIdentityArticles(data []byte) ([]syntoIdentityArticle, error) {
	if !jsonContainer(data, '[') {
		return nil, errors.New("Synto INDEX articles must be a bounded array")
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) > generation.MaxFiles {
		return nil, errors.New("Synto INDEX articles must be a bounded array")
	}
	out := make([]syntoIdentityArticle, 0, len(raw))
	for _, item := range raw {
		object, err := decodeStrictObject(item, map[string]bool{
			"id": true, "entity_id": true, "name": true, "path": true,
			"summary": true, "tags": true, "aliases": true, "confidence": true,
		})
		if err != nil {
			return nil, fmt.Errorf("decode Synto article: %w", err)
		}
		for _, key := range []string{"id", "name", "path", "summary", "tags", "aliases", "confidence"} {
			if _, ok := object[key]; !ok {
				return nil, fmt.Errorf("missing Synto article field %q", key)
			}
		}
		article := syntoIdentityArticle{}
		for key, target := range map[string]*string{"id": &article.ID, "name": &article.Name, "path": &article.Path} {
			if err := json.Unmarshal(object[key], target); err != nil || *target == "" {
				return nil, fmt.Errorf("invalid Synto article %q", key)
			}
		}
		if entity, ok := object["entity_id"]; ok {
			var entityID *string
			if err := json.Unmarshal(entity, &entityID); err != nil {
				return nil, errors.New("invalid Synto article entity_id")
			}
			if entityID == nil {
				article.EntityID = ""
			} else {
				article.EntityID = strings.TrimSpace(*entityID)
				if article.EntityID == "" || !ValidSyntoEntityID(article.EntityID) {
					return nil, errors.New("invalid Synto article entity_id")
				}
			}
		}
		out = append(out, article)
	}
	return out, nil
}

func decodeSyntoIdentitySourceConcepts(data []byte) (map[string]bool, error) {
	if !jsonContainer(data, '[') {
		return nil, errors.New("Synto source_concepts must be a bounded array")
	}
	var groups []json.RawMessage
	if err := json.Unmarshal(data, &groups); err != nil || len(groups) > generation.MaxFiles {
		return nil, errors.New("Synto source_concepts must be a bounded array")
	}
	active := make(map[string]bool)
	for _, groupData := range groups {
		group, err := decodeStrictObject(groupData, map[string]bool{"source_path": true, "content_hash": true, "concepts": true})
		if err != nil {
			return nil, fmt.Errorf("decode Synto source_concepts: %w", err)
		}
		for _, key := range []string{"source_path", "content_hash", "concepts"} {
			if _, ok := group[key]; !ok {
				return nil, fmt.Errorf("missing Synto source_concepts field %q", key)
			}
		}
		var sourcePath, contentHash string
		if json.Unmarshal(group["source_path"], &sourcePath) != nil || !safeSyntoSourcePath(sourcePath) || json.Unmarshal(group["content_hash"], &contentHash) != nil || !validSyntoHash(contentHash) {
			return nil, errors.New("invalid Synto source concept provenance")
		}
		if !jsonContainer(group["concepts"], '[') {
			return nil, errors.New("Synto source concepts must be a bounded array")
		}
		var items []json.RawMessage
		if err := json.Unmarshal(group["concepts"], &items); err != nil || len(items) > generation.MaxFiles {
			return nil, errors.New("Synto source concepts must be a bounded array")
		}
		for _, itemData := range items {
			item, err := decodeStrictObject(itemData, map[string]bool{"name": true, "entity_id": true})
			if err != nil {
				return nil, fmt.Errorf("decode Synto source concept: %w", err)
			}
			var name, entityID string
			if err := json.Unmarshal(item["name"], &name); err != nil || name == "" || json.Unmarshal(item["entity_id"], &entityID) != nil || !ValidSyntoEntityID(entityID) {
				return nil, errors.New("invalid Synto source concept identity")
			}
			active[entityID] = true
		}
	}
	return active, nil
}

func jsonContainer(data []byte, opening byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && trimmed[0] == opening
}

func safeSyntoSourcePath(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.HasPrefix(value, "/") && !strings.ContainsAny(value, "\\\x00") && !strings.Contains(value, "..") && !strings.Contains(value, "//")
}

func validSyntoHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func syntoIdentitySlug(value string) (string, error) {
	if strings.HasPrefix(value, "articles/") {
		value = "wiki/" + strings.TrimPrefix(value, "articles/")
	}
	if !validSyntoArticlePath(value) {
		return "", fmt.Errorf("unsafe Synto article path %q", value)
	}
	return strings.TrimSuffix(strings.TrimPrefix(value, "wiki/"), ".md"), nil
}

func decodeStrictObject(data []byte, allowed map[string]bool) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var object map[string]json.RawMessage
	if err := dec.Decode(&object); err != nil || object == nil {
		return nil, errors.New("expected JSON object")
	}
	if err := generation.EnsureJSONEOF(dec); err != nil {
		return nil, err
	}
	// Re-decode with tokens because encoding/json's map decode otherwise hides
	// duplicate keys.
	dec = json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	seen := make(map[string]bool, len(object))
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok || seen[name] || (allowed != nil && !allowed[name]) {
			return nil, fmt.Errorf("invalid or duplicate JSON object field %q", name)
		}
		seen[name] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return object, nil
}
