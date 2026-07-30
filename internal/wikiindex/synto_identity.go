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
	active, names, err := decodeSyntoIdentitySourceConcepts(document["source_concepts"])
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
		if IsSyntoRootPage(canonicalPath) {
			continue
		}
		if article.EntityID == "" {
			continue
		}
		if !annotation.ValidSourceID(article.EntityID) {
			return SyntoIdentityPlan{}, fmt.Errorf("unsafe Synto article entity_id %q", article.EntityID)
		}
		if owners := names[article.Name]; len(owners) > 0 {
			if len(owners) != 1 || !owners[article.EntityID] {
				return SyntoIdentityPlan{}, fmt.Errorf("Synto article/source entity disagreement for %q", slug)
			}
		}
		if previous, ok := seenEntities[article.EntityID]; ok {
			return SyntoIdentityPlan{}, fmt.Errorf("Synto entity_id %q maps to multiple articles %q and %q", article.EntityID, previous, article.Path)
		}
		path := canonicalPath
		seenEntities[article.EntityID] = path
		plan.ByPath[path] = article.EntityID
	}
	return plan, nil
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
				if article.EntityID == "" || !annotation.ValidSourceID(article.EntityID) {
					return nil, errors.New("invalid Synto article entity_id")
				}
			}
		}
		out = append(out, article)
	}
	return out, nil
}

func decodeSyntoIdentitySourceConcepts(data []byte) (map[string]bool, map[string]map[string]bool, error) {
	if !jsonContainer(data, '[') {
		return nil, nil, errors.New("Synto source_concepts must be a bounded array")
	}
	var groups []json.RawMessage
	if err := json.Unmarshal(data, &groups); err != nil || len(groups) > generation.MaxFiles {
		return nil, nil, errors.New("Synto source_concepts must be a bounded array")
	}
	active := make(map[string]bool)
	names := make(map[string]map[string]bool)
	for _, groupData := range groups {
		group, err := decodeStrictObject(groupData, map[string]bool{"source_path": true, "content_hash": true, "concepts": true})
		if err != nil {
			return nil, nil, fmt.Errorf("decode Synto source_concepts: %w", err)
		}
		for _, key := range []string{"source_path", "content_hash", "concepts"} {
			if _, ok := group[key]; !ok {
				return nil, nil, fmt.Errorf("missing Synto source_concepts field %q", key)
			}
		}
		var sourcePath, contentHash string
		if json.Unmarshal(group["source_path"], &sourcePath) != nil || !safeSyntoSourcePath(sourcePath) || json.Unmarshal(group["content_hash"], &contentHash) != nil || !validSyntoHash(contentHash) {
			return nil, nil, errors.New("invalid Synto source concept provenance")
		}
		if !jsonContainer(group["concepts"], '[') {
			return nil, nil, errors.New("Synto source concepts must be a bounded array")
		}
		var items []json.RawMessage
		if err := json.Unmarshal(group["concepts"], &items); err != nil || len(items) > generation.MaxFiles {
			return nil, nil, errors.New("Synto source concepts must be a bounded array")
		}
		for _, itemData := range items {
			item, err := decodeStrictObject(itemData, map[string]bool{"name": true, "entity_id": true})
			if err != nil {
				return nil, nil, fmt.Errorf("decode Synto source concept: %w", err)
			}
			var name, entityID string
			if err := json.Unmarshal(item["name"], &name); err != nil || name == "" || json.Unmarshal(item["entity_id"], &entityID) != nil || !annotation.ValidSourceID(entityID) {
				return nil, nil, errors.New("invalid Synto source concept identity")
			}
			active[entityID] = true
			if names[name] == nil {
				names[name] = make(map[string]bool)
			}
			names[name][entityID] = true
		}
	}
	return active, names, nil
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
		if !ok || seen[name] || !allowed[name] {
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
